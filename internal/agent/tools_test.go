package agent

import (
	"context"
	"encoding/json"
	"errors"
	"minipi/internal/llm"
	"strings"
	"testing"
)

// ---- 测试专用工具 ----

type failTool struct{}

func (failTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "fail", Description: "always fails",
		InputSchema: map[string]any{"type": "object"}}
}
func (failTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	return "", errors.New("boom")
}

type panicTool struct{}

func (panicTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "panic", Description: "always panics",
		InputSchema: map[string]any{"type": "object"}}
}
func (panicTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	panic("kaboom")
}

// 场景 1：并行执行 —— 一轮两个调用，两条结果都到位且配对
func TestParallelTools(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{ToolCalls: []llm.ToolCall{
			{ID: "p1", Name: "get_time", Input: "{}"},
			{ID: "p2", Name: "calc", Input: `{"a":3,"b":4,"op":"*"}`},
		}},
		{Text: "done", StopReason: llm.StopReasonEndTurn},
	}}
	st := newTestState(m)
	st.Tools.Register(NewCalcTool())

	msgs, err := Run(context.Background(), st, "time and 3*4")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var pairs []llm.Message
	for _, msg := range msgs {
		if msg.Role == llm.RoleToolResult {
			pairs = append(pairs, msg)
		}
	}
	if len(pairs) != 2 {
		t.Fatalf("toolResults = %d, want 2", len(pairs))
	}
	ids := map[string]string{}
	for _, p := range pairs {
		ids[p.ToolCallID] = p.Content
	}
	if _, ok := ids["p1"]; !ok {
		t.Error("missing result for p1")
	}
	if ids["p2"] != "12" {
		t.Errorf("calc result = %q, want 12", ids["p2"])
	}
}

// 场景 2：错误回喂 —— 工具失败不中断循环，错误文本进 toolResult
func TestToolErrorFeedsBack(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "f1", Name: "fail"}}},
		{Text: "好的，我换个方式回答", StopReason: llm.StopReasonEndTurn},
	}}
	st := newTestState(m)
	st.Tools.Register(failTool{})

	msgs, err := Run(context.Background(), st, "do it")
	if err != nil { // ★ 核心断言：错误回喂，循环正常结束
		t.Fatalf("Run: %v (tool error must not abort the loop)", err)
	}
	if len(m.Calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(m.Calls))
	}
	// 第二轮请求里，toolResult 应包含 error 前缀
	feedback := m.Calls[1].Messages[2]
	if feedback.Role != llm.RoleToolResult || !strings.Contains(feedback.Content, "error: boom") {
		t.Errorf("feedback = %+v", feedback)
	}
	_ = msgs
}

// 场景 3：panic 防护 —— 工具 panic 被折叠进结果，Agent 不崩
func TestToolPanicRecovered(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "k1", Name: "panic"}}},
		{Text: "ok", StopReason: llm.StopReasonEndTurn},
	}}
	st := newTestState(m)
	st.Tools.Register(panicTool{})

	msgs, err := Run(context.Background(), st, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, msg := range msgs {
		if msg.Role == llm.RoleToolResult &&
			!strings.Contains(msg.Content, "panicked") {
			t.Errorf("panic not folded into result: %q", msg.Content)
		}
	}
}
