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

	st := &agent.AgentState{
		Model:        m,
		SystemPrompt: "You are a helpful assistant. Answer concisely.",
		Tools: agent.NewRegistry( // ★ 真实工具上线
			agent.NewTimeTool(),
			agent.NewCalcTool(),
		),
	}

	fmt.Printf("mini-pi day2 | provider=%s model=%s\n", provider, model)
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

		msgs, err := agent.Run(ctx, st, line)
		if err != nil {
			fmt.Println("error:", err)
			st.Messages = st.Messages[:turnStart]
			continue
		}

		// 正常出口的最后一条消息必是 assistant
		fmt.Println(msgs[len(msgs)-1].Content)
		fmt.Printf("  [usage] total in=%d out=%d\n",
			st.TotalUsage.InputTokens, st.TotalUsage.OutputTokens)

		for _, m := range msgs[turnStart:] {
			if m.Role == llm.RoleToolResult {
				fmt.Printf("  [tool %s] %s\n", m.ToolCallID, m.Content)
			}
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
