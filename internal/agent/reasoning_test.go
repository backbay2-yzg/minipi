package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"minipi/internal/llm"
)

// The provider must forward reasoning before the answer finishes, through the
// agent bus, while keeping it out of the conversation sent back to the model.
func TestOpenCodeReasoningArrivesBeforeAnswer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"先估算区间\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"111.108056\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bus := NewEventBus(32)
	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()
	st := &AgentState{AgentID: "sub-test", Model: llm.NewOpenCode("test", srv.URL, "test"), Bus: bus}
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, st, "estimate")
		done <- err
	}()
	waiting := true
	for waiting {
		select {
		case ev := <-ch:
			if ev.Kind == EventReasoningUpdate {
				if ev.AgentID != "sub-test" || ev.Delta != "先估算区间" || ev.Step != 0 {
					t.Fatalf("reasoning event = %+v", ev)
				}
				waiting = false
			}
		case err := <-done:
			t.Fatalf("completed before reasoning: %v", err)
		case <-ctx.Done():
			t.Fatal("reasoning was buffered until completion")
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("request did not finish")
	}
	if len(st.Messages) != 2 || st.Messages[1].Content != "111.108056" {
		t.Fatalf("history = %+v", st.Messages)
	}
	for _, msg := range st.Messages {
		if strings.Contains(msg.Content, "先估算区间") {
			t.Fatal("reasoning leaked into conversation history")
		}
	}
}
