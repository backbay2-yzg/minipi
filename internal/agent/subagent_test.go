package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"minipi/internal/llm"
)

// 本文件没有共享队列 helper —— 单一队列编排多个执行者正是反模式 62 的载体。
// 每个执行者各自持有 MockModel（"谁的剧本谁的队列"），排列即时间轴。
// newTestState 定义于 loop_test.go（Day 2），同包复用。

// 场景 1：端到端派发 —— 父调 delegate_task，子的最终回答作为工具结果回喂
func TestSubAgentDelegation(t *testing.T) {
	// 子 Agent 剧本：发起 get_time → 最终回答（子队列，时序即排列序）
	mChild := &llm.MockModel{Responses: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "s1", Name: "get_time"}},
			StopReason: llm.StopReasonToolUse},
		{Text: "子任务完成：现在 10 点", StopReason: llm.StopReasonEndTurn},
	}}
	// 父 Agent 剧本：发起 delegate → 汇总（父队列，独立时间轴）
	mParent := &llm.MockModel{Responses: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "p1", Name: "delegate_task",
			Input: `{"task":"查时间并汇报"}`}},
			StopReason: llm.StopReasonToolUse},
		{Text: "委托完成：10 点", StopReason: llm.StopReasonEndTurn},
	}}

	sub := NewSubAgentTool(mChild, NewRegistry(NewTimeTool()), 4, nil)
	parent := &AgentState{
		Model: mParent,
		Tools: func() *Registry {
			reg := NewRegistry(NewCalcTool())
			reg.Register(sub) // ★ 父工具集 = 普通工具 + delegate
			return reg
		}(),
	}

	msgs, err := Run(context.Background(), parent, "帮我查时间")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 两侧队列各自精确消费完（剩余响应 = 实际编排与剧本不符，必须报错）
	if len(mParent.Calls) != 2 {
		t.Fatalf("parent calls = %d, want 2", len(mParent.Calls))
	}
	if len(mChild.Calls) != 2 {
		t.Fatalf("child calls = %d, want 2", len(mChild.Calls))
	}

	// 子的第 1 轮视图首条 = 任务字符串（子状态隔离的证据）
	if got := mChild.Calls[0].Messages[0].Content; got != "查时间并汇报" {
		t.Errorf("child first input = %q, want task string", got)
	}

	// 父的第 2 轮请求里，delegate 的结果 = 子的最终回答
	var fedBack string
	for _, msg := range mParent.Calls[1].Messages {
		if msg.Role == llm.RoleToolResult && msg.ToolCallID == "p1" {
			fedBack = msg.Content
		}
	}
	if fedBack != "子任务完成：现在 10 点" {
		t.Errorf("delegation result = %q", fedBack)
	}

	// 父历史：user, assistant(p1), toolResult(p1), assistant(答案) = 4 条
	if len(msgs) != 4 {
		t.Errorf("parent messages = %d, want 4", len(msgs))
	}
}

// 场景 2：ctx 级联 —— 父取消穿透到子 Agent 内部，两侧队列零消费
func TestSubAgentCancelPropagation(t *testing.T) {
	mChild := &llm.MockModel{Responses: []llm.Completion{
		{Text: "永不消费", StopReason: llm.StopReasonEndTurn},
	}}
	mParent := &llm.MockModel{Responses: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "p1", Name: "delegate_task",
			Input: `{"task":"long"}`}},
			StopReason: llm.StopReasonToolUse},
	}}
	sub := NewSubAgentTool(mChild, NewRegistry(NewTimeTool()), 4, nil)
	parent := &AgentState{
		Model: mParent,
		Tools: func() *Registry {
			reg := NewRegistry()
			reg.Register(sub)
			return reg
		}(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消：链 = 父 Run → executeTools gctx → 子 Run → HTTP 层

	_, err := Run(ctx, parent, "go")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(mParent.Calls) != 0 {
		t.Errorf("parent calls = %d, want 0", len(mParent.Calls))
	}
	if len(mChild.Calls) != 0 {
		t.Errorf("child calls = %d, want 0", len(mChild.Calls))
	}
}

// 场景 3：同一轮的两个委派必须真正并发运行，并各自 fork 独立模型会话。
// 若 SubAgentTool 直接复用根模型，本测试会在子代理进入点之前失败；若工具
// 被串行执行，第二个子代理也无法在 release 关闭前进入。
func TestConcurrentSubAgentsForkModelSessions(t *testing.T) {
	root := &forkProbeModel{
		entered: make(chan int, 2),
		release: make(chan struct{}),
	}
	sub := NewSubAgentTool(root, NewRegistry(), 1, nil)
	reg := NewRegistry()
	reg.Register(sub)
	st := &AgentState{Tools: reg}
	calls := []llm.ToolCall{
		{ID: "first", Name: "delegate_task", Input: `{"task":"first task"}`},
		{ID: "second", Name: "delegate_task", Input: `{"task":"second task"}`},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		executeTools(ctx, st, calls, 0)
		close(done)
	}()

	seen := make(map[int]bool, 2)
	for len(seen) < 2 {
		select {
		case id := <-root.entered:
			seen[id] = true
		case <-done:
			t.Fatal("tool execution ended before two forked child sessions entered")
		case <-ctx.Done():
			t.Fatal("two forked child sessions did not run concurrently")
		}
	}
	close(root.release)

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("concurrent sub-agents did not finish after release")
	}

	if got := root.ForkCount(); got != 2 {
		t.Fatalf("Fork calls = %d, want 2", got)
	}
	if len(seen) != 2 {
		t.Fatalf("child session IDs = %v, want two distinct IDs", seen)
	}
	if len(st.Messages) != 2 {
		t.Fatalf("tool results = %d, want 2", len(st.Messages))
	}
	for i, msg := range st.Messages {
		if msg.Role != llm.RoleToolResult || msg.ToolCallID != calls[i].ID ||
			!strings.HasPrefix(msg.Content, "fork-session-") {
			t.Errorf("result[%d] = %+v", i, msg.Message)
		}
	}
}

