package workflow

import (
	"context"
	"errors"
	"fmt"
	"minipi/internal/agent"
	"minipi/internal/llm"
)

// Stage 是链的一个环节：独立的 SystemPrompt 与工具集。
// 独立性的意义：每个环节是"一个专才"——提纲编辑不该背着写作工具，
// 翻译者不该带着审校者的 system。混装 = 环节互相污染。
type Stage struct {
	Name         string
	SystemPrompt string
	Tools        *agent.Registry // nil = 纯文本环节
}

// Gate 是环节出口的廉价检查点。返回 nil 通过；返回 error 拒绝，
// 错误文本将作为修正反馈拼进重试输入（编号 71 的防线）。
// 实现可以是程序校验（正则/长度/结构）或一次轻量 LLM 校验。
type Gate func(ctx context.Context, output string) error

// ChainStep = Stage + 出口门。
type ChainStep struct {
	Stage   Stage
	Gate    Gate // nil = 无门
	Retries int  // 门拒绝后的重试上限；0 = 拒绝即中止（编号 72 的防线）
}

// ChainResult 汇总全链产出与成本。重试的 token 也计入 TotalUsage——
// 重试不是免费的，成本必须可见（Day 6 反模式 53 的链式版）。
type ChainResult struct {
	Outputs    []string       // 各环节最终输出；被门拒绝的产出也保留（失败现场）
	TotalUsage llm.TokenUsage // 全链累计（含重试）
}

// RunChain 串行执行链：上一环节的输出 = 下一环节的输入。
//
// 契约：
//   - 每环节（含每次重试）全新 AgentState（编号 70 的防线）：
//     环节间只经"输出字符串"传递——链上是数据流，不是状态共享
//   - 任何环节耗尽重试仍被拒 → 整链中止（编号 73 的防线），
//     已完成的产出随错误返回，供调用方排查或展示
//   - 串行执行，不并发——并发放 Day 10（Parallelization）
//   - bus 可为 nil（静默）。非 nil 时把总线下传给每个环节：流式 delta
//     因此能实时出到订阅者界面——链是数据管道，不该屏蔽过程
func RunChain(ctx context.Context, model llm.Model, bus *agent.EventBus, steps []ChainStep, input string) (*ChainResult, error) {
	if model == nil {
		return nil, errors.New("workflow: nil model")
	}
	if len(steps) == 0 {
		return nil, errors.New("workflow: empty chain")
	}

	res := &ChainResult{Outputs: make([]string, 0, len(steps))}
	cur := input
	for i, step := range steps {
		out, usage, err := runStep(ctx, model, bus, i, step, cur)
		// 成本无论成败都入账——失败环节烧掉的 token 也是真金白银
		res.TotalUsage.InputTokens += usage.InputTokens
		res.TotalUsage.OutputTokens += usage.OutputTokens
		if out != "" {
			res.Outputs = append(res.Outputs, out)
		}
		if err != nil {
			return res, fmt.Errorf("workflow: step %d (%s): %w", i, step.Stage.Name, err)
		}
		cur = out
	}
	return res, nil
}

// runStep 执行单环节，含门重试循环。返回 (最终输出, 累计 usage, err)。
// 拒绝路径的输出非空返回——它就是失败现场，调用方要看。
// stepIdx 用于事件标注环节序号（AgentID 形如 chain-1，界面上可区分环节）。
// 重试输入三件套：任务（cur）+ 被拒产物（out）+ 拒绝理由（gerr）。
// "修正"的宾语必须在场：模型看不到上次产物，就无从修正；
// 人的否决理由几乎总是指涉产物的内容（"结尾太仓促"），产物缺席则反馈失去锚点。
// 代价：重试输入多出 |out| 的 token —— 受 Retries 上限约束，可接受（编号 84）。
func runStep(ctx context.Context, model llm.Model, bus *agent.EventBus, stepIdx int, step ChainStep, input string) (string, llm.TokenUsage, error) {
	var usage llm.TokenUsage
	cur := input

	for attempt := 0; ; attempt++ {
		// ★ 每次尝试全新 state：重试不是"继续聊"，是"带着反馈重做"
		st := &agent.AgentState{
			AgentID:      fmt.Sprintf("chain-%d", stepIdx+1),
			Model:        model,
			SystemPrompt: step.Stage.SystemPrompt,
			Tools:        step.Stage.Tools,
			Bus:          bus, // nil = 静默；非 nil = delta 实时出流
		}
		msgs, err := agent.Run(ctx, st, cur)
		usage.InputTokens += st.TotalUsage.InputTokens
		usage.OutputTokens += st.TotalUsage.OutputTokens
		if err != nil {
			return "", usage, err // 模型/循环错误：非门拒绝，直接上抛
		}

		out, ok := agent.FinalText(msgs)
		if !ok {
			return "", usage, errors.New("stage produced no final text")
		}

		if step.Gate == nil {
			return out, usage, nil
		}
		if gerr := step.Gate(ctx, out); gerr == nil {
			return out, usage, nil
		} else {
			if attempt >= step.Retries {
				return out, usage, fmt.Errorf("gate rejected after %d attempt(s): %w", attempt+1, gerr)
			}
			// 拒绝理由 + 原任务拼进重试输入：模型必须知道错在哪
			cur = fmt.Sprintf("%s\n\n[上一次的输出：\n%s\n\n该输出被拒绝：%v]\n请针对拒绝理由修正后重新完成任务。", cur, out, gerr)
		}
	}
}
