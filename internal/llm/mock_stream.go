package llm

import "context"

// MockStreaming 复用 MockModel 的响应队列与调用记录，
// 把 Text 切成 ChunkSize 大小的（rune 安全）片段回调，再原样返回。
// 与真实流式适配器对齐同一契约：拼接(deltas) == 返回值.Text。
type MockStreaming struct {
	MockModel
	ChunkSize int // rune 数，<=0 时取 3
}

func (m *MockStreaming) StreamComplete(ctx context.Context, req CompletionRequest,
	onDelta func(string)) (*Completion, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := m.MockModel.Complete(ctx, req) // 复用队列弹出 + Calls 记录
	if err != nil {
		return nil, err
	}

	size := m.ChunkSize
	if size <= 0 {
		size = 3
	}
	r := []rune(out.Text)
	for i := 0; i < len(r); i += size {
		end := i + size
		if end > len(r) {
			end = len(r)
		}
		if onDelta != nil {
			onDelta(string(r[i:end]))
		}
	}
	return out, nil
}
