package agent

import (
	"context"
	"strings"
	"testing"

	"minipi/internal/llm"
)

// 3 个轮次：tx1(事实) / tx2(超长问题+超长工具输出) / tx3(最近一轮)
func seedThreeTx() []Message {
	return []Message{
		UserMsg("我的名字是小明"),
		AssistantMsg("你好小明！", nil),
		UserMsg(strings.Repeat("长", 2000)),
		AssistantMsg("", []llm.ToolCall{{ID: "t1", Name: "calc", Input: `{"a":1}`}}),
		ToolResultMsg("t1", strings.Repeat("工具输出", 600)), // 1800 runes，必被截断
		UserMsg("最近的问题"),
		AssistantMsg("最近的回答", nil),
	}
}

func TestCompact(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "用户名叫小明；曾提问并调用 calc 工具。",
			StopReason: llm.StopReasonEndTurn,
			Usage:      llm.TokenUsage{InputTokens: 100, OutputTokens: 20}},
	}}
	st := &AgentState{Model: m, Messages: seedThreeTx()}

	dropped, err := Compact(context.Background(), st, 1)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}

	// 结构：[summary(user)] + 最近 1 轮（2 条）
	if len(st.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(st.Messages))
	}
	sum := st.Messages[0]
	if sum.Role != llm.RoleUser || !strings.Contains(sum.Content, "小明") {
		t.Errorf("summary = %+v", sum.Message)
	}
	if st.Messages[1].Content != "最近的问题" || st.Messages[2].Content != "最近的回答" {
		t.Errorf("recent tx broken: %+v %+v", st.Messages[1].Message, st.Messages[2].Message)
	}

	// 摘要输入的三个性质：含老历史 / 不含保留轮 / 工具输出被截断
	body := m.Calls[0].Messages[0].Content
	if !strings.Contains(body, "我的名字是小明") {
		t.Error("transcript missing old user content")
	}
	if strings.Contains(body, "最近的问题") {
		t.Error("transcript must not contain the kept tx")
	}
	// 截断后 ≈166 次出现；未截断 = 600 次
	if n := strings.Count(body, "工具输出"); n >= 300 {
		t.Errorf("tool output not truncated in transcript: %d occurrences", n)
	}
	// 摘要成本入账
	if st.TotalUsage.InputTokens != 100 || st.TotalUsage.OutputTokens != 20 {
		t.Errorf("usage not recorded: %+v", st.TotalUsage)
	}
}

func TestCompactNoop(t *testing.T) {
	m := &llm.MockModel{}
	st := &AgentState{Model: m, Messages: seedThreeTx()}
	dropped, err := Compact(context.Background(), st, 3) // 轮数不足
	if err != nil || dropped != 0 {
		t.Fatalf("got %d, %v", dropped, err)
	}
	if len(m.Calls) != 0 {
		t.Error("model must not be called on noop")
	}
	if len(st.Messages) != 7 {
		t.Error("history must be untouched")
	}
}
