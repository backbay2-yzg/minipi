package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"minipi/internal/llm"
)

// SubAgentTool 把一个完整的子 Agent 循环包装成普通工具。
// 父模型的视角：声明 → 调用 → 结果回喂，与 calc 无异；
// 背后：task 字符串驱动一个全新的 AgentState 跑完整个 Run 循环。
//
// 三个并发契约（violation 任一即事故）：
//  1. ctx 从父派生（本工具的 ctx 就是 runOne 传入的 gctx，天然继承父链）——
//     父取消 → executeTools 的 errgroup 取消 → 本 Run 的 HTTP 层中断。
//     绝不换成 context.Background()（反模式 55）
//  2. 子 Agent 持有全新 AgentState——父子零共享切片（反模式 56）。
//     任务上下文靠 task 字符串自包含（Spec 的 description 里向父模型声明）
//  3. t.Tools 必须不含本工具（反模式 57）——由组合根保证，
//     本类型无法自行验证（Registry 无introspection），注释即契约
type SubAgentTool struct {
	Model    llm.Model // 父模型；有会话状态时每次 Run 会从它 fork
	Tools    *Registry // ★ 子 Agent 的工具集：不含任何 SubAgentTool
	MaxSteps int       // 子 Agent 步数预算（防子循环失控）
	Bus      *EventBus // 共享父总线：子事件流入同一订阅者（已知代价见下）
}

func NewSubAgentTool(model llm.Model, tools *Registry, maxSteps int, bus *EventBus) *SubAgentTool {
	return &SubAgentTool{Model: model, Tools: tools, MaxSteps: maxSteps, Bus: bus}
}

func (t *SubAgentTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "delegate_task",
		Description: "Delegate a self-contained subtask to a fresh sub-agent with its own context. " +
			"It cannot see this conversation: put ALL needed context, goals and constraints " +
			"into the task. Returns the sub-agent's final answer.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "Complete, self-contained description of the subtask",
				},
			},
			"required": []string{"task"},
		},
	}
}

type subAgentArgs struct {
	Task string `json:"task"`
}

var subAgentSequence atomic.Uint64

func (t *SubAgentTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p subAgentArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("delegate_task: bad args: %w", err)
	}
	if strings.TrimSpace(p.Task) == "" {
		return "", errors.New("delegate_task: empty task")
	}

	// 子 Agent：全新状态 + 继承 ctx + 继承事件总线。
	// 事件通过 AgentID 区分来源，方便并发显示思考和正文。
	//
	// 必须在每次 Run 时 fork，而不能在 NewSubAgentTool 时只 fork 一次：同一个
	// 工具实例会被 executeTools 并发调用，构造期 fork 仍会让所有子代理共享
	// 一个会话。没有 Provider 会话状态的 Model 会直接复用原实例。
	model := t.Model
	if forker, ok := model.(llm.SessionForker); ok {
		model = forker.Fork()
	}
	child := &AgentState{
		AgentID:  fmt.Sprintf("sub-%d", subAgentSequence.Add(1)),
		Model:    model,
		Tools:    t.Tools,
		MaxSteps: t.MaxSteps, // 0 时子 Run 内部回落 DefaultMaxSteps
		Bus:      t.Bus,
	}

	msgs, err := Run(ctx, child, p.Task)
	if err != nil {
		// 错误回喂（Day 3 语义）：子任务失败是给父模型的信息，不是程序故障
		return "", fmt.Errorf("delegate_task: %w", err)
	}

	// 提取子 Agent 的最终回答（末条 assistant 消息）
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant {
			return msgs[i].Content, nil
		}
	}
	return "", errors.New("delegate_task: sub-agent produced no reply")
}
