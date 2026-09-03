package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenCode 对接 OpenCode Zen 的 Go 套餐（https://opencode.ai/zen/go）。
//
// 协议是 OpenAI Chat Completions 兼容（POST /chat/completions）。
// 注意：不要发 Responses API 的 instructions/input 字段——上游校验极严，
// 多余字段会直接报 "Extra inputs are not permitted" 的 400。
// wire 字段必须严格标准：model / messages / max_tokens / tools /
// tool_calls / tool_call_id，一个自造字段都不能加。
type OpenCode struct {
	apiKey  string
	model   string
	baseURL string
	HTTP    *http.Client
}

func NewOpenCode(apiKey, baseURL, model string) *OpenCode {
	if baseURL == "" {
		baseURL = "https://opencode.ai/zen/go/v1"
	}
	return &OpenCode{
		apiKey:  apiKey,
		model:   model,
		baseURL: baseURL,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

// ---- wire types（Chat Completions）----

type ocFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
}

// ocToolCall 是 assistant 消息里的工具调用
type ocToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"` // "function"
	Function ocFunction `json:"function"`
}

type ocMessage struct {
	Role       string       `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content    string       `json:"content"`
	ToolCalls  []ocToolCall `json:"tool_calls,omitempty"`   // assistant 专属
	ToolCallID string       `json:"tool_call_id,omitempty"` // tool 结果专属
}

type ocFunctionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"` // JSON Schema
}

type ocToolSpec struct {
	Type     string         `json:"type"` // "function"
	Function ocFunctionSpec `json:"function"`
}

type ocRequest struct {
	Model     string       `json:"model"`
	Messages  []ocMessage  `json:"messages"`
	MaxTokens int          `json:"max_tokens,omitempty"` // 注意：标准字段名，不能写 maxTokens
	Tools     []ocToolSpec `json:"tools,omitempty"`
}

type ocChoice struct {
	Message struct {
		Role      string       `json:"role"`
		Content   string       `json:"content"`
		ToolCalls []ocToolCall `json:"tool_calls"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"` // "stop" | "tool_calls" | "length"
}

type ocResponse struct {
	Choices []ocChoice `json:"choices"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (o *OpenCode) Complete(ctx context.Context, req CompletionRequest) (*Completion, error) {
	body, err := json.Marshal(o.buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("opencode: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("opencode: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("opencode: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		buf.ReadFrom(io.LimitReader(resp.Body, 4096))
		return nil, &APIError{
			Provider:   "opencode",
			StatusCode: resp.StatusCode,
			Body:       buf.String(),
		}
	}

	var out ocResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("opencode: decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("opencode: empty choices")
	}

	c := out.Choices[0]
	toolCalls := make([]ToolCall, 0, len(c.Message.ToolCalls))
	for _, tc := range c.Message.ToolCalls {
		toolCalls = append(toolCalls, ToolCall{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: tc.Function.Arguments,
		})
	}

	return &Completion{
		Text:       c.Message.Content,
		ToolCalls:  toolCalls,
		StopReason: mapOpenCodeStop(c.FinishReason),
		Usage: TokenUsage{
			InputTokens:  out.Usage.PromptTokens,
			OutputTokens: out.Usage.CompletionTokens,
		},
	}, nil
}

// buildRequest 完成"统一格式 → Chat Completions"的翻译：
//  1. System 参数变成第一条 system 消息
//  2. assistant 的 ToolCalls → tool_calls 数组
//  3. toolResult → role:"tool" 消息，靠 tool_call_id 配对
func (o *OpenCode) buildRequest(req CompletionRequest) ocRequest {
	msgs := make([]ocMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, ocMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleToolResult:
			msgs = append(msgs, ocMessage{
				Role:       "tool",
				ToolCallID: m.ToolCallID,
				Content:    m.Content,
			})
		case RoleAssistant:
			msg := ocMessage{Role: "assistant", Content: m.Content}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, ocToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: ocFunction{
						Name:      tc.Name,
						Arguments: tc.Input,
					},
				})
			}
			msgs = append(msgs, msg)
		default: // user / system
			msgs = append(msgs, ocMessage{Role: string(m.Role), Content: m.Content})
		}
	}

	out := ocRequest{
		Model:     o.model,
		Messages:  msgs,
		MaxTokens: req.MaxTokens,
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, ocToolSpec{
			Type: "function",
			Function: ocFunctionSpec{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return out
}

func mapOpenCodeStop(reason string) StopReason {
	switch reason {
	case "tool_calls":
		return StopReasonToolUse
	case "length":
		return StopReasonMaxTokens
	default: // "stop" 等
		return StopReasonEndTurn
	}
}
