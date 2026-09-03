package llm

import (
	"context"
	"fmt"
)

type Model interface {
	Complete(ctx context.Context, req CompletionRequest) (*Completion, error)
}
type Config struct {
	Provider string // "openai" | "anthropic" | "opencode"
	APIKey   string
	BaseURL  string // 可选：兼容端点（代理、DeepSeek 等）
	Model    string // 如 gpt-4o-mini
}

func New(cfg Config) (Model, error) {
	switch cfg.Provider {
	case "openai":
		return NewOpenAI(cfg.APIKey, cfg.BaseURL, cfg.Model), nil
	case "anthropic":
		return NewAnthropic(cfg.APIKey, cfg.BaseURL, cfg.Model), nil
	case "opencode": // OpenCode Zen Go 套餐，OpenAI Chat Completions 兼容
		return NewOpenCode(cfg.APIKey, cfg.BaseURL, cfg.Model), nil
	default:
		return nil, fmt.Errorf("llm: unknown provider %q", cfg.Provider)
	}
}

type APIError struct {
	Provider   string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm: %s api error http %d: %s", e.Provider, e.StatusCode, e.Body)
}
