package main

import (
	"bytes"
	"strings"
	"testing"

	"minipi/internal/agent"
)

func TestPrintEventsSeparatesReasoningAndConcurrentAnswers(t *testing.T) {
	ch := make(chan agent.Event, 10)
	ch <- agent.Event{AgentID: "sub-1", Kind: agent.EventReasoningUpdate, Delta: "先估算"}
	ch <- agent.Event{AgentID: "sub-1", Kind: agent.EventReasoningUpdate, Delta: "区间"}
	ch <- agent.Event{AgentID: "sub-2", Kind: agent.EventReasoningUpdate, Delta: "开始迭代"}
	ch <- agent.Event{AgentID: "sub-1", Kind: agent.EventMessageUpdate, Delta: "结果一"}
	// A reasoning-only stream must not suppress its non-streamed answer.
	ch <- agent.Event{AgentID: "sub-2", Kind: agent.EventMessageEnd, Text: "结果二"}
	ch <- agent.Event{AgentID: "sub-1", Kind: agent.EventMessageEnd, Text: "结果一"}
	ch <- agent.Event{AgentID: "main", Kind: agent.EventMessageUpdate, Delta: "汇总"}
	ch <- agent.Event{AgentID: "main", Kind: agent.EventMessageEnd, Text: "汇总"}
	close(ch)
	var out bytes.Buffer
	printEvents(&out, ch)
	got := out.String()
	for _, want := range []string{
		"[sub-1 step=1 思考]\n先估算区间",
		"[sub-2 step=1 思考]\n开始迭代",
		"[sub-1 step=1 回答]\n结果一",
		"[sub-2 step=1 回答]\n结果二",
		"[main step=1 回答]\n汇总",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output: %s", want, got)
		}
	}
	if strings.Count(got, "结果一") != 1 || strings.Count(got, "汇总") != 1 {
		t.Fatalf("duplicated answer: %s", got)
	}
}
