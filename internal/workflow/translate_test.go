package workflow

import (
	"context"
	"strings"
	"testing"

	"minipi/internal/llm"
)

// 场景 1：长度门 —— 换算量纲后的通过/拒绝双向验证
func TestLengthFloorGate(t *testing.T) {
	src := strings.Repeat("word ", 12) // 12 词 → 下限 9 字
	g := LengthFloorGate(src)
	ctx := context.Background()

	if err := g(ctx, "太短"); err == nil { // 2 字 < 9
		t.Error("degenerate translation must be rejected")
	}
	if err := g(ctx, "这是一段足够长的合格中文译文"); err != nil { // 14 字 ≥ 9
		t.Errorf("adequate translation rejected: %v", err)
	}
}

// 场景 2：残留门 —— 白名单豁免 / 残留拒绝 / 短 token 天然豁免
func TestResidueGate(t *testing.T) {
	g := ResidueGate([]string{"Kubernetes"})
	ctx := context.Background()

	if err := g(ctx, "这是 Kubernetes 的部署译文"); err != nil {
		t.Errorf("allowlisted term rejected: %v", err)
	}
	if err := g(ctx, "这个 however 没有翻译"); err == nil {
		t.Error("residue must be rejected")
	}
	if err := g(ctx, "使用 Go 与 SQL 和 API"); err != nil {
		t.Errorf("short tokens must be exempt: %v", err)
	}
}

// 场景 3：三段链 + 审校门重试 —— 残留被拒、带理由重做、干净通过
// 链纯串行：MockModel 单队列 FIFO 与执行序天然对齐（对照 61/62）
func TestTranslateChainReviewRetry(t *testing.T) {
	src := "The quick brown fox jumps over the lazy dog and runs away." // 12 词
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "敏捷的棕色狐狸跳过懒惰的狗，然后跑走了。", StopReason: llm.StopReasonEndTurn},    // 翻译：20 字 ≥9 ✓
		{Text: "敏捷的棕色狐狸跳过 however 懒惰的狗。", StopReason: llm.StopReasonEndTurn}, // 审校1：残留 → 拒
		{Text: "敏捷的棕色狐狸跳过了那只懒惰的狗。", StopReason: llm.StopReasonEndTurn},       // 审校2：干净 ✓
		{Text: "灵敏的棕狐一跃而过懒狗。", StopReason: llm.StopReasonEndTurn},            // 润色
	}}

	res, err := RunChain(context.Background(), m, nil, TranslateChain(src), src)
	if err != nil {
		t.Fatalf("RunChain: %v", err)
	}

	if len(m.Calls) != 4 { // 翻译 + 审校×2 + 润色
		t.Fatalf("calls = %d, want 4", len(m.Calls))
	}
	// 审校 system 含烘焙的原文（对照审校的证据）
	if !strings.Contains(m.Calls[1].System, "quick brown fox") {
		t.Error("review system lacks baked source text")
	}
	// 重试输入 = 初译 + 拒绝理由（含残留 token 本身）
	retry := m.Calls[2].Messages[0].Content
	if !strings.Contains(retry, "however") {
		t.Errorf("retry input lacks feedback: %q", retry)
	}
	if !strings.Contains(retry, "敏捷的棕色狐狸") {
		t.Error("retry input lost stage input (initial translation)")
	}
	if len(res.Outputs) != 3 || res.Outputs[2] != "灵敏的棕狐一跃而过懒狗。" {
		t.Errorf("outputs = %q", res.Outputs)
	}
}
