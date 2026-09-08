package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"minipi/internal/agent"
	"minipi/internal/llm"
)

// ErrNotFound：Load 目标不存在。调用方据此区分"没有"与"损坏"。
var ErrNotFound = errors.New("session: not found")

// Config 记录"重建行为所需的配置"——只有名字，没有行为。
// ⚠️ 永不存 API key：key 只活在环境变量里，加载时重建（反模式 46）。
type Config struct {
	Provider     string   `json:"provider"`
	ModelName    string   `json:"model_name"`
	BaseURL      string   `json:"base_url,omitempty"`
	SystemPrompt string   `json:"system_prompt"`
	ToolNames    []string `json:"tool_names,omitempty"`
}

// State 是对话的数据部分。Messages 是 agent.Message——
// 内嵌 llm.Message 的 JSON 字段自动扁平提升（Step 2），直接序列化即可。
type State struct {
	Messages   []agent.Message `json:"messages"`
	TotalUsage llm.TokenUsage  `json:"total_usage"`
}

type Session struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Config    Config    `json:"config"`
	State     State     `json:"state"`
}

// NewID 生成可读 ID：时间戳 + 4 字节随机（同秒多次创建也不撞）
func NewID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return time.Now().Format("20060102-150405.000000000")
	}
	return fmt.Sprintf("%s-%s", time.Now().Format("20060102-150405"), hex.EncodeToString(b))
}

type Meta struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
	Messages  int
	Title     string // 首条可见 user 消息前 40 rune
}

// Store 是持久化的唯一抽象。ctx 预留：JSONStore 用不上，
// SQLite/Postgres 后端的查询取消靠它 —— 接口先按生产形状定义。
type Store interface {
	Save(ctx context.Context, s *Session) error
	Load(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context) ([]Meta, error)
	Delete(ctx context.Context, id string) error
}
