package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"minipi/internal/llm"
)

func quiet(string) {}

// 场景 1：y → 通过
func TestHumanGateApprove(t *testing.T) {
	in := make(chan string)
	g := HumanGate(in, quiet)
	go func() { in <- "y" }()
	if err := g(context.Background(), "产物"); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

// 场景 2：n + 理由 → 理由嵌入拒绝错误（重试反馈的原料）
func TestHumanGateRejectWithReason(t *testing.T) {
	in := make(chan string)
	g := HumanGate(in, quiet)
	go func() {
		in <- "n"
		in <- "第二段漏译了"
	}()
	err := g(context.Background(), "产物")
	if err == nil || !strings.Contains(err.Error(), "第二段漏译了") {
		t.Fatalf("err = %v, want embedded reason", err)
	}
}

// 场景 3：无效输入 → 不猜不默认，提示后继续等
func TestHumanGateInvalidThenApprove(t *testing.T) {
	in := make(chan string)
	g := HumanGate(in, quiet)
	go func() {
		in <- "随便打的"
		in <- "Y"
	}()
	if err := g(context.Background(), "产物"); err != nil {
		t.Fatalf("invalid-then-approve: %v", err)
	}
}

// 场景 4：ctx 取消 → 门立即让路（goleak 顺带验证无泄漏 goroutine）
func TestHumanGateCtxCancel(t *testing.T) {
	in := make(chan string)
	g := HumanGate(in, quiet)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if err := g(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// 场景 5：人在环终门嵌进全链 —— 否决理由真的流进重试输入
func TestTranslateChainHumanFinalGate(t *testing.T) {
	src := "Go is fast." // 3 词 → 下限 2 字
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "Go 很快。", StopReason: llm.StopReasonEndTurn},
		{Text: "Go 语言很快。", StopReason: llm.StopReasonEndTurn},
		{Text: "Go 语言速度飞快。", StopReason: llm.StopReasonEndTurn},        // 润色1：被人否决
		{Text: "Go 语言速度极快，编译瞬间完成。", StopReason: llm.StopReasonEndTurn}, // 润色2：按理由重做
	}}

	steps := TranslateChain(src)
	in := make(chan string)
	steps[2].Gate = HumanGate(in, quiet)
	steps[2].Retries = 1

	// 用户剧本：否决（给理由）→ 重做 → 通过
	go func() {
		in <- "n"
		in <- "结尾太仓促"
		in <- "y"
	}()

	res, err := RunChain(context.Background(), m, nil, steps, src)
	if err != nil {
		t.Fatalf("RunChain: %v", err)
	}
	if len(m.Calls) != 4 { // 翻译 + 审校 + 润色 + 润色重试
		t.Fatalf("calls = %d, want 4", len(m.Calls))
	}
	// 润色重试输入：含人给的否决理由 + 被否决的润色产物
	retry := m.Calls[3].Messages[0].Content
	if !strings.Contains(retry, "结尾太仓促") {
		t.Errorf("retry input lacks human feedback: %q", retry)
	}
	if !strings.Contains(retry, "Go 语言速度飞快") {
		t.Error("retry input lost the rejected polish output")
	}
	if res.Outputs[2] != "Go 语言速度极快，编译瞬间完成。" {
		t.Errorf("final = %q", res.Outputs[2])
	}
}
