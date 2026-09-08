package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"minipi/internal/agent"
	"minipi/internal/llm"
)

func newTestSession(t *testing.T) *Session {
	t.Helper()
	return &Session{
		ID:        NewID(),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Config: Config{
			Provider: "openai", ModelName: "gpt-4o-mini",
			SystemPrompt: "test", ToolNames: []string{"get_time", "calc"},
		},
		State: State{
			Messages: []agent.Message{
				agent.UserMsg("第一问"),
				agent.AssistantMsg("", []llm.ToolCall{{ID: "c1", Name: "get_time", Input: "{}"}}),
				agent.ToolResultMsg("c1", "2026-09-01T09:00:00+08:00"),
				agent.UINote("internal note"),
				agent.AssistantMsg("答一", nil),
				agent.UserMsg("第二问"),
			},
			TotalUsage: llm.TokenUsage{InputTokens: 42, OutputTokens: 7},
		},
	}
}

// 往返：任何字段都不丢（Hidden / ToolCalls / CreatedAt / usage / config）
func TestRoundTrip(t *testing.T) {
	store, err := NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	in := newTestSession(t)
	if err := store.Save(ctx, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := store.Load(ctx, in.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.ID != in.ID || out.Config.SystemPrompt != "test" {
		t.Errorf("meta/config drift: %+v", out)
	}
	if len(out.State.Messages) != 6 {
		t.Fatalf("messages = %d, want 6", len(out.State.Messages))
	}
	if !out.State.Messages[3].Hidden {
		t.Error("hidden flag lost")
	}
	tc := out.State.Messages[1].ToolCalls
	if len(tc) != 1 || tc[0].ID != "c1" || tc[0].Name != "get_time" {
		t.Errorf("tool calls lost: %+v", tc)
	}
	if out.State.TotalUsage.InputTokens != 42 {
		t.Errorf("usage lost: %+v", out.State.TotalUsage)
	}
}

// 内嵌扁平化回归：role/content 必须是顶层键（钉住 Step 2 的行为）
func TestEmbeddedFlatJSON(t *testing.T) {
	b, err := json.Marshal(agent.UserMsg("hi"))
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["role"]; !ok {
		t.Errorf("role not promoted to top level: %s", b)
	}
	if _, ok := probe["content"]; !ok {
		t.Error("content not promoted")
	}
}

func TestLoadNotFound(t *testing.T) {
	store, _ := NewJSONStore(t.TempDir())
	_, err := store.Load(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLoadRejectsPathTraversal(t *testing.T) {
	store, _ := NewJSONStore(t.TempDir())
	_, err := store.Load(context.Background(), "../evil")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("path traversal must be rejected, got %v", err)
	}
}

func TestListAndDelete(t *testing.T) {
	store, _ := NewJSONStore(t.TempDir())
	ctx := context.Background()

	a := newTestSession(t)
	a.State.Messages = []agent.Message{agent.UserMsg("帮我写一首诗")}
	b := newTestSession(t)
	b.State.Messages = []agent.Message{agent.UserMsg("查询天气")}
	store.Save(ctx, a)
	store.Save(ctx, b)

	metas, err := store.List(ctx)
	if err != nil || len(metas) != 2 {
		t.Fatalf("list = %v, %v", metas, err)
	}
	titles := map[string]bool{}
	for _, mt := range metas {
		titles[mt.Title] = true
	}
	if !titles["帮我写一首诗"] || !titles["查询天气"] {
		t.Errorf("titles = %v", titles)
	}

	if err := store.Delete(ctx, a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Load(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if err := store.Delete(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete: %v", err)
	}
}

// 原子写：二次覆盖生效 + 不留 .tmp 垃圾
func TestAtomicOverwrite(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewJSONStore(dir)
	ctx := context.Background()

	s := newTestSession(t)
	s.State.Messages = []agent.Message{agent.UserMsg("v1")}
	store.Save(ctx, s)
	s.State.Messages = []agent.Message{agent.UserMsg("v2")}
	s.UpdatedAt = s.UpdatedAt.Add(time.Minute)
	store.Save(ctx, s)

	out, _ := store.Load(ctx, s.ID)
	if out.State.Messages[0].Content != "v2" {
		t.Error("overwrite failed")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Error(".tmp file left behind")
		}
	}
}
