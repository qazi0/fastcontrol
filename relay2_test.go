package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRolloutFailedTurnContextAndDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	lines := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1","started_at":10,"model_context_window":1000}}
{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":250},"model_context_window":1000}}}
{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"FileChange","id":"f","changes":{"/a.txt":{"type":"update","unified_diff":"@@ -1 +1 @@\n-a\n+b\n"}}}}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","error":{"message":"{\"error\":{\"message\":\"model not supported\"}}"}}}
`
	if err := writeFile(path, lines); err != nil {
		t.Fatal(err)
	}
	v := newRolloutView(path)
	if err := v.load(); err != nil {
		t.Fatal(err)
	}
	tr := v.turns[0]
	if tr.Status != "failed" || tr.Error != "model not supported" || !tr.HasDiff {
		t.Fatalf("turn: %+v", tr)
	}
	if v.ctxUsed != 250 || v.ctxMax != 1000 {
		t.Fatalf("ctx %d/%d", v.ctxUsed, v.ctxMax)
	}
	if d := v.diff("t1"); !strings.Contains(d, "--- /a.txt") || !strings.Contains(d, "+b") {
		t.Fatalf("diff %q", d)
	}
	st, _ := os.Stat(path)
	if rs := tailStatus(path, st); rs.running || rs.lastErr != "model not supported" {
		t.Fatalf("tail status %+v", rs)
	}
}

func TestRolloutGrowReadsMore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	var b strings.Builder
	pad := strings.Repeat("x", 1000)
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, `{"type":"event_msg","payload":{"type":"task_started","turn_id":"t%d"}}`+"\n", i)
		fmt.Fprintf(&b, `{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t%d","item":{"type":"UserMessage","id":"u%d","content":[{"type":"text","text":"%s"}]}}}`+"\n", i, i, pad)
		fmt.Fprintf(&b, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t%d"}}`+"\n", i)
	}
	if err := writeFile(path, b.String()); err != nil {
		t.Fatal(err)
	}
	v := newRolloutView(path)
	v.tail = 20 << 10 // about 18 turns
	if err := v.load(); err != nil {
		t.Fatal(err)
	}
	n := len(v.turns)
	if n >= 30 || !v.hasOlder(15) {
		t.Fatalf("expected a clipped load, got %d turns", n)
	}
	v.grow(45)
	if len(v.turns) < 45 {
		t.Fatalf("grow: %d turns (was %d)", len(v.turns), n)
	}
	if v.turns[len(v.turns)-1].Key != "t59" {
		t.Fatalf("last turn %s", v.turns[len(v.turns)-1].Key)
	}
}

func TestEventsFromListTransitions(t *testing.T) {
	h := &Hub{clients: map[*Client]bool{}, sessions: map[string]*Session{}, execs: map[string]*execRun{}}
	listener := &Client{events: true}
	h.clients[listener] = true
	base := []ThreadInfo{{ID: "a", Title: "A"}, {ID: "b", Title: "B", Running: true}}
	if ev, _ := h.diffStates(base); len(ev) != 0 {
		t.Fatalf("first list must not raise events: %+v", ev)
	}
	next := []ThreadInfo{{ID: "a", Title: "A", Running: true, Recency: 2}, {ID: "b", Title: "B", LastMsg: "Done"}, {ID: "c", Title: "C", Subagent: true, Running: true}}
	ev, watch := h.diffStates(next)
	kinds := map[string]string{}
	for _, e := range ev {
		kinds[e.ID] = e.Kind + ":" + e.Text
	}
	if kinds["a"] != "started:" || kinds["b"] != "completed:Done" || kinds["c"] != "" {
		t.Fatalf("events %+v", kinds)
	}
	if len(watch) != 1 || watch[0] != "a" {
		t.Fatalf("watch %v", watch)
	}
	ev, _ = h.diffStates([]ThreadInfo{{ID: "a", Title: "A", LastErr: "Stopped"}})
	if len(ev) != 1 || ev[0].Kind != "failed" {
		t.Fatalf("failed event %+v", ev)
	}
	ev, _ = h.diffStates([]ThreadInfo{{ID: "a", Title: "A", Attention: true}})
	if len(ev) != 1 || ev[0].Kind != "attention" {
		t.Fatalf("attention event %+v", ev)
	}
}

func TestCheckUploads(t *testing.T) {
	if err := checkUploads([]string{"/etc/passwd"}); err == nil {
		t.Fatal("paths outside the upload folder must be rejected")
	}
	if err := checkUploads([]string{filepath.Join(uploadDir(), "..", "token")}); err == nil {
		t.Fatal("traversal must be rejected")
	}
	if err := checkUploads(nil); err != nil {
		t.Fatal(err)
	}
}

func TestClipPlainAndErrorText(t *testing.T) {
	if got := clipPlain("hello world", 5); got != "hello…" {
		t.Fatal(got)
	}
	if got := errorText(`{"type":"error","status":400,"error":{"message":"nope"}}`); got != "nope" {
		t.Fatal(got)
	}
}
