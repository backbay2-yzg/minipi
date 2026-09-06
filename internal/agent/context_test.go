package agent

import (
	"context"
	"minipi/internal/llm"
	"strings"
	"testing"
)

// seedHistory 构造带两个完整工具轮次的历史：
// tx1: user(q1) → assistant(c1) → toolResult(c1)
// tx2: user(q2) → assistant(c2) → toolResult(c2) → assistant(最终答案)
// tx3: user(q3)（进行中）
func seedHistory() []Message {
	return []Message{
		UserMsg("第一问"),
		AssistantMsg("", []llm.ToolCall{{ID: "c1", Name: "get_time"}}),
		ToolResultMsg("c1", "2026-09-01T09:00:00+08:00"),
		UserMsg("第二问"),
		AssistantMsg("", []llm.ToolCall{{ID: "c2", Name: "calc"}}),
		ToolResultMsg("c2", "12"),
		AssistantMsg("答案是 12", nil),
		UserMsg("第三问"),
	}
}

// 场景 1：滑动窗口 —— 最老一轮整体出局，配对完整，输入不被改写
func TestPruneByTurns(t *testing.T) {
	msgs := seedHistory()

	out, err := PruneByTurns(2)(context.Background(), msgs)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if len(out) != 5 { // tx2 的 4 条 + tx3 的 1 条
		t.Fatalf("view = %d messages, want 5", len(out))
	}
	if out[0].Role != llm.RoleUser || out[0].Content != "第二问" {
		t.Errorf("view should start at tx2's user, got %+v", out[0].Message)
	}
	// tx1 的任何痕迹都不应存在
	for _, m := range out {
		if m.ToolCallID == "c1" {
			t.Errorf("tx1 toolResult leaked: %+v", m.Message)
		}
		if len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "c1" {
			t.Errorf("tx1 toolCall leaked: %+v", m.Message)
		}
	}
	// tx2 配对完整：call 与 result 都在
	if !hasToolCall(out, "c2") || !hasToolResult(out, "c2") {
		t.Error("tx2 pair broken")
	}
	// 无损性：输入历史原封不动
	if len(msgs) != 8 {
		t.Errorf("input history mutated: len=%d, want 8", len(msgs))
	}
}

func hasToolCall(msgs []Message, id string) bool {
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID == id {
				return true
			}
		}
	}
	return false
}

func hasToolResult(msgs []Message, id string) bool {
	for _, m := range msgs {
		if m.Role == llm.RoleToolResult && m.ToolCallID == id {
			return true
		}
	}
	return false
}

// 场景 2：token 预算 —— 巨大的 tx1 整轮出局，而不是被拦腰斩断
func TestPruneByTokens(t *testing.T) {
	big := strings.Repeat("x", 400) // ≈100 tokens
	msgs := []Message{
		UserMsg(big),           // tx1 ≈ 4+100 + 4 = 108
		AssistantMsg("", nil),  // ┘
		UserMsg("q"),           // tx2 ≈ 4+1 + 4+1 = 10
		AssistantMsg("a", nil), // ┘
	}

	out, err := PruneByTokens(60)(context.Background(), msgs)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(out) != 2 || out[0].Content != "q" {
		t.Errorf("view = %+v, want only tx2", out)
	}
	if len(msgs) != 4 {
		t.Errorf("input history mutated")
	}
}

// 场景 3：预算远小于单轮 —— 最新一轮仍然整轮保留（永不丢尾部）
func TestPruneByTokensKeepsNewest(t *testing.T) {
	msgs := []Message{
		UserMsg(strings.Repeat("x", 800)), // ≈200 tokens
		AssistantMsg("done", nil),
	}
	out, err := PruneByTokens(10)(context.Background(), msgs)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("newest tx must survive even over budget, got %d msgs", len(out))
	}
}

// 场景 4：工具输出截断 —— rune 安全、带标记、不改输入
func TestTruncateToolResults(t *testing.T) {
	msgs := []Message{
		UserMsg("read it"),
		ToolResultMsg("c1", strings.Repeat("豆", 100)), // CJK：专测多字节
	}

	out, err := TruncateToolResults(10)(context.Background(), msgs)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	got := out[1].Content
	if !strings.HasPrefix(got, strings.Repeat("豆", 10)) {
		t.Errorf("prefix broken (mid-rune cut?): %q", got)
	}
	if !strings.Contains(got, "truncated 90 chars") {
		t.Errorf("missing marker: %q", got)
	}
	if len([]rune(msgs[1].Content)) != 100 {
		t.Errorf("input history mutated")
	}
}

// 场景 5：Hidden 过滤 —— UI-only 消息不进线格式
func TestConvertToLlmFiltersHidden(t *testing.T) {
	msgs := []Message{
		UserMsg("hi"),
		UINote("agent stopped: max steps (8) exceeded"),
		AssistantMsg("ok", nil),
	}
	out := convertToLlm(msgs)
	if len(out) != 2 {
		t.Fatalf("wire msgs = %d, want 2", len(out))
	}
	if out[0].Content != "hi" || out[1].Content != "ok" {
		t.Errorf("wire = %+v", out)
	}
}

// 场景 6：端到端 —— Run 的第二次调用只见最近一轮，但历史本体完整
func TestRunWithPruning(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "答一", StopReason: llm.StopReasonEndTurn},
		{Text: "答二", StopReason: llm.StopReasonEndTurn},
	}}
	st := &AgentState{
		Model:      m,
		Transforms: []TransformFunc{PruneByTurns(1)}, // 只看最近 1 轮
	}

	if _, err := Run(context.Background(), st, "q1"); err != nil {
		t.Fatalf("run1: %v", err)
	}
	if _, err := Run(context.Background(), st, "q2"); err != nil {
		t.Fatalf("run2: %v", err)
	}

	// 第二次调用：第一轮 [q1, 答一] 被裁掉，视图只有 [q2]
	second := m.Calls[1]
	if len(second.Messages) != 1 || second.Messages[0].Content != "q2" {
		t.Errorf("2nd call view = %+v, want [q2]", second.Messages)
	}
	// 真相无损：历史 4 条（q1, 答一, q2, 答二）
	if len(st.Messages) != 4 {
		t.Errorf("history = %d, want 4", len(st.Messages))
	}
}
