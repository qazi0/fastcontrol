package main

// Notifications. Clients that send {op:"events", on:true} (the Android watch
// service, or an open web page) receive {t:"event"} messages when a thread
// starts or finishes a turn, or starts waiting on an approval or a question.
//
// Running state comes from the list refresh (rollout tails, plus the live
// state of followed threads). Approvals and questions are only visible in the
// desktop's live state, so while anyone listens for events the relay also
// follows the most recent running threads.

import (
	"os"
	"sort"
)

const maxWatched = 6

type threadState struct {
	running   bool
	attention bool
}

type event struct {
	Kind  string `json:"kind"` // started, completed, failed, attention
	ID    string `json:"id"`
	Title string `json:"title"`
	Text  string `json:"text,omitempty"`
	Cwd   string `json:"cwd,omitempty"`
}

// diffStates compares the new list with the previous one. It returns the
// events to send and, when anyone listens, the running threads to follow.
// Called with h.mu held.
func (h *Hub) diffStates(ts []ThreadInfo) (events []event, watch []string) {
	next := make(map[string]threadState, len(ts))
	listening := false
	for c := range h.clients {
		if c.events {
			listening = true
			break
		}
	}
	var running []ThreadInfo
	for _, t := range ts {
		if t.Subagent {
			continue
		}
		st := threadState{running: t.Running, attention: t.Attention}
		next[t.ID] = st
		if t.Running {
			running = append(running, t)
		}
		if h.prev == nil {
			continue
		}
		old, seen := h.prev[t.ID]
		title := t.Title
		if title == "" {
			title = t.Preview
		}
		ev := event{ID: t.ID, Title: title, Cwd: t.Cwd}
		switch {
		case st.running && (!seen || !old.running):
			ev.Kind = "started"
			events = append(events, ev)
		case !st.running && seen && old.running:
			if t.LastErr != "" {
				ev.Kind, ev.Text = "failed", t.LastErr
			} else {
				ev.Kind, ev.Text = "completed", t.LastMsg
			}
			events = append(events, ev)
		}
		if st.attention && (!seen || !old.attention) {
			ev.Kind, ev.Text = "attention", ""
			if s := h.sessions[t.ID]; s != nil {
				ev.Text = s.listState().attnMsg
			}
			events = append(events, ev)
		}
	}
	h.prev = next
	if listening {
		sort.Slice(running, func(i, j int) bool { return running[i].Recency > running[j].Recency })
		for i, t := range running {
			if i == maxWatched {
				break
			}
			watch = append(watch, t.ID)
		}
	}
	return events, watch
}

func (h *Hub) emit(events []event) {
	if len(events) == 0 {
		return
	}
	h.mu.Lock()
	var cs []*Client
	for c := range h.clients {
		if c.events {
			cs = append(cs, c)
		}
	}
	h.mu.Unlock()
	for _, e := range events {
		b := mustJSON(map[string]any{"t": "event", "kind": e.Kind, "id": e.ID, "title": e.Title, "text": e.Text, "cwd": e.Cwd})
		for _, c := range cs {
			c.push(b)
		}
	}
}

// updateWatch keeps sessions open for the given threads and releases the rest.
func (h *Hub) updateWatch(ids []string) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	h.mu.Lock()
	var off []*Session
	for id, s := range h.sessions {
		if !want[id] {
			off = append(off, s)
		}
	}
	h.mu.Unlock()
	for _, s := range off {
		s.setWatch(false)
	}
	for id := range want {
		if s := h.session(id, true); s != nil {
			s.setWatch(true)
		}
	}
}

// runningRolloutsChanged reports whether a running thread's rollout was
// written since the last list refresh, so finished turns show up within a
// tick instead of waiting for the periodic refresh.
func (h *Hub) runningRolloutsChanged() bool {
	h.mu.Lock()
	var paths []string
	for _, t := range h.threads {
		if t.Running && t.Rollout != "" {
			paths = append(paths, t.Rollout)
		}
	}
	h.mu.Unlock()
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		statusMu.Lock()
		c, ok := statusCache[p]
		statusMu.Unlock()
		if !ok || c.size != st.Size() || !c.mod.Equal(st.ModTime()) {
			return true
		}
	}
	return false
}
