package agent

import (
	"context"
	"errors"
	"minipi/internal/llm"
	"strings"
	"testing"
	"time"
)

func newTestState(m llm.Model) *AgentState {
	return &AgentState{
		SystemPrompt: "test",
		Model:        m,
		Tools:        NewRegistry(NewTimeTool()),
	}
}

// 场景 1：纯文本问答 —— 一次调用即结束
func TestRunPlainText(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "你好！", StopReason: llm.StopReasonEndTurn,
			Usage: llm.TokenUsage{InputTokens: 5, OutputTokens: 7}},
	}}
	st := newTestState(m)

	msgs, err := Run(context.Background(), st, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(m.Calls) != 1 {
		t.Fatalf("model calls = %d, want 1", len(m.Calls))
	}
	// 第一次调用就带上了 system 和工具声明
	if m.Calls[0].System != "test" {
		t.Errorf("system = %q", m.Calls[0].System)
	}
	if len(m.Calls[0].Tools) != 1 || m.Calls[0].Tools[0].Name != "get_time" {
		t.Errorf("tools = %+v", m.Calls[0].Tools)
	}
	// 消息序列：user → assistant
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[1].Role != llm.RoleAssistant {
		t.Errorf("roles = %s, %s", msgs[0].Role, msgs[1].Role)
	}
	if msgs[1].Content != "你好！" {
		t.Errorf("answer = %q", msgs[1].Content)
	}
	// 遥测累计
	if st.TotalUsage.OutputTokens != 7 {
		t.Errorf("total usage = %+v", st.TotalUsage)
	}
}

// 场景 2：一次完整工具轮次 —— 今天的主角
// 剧本：第 1 次返回工具请求，第 2 次返回最终回答
func TestRunLoopWithToolCall(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{
			ToolCalls:  []llm.ToolCall{{ID: "call_1", Name: "get_time", Input: "{}"}},
			StopReason: llm.StopReasonToolUse,
		},
		{
			Text:       "现在是 12:00。",
			StopReason: llm.StopReasonEndTurn,
		},
	}}
	st := newTestState(m)

	msgs, err := Run(context.Background(), st, "现在几点了")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(m.Calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(m.Calls))
	}

	// 第二次调用的请求应包含回喂的 toolResult，且与 call_1 配对
	second := m.Calls[1]
	if len(second.Messages) != 3 {
		t.Fatalf("2nd call messages = %d, want 3", len(second.Messages))
	}
	toolMsg := second.Messages[2]
	if toolMsg.Role != llm.RoleToolResult {
		t.Errorf("msg[2].role = %s, want toolResult", toolMsg.Role)
	}
	if toolMsg.ToolCallID != "call_1" {
		t.Errorf("toolCallID = %q, want call_1", toolMsg.ToolCallID)
	}
	if _, err := time.Parse(time.RFC3339, toolMsg.Content); err != nil {
		t.Errorf("tool result %q is not RFC3339 time", toolMsg.Content)
	}

	// 最终序列：user → assistant(toolcall) → toolResult → assistant(text)
	wantRoles := []llm.Role{
		llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult, llm.RoleAssistant,
	}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("final messages = %d, want %d", len(msgs), len(wantRoles))
	}
	for i, r := range wantRoles {
		if msgs[i].Role != r {
			t.Errorf("msgs[%d].role = %s, want %s", i, msgs[i].Role, r)
		}
	}
	if msgs[3].Content != "现在是 12:00。" {
		t.Errorf("final answer = %q", msgs[3].Content)
	}
}

// 场景 3：MaxSteps 兜底 —— 模型永远要工具，循环必须止损
func TestRunMaxSteps(t *testing.T) {
	tc := func(id string) llm.Completion {
		return llm.Completion{
			ToolCalls:  []llm.ToolCall{{ID: id, Name: "get_time"}},
			StopReason: llm.StopReasonToolUse,
		}
	}
	m := &llm.MockModel{Responses: []llm.Completion{tc("c1"), tc("c2"), tc("c3")}}
	st := newTestState(m)
	st.MaxSteps = 3 // 剧本长度恰好 = MaxSteps

	_, err := Run(context.Background(), st, "hi")
	if !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("err = %v, want ErrMaxSteps", err)
	}
	if len(m.Calls) != 3 {
		t.Errorf("model calls = %d, want 3", len(m.Calls))
	}
	// 1 user + 3×(assistant + toolResult) + 1 UINote = 8
	if len(st.Messages) != 8 {
		t.Errorf("messages = %d, want 8", len(st.Messages))
	}
	// Day 4 新增断言：末尾是 UI-only 注释，且它不会进入模型视图
	last := st.Messages[len(st.Messages)-1]
	if !last.Hidden {
		t.Errorf("last message should be a hidden UI note, got %+v", last.Message)
	}
	if !strings.Contains(last.Content, "max steps") {
		t.Errorf("note content = %q", last.Content)
	}
}

// 场景 4：ctx 取消 —— 取消信号穿透循环与 Mock（模拟真实 HTTP 行为）
func TestRunContextCanceled(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "永远不会被消费", StopReason: llm.StopReasonEndTurn},
	}}
	st := newTestState(m)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消

	_, err := Run(ctx, st, "hi")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(m.Calls) != 0 {
		t.Errorf("model calls = %d, want 0（取消后不应再调模型）", len(m.Calls))
	}
	// 部分历史保留：user 已入，调用方决定回滚还是续用
	if len(st.Messages) != 1 {
		t.Errorf("messages = %d, want 1", len(st.Messages))
	}
}
