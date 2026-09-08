package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"minipi/internal/llm"

	"go.uber.org/goleak"
)

// TestMain：整个包的 goroutine 泄漏哨兵。
// 任何测试留下未退出的 goroutine（订阅者没收尾、泵 goroutine 没关……），
// 测试结束后 goleak 都会点名报告。从此包内所有测试默认受保护。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// drainEvents 从订阅通道读到 agent_end（或超时 fatal），返回事件序列。
// 超时兜底必不可少：终态事件丢失时若不设超时，测试会永远挂起。
func drainEvents(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var evs []Event
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			evs = append(evs, ev)
			if ev.Kind == EventAgentEnd {
				return evs
			}
		case <-deadline:
			t.Fatalf("timeout waiting for agent_end; got %v", kinds(evs))
		}
	}
}

func kinds(evs []Event) []EventKind {
	out := make([]EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

// 场景 1：完整事件序列 —— 工具轮次的规范时序（pi 事件序列的 Go 版）
func TestEventSequence(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "get_time"}},
			StopReason: llm.StopReasonToolUse},
		{Text: "done", StopReason: llm.StopReasonEndTurn},
	}}
	bus := NewEventBus(128)
	st := newTestState(m)
	st.Bus = bus
	ch, cancel := bus.Subscribe()
	defer cancel()

	// 缓冲 128 > 本场景事件总数，同步 Run 不会阻塞；事件随后逐个读取
	if _, err := Run(context.Background(), st, "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	evs := drainEvents(t, ch)

	want := []EventKind{
		EventAgentStart,
		EventTurnStart, EventMessageStart, EventMessageEnd,
		EventToolExecStart, EventToolExecEnd,
		EventTurnEnd,
		EventTurnStart, EventMessageStart, EventMessageEnd,
		EventTurnEnd,
		EventAgentEnd,
	}
	got := kinds(evs)
	if len(got) != len(want) {
		t.Fatalf("events = %v\nwant = %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("events[%d] = %s, want %s", i, got[i], want[i])
		}
	}

	// 工具结束事件应携带调用与结果
	toolEnd := evs[5]
	if toolEnd.ToolCall == nil || toolEnd.ToolCall.ID != "c1" || toolEnd.Result == "" {
		t.Errorf("tool end event = %+v", toolEnd)
	}
	// 终态事件携带错误状态（正常结束 → nil）
	if evs[len(evs)-1].Err != nil {
		t.Errorf("agent_end.Err = %v", evs[len(evs)-1].Err)
	}
}

// 场景 2：流式 —— delta 事件实时流出，拼接 == 最终文本
func TestStreamingEvents(t *testing.T) {
	m := &llm.MockStreaming{
		MockModel: llm.MockModel{Responses: []llm.Completion{
			{Text: "你好世界", StopReason: llm.StopReasonEndTurn},
		}},
		ChunkSize: 2, // "你好" "世界"
	}
	bus := NewEventBus(128)
	st := &AgentState{Model: m, Bus: bus} // 无工具，纯文本
	ch, cancel := bus.Subscribe()
	defer cancel()

	msgs, err := Run(context.Background(), st, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := drainEvents(t, ch)

	var deltas []string
	for _, ev := range evs {
		if ev.Kind == EventMessageUpdate {
			deltas = append(deltas, ev.Delta)
		}
	}
	if strings.Join(deltas, "") != "你好世界" {
		t.Errorf("deltas joined = %q, want 你好世界", strings.Join(deltas, ""))
	}
	if len(deltas) != 2 {
		t.Errorf("delta count = %d, want 2", len(deltas))
	}
	// 逻辑真相走返回值，与 delta 展示一致（契约）
	if msgs[len(msgs)-1].Content != "你好世界" {
		t.Errorf("final message = %q", msgs[len(msgs)-1].Content)
	}
}

// 场景 3：错误路径也必须发出 agent_end（终态事件 = 订阅者收尾信号）
func TestAgentEndOnError(t *testing.T) {
	m := &llm.MockModel{Err: context.DeadlineExceeded}
	bus := NewEventBus(128)
	st := newTestState(m)
	st.Bus = bus
	ch, cancel := bus.Subscribe()
	defer cancel()

	if _, err := Run(context.Background(), st, "hi"); err == nil {
		t.Fatal("want error")
	}
	evs := drainEvents(t, ch)
	last := evs[len(evs)-1]
	if last.Kind != EventAgentEnd || last.Err == nil {
		t.Errorf("last event = %+v, want agent_end with error", last)
	}
}
