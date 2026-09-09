package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 手法沿用 TestOpenAIComplete：httptest 起一个假 OpenCode Zen，
// 验证 Chat Completions 的请求构造与响应解析
func TestOpenCodeComplete(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody ocRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": "hello zen"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3},
		})
	}))
	defer srv.Close()

	m := NewOpenCode("zen-key", srv.URL, "grok-code-fast-1")
	out, err := m.Complete(context.Background(), CompletionRequest{
		System:    "be brief",
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// 断言 1：端点与鉴权
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer zen-key" {
		t.Errorf("auth = %q", gotAuth)
	}

	// 断言 2：model + 标准字段名 max_tokens（上游拒绝一切多余字段）
	if gotBody.Model != "grok-code-fast-1" {
		t.Errorf("model = %q", gotBody.Model)
	}
	if gotBody.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", gotBody.MaxTokens)
	}

	// 断言 3：System 参数变成第一条 system 消息，后跟 user 消息
	if len(gotBody.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(gotBody.Messages))
	}
	if gotBody.Messages[0].Role != "system" || gotBody.Messages[0].Content != "be brief" {
		t.Errorf("messages[0] = %+v", gotBody.Messages[0])
	}
	if gotBody.Messages[1].Role != "user" || gotBody.Messages[1].Content != "hi" {
		t.Errorf("messages[1] = %+v", gotBody.Messages[1])
	}

	// 断言 4：响应解析与语义翻译
	if out.Text != "hello zen" {
		t.Errorf("text = %q", out.Text)
	}
	if out.StopReason != StopReasonEndTurn {
		t.Errorf("stop = %q, want end_turn", out.StopReason)
	}
	if out.Usage.InputTokens != 7 || out.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v, want 7/3", out.Usage)
	}
}

// 工具全链路：请求里带 tools + assistant.tool_calls + tool 结果，
// 响应里返回 tool_calls → 翻译成统一 ToolCall 与 StopReasonToolUse
func TestOpenCodeToolCalls(t *testing.T) {
	var gotBody ocRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "let me read it",
					"tool_calls": []any{map[string]any{
						"id":   "call_2",
						"type": "function",
						"function": map[string]any{
							"name":      "read",
							"arguments": `{"path":"go.mod"}`,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer srv.Close()

	m := NewOpenCode("zen-key", srv.URL, "grok-code-fast-1")
	out, err := m.Complete(context.Background(), CompletionRequest{
		Messages: []Message{
			{Role: RoleUser, Content: "read go.mod"},
			// 历史里的工具调用：应变成 assistant.tool_calls
			{Role: RoleAssistant, Content: "checking", ToolCalls: []ToolCall{
				{ID: "call_1", Name: "ls", Input: `{"dir":"."}`},
			}},
			// 工具结果：应变成 role:"tool"，靠 tool_call_id 配对
			{Role: RoleToolResult, ToolCallID: "call_1", Content: "file listing"},
		},
		Tools: []ToolSpec{{
			Name:        "read",
			Description: "read a file",
			InputSchema: map[string]any{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// 断言 1：工具描述翻译成 type:"function" 嵌套格式
	if len(gotBody.Tools) != 1 || gotBody.Tools[0].Type != "function" ||
		gotBody.Tools[0].Function.Name != "read" ||
		gotBody.Tools[0].Function.Description != "read a file" {
		t.Fatalf("tools = %+v", gotBody.Tools)
	}

	// 断言 2：三条消息依次为 user、assistant(带 tool_calls)、tool 结果
	if len(gotBody.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(gotBody.Messages))
	}
	am, tm := gotBody.Messages[1], gotBody.Messages[2]
	if am.Role != "assistant" || am.Content != "checking" || len(am.ToolCalls) != 1 {
		t.Fatalf("assistant message = %+v", am)
	}
	tc := am.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" ||
		tc.Function.Name != "ls" || tc.Function.Arguments != `{"dir":"."}` {
		t.Errorf("assistant tool_call = %+v", tc)
	}
	if tm.Role != "tool" || tm.ToolCallID != "call_1" || tm.Content != "file listing" {
		t.Errorf("tool message = %+v", tm)
	}

	// 断言 3：响应中的 tool_calls 被翻译成统一 ToolCall
	if len(out.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(out.ToolCalls))
	}
	if out.ToolCalls[0].ID != "call_2" || out.ToolCalls[0].Name != "read" ||
		out.ToolCalls[0].Input != `{"path":"go.mod"}` {
		t.Errorf("tool call = %+v", out.ToolCalls[0])
	}

	// 断言 4：finish_reason "tool_calls" → StopReasonToolUse
	if out.StopReason != StopReasonToolUse {
		t.Errorf("stop = %q, want tool_use", out.StopReason)
	}
	if out.Text != "let me read it" {
		t.Errorf("text = %q", out.Text)
	}
}

// finish_reason "length" → StopReasonMaxTokens
func TestOpenCodeMaxTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": "cut off..."},
				"finish_reason": "length",
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 100},
		})
	}))
	defer srv.Close()

	m := NewOpenCode("k", srv.URL, "m")
	out, err := m.Complete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.StopReason != StopReasonMaxTokens {
		t.Errorf("stop = %q, want max_tokens", out.StopReason)
	}
	if out.Text != "cut off..." {
		t.Errorf("text = %q", out.Text)
	}
}

