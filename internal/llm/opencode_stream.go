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
	"time"
)

// ocChunk 是 SSE 里单个事件的形状：choices[].delta（不是 message）、
// finish_reason 在最后一个非空 chunk 上、usage 在最后那个空 choices 的 chunk 上
type ocChunk struct {
	Choices []struct {
		Delta struct {
			ReasoningContent string       `json:"reasoning_content"`
			Content          string       `json:"content"`
			ToolCalls        []ocToolCall `json:"tool_calls,omitempty"`
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
	return o.StreamCompleteWithReasoning(ctx, req, onDelta, nil)
}

// StreamCompleteWithReasoning forwards reasoning deltas as they arrive without
// adding them to the answer or retaining the full reasoning in memory.
func (o *OpenCode) StreamCompleteWithReasoning(ctx context.Context, req CompletionRequest,
	onDelta func(string), onReasoning func(string)) (*Completion, error) {

	// 1. 复用非流式的翻译，再叠加 ★ 流式三件套
	r := o.buildRequest(req)
	r.Stream = true
	r.StreamOptions = &ocStreamOptions{IncludeUsage: true}

	body, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("opencode: marshal: %w", err)
	}

	// 流式请求不能使用 http.Client 的总超时。单独派生一个可取消 context，
	// 后面的空闲看门狗触发时可直接关闭响应体，解除 Scanner 的阻塞读取。
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := o.do(streamCtx, body)
	if err != nil {
		return nil, fmt.Errorf("opencode: do: %w", err)
	}
	defer resp.Body.Close()

	idleTimeout := o.sseIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultOpenCodeSSEIdleTimeout
	}
	// 建连、TLS 和响应头分别由 Transport 限时；收到响应头以后，只有连续
	// idleTimeout 没有任何 SSE 行才取消。推理片段和注释心跳也会重置计时器。
	watchdog := time.AfterFunc(idleTimeout, cancel)
	defer watchdog.Stop()

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
		watchdog.Reset(idleTimeout)
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
			if c.Delta.ReasoningContent != "" && onReasoning != nil {
				onReasoning(c.Delta.ReasoningContent)
			}
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
		// 外层 ctx 未取消、只有 streamCtx 被取消，说明是空闲看门狗触发。
		// 单独标记便于区分用户取消、网络错误和真正的无数据超时。
		if ctx.Err() == nil && streamCtx.Err() != nil {
			return nil, fmt.Errorf("opencode: sse idle > %v: %w", idleTimeout, streamCtx.Err())
		}
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
