package agent

import (
	"context"
	"fmt"
	"strings"

	"minipi/internal/llm"
)

const compactionPrompt = `You will be given a transcript of an earlier conversation
between a user and an assistant. Write a concise summary that preserves:
- the user's goals, preferences, and any personal facts they stated
- key decisions made and their reasons
- important facts and tool results
- anything unresolved or pending
Output only the summary text.`

// Compact 破坏性压缩：把除最近 keepTx 轮之外的历史替换为一条摘要消息，
// 返回被压缩的轮数（0 表示无需压缩）。
//
// 与 TransformFunc 的本质区别（Day 4 契约 1）：本函数改写 st.Messages 本体，
// 不可逆 —— 所以它绝不能实现为 TransformFunc 挂进管道。
//
// 设计决策：
//   - 按轮压缩（复用 splitTransactions）→ toolCall/toolResult 配对天然安全
//   - 摘要输入先过 TruncateToolResults → 超长工具输出不污染摘要输入
//   - 摘要消息用 user 角色 → 协议安全（Anthropic 首条须为 user；与后续
//     轮次的连续 user 会被 convert 合并）；未来再压缩时可被再次摘要（递归压缩）
//   - 摘要调用计入 TotalUsage 并发事件 → 成本可见，绝不隐藏 LLM 调用
//   - 老历史中的 Hidden UI 注释随压缩丢弃（它们描述的是被压缩的内容）
//   - 失败路径不发 compaction_end（advisory 事件，非生命周期事件；
//     错误呈现由调用方负责）
func Compact(ctx context.Context, st *AgentState, keepTx int) (int, error) {
	if keepTx < 1 {
		keepTx = 1
	}
	txs := splitTransactions(st.Messages)
	if len(txs) <= keepTx+1 {
		return 0, nil // 可压的老历史不足（至少要省出一轮的收益才值得花一次调用）
	}

	old := flatten(txs[:len(txs)-keepTx])
	recent := flatten(txs[len(txs)-keepTx:])

	st.emit(ctx, Event{Kind: EventCompactionStart})

	// 摘要输入：截断超长工具输出 → 过滤 Hidden → 拼 transcript
	oldMsgs, err := TruncateToolResults(500)(ctx, old)
	if err != nil {
		return 0, fmt.Errorf("agent: compact: %w", err)
	}
	var transcript strings.Builder
	for _, m := range convertToLlm(oldMsgs) {
		if m.Content != "" {
			fmt.Fprintf(&transcript, "%s: %s\n", m.Role, m.Content)
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&transcript, "%s: [called tool %s(%s)]\n", m.Role, tc.Name, tc.Input)
		}
	}

	out, err := st.Model.Complete(ctx, llm.CompletionRequest{
		System:   compactionPrompt,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: transcript.String()}},
	})
	if err != nil {
		return 0, fmt.Errorf("agent: compact: %w", err)
	}
	// ★ 组装点：摘要成本必须入账（忘掉这两行 = 隐藏成本）
	st.TotalUsage.InputTokens += out.Usage.InputTokens
	st.TotalUsage.OutputTokens += out.Usage.OutputTokens

	summary := UserMsg("[earlier conversation summary]\n" + out.Text)
	// recent 来自 flatten（全新底层数组）→ 新旧历史零共享，老消息整体可回收
	st.Messages = append([]Message{summary}, recent...)

	st.emit(ctx, Event{Kind: EventCompactionEnd, Text: out.Text})
	return len(txs) - keepTx, nil
}
