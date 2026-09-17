package workflow

import (
	"context"
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
