package llm

import (
	"context"
	"fmt"
)

type Model interface {
	Complete(ctx context.Context, req CompletionRequest) (*Completion, error)
}

// Sessioned 是可选能力：某些 Provider（如 OpenCode Go）要求按会话上报 ID，
// 客户端"开新会话"（/new）时需要换一个新的会话 ID。
// 与 StreamingModel 同款探测方式：
//
//	if s, ok := model.(llm.Sessioned); ok { s.NewSession() }
type Sessioned interface {
	NewSession() string // 返回新会话 ID
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
