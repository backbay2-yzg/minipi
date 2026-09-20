package workflow

import (
	"context"
	"errors"
	"minipi/internal/agent"
	"minipi/internal/llm"
	"strings"
	"testing"
)

// extractJSON 表驱动测试：三级降级的每个入口 + 失败形态。
// fixture 按真实 LLM 噪音的形态构造，不是合法样本的复读。
func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"raw", `{"a":"b"}`, `{"a":"b"}`, true},
		{"fenced", "```json\n{\"a\":\"b\"}\n```", `{"a":"b"}`, true},
		{"fenced_no_lang", "```\n{\"a\":\"b\"}\n```", `{"a":"b"}`, true},
		{"noisy", "答案如下：{\"a\":\"b\"} 请查收", `{"a":"b"}`, true},
		{"no_json", "就是一段话", "", false},
		{"broken", `{"a":`, "", false},
	}
	for _, c := range cases {
		got, ok := extractJSON(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: extractJSON(%q) = %q,%v; want %q,%v",
				c.name, c.in, got, ok, c.want, c.ok)
		}
	}
}

// testRoutes 是路由测试的公共分支表：一个带工具的技术分支 + 一个纯文本闲聊分支。
// tech 挂 calc 工具（NewCalcTool）用于验证「工具只随命中分支派发」。
func testRoutes() []Route {
	return []Route{
		{
			Name:         "tech",
			Description:  "技术问题：计算、时间查询、编程",
			SystemPrompt: "你是技术助手，用工具精确解答。",
			Tools:        agent.NewRegistry(agent.NewCalcTool()),
		},
		{
			Name:         "chat",
			Description:  "闲聊、寒暄、开放话题",
			SystemPrompt: "你是轻松的聊天伙伴。",
		},
	}
}

// 场景 1：干净 JSON → 选路 + 原始任务派发 + system/工具隔离
func TestRunRouteHappyPath(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: `{"route":"tech","reason":"计算类问题"}`, StopReason: llm.StopReasonEndTurn},
		{Text: "42", StopReason: llm.StopReasonEndTurn},
	}}
	res, err := RunRoute(context.Background(), m, nil, testRoutes(), "计算 6*7", 0)
	if err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if res.Route != "tech" || res.Output != "42" {
		t.Fatalf("res = %+v", res)
	}
	// 派发输入 = 原始任务（不是分类 JSON）
	disp := m.Calls[1].Messages
	if len(disp) != 1 || disp[0].Content != "计算 6*7" {
		t.Errorf("dispatch input = %+v, want original task", disp)
	}
	// 派发 system = 命中分支的（路由隔离证据）
	if m.Calls[1].System != "你是技术助手，用工具精确解答。" {
		t.Errorf("dispatch system = %q", m.Calls[1].System)
	}
	// 分类请求零工具（调度员不带施工队）；派发请求带命中分支的工具
	if len(m.Calls[0].Tools) != 0 {
		t.Errorf("classify must carry no tools, got %d", len(m.Calls[0].Tools))
	}
	if len(m.Calls[1].Tools) != 1 {
		t.Errorf("dispatch tools = %d, want 1", len(m.Calls[1].Tools))
	}
}

// 场景 2：围栏 + 前后噪音 —— 三级提取兜底，路由照常工作
func TestRunRouteNoisyJSON(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "好的，分类结果：\n```json\n{\"route\":\"chat\",\"reason\":\"闲聊\"}\n```\n以上。",
			StopReason: llm.StopReasonEndTurn},
		{Text: "你好呀！", StopReason: llm.StopReasonEndTurn},
	}}
	res, err := RunRoute(context.Background(), m, nil, testRoutes(), "你好", 0)
	if err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if res.Route != "chat" || res.Reason != "闲聊" || res.Output != "你好呀！" {
		t.Fatalf("res = %+v", res)
	}
}

