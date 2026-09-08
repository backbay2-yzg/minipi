package agent

import (
	"context"
	"errors"
	"fmt"
	"minipi/internal/llm"
)

// ErrMaxSteps 是哨兵错误：循环耗尽 MaxSteps 仍没拿到最终答案。
// 调用方用 errors.Is(err, ErrMaxSteps) 判断，而不是比较错误文本。
var ErrMaxSteps = errors.New("agent: max steps exceeded")

const DefaultMaxStep = 8

// AgentState 是一段对话的全部状态。
// 关键设计：Run 是无状态函数，所有可变状态集中在这里，
// 由 Run 独占读写 —— 单 goroutine 使用无需加锁。
// （将来多 Agent 并发时，每个 Agent 各持一份 state）
type AgentState struct {
	SystemPrompt string
	Model        llm.Model
	//ToolSpecs    []llm.ToolSpec // 声明给模型的工具描述（Day 3 引入执行绑定）
	Tools    *Registry
	MaxSteps int // 0 表示用 DefaultMaxSteps

	// Transforms 是 Context 管道第一级（Day 4），按序执行。
	// nil/空 = 不裁剪，全量历史直发 —— 小对话的正确选择。
	Transforms []TransformFunc

	Bus *EventBus // ★ 事件总线；nil = 静默（既有测试零改动的前提）

	Messages   []Message      // 对话历史（含中间的 tool 轮次）
	TotalUsage llm.TokenUsage // 累计 token 消耗：循环顺带收集的遥测
}

// emit 是 Bus 的nil 安全包装：所有埋点统一走它，埋点处不再判空。
func (st *AgentState) emit(ctx context.Context, ev Event) {
	if st.Bus == nil {
		return
	}
	st.Bus.Emit(ctx, ev)
}

// Run = 壳（生命周期事件）+ 核（循环）。
// 壳保证 agent_start / agent_end 恰好各一次，错误路径也不漏发 agent_end
// （订阅者靠它收尾 —— 终态事件丢失 = 订阅者 goroutine 泄漏）。
func Run(ctx context.Context, st *AgentState, input string) ([]Message, error) {
	if st.Model == nil {
		return st.Messages, errors.New("agent: nil model")
	}
	st.emit(ctx, Event{Kind: EventAgentStart})

	var runErr error
	defer func() { st.emit(ctx, Event{Kind: EventAgentEnd, Err: runErr}) }()

	msgs, err := run(ctx, st, input)
	runErr = err
	return msgs, err
}

// Run 执行一轮完整交互：把用户输入加入历史，
// 循环"调模型 → 是否要工具？→ 执行工具回喂"直到模型给出最终回答。
//
// 返回 st.Messages（含本轮所有中间消息）；出错时返回当时的部分历史——
// 部分历史是数据（Day 6 持久化可能要保存它），是否回滚由调用方决定。
//
// ⚠️ 返回切片与 st.Messages 共享底层数组：调用方只读，切勿 append/修改。
// （Day 6 引入 Session 持久化时会加显式快照）
func run(ctx context.Context, st *AgentState, input string) ([]Message, error) {
	maxSteps := st.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxStep
	}
	// ① 用户输入历史
	st.Messages = append(st.Messages, UserMsg(input))

	var specs []llm.ToolSpec
	if st.Tools != nil {
		specs = st.Tools.Specs()
	}

	// 流式能力探测：一次断言，整个循环生效。
	// MockModel 等未实现 StreamingModel 的模型自动走 Complete 路径。
	sm, canStream := st.Model.(llm.StreamingModel)

	// ② 核心循环
	for step := 0; step < maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return st.Messages, fmt.Errorf("agent: step %d: %w", step, err)

		}

		st.emit(ctx, Event{Kind: EventTurnStart, Step: step})

		// 2.1 ★ Day 4 核心：全量历史 → 两级管道 → 本次请求的视图
		view, err := st.buildView(ctx)
		if err != nil {
			return st.Messages, fmt.Errorf("agent: step %d: build view: %w", step, err)
		}

		req := llm.CompletionRequest{
			Messages: view, // ★ 组装点：视图，非全量
			System:   st.SystemPrompt,
			Tools:    specs,
		}

		// 调模型：流式路径边到边发 message_update；两条路径最终都拿完整 Completion
		st.emit(ctx, Event{Kind: EventMessageStart, Step: step})

		var out *llm.Completion
		if canStream {
			out, err = sm.StreamComplete(ctx, req, func(delta string) {
				st.emit(ctx, Event{Kind: EventMessageUpdate, Step: step, Delta: delta})
			})
		} else {
			out, err = st.Model.Complete(ctx, req)
		}

		if err != nil {
			return st.Messages, fmt.Errorf("agent: step %d: %w", step, err)
		}

		st.TotalUsage.InputTokens += out.Usage.InputTokens
		st.TotalUsage.OutputTokens += out.Usage.OutputTokens

		// 2.3 assistant 回复（含可能的工具调用请求）入历史。
		// 即使要走工具分支也先入历史——Provider 靠它和 toolResult 配对
		st.Messages = append(st.Messages, AssistantMsg(out.Text, out.ToolCalls))

		// ★ message_end 带完整文本：非流式 Provider 下订阅者靠它展示
		st.emit(ctx, Event{Kind: EventMessageEnd, Step: step, Text: out.Text})

		// 2.4 正常出口：模型没有请求工具 → 最终答案已产生
		if len(out.ToolCalls) == 0 {
			st.emit(ctx, Event{Kind: EventTurnEnd, Step: step})
			return st.Messages, nil
		}
		// 2.5 执行工具、结果回喂历史，进入下一轮
		executeTools(ctx, st, out.ToolCalls, step)
		st.emit(ctx, Event{Kind: EventTurnEnd, Step: step})
	}
	// ③ 兜底出口：补一条 UI-only 注释（模型不可见，前端/排查可见）
	st.Messages = append(st.Messages, UINote("agent stopped: max steps (%d) exceeded", maxSteps))

	return st.Messages, ErrMaxSteps
}

// buildView 执行两级管道：Transforms（裁剪投影）→ convertToLlm（过滤 Hidden）。
// 输出是本次请求的临时视图；st.Messages 不被修改。
func (st *AgentState) buildView(ctx context.Context) ([]llm.Message, error) {
	msgs := st.Messages
	for _, t := range st.Transforms {
		var err error
		if msgs, err = t(ctx, msgs); err != nil {
			return nil, err
		}
	}
	return convertToLlm(msgs), nil
}
