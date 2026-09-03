package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Anthropic struct {
	apiKey  string
	model   string
	baseURL string
	HTTP    *http.Client
}

func NewAnthropic(apiKey, baseURL, model string) *Anthropic {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	return &Anthropic{
		apiKey:  apiKey,
		model:   model,
		baseURL: baseURL,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

// ---- wire types ----
// anBlock 用一个结构体覆盖四种 block：text / tool_use / tool_result。
// omitempty 保证不同类型各自只序列化自己的字段。
type anBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"` // text

	ID    string          `json:"id,omitempty"`    // tool_use
	Name  string          `json:"name,omitempty"`  // tool_use
	Input json.RawMessage `json:"input,omitempty"` // tool_use：对象（不是字符串！）

	ToolUseID string `json:"tool_use_id,omitempty"` // tool_result
	Content   string `json:"content,omitempty"`     // tool_result
}

type anMessage struct {
	Role    string    `json:"role"` // Anthropic 只有 "user" | "assistant"
	Content []anBlock `json:"content"`
}

type anTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anRequest struct {
	Model     string      `json:"model"`
	MaxTokens int         `json:"max_tokens"`
	System    string      `json:"system,omitempty"`
	Tools     []anTool    `json:"tools,omitempty"`
	Messages  []anMessage `json:"messages"`
}

type anResponse struct {
	Content    []anBlock `json:"content"`
	StopReason string    `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (a *Anthropic) Complete(ctx context.Context, req CompletionRequest) (*Completion, error) {
	system, msgs := a.convert(req.Messages)

	// 请求：ToolSpec → anTool
	var tools []anTool
	for _, s := range req.Tools {
		tools = append(tools, anTool{
			Name: s.Name, Description: s.Description, InputSchema: s.InputSchema,
		})
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096 // Anthropic 必填，调用方没给就给默认
	}

	body, err := json.Marshal(anRequest{
		Model:     a.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  msgs,
		Tools:     tools,
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		buf.ReadFrom(io.LimitReader(resp.Body, 4096))
		return nil, &APIError{Provider: "anthropic", StatusCode: resp.StatusCode, Body: buf.String()}
	}

	var out anResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("anthropic: decode: %w", err)
	}

	// 响应：text block 拼接 + tool_use block 解析
	var sb strings.Builder
	var calls []ToolCall
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "tool_use":
			calls = append(calls, ToolCall{
				ID:    b.ID,
				Name:  b.Name,
				Input: string(b.Input),
			})

		}
	}
	return &Completion{
		Text:       sb.String(),
		StopReason: mapAnthropicStop(out.StopReason),
		ToolCalls:  calls,
		Usage: TokenUsage{
			InputTokens:  out.Usage.InputTokens,
			OutputTokens: out.Usage.OutputTokens,
		},
	}, nil
}

// convert 完成"统一格式 → Anthropic 约束"的翻译：
//  1. 抽取所有 system 消息合并成顶层 system 参数
//  2. toolResult 暂不处理（Day 3：转为 user 消息里的 tool_result block）
//  3. 合并连续同角色消息（Anthropic 要求 user/assistant 交替）

// convert: 统一消息 → Anthropic 约束
//   - system 抽为顶层参数
//   - assistant 的 ToolCalls → content 里的 tool_use block
//   - toolResult → user 消息里的 tool_result block（★ 不是独立 role！）
//   - 连续同角色合并（并行工具的多条 toolResult → 一条 user 消息的多个 block）
func (a *Anthropic) convert(msgs []Message) (system string, out []anMessage) {
	var sys strings.Builder

	appendBlock := func(role string, b anBlock) {
		if len(out) > 0 && out[len(out)-1].Role == role {
			last := &out[len(out)-1]
			last.Content = append(last.Content, b)
		} else {
			out = append(out, anMessage{Role: role, Content: []anBlock{b}})
		}
	}

	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			if sys.Len() > 0 {
				sys.WriteString("\n")
			}
			sys.WriteString(m.Content)
		case RoleUser:
			appendBlock("user", anBlock{Type: "text", Text: m.Content})
		case RoleAssistant:
			if m.Content != "" {
				appendBlock("assistant", anBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				appendBlock("assistant", anBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: json.RawMessage(orEmptyJSON(tc.Input)),
				})
			}
		case RoleToolResult:
			appendBlock("user", anBlock{ // ★ 关键：user 消息 + tool_result block
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			})
		}
	}
	return sys.String(), out
}

func mapAnthropicStop(reason string) StopReason {
	switch reason {
	case "tool_use":
		return StopReasonToolUse
	case "max_tokens":
		return StopReasonMaxTokens
	default: // "end_turn"、"stop_sequence" 等
		return StopReasonEndTurn
	}
}

// orEmptyJSON: Anthropic 的 tool_use.input 必须是 JSON 对象（空参也要 {}）
func orEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}
