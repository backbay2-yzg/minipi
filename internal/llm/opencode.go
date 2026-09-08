package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
// openCodeUserAgent 与文档要求一致：客户端要用自己的名字标识自己，
// 不能发通用 HTTP 库名（Go 默认的 "Go-http-client/1.1" 会被当成 SDK/库流量）。
const openCodeUserAgent = "minipi/0.1"

type OpenCode struct {
	apiKey  string
	model   string
	baseURL string
	HTTP    *http.Client
	// sessionID 在一次会话内保持稳定，随请求放进 x-opencode-session 头。
	// 缺失该头 → 400 {"type":"MissingSessionID"}。
	sessionID string
}

func NewOpenCode(apiKey, baseURL, model string) *OpenCode {
	if baseURL == "" {
		baseURL = "https://opencode.ai/zen/go/v1"
	}
	return &OpenCode{
		apiKey:    apiKey,
		model:     model,
		baseURL:   baseURL,
		HTTP:      &http.Client{Timeout: 120 * time.Second, Transport: newOpenCodeTransport()},
		sessionID: newOpenCodeSessionID(),
	}
}

// newOpenCodeTransport 显式配置底层连接参数（默认的 DefaultTransport 行为不一致且不可见）。
// Proxy: ProxyFromEnvironment 是关键——设置 HTTPS_PROXY/HTTPS_PROXY 即可走代理，
// 无需改代码。对直连困难的网络，这是最有效的解药。
func newOpenCodeTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second, // TCP 建连
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   20 * time.Second, // 默认 10s，弱网下太紧
		ResponseHeaderTimeout: 60 * time.Second, // 只等响应头，不影响流式 body 的读取
		ExpectContinueTimeout: 5 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     true,
	}
}

// newOpenCodeSessionID 生成 16 字节随机会话 ID。
// 随机源不可用时退化为时间戳：宁可不够随机，也不能让请求因缺头被 400 拒掉。
func newOpenCodeSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// NewSession 结束当前会话并启用一个新会话 ID（对应 /new 清空对话）。
// 上层用类型断言探测，方式与 StreamingModel 一致：
//
//	if s, ok := model.(llm.Sessioned); ok { s.NewSession() }
func (o *OpenCode) NewSession() string {
	o.sessionID = newOpenCodeSessionID()
	return o.sessionID
}

func (o *OpenCode) SessionID() string { return o.sessionID }

// setHeaders 集中放置 OpenCode Go 对客户端的三个硬性要求，
// Complete 与 StreamComplete 共用，避免流式路径漏掉会话头。
func (o *OpenCode) setHeaders(h http.Header) {
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+o.apiKey)
	h.Set("x-opencode-session", o.sessionID)
	h.Set("User-Agent", openCodeUserAgent)
}

// do 发一次请求；仅在"连接级失败"（还没拿到响应，如 TLS 握手超时、连接被重置）时重试。
//
// 重试的边界很重要：一旦拿到响应头就立刻返回、绝不重试 ——
// 否则流式的 body 会被读两次，且可能重复计费。
func (o *OpenCode) do(ctx context.Context, body []byte) (*http.Response, error) {
	const maxAttempts = 3
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			o.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("opencode: build request: %w", err)
		}
		o.setHeaders(httpReq.Header)

		resp, err := o.HTTP.Do(httpReq)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// 用户主动取消 / 超时 → 不再重试
		if ctx.Err() != nil {
			break
		}
		if attempt < maxAttempts {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond) // 0.5s → 1s
		}
	}
	return nil, lastErr
}

// ---- wire types（Chat Completions）----

type ocFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
}

// ocToolCall 是 assistant 消息里的工具调用
type ocToolCall struct {
	// Index 仅流式分片用到：同一调用的多个 delta 靠它关联。
	// 带上 omitempty → 非流式请求里不会出现该字段（上游校验极严，
	// 宁可少发也别多发；解析响应时有没有它都不影响）。
	Index    int        `json:"index,omitempty"`
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

// ocStreamOptions 只在流式请求里出现（stream_options 是 Chat Completions 标准字段，
// 不是自造字段）。若上游真的因它报 "Extra inputs are not permitted"，
// 把 StreamComplete 里那一行注掉即可，其余解析逻辑不受影响。
type ocStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ocRequest struct {
	Model     string       `json:"model"`
	Messages  []ocMessage  `json:"messages"`
	MaxTokens int          `json:"max_tokens,omitempty"` // 注意：标准字段名，不能写 maxTokens
	Tools     []ocToolSpec `json:"tools,omitempty"`
	// ★ 流式三件套之一：仅 StreamComplete 会置位，Complete 保持零值 → omitempty 不发送
	Stream        bool             `json:"stream,omitempty"`
	StreamOptions *ocStreamOptions `json:"stream_options,omitempty"`
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

	resp, err := o.do(ctx, body)
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
