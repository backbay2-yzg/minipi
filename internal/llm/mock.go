package llm

import (
	"context"
	"fmt"
)

// MockModel 按顺序吐出预设的 Completion，用于离线测试上层逻辑。
// Day 2 的 Agent 循环测试全靠它，一分钱 API 费都不用花。
type MockModel struct {
	Responses []Completion        // 队列：每次 Complete 弹出一个
	Calls     []CompletionRequest // 记录收到的请求，供断言
	Err       error               // 非 nil 时每次调用都返回该错误
}

func (m *MockModel) Complete(ctx context.Context, req CompletionRequest) (*Completion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err // 与真实 HTTP 适配器对齐：ctx 取消 → 立即返回错误
	}
	m.Calls = append(m.Calls, req)
	if m.Err != nil {
		return nil, m.Err
	}
	if len(m.Responses) == 0 {
		return nil, fmt.Errorf("mock: no more responses")
	}

	resp := m.Responses[0]
	m.Responses = m.Responses[1:]
	return &resp, nil
}
