package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ocChunk 是 SSE 里单个事件的形状：choices[].delta（不是 message）、
// finish_reason 在最后一个非空 chunk 上、usage 在最后那个空 choices 的 chunk 上
type ocChunk struct {
	Choices []struct {
		Delta struct {
			Content   string       `json:"content"`
			ToolCalls []ocToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// ocCallAcc 累积单个工具调用的流式分片（按 index 索引）
type ocCallAcc struct {
	id   string
	name string
	args strings.Builder
}

// StreamComplete 实现 StreamingModel。
//
// OpenCode Zen 是 OpenAI Chat Completions 兼容端点，所以 SSE 协议与 openai_stream.go 完全一致：
//   - 每个事件是 "data: {json}" 行，空行分隔，"data: [DONE]" 结束
//   - 文本以 delta.content 分片到达
//   - 工具调用以 delta.tool_calls 分片到达，靠 index 关联同一调用，
//     id/name 只在首片出现，arguments 跨片拼接
//   - stream_options.include_usage 使最后一个空 choices 的 chunk 携带 usage
//
// 与 OpenAI 实现的唯一差别：消息/工具编码直接复用 buildRequest，
// 不重复那 20 行翻译逻辑（OpenCode 版本一开始就抽好了这个函数）。
func (o *OpenCode) StreamComplete(ctx context.Context, req CompletionRequest,
	onDelta func(string)) (*Completion, error) {

	// 1. 复用非流式的翻译，再叠加 ★ 流式三件套
	r := o.buildRequest(req)
	r.Stream = true
	r.StreamOptions = &ocStreamOptions{IncludeUsage: true}

	body, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("opencode: marshal: %w", err)
	}

	resp, err := o.do(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("opencode: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		buf.ReadFrom(io.LimitReader(resp.Body, 4096))
		return nil, &APIError{
			Provider:   "opencode",
			StatusCode: resp.StatusCode,
			Body:       buf.String(),
		}
	}

	// 2. 逐行解析 SSE
	//    ctx 取消时 HTTP 层会关闭连接 → Scan 返回 false，无需额外检查
	var text strings.Builder
	var finish string
	var usage TokenUsage

	accs := map[int]*ocCallAcc{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 单行上限 1MB
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") { // 空行 / 注释心跳
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk ocChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, fmt.Errorf("opencode: bad sse chunk: %w", err)
		}
		if chunk.Usage != nil { // 最后一个 chunk：choices 为空但有 usage
			usage = TokenUsage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}

		for _, c := range chunk.Choices {
			if c.FinishReason != nil {
				finish = *c.FinishReason
			}
			if c.Delta.Content != "" {
				text.WriteString(c.Delta.Content)
				if onDelta != nil {
					onDelta(c.Delta.Content)
				}
			}
			for _, tc := range c.Delta.ToolCalls {
				acc := accs[tc.Index]
				if acc == nil {
					acc = &ocCallAcc{}
					accs[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				acc.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("opencode: sse read: %w", err)
	}
	if finish == "" {
		return nil, fmt.Errorf("opencode: stream ended without finish_reason")
	}

	// 3. 按 index 升序重组 tool_calls（乱序组装 = ID 配对错乱）
	idxs := make([]int, 0, len(accs))
	for i := range accs {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)

	var calls []ToolCall
	for _, i := range idxs {
		acc := accs[i]
		calls = append(calls, ToolCall{
			ID: acc.id, Name: acc.name, Input: acc.args.String(),
		})
	}

	return &Completion{
		Text:       text.String(),
		ToolCalls:  calls,
		StopReason: mapOpenCodeStop(finish),
		Usage:      usage,
	}, nil
}
