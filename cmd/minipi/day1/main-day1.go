package main

import (
	"bufio"
	"context"
	"fmt"
	"minipi/internal/llm"
	"os"
	"os/signal"
	"strings"
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

	// 模型名更新很快，以官方文档为准；MINIPI_MODEL 可覆盖
	model := envOr("MINIPI_MODEL", defaultModel)

	m, err := llm.New(llm.Config{
		Provider: provider,
		APIKey:   apiKey,
		Model:    model,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(0)
	}
	// Ctrl+C 会取消当前请求（ctx 取消穿透到 HTTP），输入 /quit 正常退出
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Printf("mini-pi day1 | provider=%s model=%s\n", provider, model)
	fmt.Println("输入消息回车发送；/new 清空历史；/quit 退出")

	sc := bufio.NewScanner(os.Stdin)
	var history []llm.Message
	for {
		fmt.Printf("> ")
		if !sc.Scan() {
			return
		}

		line := strings.TrimSpace(sc.Text())
		switch line {
		case "":
			continue
		case "/quit", "/exit":
			return
		case "/new":
			history = nil
			fmt.Println("(history cleared)")
			continue
		}

		history = append(history, llm.Message{
			Role:    llm.RoleUser,
			Content: line,
		})
		out, err := m.Complete(ctx, llm.CompletionRequest{
			Messages: history,
			System:   "You are a helpful assistant. Answer concisely.",
		})
		if err != nil {
			fmt.Println("errpr:", err)
			history = history[:len(history)-1]
			continue
		}
		fmt.Println(out.Text)
		fmt.Printf("  [usage] in=%d out=%d | stop=%s\n", out.Usage.InputTokens, out.Usage.OutputTokens, out.StopReason)

		history = append(history, llm.Message{
			Role:    llm.RoleAssistant,
			Content: out.Text,
		})
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
