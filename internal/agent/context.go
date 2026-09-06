package agent

import (
	"context"
	"fmt"
	"minipi/internal/llm"
)

// TransformFunc 是管道第一级的一个变换：输入当前视图，输出新视图。
// 契约（违反任何一条都是 bug）：
//  1. 只裁视图不改历史 —— 绝不写输入切片的底层数组（见 TruncateToolResults 的 copy）
//  2. 结构安全 —— assistant(ToolCalls) 与其 toolResult 同进退（以轮为单位操作）
//  3. 前缀稳定 —— 相同历史前缀 → 相同视图前缀，否则打断 provider 提示缓存
//     （Registry.Specs() 固定顺序正是同一原理：工具声明也是前缀的一部分）
//  4. 永不丢尾部 —— 最新一轮（进行中的对话）必须在输出里
type TransformFunc func(ctx context.Context, msgs []Message) ([]Message, error)

// splitTransactions 把历史切成轮次：每轮从一条可见的 user 消息开始，
// 到下一条 user 消息之前。Hidden 消息不开启新轮，附着在当前轮内。
// 历史首条消息（无论角色）开启第一轮（防御性，正常流程首条必是 user）。
//
// 已知约束：本实现假设 toolResult 永远紧随其 assistant(toolCalls)——
// 当前 Run 的执行顺序保证了这一点。Day 7 的 steering 在轮中注入用户
// 消息时，必须延长此定义（工具阶段内的 user 插入不开启新轮）。
func splitTransactions(msgs []Message) [][]Message {
	var txs [][]Message
	for _, m := range msgs {
		isStart := m.Role == llm.RoleUser && !m.Hidden
		if isStart || len(txs) == 0 {
			txs = append(txs, []Message{m})
			continue
		}
		txs[len(txs)-1] = append(txs[len(txs)-1], m)
	}
	return txs
}

// flatten 把若干轮摊平为一条切片（全新底层数组，与输入零共享）。
func flatten(txs [][]Message) []Message {
	n := 0
	for _, tx := range txs {
		n += len(tx)
	}
	out := make([]Message, 0, n)
	for _, tx := range txs {
		out = append(out, tx...)
	}
	return out
}

// PruneByTurns 返回"保留最近 keepTurns 轮"的滑动窗口变换。
// 以轮为单位 → toolCall/toolResult 配对天然安全。
func PruneByTurns(keepTurns int) TransformFunc {
	return func(ctx context.Context, msgs []Message) ([]Message, error) {
		if keepTurns <= 0 || len(msgs) == 0 {
			return msgs, nil
		}
		txs := splitTransactions(msgs)
		if len(txs) <= keepTurns {
			return msgs, nil // 预算内：原样返回（零拷贝 → 字节级一致 → 缓存友好）
		}
		return flatten(txs[len(txs)-keepTurns:]), nil
	}
}

// estimateTextTokens 粗估文本 token 数（预算规划用，非精确计数）：
//
//	ASCII ≈ 4 字符/token；CJK 等宽字符 ≈ 1 字符/token（保守偏高，安全方向）
//
// 真实值以 provider 返回的 usage 为准（AgentState.TotalUsage），
// 作业 2 会用真实 usage 校准这个估算器。
func estimateTextTokens(s string) int {
	ascii, wide := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	return ascii/4 + wide
}

// 每条消息 / 每个工具调用的框架开销粗估（role 标记、ID、JSON 包装等）
const (
	tokensPerMsgOverhead      = 4
	tokensPerToolCallOverhead = 8
)

func estimateTx(tx []Message) int {
	n := 0
	for _, m := range tx {
		n += tokensPerMsgOverhead + estimateTextTokens(m.Content)
		for _, tc := range m.ToolCalls {
			n += tokensPerToolCallOverhead + estimateTextTokens(tc.Input)
		}
	}
	return n
}

// PruneByTokens 返回按 token 预算裁剪的变换：
// 从最新轮往回累积，装得下多少轮保留多少轮；
// 最新一轮永远保留（即使自身超预算）——进行中的对话绝不能丢。
// 注意：预算只覆盖消息体；system 与工具声明在真实 provider 同样
// 占上下文，需要更精确的预算时把它们并入（见作业 3）。
func PruneByTokens(maxTokens int) TransformFunc {
	return func(ctx context.Context, msgs []Message) ([]Message, error) {
		if maxTokens <= 0 || len(msgs) == 0 {
			return msgs, nil
		}
		txs := splitTransactions(msgs)

		est := make([]int, len(txs))
		total := 0
		for i, tx := range txs {
			est[i] = estimateTx(tx)
			total += est[i]
		}
		if total <= maxTokens {
			return msgs, nil
		}
		start := len(txs) - 1
		used := est[start]
		for start > 0 && used+est[start-1] <= maxTokens {
			start--
			used += est[start]
		}
		return flatten(txs[start:]), nil
	}
}

// TruncateToolResults 返回截断超长工具输出的变换（内容级，结构安全）。
// maxChars 按 rune 计（多字节安全）；截断处加标记，模型可感知不完整。
func TruncateToolResults(maxChars int) TransformFunc {
	return func(ctx context.Context, msgs []Message) ([]Message, error) {
		// 快路径：没有需要截断的 → 原样返回（零拷贝 + 缓存友好）
		needCopy := false
		for _, m := range msgs {
			if m.Role == llm.RoleToolResult && len([]rune(m.Content)) > maxChars {
				needCopy = true
				break
			}
		}
		if !needCopy {
			return msgs, nil
		}

		// ⚠️ 慢路径必须先拷贝再改。若直接 msgs[i].Content = ...，
		// 写的是输入切片的底层数组 —— 而 buildView 传入的就是
		// st.Messages 的切片头，等于永久改写历史（别名写入陷阱）。
		out := make([]Message, len(msgs))
		copy(out, msgs)

		for i := range out {
			m := &out[i]
			if m.Role != llm.RoleToolResult {
				continue
			}
			r := []rune(m.Content)
			if len(r) <= maxChars {
				continue
			}
			m.Content = string(r[:maxChars]) + fmt.Sprintf("\n...[truncated %d chars]", len(r)-maxChars)
		}
		return out, nil
	}
}