// 非 200 → *APIError，Provider 标记为 opencode（复现上游 400 校验错误）
func TestOpenCodeAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"Extra inputs are not permitted"}}`))
	}))
	defer srv.Close()

	m := NewOpenCode("bad-key", srv.URL, "m")
	_, err := m.Complete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
	if apiErr.Provider != "opencode" {
		t.Errorf("provider = %q, want opencode", apiErr.Provider)
	}
	if !strings.Contains(apiErr.Body, "Extra inputs are not permitted") {
		t.Errorf("body = %q", apiErr.Body)
	}
}

// 空 choices → 报错，不返回零值 Completion
func TestOpenCodeEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	}))
	defer srv.Close()

	m := NewOpenCode("k", srv.URL, "m")
	if _, err := m.Complete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected error for empty choices")
	}
}

// 活跃的 SSE 即使总时长超过空闲阈值也应完成。GLM 的 reasoning_content
// 当前不会展示，但它仍是有效流量，必须为连接续期。
func TestOpenCodeStreamAllowsLongActiveResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 7; i++ {
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}`)
			fmt.Fprintln(w)
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: [DONE]`)
		fmt.Fprintln(w)
		flusher.Flush()
	}))
	defer srv.Close()

	m := NewOpenCode("k", srv.URL, "m")
	m.sseIdleTimeout = 150 * time.Millisecond
	if m.HTTP.Timeout != 0 {
		t.Fatalf("streaming client has whole-request timeout %v, want 0", m.HTTP.Timeout)
	}
	started := time.Now()
	out, err := m.StreamComplete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("StreamComplete: %v", err)
	}
	if elapsed := time.Since(started); elapsed <= m.sseIdleTimeout {
		t.Fatalf("test stream lasted %v, want longer than idle timeout %v", elapsed, m.sseIdleTimeout)
	}
	if out.Text != "done" || out.StopReason != StopReasonEndTurn {
		t.Fatalf("completion = %+v", out)
	}
}

// 响应头已经到达但响应体不再产生任何数据时，看门狗应主动解除读取阻塞。
func TestOpenCodeStreamIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := NewOpenCode("k", srv.URL, "m")
	m.sseIdleTimeout = 40 * time.Millisecond
	_, err := m.StreamComplete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "sse idle") {
		t.Fatalf("err = %v, want SSE idle timeout", err)
	}
}

// 工厂注册：New 认得 "opencode"
func TestNewOpenCodeProvider(t *testing.T) {
	m, err := New(Config{Provider: "opencode", APIKey: "k", Model: "grok-code-fast-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	oc, ok := m.(*OpenCode)
	if !ok {
		t.Fatalf("New returned %T, want *OpenCode", m)
	}
	if oc.baseURL != "https://opencode.ai/zen/go/v1" {
		t.Errorf("default baseURL = %q", oc.baseURL)
	}
}
