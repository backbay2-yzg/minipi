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

type oaChunk struct {
	Choices []struct {
		Delta struct {
			Content   string       `json:"content"`
			ToolCalls []oaToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// oaCallAcc 累积单个工具调用的流式分片（按 index 索引）
type oaCallAcc struct {
	id   string
	name string
	args strings.Builder
}

// StreamComplete 实现 StreamingModel。
// SSE 协议要点：
//   - 每个事件是 "data: {json}" 行，空行分隔，"data: [DONE]" 结束
//   - 文本以 delta.content 分片到达
//   - 工具调用以 delta.tool_calls 分片到达，靠 index 关联同一调用，
//     id/name 只在首片出现，arguments 跨片拼接
//   - stream_options.include_usage 使最后一个空 choices 的 chunk 携带 usage
func (o *OpenAI) StreamComplete(ctx context.Context, req CompletionRequest, onDelta func(string)) (*Completion, error) {
	// 1. 消息/工具编码与 Complete 相同（教学期容忍这 20 行重复，
	//    作业 4 提取 toOAMessages/toOATools 消除）
	msgs := make([]oaMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, oaMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			msgs = append(msgs, oaMessage{Role: "system", Content: m.Content})
		case RoleUser, RoleAssistant:
			msgs = append(msgs, oaMessage{
				Role:      string(m.Role),
				Content:   m.Content,
				ToolCalls: toOAToolCalls(m.ToolCalls),
			})
		case RoleToolResult:
			msgs = append(msgs, oaMessage{
				Role: "tool", Content: m.Content, ToolCallID: m.ToolCallID,
			})
		}
	}

	var tools []oaTool
	for _, s := range req.Tools {
		tools = append(tools, oaTool{
			Type: "function",
			Function: oaToolSpec{
				Name: s.Name, Description: s.Description, Parameters: s.InputSchema,
			},
		})
	}

	// 2. 组装 ★ 流式三件套：Stream / StreamOptions / 既有字段
	body, err := json.Marshal(oaRequest{
		Model:         o.model,
		Messages:      msgs,
		Tools:         tools, // ★ 组装点：别忘了（Day 3 教训）
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: &oaStreamOptions{IncludeUsage: true},
	})
	if err != nil {
		return nil, fmt.Errorf("openai: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		buf.ReadFrom(io.LimitReader(resp.Body, 4096))
		return nil, &APIError{Provider: "openai", StatusCode: resp.StatusCode, Body: buf.String()}
	}

	// 3. 逐行解析 SSE
	//    ctx 取消时 HTTP 层会关闭连接 → Scan 返回 false，无需额外检查
	var text strings.Builder
	var finish string
	var usage TokenUsage

	accs := map[int]*oaCallAcc{}
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

		var chunk oaChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, fmt.Errorf("openai: bad sse chunk: %w", err)
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
					acc = &oaCallAcc{}
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
		return nil, fmt.Errorf("openai: sse read: %w", err)
	}
	if finish == "" {
		return nil, fmt.Errorf("openai: stream ended without finish_reason")
	}

	// 4. 按 index 升序重组 tool_calls（乱序组装 = ID 配对错乱）
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
		StopReason: mapOpenAIStop(finish),
		Usage:      usage,
	}, nil
}
