package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"minipi/internal/agent"
	"minipi/internal/llm"

	"go.uber.org/goleak"
)

// TestMain：workflow 包同样置于 goleak 哨兵之下（Day 5 起的纪律）
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// 场景 1：两环节链 —— 输出传递、状态隔离、成本汇总
// 注：链是纯串行，MockModel 单队列 FIFO 与执行顺序天然对齐
// （对照 Day 7 反模式 61/62：危险的是"共享 + 交错"，不是共享本身）
func TestChainHappyPath(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "1. 背景\n2. 设计\n3. 总结", StopReason: llm.StopReasonEndTurn,
			Usage: llm.TokenUsage{InputTokens: 10, OutputTokens: 5}},
		{Text: "正文……", StopReason: llm.StopReasonEndTurn,
			Usage: llm.TokenUsage{InputTokens: 20, OutputTokens: 8}},
	}}
	steps := []ChainStep{
		{Stage: Stage{Name: "outline", SystemPrompt: "生成提纲"}},
		{Stage: Stage{Name: "article", SystemPrompt: "写正文"}},
	}

	res, err := RunChain(context.Background(), m, nil, steps, "Go 的优缺点")
	if err != nil {
		t.Fatalf("RunChain: %v", err)
	}

	if len(res.Outputs) != 2 || res.Outputs[0] != "1. 背景\n2. 设计\n3. 总结" {
		t.Fatalf("outputs = %q", res.Outputs)
	}
	if len(m.Calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(m.Calls))
	}
	// ★ 状态隔离证据：环节 2 的视图只有 1 条 user = 环节 1 的输出
	second := m.Calls[1].Messages
	if len(second) != 1 || second[0].Content != "1. 背景\n2. 设计\n3. 总结" {
		t.Errorf("stage2 view = %+v, want [outline output] only", second)
	}
	// system 隔离：各环节用各的
	if m.Calls[0].System != "生成提纲" || m.Calls[1].System != "写正文" {
		t.Errorf("system leaked across stages: %q / %q", m.Calls[0].System, m.Calls[1].System)
	}
	// 成本汇总：in=10+20, out=5+8
	if res.TotalUsage.InputTokens != 30 || res.TotalUsage.OutputTokens != 13 {
		t.Errorf("usage = %+v, want in=30 out=13", res.TotalUsage)
	}
}

// 场景 2：门拒绝 → 带理由重试 → 通过
func TestChainGateRetry(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "没有编号的提纲", StopReason: llm.StopReasonEndTurn},
		{Text: "1. 背景\n2. 设计", StopReason: llm.StopReasonEndTurn},
		{Text: "正文", StopReason: llm.StopReasonEndTurn},
	}}
	gate := func(ctx context.Context, out string) error {
		if !strings.Contains(out, "1.") {
			return errors.New("提纲必须含编号要点")
		}
		return nil
	}
	steps := []ChainStep{
		{Stage: Stage{Name: "outline"}, Gate: gate, Retries: 1},
		{Stage: Stage{Name: "article"}},
	}

	res, err := RunChain(context.Background(), m, nil, steps, "topic")
	if err != nil {
		t.Fatalf("RunChain: %v", err)
	}

	// 3 次调用 = 环节1×2（重试）+ 环节2×1
	if len(m.Calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(m.Calls))
	}
	// 重试输入携带拒绝理由 + 原始任务（编号 71 的两面防线）
	retry := m.Calls[1].Messages[0].Content
	if !strings.Contains(retry, "提纲必须含编号要点") {
		t.Errorf("retry input lacks rejection feedback: %q", retry)
	}
	if !strings.Contains(retry, "topic") {
		t.Error("retry input lost original task")
	}
	if res.Outputs[0] != "1. 背景\n2. 设计" {
		t.Errorf("outputs[0] = %q", res.Outputs[0])
	}
}

// 场景 3：门拒绝且 Retries=0 → 中止，下游环节绝不执行
func TestChainGateFatal(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "bad", StopReason: llm.StopReasonEndTurn},
	}}
	steps := []ChainStep{
		{Stage: Stage{Name: "outline"}, Gate: alwaysReject, Retries: 0},
		{Stage: Stage{Name: "article"}},
	}

	res, err := RunChain(context.Background(), m, nil, steps, "t")
	if err == nil || !strings.Contains(err.Error(), "gate rejected") {
		t.Fatalf("err = %v, want gate rejection", err)
	}
	// 中止而不带病续跑（编号 73）：环节 2 从未执行
	if len(m.Calls) != 1 {
		t.Errorf("calls = %d, want 1 (chain must stop)", len(m.Calls))
	}
	// 被拒产出随错误返回——失败现场要可见
	if res == nil || len(res.Outputs) != 1 || res.Outputs[0] != "bad" {
		t.Errorf("partial outputs = %q", res.Outputs)
	}
}

func alwaysReject(ctx context.Context, out string) error {
	return errors.New("nope")
}

// 场景 4：空链防御
func TestChainEmpty(t *testing.T) {
	_, err := RunChain(context.Background(), &llm.MockModel{}, nil, nil, "x")
	if err == nil || !strings.Contains(err.Error(), "empty chain") {
		t.Fatalf("err = %v, want empty chain error", err)
	}
}

// 场景 5：注入总线后，链内各环节的流式 delta 实时可见（含环节标注）。
// 这是 "/chain 为什么没有流式输出" 的回归防线：链必须把 bus 传进每个
// 环节的 AgentState，而不是让它停在 nil。
func TestChainStreamsEvents(t *testing.T) {
	m := &llm.MockStreaming{
		MockModel: llm.MockModel{Responses: []llm.Completion{
			{Text: "1. 背景\n2. 设计", StopReason: llm.StopReasonEndTurn},
			{Text: "正文……", StopReason: llm.StopReasonEndTurn},
		}},
		ChunkSize: 4,
	}
	bus := agent.NewEventBus(128)
	ch, cancel := bus.Subscribe()
	defer cancel()

	steps := []ChainStep{
		{Stage: Stage{Name: "outline"}},
		{Stage: Stage{Name: "article"}},
	}
	if _, err := RunChain(context.Background(), m, bus, steps, "Go"); err != nil {
		t.Fatalf("RunChain: %v", err)
	}

	// 收集全部 message_update，按 AgentID 拼回各环节的完整输出。
	got := map[string]string{}
	for {
		select {
		case ev := <-ch:
			if ev.Kind == agent.EventMessageUpdate {
				got[ev.AgentID] += ev.Delta
			}
		default:
			// 链是同步执行的，返回时事件已全部入缓冲
			if len(got) == 2 {
				goto done
			}
			t.Fatalf("streamed deltas = %v, want both stages", got)
		}
	}
done:
	if got["chain-1"] != "1. 背景\n2. 设计" {
		t.Errorf("stage1 deltas = %q", got["chain-1"])
	}
	if got["chain-2"] != "正文……" {
		t.Errorf("stage2 deltas = %q", got["chain-2"])
	}
}