// forkProbeModel 模拟“根模型不能直接请求、每个 fork 才是一段可请求会话”的
// Provider。entered/release 形成栅栏，使测试能证明两个子会话确实同时在运行。
type forkProbeModel struct {
	mu      sync.Mutex
	nextID  int
	entered chan int
	release chan struct{}
}

func (m *forkProbeModel) Complete(context.Context, llm.CompletionRequest) (*llm.Completion, error) {
	return nil, errors.New("root model used without Fork")
}

func (m *forkProbeModel) Fork() llm.Model {
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	m.mu.Unlock()
	return &forkProbeChild{parent: m, id: id}
}

func (m *forkProbeModel) ForkCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextID
}

type forkProbeChild struct {
	parent *forkProbeModel
	id     int
}

func (m *forkProbeChild) Complete(ctx context.Context, _ llm.CompletionRequest) (*llm.Completion, error) {
	select {
	case m.parent.entered <- m.id:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-m.parent.release:
		return &llm.Completion{
			Text:       fmt.Sprintf("fork-session-%d", m.id),
			StopReason: llm.StopReasonEndTurn,
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// 场景 4：Steering —— 插话在下一轮请求可见，且历史配对完好。
// go test -race 下运行：Steer 来自测试 goroutine，drainSteer 在 Run goroutine。
func TestSteering(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "get_time"}},
			StopReason: llm.StopReasonToolUse},
		{Text: "final", StopReason: llm.StopReasonEndTurn},
	}}
	st := newTestState(m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 预插话：Run 启动前入队，step 0 的 drainSteer 消费（确定性时序）
	if err := st.Steer(ctx, "顺便告诉我日期"); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if _, err := Run(ctx, st, "现在几点"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 第 1 次调用的视图应含插话：[user("现在几点"), user("[steer] ...")]
	first := m.Calls[0].Messages
	if len(first) != 2 || !strings.Contains(first[1].Content, "顺便告诉我日期") {
		t.Errorf("steer not injected into first view: %+v", first)
	}

	// 结构断言：完整历史 5 条，steer 落在 [1]（assistant 之前）
	history := st.Messages
	if len(history) != 5 {
		t.Fatalf("history = %d msgs, want 5 (user, steer, asst(c1), toolResult, asst), got: %s",
			len(history), dumpRoles(history))
	}
	if history[1].Role != llm.RoleUser || !strings.Contains(history[1].Content, "[steer]") {
		t.Errorf("history[1] = %+v, want steer user message", history[1].Message)
	}

	// 配对断言（方向正确版）：toolResult(c1) 前一条恰是携带 c1 的 assistant
	if !pairIntact(history, "c1") {
		t.Errorf("toolCall/toolResult pair broken; history = %s", dumpRoles(history))
	}
}

// pairIntact 检查 id 的 toolResult 的前一条恰是包含该调用的 assistant。
// 没找到该 toolResult 也返回 false —— 配对缺失同样是失败。
func pairIntact(history []Message, id string) bool {
	for i, m := range history {
		if m.Role == llm.RoleToolResult && m.ToolCallID == id {
			if i == 0 || history[i-1].Role != llm.RoleAssistant {
				return false
			}
			for _, tc := range history[i-1].ToolCalls {
				if tc.ID == id {
					return true
				}
			}
			return false
		}
	}
	return false
}

// dumpRoles 辅助失败信息可读化：一行打印角色序列
func dumpRoles(msgs []Message) string {
	var sb strings.Builder
	for i, m := range msgs {
		if i > 0 {
			sb.WriteString(" → ")
		}
		sb.WriteString(string(m.Role))
		if m.ToolCallID != "" {
			sb.WriteString("(" + m.ToolCallID + ")")
		}
	}
	return sb.String()
}
