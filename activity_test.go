package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLongRunningTurnOutsideTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.jsonl")
	started := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"active","started_at":123}}` + "\n"
	// A single huge record spans the initial tail and several reverse chunks.
	large := `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("x", 3<<20) + `"}]}}` + "\n"
	output := `{"type":"event_msg","payload":{"type":"agent_message","message":"Still working"}}` + "\n"
	if err := os.WriteFile(path, []byte(started+large+output), 0600); err != nil {
		t.Fatal(err)
	}
	view := newRolloutView(path)
	if err := view.load(); err != nil {
		t.Fatal(err)
	}
	if !view.running() || view.lastTurn().Key != "active" {
		t.Fatalf("lost active turn: %+v", view.lastTurn())
	}
	st, _ := os.Stat(path)
	if status := tailStatus(path, st); !status.running || status.turnAt != 123000 {
		t.Fatalf("lost list activity: %+v", status)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"active"}}` + "\n")
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = view.poll(); err != nil {
		t.Fatal(err)
	}
	st, _ = os.Stat(path)
	if view.running() || tailStatus(path, st).running {
		t.Fatal("completed turn remained active")
	}
}
func TestTurnMarkerIgnoresQuotedProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	text := `{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"a"}}` + "\n" + `{"type":"response_item","payload":{"text":"fake \\\"event_msg\\\" \\\"type\\\":\\\"task_started\\\""}}` + "\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	marker := previousTurnMarker(f, int64(len(text)))
	if !strings.Contains(string(marker), "turn_aborted") {
		t.Fatalf("wrong marker %s", marker)
	}
}

func TestTurnMarkerBeyondOldScanLimitAndIncrementalAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	started := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"long","started_at":456}}` + "\n"
	if _, err = f.WriteString(started); err != nil {
		t.Fatal(err)
	}
	// A sparse gap avoids a large fixture while placing the marker beyond 64 MB.
	if _, err = f.Seek(70<<20, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("\n" + `{"type":"event_msg","payload":{"type":"agent_message","message":"Working"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	if !tailStatus(path, st).running {
		t.Fatal("long active turn was reported idle")
	}
	view := newRolloutView(path)
	if err = view.load(); err != nil || !view.running() {
		t.Fatal("tail lost the active turn", err)
	}
	if _, err = f.WriteString(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"long"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	st, _ = f.Stat()
	if tailStatus(path, st).running {
		t.Fatal("incremental completion remained active")
	}
	marker := previousTurnMarker(f, int64(len(started)))
	if !strings.Contains(string(marker), "task_started") {
		t.Fatal("historical query reused a future marker")
	}
	if err = f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"replacement"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	st, _ = f.Stat()
	marker = previousTurnMarker(f, st.Size())
	if !strings.Contains(string(marker), "replacement") {
		t.Fatal("truncated transcript kept an old marker")
	}
}