// 场景 3：分类输出非 JSON → 门拒绝 → 带理由重试 → 通过
// 断言与修复后的 runStep 对齐：重试输入含被拒产物（三件套，编号 83）
func TestRunRouteRetryOnGarbage(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "这个问题嘛，我看走技术路线比较好。", StopReason: llm.StopReasonEndTurn},
		{Text: `{"route":"tech","reason":"改主意了"}`, StopReason: llm.StopReasonEndTurn},
		{Text: "42", StopReason: llm.StopReasonEndTurn},
	}}
	res, err := RunRoute(context.Background(), m, nil, testRoutes(), "计算 6*7", 1)
	if err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if res.Route != "tech" {
		t.Fatalf("route = %q", res.Route)
	}
	retry := m.Calls[1].Messages[0].Content
	if !strings.Contains(retry, "不是合法 JSON") {
		t.Errorf("retry lacks feedback: %q", retry)
	}
	if !strings.Contains(retry, "计算 6*7") {
		t.Error("retry lost original task")
	}
	if !strings.Contains(retry, "这个问题嘛") {
		t.Error("retry lacks rejected output (antipattern 83 fix)")
	}
	if len(m.Calls) != 3 {
		t.Errorf("calls = %d, want 3", len(m.Calls))
	}
}

// 场景 4：JSON 合法但分支未知 → 拒绝；耗尽重试 → 中止（不静默落 default）
func TestRunRouteUnknownRoute(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: `{"route":"unknown","reason":"?"}`, StopReason: llm.StopReasonEndTurn},
		{Text: `{"route":"unknown","reason":"还是?"}`, StopReason: llm.StopReasonEndTurn},
	}}
	_, err := RunRoute(context.Background(), m, nil, testRoutes(), "x", 1)
	if err == nil || !strings.Contains(err.Error(), "未知分支") {
		t.Fatalf("err = %v, want unknown-route error", err)
	}
	if len(m.Calls) != 2 { // 分类 2 次（初始 + 重试），派发 0 次
		t.Errorf("calls = %d, want 2", len(m.Calls))
	}
}

// 场景 5：route 字段为空 → 门以具体理由拒绝 → 重试通过
func TestRunRouteEmptyRouteField(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: `{"route":"","reason":"懒得选"}`, StopReason: llm.StopReasonEndTurn},
		{Text: `{"route":"chat","reason":"正常了"}`, StopReason: llm.StopReasonEndTurn},
		{Text: "好", StopReason: llm.StopReasonEndTurn},
	}}
	res, err := RunRoute(context.Background(), m, nil, testRoutes(), "hi", 1)
	if err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if res.Route != "chat" {
		t.Fatalf("route = %q", res.Route)
	}
	if !strings.Contains(m.Calls[1].Messages[0].Content, "route 字段为空") {
		t.Errorf("retry lacks specific feedback")
	}
}

// 场景 6：分支表防御 —— 空表 / 重名
func TestRunRouteBadTable(t *testing.T) {
	if _, err := RunRoute(context.Background(), &llm.MockModel{}, nil, nil, "x", 0); err == nil {
		t.Error("empty routes must error")
	}
	dup := []Route{
		{Name: "a", SystemPrompt: "x"},
		{Name: "a", SystemPrompt: "y"},
	}
	if _, err := RunRoute(context.Background(), &llm.MockModel{}, nil, dup, "x", 0); err == nil {
		t.Error("duplicate route must error")
	}
}

// ───────────── 作业 1：路由 + 链组合 ─────────────

// 场景 7：链分支 —— 命中后 RunChain 执行，usage/输出贯通
func TestRunRouteChainBranch(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: `{"route":"compose","reason":"写作请求","confidence":0.9}`,
			StopReason: llm.StopReasonEndTurn, Usage: llm.TokenUsage{InputTokens: 10, OutputTokens: 5}},
		{Text: "草稿好了", StopReason: llm.StopReasonEndTurn, Usage: llm.TokenUsage{InputTokens: 20, OutputTokens: 10}},
		{Text: "终稿", StopReason: llm.StopReasonEndTurn, Usage: llm.TokenUsage{InputTokens: 30, OutputTokens: 10}},
	}}
	routes := []Route{
		{Name: "compose", Description: "写作", Chain: []ChainStep{
			{Stage: Stage{Name: "draft", SystemPrompt: "打草稿"}},
			{Stage: Stage{Name: "refine", SystemPrompt: "精修"}},
		}},
		{Name: "chat", Description: "闲聊", SystemPrompt: "聊天"},
	}
	res, err := RunRoute(context.Background(), m, nil, routes, "写一段", 0)
	if err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if len(m.Calls) != 3 { // 分类 + 链两环节
		t.Fatalf("calls = %d, want 3", len(m.Calls))
	}
	// 链首环节输入 = 原始任务（反模式 81 同律），system = 该环节自己的
	if m.Calls[1].Messages[0].Content != "写一段" || m.Calls[1].System != "打草稿" {
		t.Errorf("chain stage1: sys %q, input %+v", m.Calls[1].System, m.Calls[1].Messages[0])
	}
	if res.Output != "终稿" || res.Route != "compose" || res.Confidence != 0.9 {
		t.Errorf("res = %+v", res)
	}
	// usage 贯通：分类(10,5) + 草稿(20,10) + 精修(30,10)
	if res.Usage.InputTokens != 60 || res.Usage.OutputTokens != 25 {
		t.Errorf("usage = %+v, want in=60 out=25", res.Usage)
	}
}

