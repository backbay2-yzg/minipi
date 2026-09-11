package agent

import (
	"fmt"
	"time"

	"minipi/internal/llm"
)

// Message 是 agent 层的历史消息：比 llm.Message 多两类信息——
//
//	CreatedAt: 元数据（随历史持久化，与模型无关）
//	Hidden:    UI-only 标记，管道第二级过滤，永不发给模型
//
// 内嵌 llm.Message 复用全部协议字段；外部经提升字段照常访问 m.Role 等。
//
// ⚠️ Hidden 只允许用于注释型消息（UINote 构造）。
// 协议消息（assistant+ToolCalls / toolResult）标记 Hidden 会破坏
// toolCall/toolResult 配对 —— 管道两级的结构安全都以"协议消息不藏"为前提。
type Message struct {
	llm.Message
	CreatedAt time.Time `json:"created_at"`
	Hidden    bool      `json:"hidden,omitempty"`
}

// ---- 构造函数：统一设置 CreatedAt，协议消息 Hidden 恒为 false ----
// 用构造函数而非裸字面量：不变量（时间戳必有、协议消息不藏）集中在一处保证。

func UserMsg(content string) Message {
	return Message{
		Message:   llm.Message{Role: llm.RoleUser, Content: content},
		CreatedAt: time.Now(),
	}
}

func AssistantMsg(content string, calls []llm.ToolCall) Message {
	return Message{
		Message:   llm.Message{Role: llm.RoleAssistant, Content: content, ToolCalls: calls},
		CreatedAt: time.Now(),
	}
}

func ToolResultMsg(callID, content string) Message {
	return Message{
		Message:   llm.Message{Role: llm.RoleToolResult, ToolCallID: callID, Content: content},
		CreatedAt: time.Now(),
	}
}

// UINote 构造 UI-only 注释：留在历史里供展示/排查，模型不可见。
// Role 用 user（若未来因 bug 泄漏，user 角色对 provider 危害最小），
// 但正确性由 Hidden 保证，不依赖角色。
func UINote(format string, args ...any) Message {
	return Message{
		Message:   llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf(format, args...)},
		CreatedAt: time.Now(),
		Hidden:    true,
	}
}

// convertToLlm 是管道第二级：过滤 Hidden，产出线格式切片。
// 只裁视图、不改历史 —— 返回全新切片。
func convertToLlm(msgs []Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Hidden {
			continue
		}
		out = append(out, m.Message)
	}
	return out
}

// FinalText 返回历史中最后一条非空 assistant 文本。
// 导出原因：组合层（workflow 等）需要提取 Run 的最终回答；
// SubAgentTool 是第一个使用者，workflow 是第二个。
// 注意语义收紧：要求 Content 非空——"最终回答"就该非空，
// 空 assistant（如纯工具调用轮之后异常结束）视为没有回答。
func FinalText(msgs []Message) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant && msgs[i].Content != "" {
			return msgs[i].Content, true
		}
	}
	return "", false
}
