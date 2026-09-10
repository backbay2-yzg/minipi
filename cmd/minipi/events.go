package main

import (
	"fmt"
	"io"

	"minipi/internal/agent"
)

// printEvents renders each delta immediately. A change of agent, step or stream
// kind starts a labelled block; streamed answers are tracked per agent.
func printEvents(w io.Writer, ch <-chan agent.Event) {
	streamed := make(map[string]bool)
	active := ""
	endBlock := func() {
		if active != "" {
			fmt.Fprintln(w)
			active = ""
		}
	}
	defer endBlock()
	for ev := range ch {
		id := ev.AgentID
		if id == "" {
			id = "main"
		}
		switch ev.Kind {
		case agent.EventReasoningUpdate, agent.EventMessageUpdate:
			if ev.Delta == "" {
				continue
			}
			kind := "思考"
			if ev.Kind == agent.EventMessageUpdate {
				kind = "回答"
				streamed[id] = true
			}
			label := fmt.Sprintf("[%s step=%d %s]", id, ev.Step+1, kind)
			if active != label {
				endBlock()
				fmt.Fprintln(w, label)
				active = label
			}
			fmt.Fprint(w, ev.Delta)
		case agent.EventMessageEnd:
			endBlock()
			if !streamed[id] && ev.Text != "" {
				fmt.Fprintf(w, "[%s step=%d 回答]\n%s\n", id, ev.Step+1, ev.Text)
			}
			delete(streamed, id)
		case agent.EventAgentEnd:
			endBlock()
			delete(streamed, id)
			if ev.Err != nil {
				fmt.Fprintf(w, "[%s error] %v\n", id, ev.Err)
			}
		case agent.EventToolExecStart:
			endBlock()
			fmt.Fprintf(w, "[%s tool %s started] %s\n", id, ev.ToolCall.Name, ev.ToolCall.ID)
		case agent.EventToolExecEnd:
			endBlock()
			fmt.Fprintf(w, "[%s tool %s] %s\n", id, ev.ToolCall.Name, ev.Result)
		case agent.EventCompactionEnd:
			endBlock()
			fmt.Fprintf(w, "[compaction] old turns summarized: %s\n", firstRunes(ev.Text, 40))
		}
	}
}
