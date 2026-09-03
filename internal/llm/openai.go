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

type OpenAI struct {
	apiKey  string
	model   string
	baseURL string
	HTTP    *http.Client
}

func NewOpenAI(apikey, baseURL, model string) *OpenAI {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAI{
		apiKey:  apikey,
		model:   model,
		baseURL: baseURL,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // 注意：是 JSON 字符串，不是对象
	} `json:"function"`
}

type oaToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type oaTool struct {
	Type     string     `json:"type"` // "function"
	Function oaToolSpec `json:"function"`
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`   // assistant 发起调用时携带
	ToolCallID string       `json:"tool_call_id,omitempty"` // role:"tool" 时回指调用
}
type oaRequest struct {
	Model       string      `json:"model"`
	Messages    []oaMessage `json:"messages"`
	Tools       []oaTool    `json:"tools,omitempty"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
	Temperature *float32    `json:"temperature,omitempty"`
}

type oaResponse struct {
	Choices []struct {
		Message struct {
			Role      string       `json:"role"`
			Content   string       `json:"content"`
			ToolCalls []oaToolCall `json:"tool_calls,omitempty"` // ★ 响应侧新字段
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (o *OpenAI) Complete(ctx context.Context, req CompletionRequest) (*Completion, error) {
	msgs := make([]oaMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, oaMessage{
			Role:    "system",
			Content: req.System,
		})
	}

	// 1a. 声明工具：ToolSpec → oaTool
	var tools []oaTool
	for _, s := range req.Tools {
		tools = append(tools, oaTool{
			Type: "function",
			Function: oaToolSpec{
				Name:        s.Name,
				Description: s.Description,
				Parameters:  s.InputSchema,
			},
		})

	}
	// 1b. 消息编码：toolResult 是独立 role:"tool" 的消息
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			// 同 Day 1：抽到顶层 system（OpenAI 也接受消息形式，保持原实现即可）
			msgs = append(msgs, oaMessage{
				Role:    "system",
				Content: m.Content,
			})
		case RoleUser, RoleAssistant:
			om := oaMessage{
				Role:    string(m.Role),
				Content: m.Content,
			}
			for _, tc := range m.ToolCalls {
				om.ToolCalls = append(om.ToolCalls, oaToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: tc.Name, Arguments: tc.Input},
				})
			}
			msgs = append(msgs, om)
		case RoleToolResult:
			msgs = append(msgs, oaMessage{
				Role:       "tool", // ★ OpenAI 的工具结果是独立 role,
				Content:    m.Content,
				ToolCallID: m.ToolCallID,
			})
		}
	}

	// 2. 构造请求。关键：NewRequestWithContext 让 ctx 的取消/超时穿透到 HTTP 层
	body, err := json.Marshal(oaRequest{
		Model:     o.model,
		Messages:  msgs,
		MaxTokens: req.MaxTokens,
		Tools:     tools,
	})
	if err != nil {
		return nil, fmt.Errorf("openai: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai:build reqeust: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		buf.ReadFrom(io.LimitReader(resp.Body, 4096)) // 错误详情在 body 里
		return nil, &APIError{
			Provider:   "openai",
			StatusCode: resp.StatusCode,
			Body:       buf.String(),
		}
	}

	// 4. 解析 + 语义翻译
	var out oaResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("openai: decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("openai: empty choices")
	}
	c := out.Choices[0]
	// 2. 响应解析：tool_calls → 统一 ToolCall（Input 保持字符串）
	var calls []ToolCall
	for _, tc := range c.Message.ToolCalls {
		calls = append(calls, ToolCall{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: tc.Function.Arguments,
		})

	}
	return &Completion{
		Text:       c.Message.Content,
		StopReason: mapOpenAIStop(c.FinishReason),
		ToolCalls:  calls,
		Usage: TokenUsage{
			InputTokens:  out.Usage.PromptTokens,
			OutputTokens: out.Usage.CompletionTokens,
		},
	}, nil
}

func mapOpenAIStop(reason string) StopReason {
	switch reason {
	case "tool_calls":
		return StopReasonToolUse
	case "length":
		return StopReasonMaxTokens
	default: // "stop" 等
		return StopReasonEndTurn

	}
}
