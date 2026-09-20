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

// Route 是一条候选分支：分类器按 Description 选路。两种形态二选一：
//   - 简单分支：SystemPrompt（+ 可选 Tools）→ 单次 Run
//   - 链分支：Chain 非空 → RunChain，各环节自带 Stage
//
// 同时设置两者 = 歧义配置，runRoute 显式报错（反模式 88 的防线）。
type Route struct {
	Name         string
	Description  string          // 给分类器看的分支画像
	SystemPrompt string          // 简单分支：单次 Run 的 system（与 Chain 互斥）
	Tools        *agent.Registry // 简单分支工具（nil = 纯文本）
	Chain        []ChainStep     // 链分支：非空时按 RunChain 执行（与 SystemPrompt 互斥）
}

// RouteResult 路由执行结果。Route/Reason 必须返回：
// 路由决策本身是核心可观测数据——"选错路"是最隐蔽的失败形态，
// 没有决策记录的路由是 debug 黑洞。
type RouteResult struct {
	Route      string
	Reason     string
	Confidence float64 // 分类置信度；模型未给 = 0；低置信落兜底时保留原值
	Output     string
	Usage      llm.TokenUsage // 分类 + 执行（链含全部环节与重试）累计
}

// FallbackPolicy 兜底策略。零值 = fail-loud（Day 9 原语义）。
type FallbackPolicy struct {
	Route         string  // 兜底分支名，必须存在于 routes；"" = 不启用
	MinConfidence float64 // 置信度阈值 (0,1]，低于它落兜底；0 = 不检查
	// 模型漏给 confidence 按零值 0 处理 → 低于任何阈值 → 保守落兜底
}

