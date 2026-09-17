package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"minipi/internal/agent"
	"minipi/internal/llm"
	"regexp"
	"strings"
)

// Route 是一条候选分支：分类器按 Description 选路，
// 命中后以 SystemPrompt + Tools 执行原始任务。
type Route struct {
	Name         string
	Description  string // 给分类器看的分支画像
	SystemPrompt string
	Tools        *agent.Registry // nil = 纯文本分支
}

// RouteResult 路由执行结果。Route/Reason 必须返回：
// 路由决策本身是核心可观测数据——"选错路"是最隐蔽的失败形态，
// 没有决策记录的路由是 debug 黑洞。
type RouteResult struct {
	Route  string
	Reason string
	Output string
	Usage  llm.TokenUsage // 分类 + 执行累计
}

type routeDecision struct {
	Route  string `json:"route"`
	Reason string `json:"reason"`
}

// fenceRe 匹配 markdown 代码围栏内容（(?s) 让 . 匹配换行）
var fenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

// extractJSON 从模型输出中健壮提取 JSON 对象，三级降级：
// 1. 原样（TrimSpace 后是合法对象）
// 2. 剥 markdown 围栏
// 3. 首 { 到末 } 的子串（容忍前后缀噪音）
// 全部失败返回 false。核心认知：LLM 的"只输出 JSON"是概率承诺
// 不是协议保证 —— 解析层必须自带卫生（反模式 79 的防线）。
func extractJSON(out string) (string, bool) {
	s := strings.TrimSpace(out)
	if s == "" {
		return "", false
	}
	if strings.HasPrefix(s, "{") && json.Valid([]byte(s)) {
		return s, true
	}
	if m := fenceRe.FindStringSubmatch(s); m != nil {
		inner := strings.TrimSpace(m[1])
		if strings.HasPrefix(inner, "{") && json.Valid([]byte(inner)) {
			return inner, true
		}
	}
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i >= 0 && j > i && json.Valid([]byte(s[i:j+1])) {
		return s[i : j+1], true
	}
	return "", false
}

// parseRouteDecision 解析分类决策：提取 + 反序列化 + 分支名校验。
// 未知分支 = 配置漂移，显式报错而非静默落 default（反模式 80，
// 与 BuildRegistry 的 unknown tool 同型）。失败信息附原始输出——
// 失败信息即诊断材料（反模式 66 精神）。
func parseRouteDecision(out string, names map[string]bool) (routeDecision, error) {
	raw, ok := extractJSON(out)
	if !ok {
		return routeDecision{}, fmt.Errorf("输出不是合法 JSON：%q", firstRunesOf(out, 80))
	}
	var dec routeDecision
	if err := json.Unmarshal([]byte(raw), &dec); err != nil {
		return routeDecision{}, fmt.Errorf("JSON 解析失败：%w（%q）", err, firstRunesOf(raw, 80))
	}
	dec.Route = strings.TrimSpace(dec.Route)
	if dec.Route == "" {
		return routeDecision{}, errors.New("route 字段为空")
	}
	if !names[dec.Route] {
		return routeDecision{}, fmt.Errorf("未知分支 %q（分类器与分支表漂移？）", dec.Route)
	}
	return dec, nil
}

// classifierPrompt 生成分类器 system：分支表即事实的唯一来源，
// prompt 由代码从 []Route 现构——两处永不手工同步（编号 80 的另一半防线）。
func classifierPrompt(routes []Route) string {
	var b strings.Builder
	b.WriteString("你是任务路由分类器。判断用户输入应交给哪个分支处理。\n可用分支：\n")
	for _, r := range routes {
		fmt.Fprintf(&b, "- %s：%s\n", r.Name, r.Description)
	}
	b.WriteString(`只输出一个 JSON 对象：{"route":"<分支名>","reason":"<一句话理由>"}。不要输出任何其他内容。`)
	return b.String()
}