// 场景 8：SystemPrompt 与 Chain 同时设置 → 歧义配置显式报错
func TestRunRoutePromptChainConflict(t *testing.T) {
	routes := []Route{{Name: "x", SystemPrompt: "a",
		Chain: []ChainStep{{Stage: Stage{Name: "s"}}}}}
	_, err := RunRoute(context.Background(), &llm.MockModel{}, nil, routes, "t", 0)
	if err == nil || !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("err = %v, want mutual-exclusion error", err)
	}
}

// ───────────── 作业 2：置信度与兜底 ─────────────

// 场景 9：低置信度 → 落兜底（不报错、不重试分类）
func TestRunRouteLowConfidenceFallback(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: `{"route":"tech","reason":"看着像技术题","confidence":0.4}`, StopReason: llm.StopReasonEndTurn},
		{Text: "闲聊回复", StopReason: llm.StopReasonEndTurn},
	}}
	res, err := RunRouteWithFallback(context.Background(), m, nil, testRoutes(), "算个啥", 1,
		FallbackPolicy{Route: "chat", MinConfidence: 0.7})
	if err != nil {
		t.Fatalf("RunRouteWithFallback: %v", err)
	}
	if res.Route != "chat" {
		t.Fatalf("route = %q, want chat (fallback)", res.Route)
	}
	if !strings.Contains(res.Reason, "置信度") || res.Confidence != 0.4 {
		t.Errorf("reason/conf = %q / %v", res.Reason, res.Confidence)
	}
	// 派发真的去了兜底分支：system 是 chat 的
	if m.Calls[1].System != "你是轻松的聊天伙伴。" {
		t.Errorf("dispatch system = %q, want chat branch", m.Calls[1].System)
	}
	// 低置信度不重试：恰好 2 次调用
	if len(m.Calls) != 2 {
		t.Errorf("calls = %d, want 2 (no retry on low confidence)", len(m.Calls))
	}
}

// 场景 10：边界 —— confidence == 阈值 不落兜底（严格小于才落）
func TestRunRouteConfidenceBoundary(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: `{"route":"tech","reason":"够确信","confidence":0.7}`, StopReason: llm.StopReasonEndTurn},
		{Text: "42", StopReason: llm.StopReasonEndTurn},
	}}
	res, err := RunRouteWithFallback(context.Background(), m, nil, testRoutes(), "计算 6*7", 0,
		FallbackPolicy{Route: "chat", MinConfidence: 0.7})
	if err != nil {
		t.Fatalf("RunRouteWithFallback: %v", err)
	}
	if res.Route != "tech" {
		t.Errorf("boundary confidence must not fall back, got %q", res.Route)
	}
}

// 场景 11：分类垃圾 + 重试耗尽 → 兜底而非报错
func TestRunRouteGarbageFallsBack(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "这个问题嘛，我看走技术路线比较好。", StopReason: llm.StopReasonEndTurn},
		{Text: "还是不想输出 JSON。", StopReason: llm.StopReasonEndTurn},
		{Text: "兜底回复", StopReason: llm.StopReasonEndTurn},
	}}
	res, err := RunRouteWithFallback(context.Background(), m, nil, testRoutes(), "算个啥", 1,
		FallbackPolicy{Route: "chat", MinConfidence: 0.7})
	if err != nil {
		t.Fatalf("RunRouteWithFallback: %v", err)
	}
	// 3 次调用 = 分类×2（含重试）+ 兜底派发×1；重试输入带门理由
	if len(m.Calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(m.Calls))
	}
	if !strings.Contains(m.Calls[1].Messages[0].Content, "不是合法 JSON") {
		t.Errorf("retry lacks gate feedback")
	}
	if res.Route != "chat" || res.Output != "兜底回复" {
		t.Errorf("res = %+v", res)
	}
}

