package main

import (
	"bufio"
	"context"
	"fmt"
	"minipi/internal/session"
	"minipi/internal/workflow"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"minipi/internal/agent"
	"minipi/internal/llm"
)

const (
	systemPrompt       = "You are a helpful assistant. Answer concisely."
	compactThresholdTx = 8 // 轮数超过此值触发压缩
	compactKeepTx      = 4 // 压缩后保留最近轮数
)

// toolFactories 组合根的工具目录：持久化只存名字，恢复经此重建行为。
func toolFactories() map[string]func() agent.Tool {
	return map[string]func() agent.Tool{
		"get_time": func() agent.Tool { return agent.NewTimeTool() },
		"calc":     func() agent.Tool { return agent.NewCalcTool() },
	}
}

// buildTools 组装父子两级工具集（★ 反模式 57 的防线在这里）：
// base = 普通工具（子 Agent 可用）；parent = base + delegate（仅父可用）
func buildTools(m llm.Model, bus *agent.EventBus) (*agent.Registry, *agent.Registry) {
	base := agent.NewRegistry(agent.NewTimeTool(), agent.NewCalcTool())
	parent := agent.NewRegistry()
	parent.Register(agent.NewTimeTool())
	parent.Register(agent.NewCalcTool())
	parent.Register(agent.NewSubAgentTool(m, base, 4, bus)) // ★ 传 base，不传 parent
	return parent, base
}

func newSession(provider, model, baseURL, sysPrompt string, reg *agent.Registry) *session.Session {
	return &session.Session{
		ID:        session.NewID(),
		CreatedAt: time.Now(),
		Config: session.Config{
			Provider:     provider,
			ModelName:    model,
			BaseURL:      baseURL,
			SystemPrompt: sysPrompt,
			ToolNames:    reg.Names(),
		},
	}
}

