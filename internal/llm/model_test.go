package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 手法 1：httptest 起一个假 OpenAI，验证请求构造与响应解析
func TestOpenAIComplete(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody oaRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": "hello!"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
	defer srv.Close()

	m := NewOpenAI("test-key", srv.URL, "gpt-4o-mini")
	out, err := m.Complete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth = %q", gotAuth)
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Role != "user" {
		t.Errorf("wire messages = %+v", gotBody.Messages)
	}
	if out.Text != "hello!" {
		t.Errorf("text = %q", out.Text)
	}
	if out.StopReason != StopReasonEndTurn {
		t.Errorf("stop = %q, want end_turn", out.StopReason)
	}
	if out.Usage.OutputTokens != 2 {
		t.Errorf("output tokens = %d", out.Usage.OutputTokens)
	}
}
func TestAnthropicConvert(t *testing.T) {
	a := NewAnthropic("k", "http://unused", "claude-x")
	msgs := []Message{
		{Role: RoleSystem, Content: "be brief"},
		{Role: RoleUser, Content: "hi"},
		{Role: RoleUser, Content: "again"}, // 连续 user → 应合并
		{Role: RoleAssistant, Content: "hello"},
	}

	system, out := a.convert(msgs)

	// 断言 1：system 被抽到顶层参数
	if system != "be brief" {
		t.Errorf("system = %q", system)
	}

	// 断言 2：system 不进 out；两条连续 user 合并成 1 条；assistant 单独 1 条
	// 4 条输入 - 1(system 提取) - 1(连续合并) = 2
	if len(out) != 2 {
		t.Fatalf("want 2 wire messages, got %d", len(out))
	}

	// 断言 3：角色顺序保持
	if out[0].Role != "user" || out[1].Role != "assistant" {
		t.Errorf("roles = %q, %q; want user, assistant", out[0].Role, out[1].Role)
	}

	// 断言 4：合并后的 blocks 在 out[0]（user 那条）
	if len(out[0].Content) != 2 {
		t.Errorf("consecutive user should merge into 2 text blocks, got %d",
			len(out[0].Content))
	}
	if len(out[0].Content) == 2 &&
		(out[0].Content[0].Text != "hi" || out[0].Content[1].Text != "again") {
		t.Errorf("merged blocks = %+v", out[0].Content)
	}

	// 断言 5：assistant 未被合并
	if len(out[1].Content) != 1 {
		t.Errorf("assistant should have 1 text block, got %d", len(out[1].Content))
	}
}

