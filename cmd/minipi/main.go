package main

import (
	"bufio"
	"context"
	"fmt"
	"minipi/internal/session"
	"os"
	"os/signal"
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

	fmt.Printf("mini-pi day6 | provider=%s model=%s | session=%s\n", provider, model, sess.ID)
	fmt.Println("输入消息回车发送，运行中可直接输入插话(steer)；/info 会话信息；/list 历史会话；/new 新会话；/quit 退出")

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
			switch line {
			case "/quit", "/exit":
				return
			case "/new":
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
			case "/info":
				fmt.Printf("  session %s | messages %d (%d turns) | usage in=%d out=%d\n",
					sess.ID,
					len(st.Messages),
					agent.CountTransactions(st.Messages),
					st.TotalUsage.InputTokens,
					st.TotalUsage.OutputTokens)
				continue
			case "/list":
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
