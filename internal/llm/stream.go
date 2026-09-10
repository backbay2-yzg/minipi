package llm

import "context"

// StreamingModel 扩展 Model：边生成边回调增量，最终仍返回完整 Completion。
// 关键设计：独立接口而非给 Model 加方法（Day 1 决策：接口最小化），
// 上层用类型断言探测能力：
//
//	if sm, ok := model.(StreamingModel); ok { ... 流式路径 ... }
//
// 契约：onDelta 的累计拼接 == 返回值 Completion.Text；流式中断返回 error。
type StreamingModel interface {
	Model
	StreamComplete(ctx context.Context, req CompletionRequest,
		onDelta func(text string)) (*Completion, error)
}

// ReasoningStreamingModel adds a separate display-only stream for provider
// reasoning. Reasoning is not part of onDelta or Completion.Text.
type ReasoningStreamingModel interface {
	StreamingModel
	StreamCompleteWithReasoning(ctx context.Context, req CompletionRequest,
		onDelta func(string), onReasoning func(string)) (*Completion, error)
}
