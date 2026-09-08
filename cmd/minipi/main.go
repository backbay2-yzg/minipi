package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"minipi/internal/agent"
	"minipi/internal/llm"
)

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	bus := agent.NewEventBus(128)
	st := &agent.AgentState{
		Model:        m,
		SystemPrompt: "You are a helpful assistant. Answer concisely.",
		Tools: agent.NewRegistry( // ★ 真实工具上线
			agent.NewTimeTool(),
			agent.NewCalcTool(),
		),
		// ★ Day 4：滑动窗口。教学期建议先设 2，肉眼观察裁剪生效后再调大
		Transforms: []agent.TransformFunc{agent.PruneByTurns(2)},
		Bus:        bus, // ★ Day 5：接上总线
	}

	// 订阅者：独立 goroutine 持续消费（Emit 阻塞投递的纪律要求）。
	// streamed 标记让订阅者对 Provider 无感：
	//   流式（OpenAI）→ 打 delta；非流式→ 打 message_end 的完整文本
	ch, cancel := bus.Subscribe()
	defer cancel()
	streamed := false

	go func() {
		for ev := range ch {
			switch ev.Kind {
			case agent.EventMessageUpdate:
				streamed = true
				fmt.Print(ev.Delta)
			case agent.EventMessageEnd:
				if streamed {
					fmt.Println()
				} else {
					fmt.Println(ev.Text)
				}
				streamed = false
			case agent.EventToolExecEnd:
				fmt.Printf("  [tool %s] %s\n", ev.ToolCall.Name, ev.Result)
			}
		}
	}()

	fmt.Printf("mini-pi day5 | provider=%s model=%s\n", provider, model)
	fmt.Println("输入消息回车发送；/new 清空会话；/quit 退出")

	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !sc.Scan() {
			return
		}
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			continue
		case line == "/quit", line == "/exit":
			return
		case line == "/new":
			st.Messages = nil
			st.TotalUsage = llm.TokenUsage{}
			// 同步换一个 Provider 会话 ID（OpenCode Go 靠它做路由与缓存优化）
			if s, ok := m.(llm.Sessioned); ok {
				s.NewSession()
			}
			fmt.Println("(session cleared)")
			continue
		case line == "/history":
			for i, m := range st.Messages {
				fmt.Printf("%d - Role: %s , Content: %s \n", i, m.Role, m.Content)
			}
			continue
		}

		// 事务式回滚：记住本轮起点，失败则截断回起点。
		// 避免半截轮次污染历史（Anthropic 要求 user/assistant 交替）
		turnStart := len(st.Messages)
		inBefore := st.TotalUsage.InputTokens // ★ 记录本轮起点，算每轮增量

		msgs, err := agent.Run(ctx, st, line)
		if err != nil {
			fmt.Println("error:", err)
			st.Messages = st.Messages[:turnStart]
			continue
		}
		// ★ 注意：助手回答不再从返回值打印 —— 它由订阅者经事件输出
		//（流式打 delta / 非流式打 message_end 文本），避免双份。
		fmt.Printf("  [usage] turn in=%d | total in=%d out=%d\n",
			st.TotalUsage.InputTokens-inBefore,
			st.TotalUsage.InputTokens, st.TotalUsage.OutputTokens)
		_ = msgs // 保留返回值供未来逻辑用（持久化等）

		//// 正常出口的最后一条消息必是 assistant
		//fmt.Println(msgs[len(msgs)-1].Content)
		//
		//for _, m := range msgs[turnStart:] {
		//	if m.Role == llm.RoleToolResult {
		//		fmt.Printf("  [tool %s] %s\n", m.ToolCallID, m.Content)
		//	}
		//}
		//fmt.Printf("  [usage] turn in=%d | total in=%d out=%d\n",
		//	st.TotalUsage.InputTokens-inBefore,
		//	st.TotalUsage.InputTokens, st.TotalUsage.OutputTokens)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