// 手法 3：MockModel 自身的契约测试
func TestMockModel(t *testing.T) {
	m := &MockModel{Responses: []Completion{
		{Text: "first", StopReason: StopReasonEndTurn},
	}}
	out, err := m.Complete(context.Background(), CompletionRequest{})
	if err != nil || out.Text != "first" {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if _, err := m.Complete(context.Background(), CompletionRequest{}); err == nil {
		t.Fatal("expected error when queue is empty")
	}
	if len(m.Calls) != 2 {
		t.Errorf("calls = %d, want 2", len(m.Calls))
	}
}

// OpenAI：请求侧带 tools + toolResult 编码正确；响应侧解析 tool_calls
func TestOpenAIToolRoundTrip(t *testing.T) {
	var gotReq oaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id":   "call_9",
						"type": "function",
						"function": map[string]any{
							"name": "calc", "arguments": `{"a":1,"b":2,"op":"+"}`,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer srv.Close()

	m := NewOpenAI("k", srv.URL, "gpt-4o-mini")
	out, err := m.Complete(context.Background(), CompletionRequest{
		Messages: []Message{
			{Role: RoleUser, Content: "1+2=?"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "call_8", Name: "calc", Input: `{"a":1,"b":2,"op":"+"}`},
			}},
			{Role: RoleToolResult, ToolCallID: "call_8", Content: "3"},
		},
		Tools: []ToolSpec{{Name: "calc", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// 请求侧：工具声明 + toolResult 编码
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Function.Name != "calc" {
		t.Errorf("req tools = %+v", gotReq.Tools)
	}
	var toolMsg *oaMessage
	for i := range gotReq.Messages {
		if gotReq.Messages[i].Role == "tool" {
			toolMsg = &gotReq.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no role:tool message in request")
	}
	if toolMsg.ToolCallID != "call_8" || toolMsg.Content != "3" {
		t.Errorf("tool msg = %+v", *toolMsg)
	}

	// 响应侧：tool_calls 解析
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].ID != "call_9" ||
		out.ToolCalls[0].Name != "calc" ||
		out.ToolCalls[0].Input != `{"a":1,"b":2,"op":"+"}` {
		t.Errorf("out.ToolCalls = %+v", out.ToolCalls)
	}
	if out.StopReason != StopReasonToolUse {
		t.Errorf("stop = %s", out.StopReason)
	}
}

// Anthropic：toolResult 编码为 user 消息的 tool_result block；
// 并行两条 toolResult 合并为一条消息两个 block
func TestAnthropicConvertToolResult(t *testing.T) {
	a := NewAnthropic("k", "http://unused", "claude-x")
	msgs := []Message{
		{Role: RoleUser, Content: "time and 1+2"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "get_time", Input: "{}"},
			{ID: "c2", Name: "calc", Input: `{"a":1,"b":2,"op":"+"}`},
		}},
		{Role: RoleToolResult, ToolCallID: "c1", Content: "2026-08-31T18:00:00+08:00"},
		{Role: RoleToolResult, ToolCallID: "c2", Content: "3"},
	}

	_, out := a.convert(msgs)

	// user → assistant(2 个 tool_use) → user(2 个 tool_result) = 3 条
	if len(out) != 3 {
		t.Fatalf("wire messages = %d, want 3", len(out))
	}
	if out[1].Role != "assistant" || len(out[1].Content) != 2 ||
		out[1].Content[0].Type != "tool_use" || out[1].Content[1].ID != "c2" {
		t.Errorf("assistant blocks = %+v", out[1].Content)
	}
	if out[2].Role != "user" || len(out[2].Content) != 2 {
		t.Fatalf("result msg = %+v", out[2])
	}
	r1, r2 := out[2].Content[0], out[2].Content[1]
	if r1.Type != "tool_result" || r1.ToolUseID != "c1" {
		t.Errorf("block0 = %+v", r1)
	}
	if r2.Type != "tool_result" || r2.ToolUseID != "c2" || r2.Content != "3" {
		t.Errorf("block1 = %+v", r2)
	}
}

// model_test.go 追加：Anthropic 工具往返（本 bug 的回归测试）
func TestAnthropicToolRoundTrip(t *testing.T) {
	var gotReq anRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []any{map[string]any{
				"type": "tool_use", "id": "tu_1", "name": "calc",
				"input": map[string]any{"a": 1, "b": 2, "op": "+"},
			}},
			"stop_reason": "tool_use",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
		})
	}))
	defer srv.Close()

	m := NewAnthropic("k", srv.URL, "claude-x")
	out, err := m.Complete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "1+2"}},
		Tools: []ToolSpec{{
			Name: "calc", Description: "d",
			InputSchema: map[string]any{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// ★ 请求侧：tools 必须真的发出去
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Name != "calc" {
		t.Errorf("req tools = %+v, want 1 calc", gotReq.Tools)
	}

	// 响应侧：tool_use block 正确解析为统一 ToolCall
	if len(out.ToolCalls) != 1 {
		t.Fatalf("out.ToolCalls = %+v", out.ToolCalls)
	}
	tc := out.ToolCalls[0]
	if tc.ID != "tu_1" || tc.Name != "calc" {
		t.Errorf("toolCall = %+v", tc)
	}
	var args struct {
		A, B float64
		Op   string
	}
	if err := json.Unmarshal([]byte(tc.Input), &args); err != nil {
		t.Fatalf("input is not a JSON object: %v", err)
	}
	if args.A != 1 || args.Op != "+" {
		t.Errorf("parsed args = %+v", args)
	}
	if out.StopReason != StopReasonToolUse {
		t.Errorf("stop = %s", out.StopReason)
	}
}

// 场景：SSE 流式 —— 文本分片回调 + tool_calls 跨片按 index 重组 + 尾部 usage
func TestOpenAIStreamComplete(t *testing.T) {
	var gotBody oaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		lines := []string{
			`data: {"choices":[{"delta":{"content":"Hel"}}]}`, "",
			`data: {"choices":[{"delta":{"content":"lo"}}]}`, "",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c9","type":"function",` +
				`"function":{"name":"calc","arguments":"{\"a\":1"}}]}}]}`, "", // 首片：id+name+参数前半
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,` +
				`"function":{"arguments":",\"b\":2}"}}]}}]}`, "", // 次片：参数后半
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, "",
			`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`, "",
			`data: [DONE]`, "",
		}
		for _, l := range lines {
			w.Write([]byte(l + "\n"))
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()

	m := NewOpenAI("k", srv.URL, "gpt-4o-mini")
	var deltas []string
	out, err := m.StreamComplete(context.Background(), CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("StreamComplete: %v", err)
	}

	// 请求侧 ★：流式三件套必须发出
	if !gotBody.Stream {
		t.Error("request stream = false, want true")
	}
	if gotBody.StreamOptions == nil || !gotBody.StreamOptions.IncludeUsage {
		t.Error("request stream_options.include_usage not set")
	}

	// delta 回调：顺序与拼接
	if strings.Join(deltas, "") != "Hello" {
		t.Errorf("deltas = %q", deltas)
	}
	// tool_calls 重组：跨片 arguments 拼接成功
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].ID != "c9" ||
		out.ToolCalls[0].Name != "calc" {
		t.Fatalf("toolCalls = %+v", out.ToolCalls)
	}
	var args struct {
		A, B int
	}
	if err := json.Unmarshal([]byte(out.ToolCalls[0].Input), &args); err != nil {
		t.Fatalf("assembled args = %q: %v", out.ToolCalls[0].Input, err)
	}
	if args.A != 1 || args.B != 2 {
		t.Errorf("args = %+v", args)
	}
	if out.StopReason != StopReasonToolUse || out.Usage.OutputTokens != 3 {
		t.Errorf("stop=%s usage=%+v", out.StopReason, out.Usage)
	}
}
