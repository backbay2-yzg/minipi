package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"minipi/internal/agent"
	"minipi/internal/llm"
)

// JSONStore 每会话一个 JSON 文件：<dir>/<id>.json
// 并发约定：同一会话同一时刻只有一个 writer（CLI 单进程单会话）。
// 跨进程并发需文件锁 —— 超出教学范围（选做作业）。
type JSONStore struct{ dir string }

func NewJSONStore(dir string) (*JSONStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: mkdir: %w", err)
	}
	return &JSONStore{dir: dir}, nil
}

func (s *JSONStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Save 原子落盘：写临时文件 + rename。
// 直接 WriteFile 到目标路径的风险：写一半崩溃/断电 → 半截 JSON，
// 该会话从此 Load 永远失败且无备份。rename 在同一文件系统上是原子的。
func (s *JSONStore) Save(_ context.Context, sess *Session) error {
	if sess.ID == "" {
		return errors.New("session: empty id")
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal: %w", err)
	}
	path := s.path(sess.ID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("session: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) // rename 失败时清理，不留垃圾
		return fmt.Errorf("session: rename: %w", err)
	}
	return nil
}

func (s *JSONStore) Load(_ context.Context, id string) (*Session, error) {
	// 路径穿越防御：ID 会拼进文件路径，必须白名单化
	if id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return nil, fmt.Errorf("session: bad id %q", id)
	}
	data, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("session: read: %w", err)
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("session: corrupt %s: %w", id, err)
	}
	if sess.ID == "" {
		sess.ID = id
	}
	return &sess, nil
}

func (s *JSONStore) List(_ context.Context) ([]Meta, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("session: readdir: %w", err)
	}
	var out []Meta
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		sess, err := s.Load(context.Background(), strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue // 损坏文件跳过，不阻塞整个列表（生产需记日志）
		}
		out = append(out, Meta{
			ID: sess.ID, CreatedAt: sess.CreatedAt, UpdatedAt: sess.UpdatedAt,
			Messages: len(sess.State.Messages), Title: title(sess.State.Messages),
		})
	}
	return out, nil
}

func (s *JSONStore) Delete(_ context.Context, id string) error {
	err := os.Remove(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

// title 取第一条可见 user 消息前 40 rune
func title(msgs []agent.Message) string {
	for _, m := range msgs {
		if m.Role == llm.RoleUser && !m.Hidden {
			r := []rune(m.Content)
			if len(r) > 40 {
				r = r[:40]
			}
			return string(r)
		}
	}
	return ""
}
