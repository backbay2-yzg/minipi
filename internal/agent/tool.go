package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"minipi/internal/llm"
	"strconv"
	"time"
)

// Tool 是"一个可被模型调用的能力"的完整契约：
//
//	Spec() 声明给模型看（名字/描述/参数 schema）
//	Run()  被程序实际执行
//
// 注册一次，两处同时生效 —— 消灭 Day 2 的两处维护。
//
// args 用 json.RawMessage：Registry 不做反射绑定，
// 参数解析权完全交给工具自身（错误也更早、更可控地回喂）。
type Tool interface {
	Spec() llm.ToolSpec
	Run(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry 按名索引工具。
// 并发约定：注册发生在启动期（NewRegistry / Register），
// 运行期只读 —— 单 goroutine 主循环下无需加锁。
// （将来热插拔工具时再补 RWMutex，不提前抽象）
type Registry struct {
	tools map[string]Tool
	order []string // 保持注册顺序，Specs() 输出稳定 → 请求可复用、测试可断言
}

func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{tools: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		r.Register(t)
	}
	return r
}

func (r *Registry) Register(t Tool) {
	spec := t.Spec()
	if spec.Name == "" {
		panic("agent: tool with empty name")
	}
	if _, dup := r.tools[spec.Name]; dup {
		panic("agent: duplicate tool " + spec.Name) // 启动期编程错误，快速失败
	}
	r.tools[spec.Name] = t
	r.order = append(r.order, spec.Name)
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) Specs() []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name].Spec())
	}
	return out
}

// Names 按注册顺序返回工具名（持久化用；返回拷贝，防外部改乱 order）
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// BuildRegistry 从名字列表重建 Registry（会话恢复路径）。
// factories 是"名字 → 构造函数"映射，由组合根（main）提供——
// agent 包依旧不认识任何具体工具，依赖方向保持干净。
// 未知名字 = 配置漂移（会话由不同版本代码创建）→ 显式报错，不静默跳过。
func BuildRegistry(names []string, factories map[string]func() Tool) (*Registry, error) {
	reg := NewRegistry()
	for _, n := range names {
		f, ok := factories[n]
		if !ok {
			return nil, fmt.Errorf("agent: unknown tool %q (session created by a different version?)", n)
		}
		reg.Register(f())
	}
	return reg, nil
}

type TimeTool struct{}

func NewTimeTool() *TimeTool {
	return &TimeTool{}
}

func (t *TimeTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "get_time",
		Description: "Returns the current local time in RFC3339 format.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func (t *TimeTool) Run(_ context.Context, _ json.RawMessage) (string, error) {
	return time.Now().Format(time.RFC3339), nil
}

type CalcTool struct{}

func NewCalcTool() *CalcTool { return &CalcTool{} }

type calcArgs struct {
	A  float64 `json:"a"`
	B  float64 `json:"b"`
	Op string  `json:"op"`
}

func (t *CalcTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "calc",
		Description: "Evaluate an arithmetic expression on two numbers.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a":  map[string]any{"type": "number"},
				"b":  map[string]any{"type": "number"},
				"op": map[string]any{"type": "string", "enum": []string{"+", "-", "*", "/"}},
			},
			"required": []string{"a", "b", "op"},
		},
	}
}

func (t *CalcTool) Run(_ context.Context, args json.RawMessage) (string, error) {
	var p calcArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("calc: bad args: %w", err) // 错误将回喂给模型自愈
	}
	var r float64
	switch p.Op {
	case "+":
		r = p.A + p.B
	case "-":
		r = p.A - p.B
	case "*":
		r = p.A * p.B
	case "/":
		if p.B == 0 {
			return "", errors.New("calc: division by zero")
		}
		r = p.A / p.B
	default:
		return "", fmt.Errorf("calc: unsupported op %q", p.Op)
	}
	return strconv.FormatFloat(r, 'f', -1, 64), nil
}