type routeDecision struct {
	Route      string  `json:"route"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
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

// RunRoute fail-loud 路由（Day 9 原语义：未知分支/解析失败即报错）。
func RunRoute(ctx context.Context, model llm.Model, bus *agent.EventBus, routes []Route, input string, retries int) (*RouteResult, error) {
	return runRoute(ctx, model, bus, routes, input, routePolicy{Retries: retries})
}

// RunRouteWithFallback 带兜底的路由：分类语义失败或置信度不足时
// 落指定分支而非报错。模型级错误照常上抛（见 FallbackPolicy 注释）。
func RunRouteWithFallback(ctx context.Context, model llm.Model, bus *agent.EventBus, routes []Route, input string, retries int, fb FallbackPolicy) (*RouteResult, error) {
	return runRoute(ctx, model, bus, routes, input, routePolicy{Retries: retries, Fallback: fb})
}

type routePolicy struct {
	Retries  int
	Fallback FallbackPolicy // 零值 = fail-loud
}

// runRoute 路由：一次轻量分类（结构化门 + 重试）+ 按命中分支执行原始任务。
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
func runRoute(ctx context.Context, model llm.Model, bus *agent.EventBus, routes []Route, input string, p routePolicy) (*RouteResult, error) {
	if model == nil {
		return nil, errors.New("workflow: nil model")
	}
	if len(routes) == 0 {
		return nil, errors.New("workflow: no routes")
	}

	// 分支表校验前置：空名 / 重名 / 歧义配置都是启动期编程错误，快速失败
	// （与 Registry.Register 同型）。★ byName 是校验与查找的【唯一真相】：
	// 门用它的键判"分支名已知"，派发用它的值取分支——两者同源 ⇒ 门通过后
	// byName[dec.Route] 必然存在，派发因此无需线性查找，也拿不到 nil。
	byName := make(map[string]*Route, len(routes))
	for i := range routes {
		r := &routes[i]
		if r.Name == "" {
			return nil, errors.New("workflow: route with empty name")
		}
		if _, dup := byName[r.Name]; dup {
			return nil, fmt.Errorf("workflow: duplicate route %q", r.Name)
		}
		if r.SystemPrompt != "" && len(r.Chain) > 0 {
			return nil, fmt.Errorf("workflow: route %q: SystemPrompt 与 Chain 互斥，二选一", r.Name)
		}
		byName[r.Name] = r
	}
	// names 供 parseRouteDecision 的 map[string]bool 签名使用；由 byName
	// 现构而非并行维护，杜绝"两张表不同步"的整类 bug。
	names := make(map[string]bool, len(byName))
	for n := range byName {
		names[n] = true
	}
	fb := p.Fallback
	if fb.Route != "" {
		if !names[fb.Route] {
			return nil, fmt.Errorf("workflow: fallback route %q not in routes", fb.Route)
		}
		if fb.MinConfidence > 1 {
			return nil, fmt.Errorf("workflow: MinConfidence %v 超出 (0,1]", fb.MinConfidence)
		}
	} else if fb.MinConfidence > 0 {
		return nil, errors.New("workflow: FallbackPolicy.MinConfidence 需要同时设置 Route")
	}
	// 分类 = 无工具 Stage + 结构化门；门拒绝 → 带理由重试（复用 runStep）。
	// stepIdx 传 -1：分类不是链上环节，但事件仍需带 AgentID 以便界面区分来源。
	out, usage, err := runStep(ctx, model, bus, -1, ChainStep{
		Stage: Stage{Name: "classify", SystemPrompt: classifierPrompt(routes)},
		Gate: func(ctx context.Context, output string) error {
			_, gerr := parseRouteDecision(output, names)
			return gerr
		},
		Retries: p.Retries,
	}, input)
	if err != nil {
		// 兜底只接管"分类语义失败"（门耗尽 = 输入歧义/输出垃圾）；
		// 模型级错误照常上抛——兜底分支用同一个模型，救不了也不该救（反模式 89）
		if fb.Route != "" && errors.Is(err, ErrGateRejected) {
			return dispatch(ctx, model, bus, byName, routeDecision{
				Route:  fb.Route,
				Reason: "分类未果（不可解析或分支未知），已落兜底",
			}, usage, input)
		}
		return nil, fmt.Errorf("workflow: classify: %w", err)
	}

	// 门通过 ⇒ 此刻解析必成功（解析是确定性的）；防御性保留，不信"不可能"
	dec, perr := parseRouteDecision(out, names)
	if perr != nil {
		return nil, fmt.Errorf("workflow: classify: %w", perr)
	}
	// 低置信度 → 直接落兜底，不重试：歧义在输入不在分类器，
	// 重试烧 token 换不来确定性（反模式 90）
	if fb.Route != "" && fb.MinConfidence > 0 && dec.Confidence < fb.MinConfidence {
		dec = routeDecision{
			Route:      fb.Route,
			Reason:     fmt.Sprintf("置信度 %.2f 低于阈值 %.2f，落兜底", dec.Confidence, fb.MinConfidence),
			Confidence: dec.Confidence,
		}
	}
	return dispatch(ctx, model, bus, byName, dec, usage, input)
}

// dispatch 按命中分支执行【原始任务】：链分支走 RunChain，简单分支单次 Run。
// 这就是作业 1 要体会的"模式的模式"：路由派发只是一个 switch——
// 一侧是单次 Run，一侧是整条链；而链内的 runStep 又复用门机制。
// 组合的深度来自机制的少，不来自机制的多。
// byName 由 runRoute 传入，与门校验同源 ⇒ 查找必命中。
func dispatch(ctx context.Context, model llm.Model, bus *agent.EventBus, byName map[string]*Route, dec routeDecision, usage llm.TokenUsage, input string) (*RouteResult, error) {
	hit, ok := byName[dec.Route]
	if !ok { // dec.Route 经门或兜底校验，理论不可达；防御性保留
		return nil, fmt.Errorf("workflow: internal: route %q vanished", dec.Route)
	}

	// 链分支：输入仍是【原始任务】（反模式 81 同律）；
	// usage 从 ChainResult 贯通到 RouteResult（含链内全部环节与重试）
	if len(hit.Chain) > 0 {
		cres, cerr := RunChain(ctx, model, bus, hit.Chain, input)
		if cres != nil {
			usage.InputTokens += cres.TotalUsage.InputTokens
			usage.OutputTokens += cres.TotalUsage.OutputTokens
		}
		if cerr != nil {
			return nil, fmt.Errorf("workflow: route %q: %w", dec.Route, cerr)
		}
		// 防御性守卫：RunChain 只在 out != "" 时 append，故 Outputs 为空时
		// 这里的 [len-1] 会 panic。当前不可达——产出空串的环节必然带回
		// err（runStep 的 "stage produced no final text"），已在上面 return；
		// 但这份保证依赖 RunChain 的内部实现，一旦它改成"空产出也算成功"
		// （如允许跳过环节），守卫就从冗余变成必需。宁可留一行，不要留 panic。
		if len(cres.Outputs) == 0 {
			return nil, fmt.Errorf("workflow: route %q chain produced no output", dec.Route)
		}
		return &RouteResult{
			Route: dec.Route, Reason: dec.Reason, Confidence: dec.Confidence,
			Output: cres.Outputs[len(cres.Outputs)-1], Usage: usage,
		}, nil
	}

	// 简单分支
	st := &agent.AgentState{Model: model, SystemPrompt: hit.SystemPrompt, Tools: hit.Tools}
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
	return &RouteResult{
		Route:      dec.Route,
		Reason:     dec.Reason,
		Confidence: dec.Confidence,
		Output:     final,
		Usage:      usage,
	}, nil
}
