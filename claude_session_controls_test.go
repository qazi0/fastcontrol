package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeForkKeepsGraphWithoutSharingSessionState(t *testing.T) {
	dir := t.TempDir()
	source := uuid()
	path := filepath.Join(dir, source+".jsonl")
	u, progress, answer, side := uuid(), uuid(), uuid(), uuid()
	rows := []map[string]any{
		{"type": "user", "uuid": u, "sessionId": source, "parentUuid": nil, "cwd": dir, "timestamp": "2026-10-25T12:00:00Z", "message": map[string]any{"role": "user", "content": "A source prompt"}},
		{"type": "progress", "uuid": progress, "sessionId": source, "parentUuid": u},
		{"type": "assistant", "uuid": answer, "sessionId": source, "parentUuid": progress, "logicalParentUuid": u, "teamName": "old-team", "agentName": "old-agent", "message": map[string]any{"model": "claude-haiku-5-5", "role": "assistant", "content": []any{map[string]any{"type": "text", "text": "A source answer"}}}},
		{"type": "assistant", "uuid": side, "sessionId": source, "isSidechain": true, "parentUuid": answer},
		{"type": "content-replacement", "sessionId": source, "replacements": []any{map[string]any{"value": "preserved"}}},
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err = json.NewEncoder(file).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	file.Close()
	before, _ := os.ReadFile(path)
	child, err := forkClaudeTranscript(path, source, "Another branch")
	if err != nil {
		t.Fatal(err)
	}
	if child == source {
		t.Fatal("fork reused the source session")
	}
	data, err := os.ReadFile(filepath.Join(dir, child+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var forkRows []map[string]any
	f, err := os.Open(filepath.Join(dir, child+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	err = eachClaudeLine(f, st.Size(), func(line []byte) error {
		var row map[string]any
		if e := json.Unmarshal(line, &row); e != nil {
			return e
		}
		forkRows = append(forkRows, row)
		return nil
	})
	if err != nil || len(forkRows) != 4 {
		t.Fatalf("fork rows: %d %v", len(forkRows), err)
	}
	if forkRows[1]["parentUuid"] != forkRows[0]["uuid"] || forkRows[1]["logicalParentUuid"] != forkRows[0]["uuid"] {
		t.Fatal("progress node broke the message graph")
	}
	if forkRows[0]["uuid"] == u || forkRows[1]["uuid"] == answer || forkRows[1]["teamName"] != nil || forkRows[1]["agentName"] != nil {
		t.Fatal("source-specific state leaked into the fork")
	}
	for _, row := range forkRows {
		if row["sessionId"] != child {
			t.Fatal("source session ID retained")
		}
	}
	current, _ := os.ReadFile(path)
	if string(current) != string(before) {
		t.Fatal("fork changed the source transcript")
	}
	view, err := readClaudeTranscript(filepath.Join(dir, child+".jsonl"), 15, int64(len(data)))
	if err != nil || view.Meta.Title != "Another branch" || view.ParentID != source || len(view.Turns) != 1 {
		t.Fatal("fork is not readable as a complete session", err)
	}
}

func TestClaudePreferenceChangesDoNotClobberARename(t *testing.T) {
	id := uuid()
	m := &ClaudeManager{entries: map[string]claudeEntry{id: {}}, runs: map[string]*ClaudeRun{}, prefs: map[string]claudePreference{}, preferencesPath: filepath.Join(t.TempDir(), "prefs.json"), root: filepath.Join(t.TempDir(), "absent"), hub: &Hub{clients: map[*Client]bool{}}}
	unlock, err := m.lockPreference(id)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{out: make(chan []byte, 8), done: make(chan struct{})}
	m.hub.clients[c] = true
	m.handle(c, mustJSON(map[string]any{"op": "settings", "id": id, "model": "claude-haiku-5-5", "effort": "low", "nonce": "settings"}))
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		gate := m.prefGates[id]
		queued := gate != nil && gate.users == 2
		m.mu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			unlock()
			t.Fatal("settings did not enter the preference gate")
		}
		time.Sleep(time.Millisecond)
	}
	p := m.preference(id)
	p.Title = "Renamed"
	if err = m.setPreference(id, p); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	select {
	case data := <-c.out:
		var ack map[string]any
		_ = json.Unmarshal(data, &ack)
		if ack["ok"] != true {
			t.Fatal(string(data))
		}
	case <-time.After(time.Second):
		t.Fatal("settings stayed blocked")
	}
	p = m.preference(id)
	if p.Title != "Renamed" || p.Model != "claude-haiku-5-5" || p.Effort != "low" {
		t.Fatal("a concurrent settings update lost the title")
	}
}

func TestClaudeRenamePreservesACompleteFinalLineWithoutNewline(t *testing.T) {
	dir := t.TempDir()
	id := uuid()
	path := filepath.Join(dir, id+".jsonl")
	row := mustJSON(map[string]any{"type": "user", "uuid": uuid(), "sessionId": id, "cwd": dir, "message": map[string]any{"content": "Original prompt"}})
	if err := os.WriteFile(path, row, 0600); err != nil {
		t.Fatal(err)
	}
	m := &ClaudeManager{entries: map[string]claudeEntry{id: {Path: path}}, runs: map[string]*ClaudeRun{}, prefs: map[string]claudePreference{}, preferencesPath: filepath.Join(dir, "prefs.json"), root: filepath.Join(dir, "absent"), hub: &Hub{clients: map[*Client]bool{}}}
	if err := m.renameSession(id, "A new title"); err != nil {
		t.Fatal(err)
	}
	view, err := readClaudeTranscript(path, 15, 1<<20)
	if err != nil || view.Meta.Title != "A new title" || view.First != "Original prompt" {
		t.Fatal("rename damaged the final message", err)
	}
}

func TestSessionLinksNullAndParentScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json")
	if err := os.WriteFile(path, []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	links := newSessionLinks(path)
	if err := links.put("child", "parent"); err != nil {
		t.Fatal(err)
	}
	s := OrganizationSnapshot{Placements: map[string]SessionPlacement{"child": {}, "parent": {}}}
	links.apply(&s)
	if s.Placements["child"].ParentID != "parent" {
		t.Fatal("saved fork relationship missing")
	}
	delete(s.Placements, "parent")
	s.Placements["child"] = SessionPlacement{}
	links.apply(&s)
	if s.Placements["child"].ParentID != "" {
		t.Fatal("deleted parent was resurrected")
	}
}

func TestClaudeClosedOrMissingChatDoesNotAcquireAWatch(t *testing.T) {
	c := &Client{}
	h := &Hub{clients: map[*Client]bool{c: true}}
	m := &ClaudeManager{hub: h, entries: map[string]claudeEntry{}, runs: map[string]*ClaudeRun{}, watch: map[*Client]map[string]int{}}
	if err := m.open(c, "missing", false); err == nil {
		t.Fatal("missing chat accepted")
	}
	if len(m.watch) != 0 {
		t.Fatal("missing chat retained a watch")
	}
	delete(h.clients, c)
	m.entries["known"] = claudeEntry{}
	if err := m.open(c, "known", false); err != nil {
		t.Fatal(err)
	}
	if len(m.watch) != 0 {
		t.Fatal("disconnected client regained a subscription")
	}
}
