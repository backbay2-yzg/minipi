package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"minipi/internal/llm"

	"golang.org/x/sync/errgroup"
)

// executeTools 并行执行一轮的全部工具调用，结束后按调用顺序
// 追加对应的 toolResult 到历史。
//
// 三个设计决策：
//  1. 并行：模型一轮可发起多个独立调用（并行工具调用），
//     串行执行浪费时间；工具间默认无依赖（有依赖模型会分两轮发起）
//  2. 错误回喂：单个工具失败 ≠ 程序失败。错误文本作为 toolResult
//     回喂，模型可自我纠正（换参数重试/换工具/放弃）——绝不中断循环
//  3. 取消传播：gctx 随父 ctx 取消（用户 Ctrl+C → 进行中的工具 HTTP 调用中断）
//
// executeTools 并行执行一轮的全部工具调用，按调用顺序写回结果；
// 每个调用发 tool_execution_start / tool_execution_end 事件。
// 注意：并行时不同工具的事件**相对顺序不保证**（同一工具的 start→end 保证有序，
// 因为它们在同一 goroutine 内顺序发出）；需要全局严格顺序的消费者应按 ToolCall.ID 归并。
func executeTools(ctx context.Context, st *AgentState, calls []llm.ToolCall, step int) {
	g, gctx := errgroup.WithContext(ctx)
	results := make([]Message, len(calls))
	for i, call := range calls {
		// Go 1.22+ 循环变量每轮新建，此行可省；保留以示兼容老版本
		i, call := i, call
		g.Go(func() error {
			st.emit(gctx, Event{Kind: EventToolExecStart, Step: step, ToolCall: &call})

			res := runOne(gctx, st.Tools, call)
			st.emit(gctx, Event{Kind: EventToolExecEnd, Step: step, ToolCall: &call, Result: res.Content})
			results[i] = res
			return nil // 永远返回 nil：失败是"给模型的信息"，不是批次失败
		})
	}
	_ = g.Wait() // goroutine 全部 return nil，此处 error 恒为 nil

	// 并行执行、串行写回：历史 append 只发生在主 goroutine，无锁竞争
	st.Messages = append(st.Messages, results...)
}

// runOne 执行单个调用并包装为 toolResult 消息。
// 工具内部任何形态的失败（error / panic / ctx 取消）都被折叠进
// 结果文本，保证每一条 toolCall 恰好得到一条 toolResult ——
// 这是与 Provider 配对的硬协议，破坏即 400。
func runOne(ctx context.Context, reg *Registry, call llm.ToolCall) Message {
	out := func() (out string) {
		// 工具是外部代码的边界：panic 不允许炸穿整个 Agent
		defer func() {
			if r := recover(); r != nil {
				out = fmt.Sprintf("error: tool %q panicked: %v", call.Name, r)
			}
		}()

		if err := ctx.Err(); err != nil {
			return "error: canceled: " + err.Error()
		}
		tool, ok := reg.Get(call.Name)
		if !ok {
			return fmt.Sprintf("error: unknown tool %q", call.Name)
		}

		args := json.RawMessage(call.Input)
		if len(args) == 0 {
			args = json.RawMessage("{}") // 部分模型/Provider 对无参工具返回空串
		}

		res, err := tool.Run(ctx, args)
		if err != nil {
			return "error: " + err.Error()
		}
		return res
	}()

	return ToolResultMsg(call.ID, out) // ★ 唯一变化：构造函数（自动带 CreatedAt）
}