func firstRunesOf(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// RunRoute 路由：一次轻量分类（结构化门 + 重试）+ 按命中分支执行原始任务。
//
// 设计要点：
//   - 分类环节复用 runStep：门 = “输出可解析且分支名已知”，拒绝后
//     重试输入自动携带三件套（任务 + 被拒产物 + 理由，编号 83 的修复版）
//     —— routing 与 chaining 共用同一套门机制，模式是组合不是新机制
//   - 分类环节无工具（反模式 82 的防线）：调度员不是施工队——
//     纯文本调用更便宜更快，也不会被工具调用带偏
//   - 派发环节的输入是【原始任务】而非分类 JSON（反模式 81 的防线）
//   - 分类与执行的 usage 都入账（Day 6 反模式 53 的路由版）
//   - bus 可为 nil（静默）。非 nil 时把总线下传给分类与派发两个环节，
//     与 RunChain 同契约：路由是数据管道，不该屏蔽过程
func RunRoute(ctx context.Context, model llm.Model, bus *agent.EventBus, routes []Route, input string, retries int) (*RouteResult, error) {
	if model == nil {
		return nil, errors.New("workflow: nil model")
	}
	if len(routes) == 0 {
		return nil, errors.New("workflow: no routes")
	}
	// 分支表校验前置：空名 / 重名都是启动期配置错误，快速失败（与 Registry.Register 同型）。
	// ★ byName 是校验与查找的【唯一真相】：门用它的键判"分支名已知"，
	// 派发用它的值取分支——两者同源 ⇒ 门通过后 byName[dec.Route] 必然存在，
	// 派发环节因此无需再"找一遍"，也不可能拿到 nil。
	byName := make(map[string]*Route, len(routes))
	names := make(map[string]bool, len(routes))
	for i := range routes {
		r := &routes[i]
		if r.Name == "" {
			return nil, errors.New("workflow: route with empty name")
		}
		if _, dup := byName[r.Name]; dup {
			return nil, fmt.Errorf("workflow: duplicate route %q", r.Name)
		}
		byName[r.Name] = r
		names[r.Name] = true
	}

	// 分类 = 无工具 Stage + 结构化门；门拒绝 → 带理由重试（复用 runStep）。
	// stepIdx 传 -1：分类不是链上环节，但事件仍需带 AgentID 以便界面区分来源。
	out, usage, err := runStep(ctx, model, bus, -1, ChainStep{
		Stage: Stage{Name: "classify", SystemPrompt: classifierPrompt(routes)},
		Gate: func(ctx context.Context, output string) error {
			_, gerr := parseRouteDecision(output, names)
			return gerr
		},
		Retries: retries,
	}, input)
	if err != nil {
		return nil, fmt.Errorf("workflow: classify: %w", err)
	}

	// 门通过 ⇒ 此刻解析必成功（解析是确定性的）；防御性保留，不信"不可能"
	dec, perr := parseRouteDecision(out, names)
	if perr != nil {
		return nil, fmt.Errorf("workflow: classify: %w", perr)
	}

	// 派发：命中分支执行【原始输入】。
	// hit 理论上必存在（见 byName 注释），但这是跨函数推理出的不变量——
	// 显式判空把"不可能"变成"不会崩"：将来若门被放宽，这里返回可诊断的
	// error 而不是给调用方一个 nil 解引用 panic（反模式 66 精神）。
	hit, ok := byName[dec.Route]
	if !ok {
		return nil, fmt.Errorf("workflow: route %q 不在分支表中（分类器与分支表漂移？）", dec.Route)
	}
	st := &agent.AgentState{
		AgentID:      "route-1",
		Model:        model,
		SystemPrompt: hit.SystemPrompt,
		Tools:        hit.Tools,
		Bus:          bus, // nil = 静默；非 nil = 派发环节的 delta 实时出流
	}
	msgs, rerr := agent.Run(ctx, st, input)
	usage.InputTokens += st.TotalUsage.InputTokens
	usage.OutputTokens += st.TotalUsage.OutputTokens
	if rerr != nil {
		return nil, fmt.Errorf("workflow: route %q: %w", dec.Route, rerr)
	}
	final, ok := agent.FinalText(msgs)
	if !ok {
		return nil, fmt.Errorf("workflow: route %q produced no final text", dec.Route)
	}

	return &RouteResult{Route: dec.Route, Reason: dec.Reason, Output: final, Usage: usage}, nil
}
