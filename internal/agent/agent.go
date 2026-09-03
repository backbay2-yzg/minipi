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

	Messages   []llm.Message  // 对话历史（含中间的 tool 轮次）
	TotalUsage llm.TokenUsage // 累计 token 消耗：循环顺带收集的遥测
}

// Run 执行一轮完整交互：把用户输入加入历史，
// 循环"调模型 → 是否要工具？→ 执行工具回喂"直到模型给出最终回答。
//
// 返回 st.Messages（含本轮所有中间消息）；出错时返回当时的部分历史——
// 部分历史是数据（Day 6 持久化可能要保存它），是否回滚由调用方决定。
//
// ⚠️ 返回切片与 st.Messages 共享底层数组：调用方只读，切勿 append/修改。
// （Day 6 引入 Session 持久化时会加显式快照）
func Run(ctx context.Context, st *AgentState, input string) ([]llm.Message, error) {
	if st.Model == nil {
		return st.Messages, errors.New("agent: nil model")
	}

	maxSteps := st.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxStep
	}
	// ① 用户输入历史
	st.Messages = append(st.Messages, llm.Message{
		Role:    llm.RoleUser,
		Content: input,
	})

	// ② 核心循环
	for step := 0; step < maxSteps; step++ {
		// 2.1 每轮先查取消。真实 Model 的 HTTP 层也会查 ctx，
		// 这里是第二道防线（Day 3 的工具执行也会查）
		if err := ctx.Err(); err != nil {
			return st.Messages, fmt.Errorf("agent: step %d: %w", step, err)
		}

		var specs []llm.ToolSpec
		if st.Tools != nil {
			specs = st.Tools.Specs()
		}
		// 2.2 调模型：全部历史 + 工具声明
		out, err := st.Model.Complete(ctx, llm.CompletionRequest{
			Messages: st.Messages,
			System:   st.SystemPrompt,
			Tools:    specs,
		})
		if err != nil {
			return st.Messages, fmt.Errorf("agent: step %d: %w", step, err)
		}

		st.TotalUsage.InputTokens += out.Usage.InputTokens
		st.TotalUsage.OutputTokens += out.Usage.OutputTokens

		// 2.3 assistant 回复（含可能的工具调用请求）入历史。
		// 即使要走工具分支也先入历史——Provider 靠它和 toolResult 配对
		st.Messages = append(st.Messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   out.Text,
			ToolCalls: out.ToolCalls,
		})

		// 2.4 正常出口：模型没有请求工具 → 最终答案已产生
		if len(out.ToolCalls) == 0 {
			return st.Messages, nil
		}
		// 2.5 执行工具、结果回喂历史，进入下一轮
		executeTools(ctx, st, out.ToolCalls)
	}
	// ③ 兜底出口：模型陷入"永远要工具"的循环时止损
	return st.Messages, ErrMaxSteps
}