func main() {
	provider := envOr("MINIPI_PROVIDER", "opencode")

	var apiKey, defaultModel string
	switch provider {
	case "openai":
		apiKey, defaultModel = os.Getenv("OPENAI_API_KEY"), "gpt-4o-mini"
	case "anthropic":
		apiKey, defaultModel = os.Getenv("ANTHROPIC_API_KEY"), "claude-sonnet-4-5"
	case "opencode":
		apiKey, defaultModel = os.Getenv("OPENCODE_API_KEY"), "glm-5.2"
	default:
		fmt.Fprintf(os.Stderr, "unknown MINIPI_PROVIDER: %s\n", provider)
		os.Exit(1)
	}
	model := envOr("MINIPI_MODEL", defaultModel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	m, err := llm.New(llm.Config{
		Provider: provider,
		APIKey:   apiKey,
		BaseURL:  os.Getenv("MINIPI_BASE_URL"),
		Model:    model,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	store, err := session.NewSQLiteStore(envOr("MINIPI_SESSION_DB", "minipi_sessions.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	bus := agent.NewEventBus(256)
	parentTools, baseTools := buildTools(m, bus)

	//os.Setenv("MINIPI_SESSION_ID", "20260908-215058-6ceea8f4")
	// ---- 会话：恢复或新建 ----
	// ★ 组装点：恢复路径 = 数据从磁盘来（sess.State）+ 行为从代码来（runtime）
	var st *agent.AgentState
	var sess *session.Session
	if id := os.Getenv("MINIPI_SESSION_ID"); id != "" {
		sess, err = store.Load(ctx, id)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resume:", err)
			os.Exit(1)
		}
		reg, rerr := agent.BuildRegistry(sess.Config.ToolNames, toolFactories())
		if rerr != nil {
			fmt.Fprintln(os.Stderr, rerr)
			os.Exit(1)
		}
		reg.Register(agent.NewSubAgentTool(m, baseTools, 4, bus))
		st = &agent.AgentState{
			Model:        m, // 当前环境的模型（设计决策：模型升级无缝）
			SystemPrompt: sess.Config.SystemPrompt,
			Tools:        reg,
			Transforms:   []agent.TransformFunc{agent.PruneByTurns(2)}, // 策略，不持久化
			Messages:     sess.State.Messages,
			TotalUsage:   sess.State.TotalUsage,
			Bus:          bus,
		}
		fmt.Printf("resumed %s (%d messages)\n", sess.ID, len(st.Messages))
	} else {
		st = &agent.AgentState{
			Model:        m,
			SystemPrompt: systemPrompt,
			Tools:        parentTools,
			Transforms:   []agent.TransformFunc{agent.PruneByTurns(2)},
			Bus:          bus,
		}

		sess = newSession(provider, model, os.Getenv("MINIPI_BASE_URL"), systemPrompt, baseTools)
	}

	// 单一订阅者按来源显示正文和思考，防止并发流无法辨认。
	ch, cancel := bus.Subscribe()
	defer cancel()
	go printEvents(os.Stdout, ch)

	fmt.Printf("mini-pi day8 | provider=%s model=%s | session=%s\n", provider, model, sess.ID)
	fmt.Println("回车发送；运行中可插话(steer)；/chain <主题> 链式工作流；/info /list /new /quit")

	inputCh := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			inputCh <- sc.Text()
		}
		close(inputCh)
	}()

	running := false
	done := make(chan error, 1)
	var turnStart, inBefore int

	for {
		select {
		case line, ok := <-inputCh:
			if !ok {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if running {
				if strings.HasPrefix(line, "/") {
					fmt.Println("  (命令在本轮运行中不可用，请等待完成)")
					continue
				}
				if serr := st.Steer(ctx, line); serr != nil {
					fmt.Println("steer error:", serr)
				} else {
					fmt.Printf("  [steer queued] %s\n", line)
				}
				continue
			}

			switch {
			case line == "/quit", line == "/exit":
				return
			case line == "/new":
				st.Messages = nil
				st.TotalUsage = llm.TokenUsage{}
				// 本地历史和 Provider 会话必须一起切换。否则新会话仍会沿用
				// 旧的 x-opencode-session，影响上游路由和提示缓存。
				if sessioned, ok := m.(llm.Sessioned); ok {
					sessioned.NewSession()
				}
				sess = newSession(provider, model, os.Getenv("MINIPI_BASE_URL"), systemPrompt, baseTools)
				fmt.Println("(new session:", sess.ID+")")
				continue
			case line == "/chain":
				fmt.Println("用法：/chain <主题>")
				continue
			case strings.HasPrefix(line, "/chain "):
				runChainDemo(ctx, m, bus, strings.TrimSpace(strings.TrimPrefix(line, "/chain")))
				continue
			case line == "/translate":
				fmt.Println("用法：/translate <English text>")
				continue
			case strings.HasPrefix(line, "/translate "):
				runTranslateDemo(ctx, m, bus, strings.TrimSpace(strings.TrimPrefix(line, "/translate")), inputCh)
				continue
			case line == "/info":
				fmt.Printf("  session %s | messages %d (%d turns) | usage in=%d out=%d\n",
					sess.ID,
					len(st.Messages),
					agent.CountTransactions(st.Messages),
					st.TotalUsage.InputTokens,
					st.TotalUsage.OutputTokens)
				continue
			case line == "/list":
				metas, lerr := store.List(ctx)
				if lerr != nil {
					fmt.Println("list error:", lerr)
					continue
				}
				for _, mt := range metas {
					fmt.Printf("  %s  msgs=%-3d  %s\n", mt.ID, mt.Messages, mt.Title)
				}
				continue
			}
			// 发起一轮：Run 在独立 goroutine，主循环继续接收插话
			running = true
			turnStart, inBefore = len(st.Messages), st.TotalUsage.InputTokens
			go func(input string) {
				_, rerr := agent.Run(ctx, st, input)
				done <- rerr
			}(line)
		case rerr := <-done:
			running = false
			if rerr != nil {
				fmt.Println("run error:", rerr)
				st.Messages = st.Messages[:turnStart] //回滚含本轮已注入的 steer：失败轮次的插话一并作废
				continue
			}
			// 压缩检查：轮数超阈值 → 压缩（显式、独立于 Run；失败非致命）
			if n := agent.CountTransactions(st.Messages); n > compactThresholdTx {
				if _, cerr := agent.Compact(ctx, st, compactKeepTx); cerr != nil {
					fmt.Println("compaction error:", cerr)
				}
			}

			// 事务点：Run 成功（可能已压缩）→ 状态一致 → 落盘
			// json.Marshal 是快照语义：此刻序列化，之后的 append 不影响文件内容
			sess.State = session.State{Messages: st.Messages, TotalUsage: st.TotalUsage}
			sess.UpdatedAt = time.Now()
			if serr := store.Save(ctx, sess); serr != nil {
				fmt.Println("save error:", serr)
			}
			fmt.Printf("  [usage] turn in=%d | total in=%d out=%d\n",
				st.TotalUsage.InputTokens-inBefore,
				st.TotalUsage.InputTokens, st.TotalUsage.OutputTokens)
		case <-ctx.Done():
			return
		}

	}
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// runChainDemo 演示 Prompt Chaining：提纲 →（门）→ 正文。
// 同步执行：链运行期间不接受输入（含插话）——已知简化，
// 异步化 + steering 贯通留作作业 2。
// bus 用于把链内各环节的流式增量出到订阅者界面；nil 则全程静默。
func runChainDemo(ctx context.Context, m llm.Model, bus *agent.EventBus, topic string) {
	outlineRe := regexp.MustCompile(`(?m)^\s*\d+[.、]`)

	chain := []workflow.ChainStep{
		{
			Stage: workflow.Stage{
				Name: "outline",
				SystemPrompt: "你是提纲编辑。针对给定主题输出一份提纲，" +
					"每个要点一行，以数字编号开头（如 1.）。不要输出其他内容。",
			},
			Gate: func(ctx context.Context, out string) error {
				if n := len(outlineRe.FindAllString(out, -1)); n < 2 {
					return fmt.Errorf("提纲仅 %d 个编号要点，至少需要 2 个", n)
				}
				return nil
			},
			Retries: 1,
		},
		{
			Stage: workflow.Stage{
				Name: "article",
				SystemPrompt: "你是技术作者。根据给定提纲撰写一篇短文，" +
					"逐点展开，总长 300 字以内。",
			},
		},
	}

	res, err := workflow.RunChain(ctx, m, bus, chain, topic)
	if res != nil && len(res.Outputs) > 0 {
		fmt.Println("── 提纲 ──")
		fmt.Println(res.Outputs[0])
	}
	if err != nil {
		fmt.Println("chain error:", err)
		return
	}
	fmt.Println("── 正文 ──")
	fmt.Println(res.Outputs[1])
	fmt.Printf("  [chain usage] in=%d out=%d\n",
		res.TotalUsage.InputTokens, res.TotalUsage.OutputTokens)
}

// runTranslateDemo 翻译三段链 + 人在环终门。
// 同步执行（同 runChainDemo）：链运行期间主 goroutine 阻塞在 RunChain 内，
// 人在环节点因此得以独占 inputCh——所有权契约见 HumanGate 注释。
// 已知简化：链内 AgentState 未接事件总线（Day 8 选做作业 3 的范围），
// 人在环门的 say 输出是链运行期间唯一的实时反馈。
func runTranslateDemo(ctx context.Context, m llm.Model, bus *agent.EventBus, src string, inputCh <-chan string) {
	steps := workflow.TranslateChain(src)
	// 终门 = 人在环：润色产物由用户把关；Retries=1 让否决理由驱动重做
	steps[2].Gate = workflow.HumanGate(inputCh, func(s string) { fmt.Println(s) })
	steps[2].Retries = 1

	fmt.Println("── 翻译链：翻译 → 审校（残留门）→ 润色（人工终门）──")
	res, err := workflow.RunChain(ctx, m, bus, steps, src)
	if err != nil {
		fmt.Println("chain error:", err)
		return
	}
	fmt.Println("── 终稿 ──")
	fmt.Println(res.Outputs[2])
	fmt.Printf("  [chain usage] in=%d out=%d\n",
		res.TotalUsage.InputTokens, res.TotalUsage.OutputTokens)
}