// 场景 12：模型级错误不落兜底——错误经 classify 路径上抛。
// 用预取消 ctx 构造模型级失败（Day 7 验证过：Run 先查 ctx，零调用）。
// 区分证据是错误包装层：classify 路径含 “workflow: classify”，
// 兜底派发路径是 “workflow: route …”——两者必居其一，确定可断言。
func TestRunRouteModelErrorNoFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RunRouteWithFallback(ctx, &llm.MockModel{}, nil, testRoutes(), "x", 1,
		FallbackPolicy{Route: "chat", MinConfidence: 0.7})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "workflow: classify") {
		t.Errorf("model-level error must propagate via classify, got: %v", err)
	}
}

// 场景 13：兜底配置防御 —— 幽灵分支名 / 只给阈值不给分支
func TestRunRouteBadFallback(t *testing.T) {
	_, err := RunRouteWithFallback(context.Background(), &llm.MockModel{}, nil, testRoutes(), "x", 0,
		FallbackPolicy{Route: "ghost"})
	if err == nil || !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("err = %v, want bad-fallback error", err)
	}
	_, err = RunRouteWithFallback(context.Background(), &llm.MockModel{}, nil, testRoutes(), "x", 0,
		FallbackPolicy{MinConfidence: 0.7})
	if err == nil || !strings.Contains(err.Error(), "MinConfidence") {
		t.Fatalf("err = %v, want coherence error", err)
	}
}

// 场景 14：锁住 ErrGateRejected 哨兵契约。
// 兜底能否生效，全系于 runStep 的"重试耗尽"错误能被 errors.Is 命中——
// 它依赖 fmt.Errorf 的双重 %w（Go 1.20+，见 chain.go 注释）。
// 若将来 go 指令降级或有人把 %w 改成 %v，兜底会静默失灵且其余测试照过，
// 故此处直接断言哨兵本身：这是整套 fallback 的承重墙。
func TestErrGateRejectedSentinel(t *testing.T) {
	step := ChainStep{
		Stage:   Stage{Name: "gated", SystemPrompt: "s"},
		Gate:    func(ctx context.Context, output string) error { return errors.New("不合格") },
		Retries: 0, // 拒绝即耗尽，直接走哨兵路径
	}
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: "产物", StopReason: llm.StopReasonEndTurn},
	}}
	_, _, err := runStep(context.Background(), m, nil, 0, step, "任务")
	if !errors.Is(err, ErrGateRejected) {
		t.Fatalf("errors.Is(err, ErrGateRejected) = false; 兜底将静默失灵。err = %v", err)
	}
	// 门的具体理由必须仍在错误链里（三件套的"理由"靠它回喂）
	if !strings.Contains(err.Error(), "不合格") {
		t.Errorf("gate reason lost in error chain: %v", err)
	}
}

// 场景 15：链环节产出空串 → 走 RunChain 的 stage-no-output 错误路径，
// 不 panic、不命中 dispatch 的 Outputs 下标。这是对"链无可交付产物"这一
// 边界的行为固化：错误必须显式带链环节名，便于定位是哪个 Stage 哑火。
func TestRunRouteChainEmptyOutput(t *testing.T) {
	m := &llm.MockModel{Responses: []llm.Completion{
		{Text: `{"route":"compose","reason":"写作"}`, StopReason: llm.StopReasonEndTurn},
		{Text: "", StopReason: llm.StopReasonEndTurn}, // 链环节产出空串
	}}
	routes := []Route{
		{Name: "compose", Description: "写作", Chain: []ChainStep{
			{Stage: Stage{Name: "draft", SystemPrompt: "打草稿"}},
		}},
	}
	_, err := RunRoute(context.Background(), m, nil, routes, "写一段", 0)
	if err == nil {
		t.Fatal("empty chain output must error, not panic")
	}
	// 错误应指向具体环节（draft），而非静默返回空 Output
	if !strings.Contains(err.Error(), "draft") {
		t.Errorf("err = %v, want stage name in error", err)
	}
}
