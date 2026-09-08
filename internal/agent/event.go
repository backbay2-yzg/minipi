package agent

import (
	"context"
	"minipi/internal/llm"
	"sync"
)

// EventKind 对齐 pi 的事件序列：
// agent_start → turn_start → message_start → message_update×N
//
//	→ message_end → [tool_execution_start → tool_execution_end]×M
//	→ turn_end → ... → agent_end
type EventKind string

const (
	EventAgentStart    EventKind = "agent_start"
	EventTurnStart     EventKind = "turn_start"
	EventMessageStart  EventKind = "message_start"
	EventMessageUpdate EventKind = "message_update" // 流式增量
	EventMessageEnd    EventKind = "message_end"
	EventToolExecStart EventKind = "tool_execution_start"
	EventToolExecEnd   EventKind = "tool_execution_end"
	EventTurnEnd       EventKind = "turn_end"
	EventAgentEnd      EventKind = "agent_end" // 终态事件：订阅者据此收尾

	EventCompactionStart EventKind = "compaction_start"
	EventCompactionEnd   EventKind = "compaction_end"
)

// Event 是总线上的一次投递。字段按 Kind 取用：
//
//	message_update       → Delta
//	message_end          → Text（最终完整文本，供非流式场景兜底展示）
//	tool_execution_*     → ToolCall；tool_execution_end 另带 Result
//	agent_end            → Err（nil 表示正常结束）
type Event struct {
	Kind     EventKind
	Step     int
	Delta    string
	Text     string
	ToolCall *llm.ToolCall // 指针避免拷贝；
	Result   string
	Err      error
}

// EventBus 是 channel fan-out 事件总线。
// 并发约定：
//   - Emit 可多 goroutine 并发调用（工具并行执行时会同时发事件）
//   - 投递在 RLock 内做阻塞 send：保序、保证送达；
//     代价是一个慢订阅者会拖住 Emit —— 由订阅纪律约束（读端必须及时消费）
type EventBus struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
	buf  int
}

func NewEventBuf(buf int) func(*EventBus) { // 供选项用；见 NewEventBus
	return func(b *EventBus) { b.buf = buf }
}

// NewEventBus 创建总线。buf<=0 时用默认缓冲。
// 缓冲的意义：订阅者 goroutine 被调度延迟时不阻塞 Agent 主循环；
// 缓冲必须 ≥ 单轮最大事件数，否则退化为同步耦合。
func NewEventBus(buf int) *EventBus {
	if buf <= 0 {
		buf = 128
	}
	return &EventBus{subs: make(map[chan Event]struct{}), buf: buf}
}

// Subscribe 注册订阅者，返回只读通道和取消函数。
// 关闭语义：只有 cancel 会 close 通道（谁开谁关）；
// Emit 永不 close —— fan-out 下关闭权交给发起方，避免 send on closed channel。
func (b *EventBus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, b.buf)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// Emit 向所有订阅者投递事件：阻塞式、保序、保证送达。
// ctx 取消时放弃投递（ctx 取消 = 程序在退出路径，事件已无意义）。
func (b *EventBus) Emit(ctx context.Context, ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		case <-ctx.Done():
			return
		}
	}
}
