package llm

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	//RoleTool       Role = "tool"
	RoleToolResult Role = "toolResult"
)

// ToolCall 是 assistant 发起的工具调用
type ToolCall struct {
	ID    string `json:"id"` // Provider 生成的调用 ID，toolResult 需回指它
	Name  string `json:"name"`
	Input string `json:"input"` // JSON 字符串
}

// ToolSpec 是暴露给模型的工具描述
type ToolSpec struct {
	Name        string
	Description string
	InputSchema map[string]any // JSON Schema
}

// StopReason 是统一后的"停止原因"枚举。
// 这是抽象层最重要的翻译工作之一：
//
//	OpenAI  的 finish_reason: "stop"/"tool_calls"/"length"
//	Anthropic 的 stop_reason: "end_turn"/"tool_use"/"max_tokens"
//
// Agent 循环只认这一套。
type StopReason string

const (
	StopReasonEndTurn   StopReason = "end_turn"
	StopReasonToolUse   StopReason = "tool_use"
	StopReasonMaxTokens StopReason = "max_tokens"
)

type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Message 是 mini-pi 的统一消息。
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant 专属
	ToolCallID string     `json:"tool_call_id,omitempty"` // toolResult 专属
}

type CompletionRequest struct {
	Messages  []Message
	System    string // 系统提示（Anthropic 是独立参数，见 anthropic.go）
	Tools     []ToolSpec
	MaxTokens int // 0 表示用 Provider 默认
}

// Completion 是模型的一次完整回复。
type Completion struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason StopReason
	Usage      TokenUsage
}
