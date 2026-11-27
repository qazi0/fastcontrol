package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func spawnExitedPID(t *testing.T) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	return pid
}

func writeClaudeSessionJSONL(t *testing.T, root, project, id string, rows []map[string]any, mod time.Time) string {
	t.Helper()
	path := filepath.Join(root, project, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, row := range rows {
		out = append(out, mustJSON(row)...)
		out = append(out, '\n')
	}
	if err := os.WriteFile(path, out, 0600); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func writeFakeClaudeCLI(t *testing.T, scriptBody string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell mock script test runs on unix")
	}
	path := filepath.Join(t.TempDir(), "claude-mock")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+scriptBody+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeThreadsFrame(t *testing.T, raw []byte) map[string]ThreadInfo {
	t.Helper()
	var msg struct {
		T        string       `json:"t"`
		Provider string       `json:"provider"`
		Threads  []ThreadInfo `json:"threads"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal threads frame: %v", err)
	}
	if msg.T != "threads" || msg.Provider != "claude" {
		t.Fatalf("unexpected frame header: %+v", msg)
	}
	byID := make(map[string]ThreadInfo, len(msg.Threads))
	for _, th := range msg.Threads {
		byID[th.ID] = th
	}
	return byID
}

func TestClaudeActiveSchemaParsingAndNormalization(t *testing.T) {
	mixed := `[
		{
			"id": "8f4ae6fe",
			"cwd": "/tmp/legacy-blocked",
			"kind": "background",
			"startedAt": 1784569703784,
			"sessionId": "11111111-1111-4111-8111-111111111111",
			"name": "Legacy blocked",
			"state": "blocked"
		},
		{
			"id": "9a1bc2de",
			"cwd": "/tmp/legacy-running",
			"kind": "background",
			"startedAt": 1784569728254,
			"sessionId": "22222222-2222-4222-8222-222222222222",
			"name": "Legacy running",
			"state": "running"
		},
		{
			"pid": 65197,
			"cwd": "/tmp/new-busy",
			"kind": "interactive",
			"startedAt": 1795652928486,
			"sessionId": "33333333-3333-4333-8333-333333333333",
			"name": "Interactive busy",
			"status": "busy"
		},
		{
			"pid": "65198",
			"cwd": "/tmp/new-idle",
			"kind": "interactive",
			"startedAt": 1795652929000,
			"sessionId": "44444444-4444-4444-8444-444444444444",
			"name": "Interactive idle",
			"status": "idle"
		}
	]`
	var items []claudeActive
	if err := json.Unmarshal([]byte(mixed), &items); err != nil {
		t.Fatalf("unmarshal mixed array: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}

	// Legacy blocked
	if items[0].ID != "8f4ae6fe" || items[0].backgroundID() != "8f4ae6fe" || items[0].PID != 0 || !items[0].present() || items[0].isRunning() || !items[0].isAttention() || !items[0].isBusy() || items[0].effectiveTurnAt() != 1784569703784 {
		t.Fatalf("legacy blocked mismatch: %+v", items[0])
	}
	// Legacy running
	if items[1].ID != "9a1bc2de" || items[1].backgroundID() != "9a1bc2de" || !items[1].isRunning() || items[1].isAttention() || !items[1].isBusy() {
		t.Fatalf("legacy running mismatch: %+v", items[1])
	}
	// Claude Code 2.1.331 numeric pid + status busy
	if items[2].PID != 65197 || items[2].ID != "pid:65197" || items[2].backgroundID() != "" || !items[2].present() || !items[2].isRunning() || items[2].isAttention() || !items[2].isBusy() || items[2].effectiveTurnAt() != 1795652928486 {
		t.Fatalf("new busy mismatch: %+v", items[2])
	}
	// Claude Code 2.1.331 string pid + status idle
	if items[3].PID != 65198 || items[3].ID != "pid:65198" || items[3].backgroundID() != "" || !items[3].present() || items[3].isRunning() || items[3].isAttention() || items[3].isBusy() || items[3].effectiveTurnAt() != 0 {
		t.Fatalf("new idle mismatch: %+v", items[3])
	}

	// State/status normalization matrix
	cases := []struct {
		raw       string
		running   bool
		attention bool
		busy      bool
		norm      string
	}{
		{"busy", true, false, true, "running"},
		{"running", true, false, true, "running"},
		{"BUSY", true, false, true, "running"},
		{"blocked", false, true, true, "blocked"},
		{"waiting", false, true, true, "blocked"},
		{"needs_attention", false, true, true, "blocked"},
		{"permission", false, true, true, "blocked"},
		{"idle", false, false, false, "idle"},
	}
	for _, tc := range cases {
		for _, field := range []string{"status", "state"} {
			var a claudeActive
			payload := mustJSON(map[string]any{"pid": 1234, "sessionId": "sess", field: tc.raw})
			if err := json.Unmarshal(payload, &a); err != nil {
				t.Fatalf("unmarshal %s=%s: %v", field, tc.raw, err)
			}
			if a.normalizedState() != tc.norm || a.isRunning() != tc.running || a.isAttention() != tc.attention || a.isBusy() != tc.busy {
				t.Fatalf("%s=%s got norm=%q run=%v attn=%v busy=%v", field, tc.raw, a.normalizedState(), a.isRunning(), a.isAttention(), a.isBusy())
			}
		}
	}

	// Owner ID fallback when id and pid are both absent
	var hostOnly claudeActive
	if err := json.Unmarshal([]byte(`{"hostSessionId":"local_abc","sessionId":"s1","status":"busy"}`), &hostOnly); err != nil {
		t.Fatal(err)
	}
	if hostOnly.ID != "host:local_abc" || !hostOnly.present() || hostOnly.backgroundID() != "" {
		t.Fatalf("host fallback mismatch: %+v", hostOnly)
	}
	var sessionOnly claudeActive
	if err := json.Unmarshal([]byte(`{"sessionId":"s2","status":"idle"}`), &sessionOnly); err != nil {
		t.Fatal(err)
	}
	if sessionOnly.ID != "session:s2" || !sessionOnly.present() || sessionOnly.backgroundID() != "" {
		t.Fatalf("session fallback mismatch: %+v", sessionOnly)
	}
}

func TestProcessAliveRealPIDs(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("current process reported not alive")
	}
	deadPID := spawnExitedPID(t)
	if processAlive(deadPID) {
		t.Fatalf("exited child process %d still reported alive", deadPID)
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("non-positive PID reported alive")
	}
}

func TestClaudeRefreshActiveSessionFilesAndDeadPIDRejection(t *testing.T) {
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	sessionsDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0700); err != nil {
		t.Fatal(err)
	}

	livePID := os.Getpid()
	deadPID := spawnExitedPID(t)
	busySession := uuid()
	deadSession := uuid()
	idleSession := uuid()
	legacyBlocked := uuid()
	legacyRunning := uuid()

	now := time.Now().Truncate(time.Second)
	for _, s := range []struct {
		id, title string
	}{
		{busySession, "Busy Interactive"},
		{deadSession, "Dead Stale Session"},
		{idleSession, "Idle Interactive"},
		{legacyBlocked, "Legacy Blocked"},
		{legacyRunning, "Legacy Running"},
	} {
		writeClaudeSessionJSONL(t, projectsDir, "proj", s.id, []map[string]any{
			{"type": "user", "uuid": uuid(), "sessionId": s.id, "cwd": dir, "message": map[string]any{"content": s.title}},
		}, now)
	}

	// Live session file with statusUpdatedAt and hostSessionId
	liveFile := filepath.Join(sessionsDir, strconv.Itoa(livePID)+".json")
	if err := os.WriteFile(liveFile, mustJSON(map[string]any{
		"pid":             livePID,
		"sessionId":       busySession,
		"cwd":             dir,
		"startedAt":       int64(1795652928486),
		"status":          "busy",
		"statusUpdatedAt": int64(1795653728825),
		"updatedAt":       int64(1795653729000),
		"hostSessionId":   "local_live_host",
		"kind":            "interactive",
	}), 0600); err != nil {
		t.Fatal(err)
	}

	// Dead session file with busy status (must be rejected)
	deadFile := filepath.Join(sessionsDir, strconv.Itoa(deadPID)+".json")
	if err := os.WriteFile(deadFile, mustJSON(map[string]any{
		"pid":             deadPID,
		"sessionId":       deadSession,
		"cwd":             dir,
		"startedAt":       int64(1795652928486),
		"status":          "busy",
		"statusUpdatedAt": int64(1795653728825),
	}), 0600); err != nil {
		t.Fatal(err)
	}

	// Non-session noise files that must be ignored
	_ = os.WriteFile(filepath.Join(sessionsDir, strconv.Itoa(livePID)+".abc.key"), []byte("secret"), 0600)
	_ = os.WriteFile(filepath.Join(sessionsDir, "notapid.json"), []byte(`{"pid":1,"sessionId":"bad","status":"busy"}`), 0600)

	h := NewHub(NewIPCClient(""), "")
	h.organization = nil
	m := newClaudeManager(h)
	m.root = projectsDir
	m.sessionsDir = sessionsDir
	m.preferencesPath = filepath.Join(dir, "prefs.json")
	m.bin = "" // First test pure session file discovery (cross-platform including Windows)

	m.refreshActive()
	m.refresh()

	m.mu.Lock()
	gotBusy, hasBusy := m.active[busySession]
	_, hasDead := m.active[deadSession]
	lastList := append([]byte(nil), m.lastList...)
	m.mu.Unlock()

	if !hasBusy || gotBusy.PID != livePID || gotBusy.ID != "pid:"+strconv.Itoa(livePID) || gotBusy.HostSessionID != "local_live_host" || gotBusy.effectiveTurnAt() != 1795653728825 {
		t.Fatalf("live session file not loaded cleanly: has=%v val=%+v", hasBusy, gotBusy)
	}
	if hasDead {
		t.Fatalf("dead PID %d session file was trusted", deadPID)
	}

	threads := decodeThreadsFrame(t, lastList)
	if !threads[busySession].Active || !threads[busySession].Running || threads[busySession].Attention || threads[busySession].TurnAt != 1795653728825 {
		t.Fatalf("busy thread info mismatch: %+v", threads[busySession])
	}
	if threads[deadSession].Active || threads[deadSession].Running || threads[deadSession].TurnAt != 0 {
		t.Fatalf("dead session leaked into thread list: %+v", threads[deadSession])
	}

	// Now test mixed CLI output (claude agents --json) + session file enrichment + dead PID rejection via CLI
	if runtime.GOOS != "windows" {
		idlePID := 424242
		agentsJSON := mustJSON([]map[string]any{
			{"id": "bg-blk", "cwd": dir, "kind": "background", "startedAt": int64(1784569703784), "sessionId": legacyBlocked, "state": "blocked"},
			{"id": "bg-run", "cwd": dir, "kind": "background", "startedAt": int64(1784569728254), "sessionId": legacyRunning, "state": "running"},
			{"pid": livePID, "cwd": dir, "kind": "interactive", "startedAt": int64(1795652928486), "sessionId": busySession, "status": "busy"},
			{"pid": strconv.Itoa(idlePID), "cwd": dir, "kind": "interactive", "startedAt": int64(1795652929000), "sessionId": idleSession, "status": "idle"},
			{"pid": deadPID, "cwd": dir, "kind": "interactive", "startedAt": int64(1795652929000), "sessionId": deadSession, "status": "busy"},
		})
		outFile := filepath.Join(dir, "agents.json")
		if err := os.WriteFile(outFile, agentsJSON, 0600); err != nil {
			t.Fatal(err)
		}
		m.bin = writeFakeClaudeCLI(t, `cat "`+outFile+`"`)
		m.pidAlive = func(pid int) bool { return pid == livePID || pid == idlePID }

		m.refreshActive()
		m.refresh()

		m.mu.Lock()
		lastList = append([]byte(nil), m.lastList...)
		m.mu.Unlock()
		threads = decodeThreadsFrame(t, lastList)

		if !threads[busySession].Running || !threads[busySession].Active || threads[busySession].TurnAt != 1795653728825 {
			t.Fatalf("mixed busy thread mismatch: %+v", threads[busySession])
		}
		if !threads[idleSession].Active || threads[idleSession].Running || threads[idleSession].Attention || threads[idleSession].TurnAt != 0 {
			t.Fatalf("mixed idle thread mismatch: %+v", threads[idleSession])
		}
		if !threads[legacyBlocked].Active || threads[legacyBlocked].Running || !threads[legacyBlocked].Attention || threads[legacyBlocked].TurnAt != 1784569703784 {
			t.Fatalf("mixed legacy blocked thread mismatch: %+v", threads[legacyBlocked])
		}
		if !threads[legacyRunning].Active || !threads[legacyRunning].Running || threads[legacyRunning].Attention || threads[legacyRunning].TurnAt != 1784569728254 {
			t.Fatalf("mixed legacy running thread mismatch: %+v", threads[legacyRunning])
		}
		if threads[deadSession].Active || threads[deadSession].Running {
			t.Fatalf("dead PID from CLI was not rejected: %+v", threads[deadSession])
		}

		// Verify open() populates Mode=external, Running, and TurnAt for both busy and idle 2.1.331 sessions
		c := &Client{out: make(chan []byte, 16), done: make(chan struct{})}
		h.addClient(c)
		for len(c.out) > 0 {
			<-c.out
		}
		if err := m.open(c, busySession, false); err != nil {
			t.Fatal(err)
		}
		var openBusy struct {
			Meta Meta `json:"meta"`
		}
		if err := json.Unmarshal(<-c.out, &openBusy); err != nil {
			t.Fatal(err)
		}
		if openBusy.Meta.Mode != "external" || !openBusy.Meta.Running || openBusy.Meta.TurnAt != 1795653728825 {
			t.Fatalf("open busy external meta mismatch: %+v", openBusy.Meta)
		}

		if err := m.open(c, idleSession, false); err != nil {
			t.Fatal(err)
		}
		var openIdle struct {
			Meta Meta `json:"meta"`
		}
		if err := json.Unmarshal(<-c.out, &openIdle); err != nil {
			t.Fatal(err)
		}
		if openIdle.Meta.Mode != "external" || openIdle.Meta.Running || openIdle.Meta.TurnAt != 0 {
			t.Fatalf("open idle external meta mismatch: %+v", openIdle.Meta)
		}
	}
}

func TestClaudeRefreshActiveNotificationsBothSchemas(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell mock script test runs on unix")
	}
	dir := t.TempDir()
	sessionsDir := filepath.Join(dir, "sessions")
	_ = os.MkdirAll(sessionsDir, 0700)
	agentsFile := filepath.Join(dir, "agents.json")

	newID := uuid()
	legacyID := uuid()
	preBlockedID := uuid()

	h := NewHub(NewIPCClient(""), "")
	h.organization = nil
	m := newClaudeManager(h)
	m.root = filepath.Join(dir, "projects")
	m.sessionsDir = sessionsDir
	m.preferencesPath = filepath.Join(dir, "prefs.json")
	m.bin = writeFakeClaudeCLI(t, `cat "`+agentsFile+`"`)
	m.pidAlive = func(pid int) bool { return pid == 5001 }
	m.entries = map[string]claudeEntry{
		newID:        {Info: ThreadInfo{ID: newID, Title: "New Session", LastMsg: "Done new"}},
		legacyID:     {Info: ThreadInfo{ID: legacyID, Title: "Legacy Session", LastMsg: "Done legacy"}},
		preBlockedID: {Info: ThreadInfo{ID: preBlockedID, Title: "Pre Blocked", LastMsg: "Waiting"}},
	}

	evClient := &Client{events: true, eventsOnly: true, out: make(chan []byte, 32), done: make(chan struct{})}
	h.addClient(evClient)

	drainEvents := func() []map[string]any {
		var out []map[string]any
		for {
			select {
			case raw := <-evClient.out:
				var msg map[string]any
				if json.Unmarshal(raw, &msg) == nil && msg["t"] == "event" {
					out = append(out, msg)
				}
			default:
				return out
			}
		}
	}

	setAgents := func(items []map[string]any) {
		if err := os.WriteFile(agentsFile, mustJSON(items), 0600); err != nil {
			t.Fatal(err)
		}
		m.refreshActive()
	}

	// Poll 1: initial discovery must not fire startup notifications
	setAgents([]map[string]any{
		{"pid": 5001, "sessionId": newID, "status": "busy", "startedAt": int64(1000)},
		{"id": "bg1", "sessionId": legacyID, "state": "running", "startedAt": int64(1000)},
		{"id": "bg2", "sessionId": preBlockedID, "state": "blocked", "startedAt": int64(1000)},
	})
	if events := drainEvents(); len(events) != 0 {
		t.Fatalf("unexpected startup events: %+v", events)
	}

	// Poll 2: transitions to attention (new "permission" + legacy "blocked")
	setAgents([]map[string]any{
		{"pid": 5001, "sessionId": newID, "status": "permission", "startedAt": int64(1000)},
		{"id": "bg1", "sessionId": legacyID, "state": "blocked", "startedAt": int64(1000)},
		{"id": "bg2", "sessionId": preBlockedID, "state": "blocked", "startedAt": int64(1000)},
	})
	events := drainEvents()
	if len(events) != 2 {
		t.Fatalf("expected 2 attention events, got %+v", events)
	}
	seenAttn := map[string]bool{}
	for _, ev := range events {
		if ev["kind"] != "attention" {
			t.Fatalf("expected attention event, got %+v", ev)
		}
		seenAttn[str(ev, "id")] = true
	}
	if !seenAttn[newID] || !seenAttn[legacyID] {
		t.Fatalf("missing attention ids: %+v", seenAttn)
	}

	// Poll 3: new session becomes idle, legacy disappears -> both emit completed
	setAgents([]map[string]any{
		{"pid": 5001, "sessionId": newID, "status": "idle", "startedAt": int64(1000)},
		{"id": "bg2", "sessionId": preBlockedID, "state": "blocked", "startedAt": int64(1000)},
	})
	events = drainEvents()
	if len(events) != 2 {
		t.Fatalf("expected 2 completed events, got %+v", events)
	}
	seenDone := map[string]bool{}
	for _, ev := range events {
		if ev["kind"] != "completed" {
			t.Fatalf("expected completed event, got %+v", ev)
		}
		seenDone[str(ev, "id")] = true
	}
	if !seenDone[newID] || !seenDone[legacyID] {
		t.Fatalf("missing completed ids: %+v", seenDone)
	}

	// Poll 4: idle session disappears -> no duplicate completed event
	setAgents([]map[string]any{
		{"id": "bg2", "sessionId": preBlockedID, "state": "blocked", "startedAt": int64(1000)},
	})
	if events := drainEvents(); len(events) != 0 {
		t.Fatalf("idle disappearance fired spurious event: %+v", events)
	}
}

func TestClaudeExternalOpsStopTakeoverArchiveAndSubagents(t *testing.T) {
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	sessionsDir := filepath.Join(dir, "sessions")
	_ = os.MkdirAll(sessionsDir, 0700)

	newID := uuid()
	legacyID := uuid()
	for _, id := range []string{newID, legacyID} {
		writeClaudeSessionJSONL(t, projectsDir, "proj", id, []map[string]any{
			{"type": "user", "uuid": uuid(), "sessionId": id, "cwd": dir, "message": map[string]any{"content": "Hello"}},
		}, time.Now())
	}

	h := NewHub(NewIPCClient(""), "")
	h.organization = nil
	m := newClaudeManager(h)
	m.root = projectsDir
	m.sessionsDir = sessionsDir
	m.preferencesPath = filepath.Join(dir, "prefs.json")
	m.bin = ""
	m.refresh()

	// Seed active sessions using both schemas (2.1.331 pid+status without id, and legacy id+state)
	var newActive, legacyActive claudeActive
	if err := json.Unmarshal([]byte(`{"pid":65197,"sessionId":"`+newID+`","status":"idle","startedAt":1795652928486}`), &newActive); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"id":"bg-42","sessionId":"`+legacyID+`","state":"running","startedAt":1784569703784}`), &legacyActive); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.active = map[string]claudeActive{
		newID:    newActive,
		legacyID: legacyActive,
	}
	m.mu.Unlock()
	m.pidAlive = func(pid int) bool { return pid == 65197 }

	// Without takeover, getRun must refuse BOTH schemas
	if _, err := m.getRun(newID, false); err == nil || !strings.Contains(err.Error(), "Take Over") {
		t.Fatalf("new schema getRun without takeover allowed: %v", err)
	}
	if _, err := m.getRun(legacyID, false); err == nil || !strings.Contains(err.Error(), "Take Over") {
		t.Fatalf("legacy schema getRun without takeover allowed: %v", err)
	}

	c := &Client{out: make(chan []byte, 32), done: make(chan struct{})}
	h.addClient(c)
	for len(c.out) > 0 {
		<-c.out
	}

	// Archive must be rejected for BOTH external schemas (including idle 2.1.331 pid session)
	for _, id := range []string{newID, legacyID} {
		m.handle(c, mustJSON(map[string]any{"op": "archive", "id": id, "nonce": "arch-" + id}))
		select {
		case raw := <-c.out:
			var ack map[string]any
			_ = json.Unmarshal(raw, &ack)
			if ack["ok"] != false || !strings.Contains(str(ack, "msg"), "Stop This Session") {
				t.Fatalf("archive escaped external guard for %s: %+v", id, ack)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("archive timed out for %s", id)
		}
	}

	// Stop op must invoke stopExternal cleanly for both schemas
	var stopMu sync.Mutex
	var stopped []claudeActive
	m.stopExternalFn = func(_ context.Context, a claudeActive) error {
		stopMu.Lock()
		stopped = append(stopped, a)
		stopMu.Unlock()
		return nil
	}
	for _, id := range []string{newID, legacyID} {
		m.handle(c, mustJSON(map[string]any{"op": "stop", "id": id, "nonce": "stop-" + id}))
		deadline := time.After(2 * time.Second)
		gotAck := false
		for !gotAck {
			select {
			case raw := <-c.out:
				var msg map[string]any
				_ = json.Unmarshal(raw, &msg)
				if msg["t"] == "ack" && msg["op"] == "stop" && msg["id"] == id {
					if msg["ok"] != true {
						t.Fatalf("stop failed for %s: %+v", id, msg)
					}
					gotAck = true
				}
			case <-deadline:
				t.Fatalf("stop timed out for %s", id)
			}
		}
	}
	stopMu.Lock()
	if len(stopped) != 2 || stopped[0].PID != 65197 || stopped[0].backgroundID() != "" || stopped[1].backgroundID() != "bg-42" {
		t.Fatalf("unexpected stop targets: %+v", stopped)
	}
	stopMu.Unlock()

	// Takeover (getRun with takeover=true) stops the external 2.1.331 process and detects if still stopping
	stillRunning := true
	m.pidAlive = func(pid int) bool { return pid == 65197 && stillRunning }
	liveSessPath := filepath.Join(sessionsDir, "65197.json")
	if err := os.WriteFile(liveSessPath, mustJSON(map[string]any{"pid": 65197, "sessionId": newID, "status": "busy", "startedAt": int64(1795652928486)}), 0600); err != nil {
		t.Fatal(err)
	}
	stopCalled := false
	m.stopExternalFn = func(_ context.Context, a claudeActive) error {
		stopCalled = true
		if a.PID != 65197 {
			t.Errorf("takeover passed unexpected active: %+v", a)
		}
		return nil
	}
	if _, err := m.getRun(newID, true); err == nil || !strings.Contains(err.Error(), "Still Stopping") {
		t.Fatalf("expected still-stopping error when PID remains alive, got: %v", err)
	}
	if !stopCalled {
		t.Fatal("takeover did not invoke stopExternal")
	}
	// Once external PID exits, takeover clears active and proceeds to start() (which fails cleanly because m.bin is empty)
	m.stopExternalFn = func(_ context.Context, a claudeActive) error {
		stillRunning = false
		return nil
	}
	if _, err := m.getRun(newID, true); err == nil || !strings.Contains(err.Error(), "Install Claude Code") {
		t.Fatalf("takeover did not proceed past stopped external session: %v", err)
	}
	_ = os.Remove(liveSessPath)

	// Safety guard: signalExternalPID must refuse own PID
	if err := signalExternalPID(os.Getpid()); err == nil {
		t.Fatal("signalExternalPID allowed signaling own PID")
	}

	// Verify legacy background stop executes `claude stop <bgID>` and NOT `pid:<n>`
	if runtime.GOOS != "windows" {
		argLog := filepath.Join(dir, "cli-args.txt")
		m.stopExternalFn = nil
		m.bin = writeFakeClaudeCLI(t, `echo "$@" >> "`+argLog+`"`)
		if err := m.stopExternal(context.Background(), legacyActive); err != nil {
			t.Fatalf("legacy stopExternal failed: %v", err)
		}
		gotArgs, err := os.ReadFile(argLog)
		if err != nil || strings.TrimSpace(string(gotArgs)) != "stop bg-42" {
			t.Fatalf("unexpected legacy stop args: %q %v", string(gotArgs), err)
		}
	}

	// Subagent parentRunning with new 2.1.331 schema (busy and idle) + dead/absent parent + read-only guard
	wm, root, _, _ := claudeWorkFixture(t)
	// New 2.1.331 idle session (no ID, only PID + Status: "idle") keeps background worker running
	wm.mu.Lock()
	wm.active = map[string]claudeActive{root: {PID: 77777, Status: "idle"}}
	wm.mu.Unlock()
	list, err := wm.listWork(root)
	if err != nil || len(list) != 1 || !list[0].Running || list[0].AgentState != "running" {
		t.Fatalf("idle 2.1.331 parent did not keep subagent running: %+v %v", list, err)
	}

	// New 2.1.331 busy session also keeps subagent running
	wm.mu.Lock()
	wm.active = map[string]claudeActive{root: {PID: 77777, Status: "busy"}}
	wm.mu.Unlock()
	list, err = wm.listWork(root)
	if err != nil || len(list) != 1 || !list[0].Running {
		t.Fatalf("busy 2.1.331 parent did not keep subagent running: %+v %v", list, err)
	}

	// Read-only ingress guard still blocks control ops on subagent chat
	wc := &Client{out: make(chan []byte, 16), done: make(chan struct{})}
	wm.hub.addClient(wc)
	for len(wc.out) > 0 {
		<-wc.out
	}
	wm.handle(wc, mustJSON(map[string]any{"op": "send", "id": list[0].ID, "text": "hello", "nonce": "ro"}))
	var roAck map[string]any
	_ = json.Unmarshal(<-wc.out, &roAck)
	if roAck["ok"] != false || !strings.Contains(str(roAck, "msg"), "Read Only") {
		t.Fatalf("subagent read-only guard failed: %+v", roAck)
	}

	// Once parent is absent (e.g. dead PID rejected), subagent degrades to saved/not-running
	wm.mu.Lock()
	wm.active = map[string]claudeActive{}
	wm.mu.Unlock()
	list, err = wm.listWork(root)
	if err != nil || len(list) != 1 || list[0].Running || list[0].AgentState != "saved" {
		t.Fatalf("inactive parent left subagent running: %+v %v", list, err)
	}
}

func TestClaudeRefreshBroadcastThrottling(t *testing.T) {
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	sessionsDir := filepath.Join(dir, "sessions")
	_ = os.MkdirAll(sessionsDir, 0700)

	idA := uuid()
	idB := uuid()
	baseTime := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock := baseTime

	writeClaudeSessionJSONL(t, projectsDir, "proj", idA, []map[string]any{
		{"type": "user", "uuid": "uA", "sessionId": idA, "cwd": "/tmp/proj-a", "message": map[string]any{"content": "Thread A"}},
		{"type": "assistant", "uuid": "aA", "sessionId": idA, "message": map[string]any{"id": "mA", "model": "claude-haiku-5-5", "content": []any{map[string]any{"type": "text", "text": "Initial reply"}}}},
	}, baseTime)
	writeClaudeSessionJSONL(t, projectsDir, "proj", idB, []map[string]any{
		{"type": "user", "uuid": "uB", "sessionId": idB, "cwd": "/tmp/proj-b", "message": map[string]any{"content": "Thread B"}},
	}, baseTime.Add(-time.Minute))

	h := NewHub(NewIPCClient(""), "")
	h.organization = nil
	m := newClaudeManager(h)
	m.root = projectsDir
	m.sessionsDir = sessionsDir
	m.preferencesPath = filepath.Join(dir, "prefs.json")
	m.bin = ""
	m.recencyBroadcastInterval = 15 * time.Second
	m.now = func() time.Time { return clock }

	c := &Client{out: make(chan []byte, 64), done: make(chan struct{})}
	ev := &Client{events: true, eventsOnly: true, out: make(chan []byte, 16), done: make(chan struct{})}
	h.addClient(c)
	h.addClient(ev)
	for len(c.out) > 0 {
		<-c.out
	}

	drainCount := func() (int, []byte) {
		count := 0
		var last []byte
		for {
			select {
			case raw := <-c.out:
				count++
				last = raw
			default:
				return count, last
			}
		}
	}

	// Initial refresh broadcasts once
	m.refresh()
	if n, _ := drainCount(); n != 1 {
		t.Fatalf("initial refresh broadcast count = %d, want 1", n)
	}
	if len(ev.out) != 0 {
		t.Fatalf("eventsOnly client received %d thread broadcasts", len(ev.out))
	}

	// Simulate active streaming writing progress lines every second for 14 seconds:
	// mtime/Recency changes every second, but title/lastMsg/running/order do not change.
	pathA := filepath.Join(projectsDir, "proj", idA+".jsonl")
	for sec := 1; sec <= 14; sec++ {
		clock = baseTime.Add(time.Duration(sec) * time.Second)
		f, err := os.OpenFile(pathA, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Write(append(mustJSON(map[string]any{"type": "progress", "uuid": uuid(), "sessionId": idA, "tick": sec}), '\n'))
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(pathA, clock, clock); err != nil {
			t.Fatal(err)
		}
		m.refresh()
	}
	if n, _ := drainCount(); n != 0 {
		t.Fatalf("recency-only ticks triggered %d broadcasts inside throttle window", n)
	}

	// m.lastList must still have the latest Recency for newly connecting clients
	m.mu.Lock()
	latestSnap := append([]byte(nil), m.lastList...)
	m.mu.Unlock()
	if got := decodeThreadsFrame(t, latestSnap)[idA].Recency; got != baseTime.Add(14*time.Second).UnixMilli() {
		t.Fatalf("m.lastList recency = %d, want %d", got, baseTime.Add(14*time.Second).UnixMilli())
	}

	// At t=15s (throttle elapsed), pending Recency refresh coalesces into 1 broadcast
	clock = baseTime.Add(15 * time.Second)
	m.refresh()
	if n, last := drainCount(); n != 1 {
		t.Fatalf("post-throttle broadcast count = %d, want 1", n)
	} else if got := decodeThreadsFrame(t, last)[idA].Recency; got != baseTime.Add(14*time.Second).UnixMilli() {
		t.Fatalf("coalesced broadcast recency = %d, want %d", got, baseTime.Add(14*time.Second).UnixMilli())
	}

	// Further ticks with no changes produce 0 broadcasts even past another throttle window
	clock = baseTime.Add(40 * time.Second)
	m.refresh()
	if n, _ := drainCount(); n != 0 {
		t.Fatalf("unchanged list triggered %d broadcasts", n)
	}

	// Meaningful changes inside throttle window must broadcast IMMEDIATELY
	// 1. Running state change (2.1.331 busy + TurnAt)
	clock = baseTime.Add(41 * time.Second)
	m.mu.Lock()
	m.active = map[string]claudeActive{idA: {ID: "pid:9001", PID: 9001, SessionID: idA, State: "running", Status: "busy", StatusUpdatedAt: clock.UnixMilli()}}
	m.mu.Unlock()
	m.refresh()
	if n, last := drainCount(); n != 1 {
		t.Fatalf("running change broadcast count = %d, want 1", n)
	} else if th := decodeThreadsFrame(t, last)[idA]; !th.Running || th.TurnAt != clock.UnixMilli() {
		t.Fatalf("running broadcast payload mismatch: %+v", th)
	}

	// 2. LastMsg change inside throttle window (1s later)
	clock = baseTime.Add(42 * time.Second)
	f, err := os.OpenFile(pathA, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(append(mustJSON(map[string]any{"type": "assistant", "uuid": uuid(), "sessionId": idA, "message": map[string]any{"id": "mA2", "model": "claude-haiku-5-5", "content": []any{map[string]any{"type": "text", "text": "Updated assistant message"}}}}), '\n'))
	_ = f.Close()
	_ = os.Chtimes(pathA, clock, clock)
	m.refresh()
	if n, last := drainCount(); n != 1 {
		t.Fatalf("lastMsg change broadcast count = %d, want 1", n)
	} else if got := decodeThreadsFrame(t, last)[idA].LastMsg; got != "Updated assistant message" {
		t.Fatalf("lastMsg payload = %q", got)
	}

	// 3. Title change inside throttle window (1s later)
	clock = baseTime.Add(43 * time.Second)
	f, _ = os.OpenFile(pathA, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.Write(append(mustJSON(map[string]any{"type": "custom-title", "customTitle": "Renamed Thread A", "sessionId": idA}), '\n'))
	_ = f.Close()
	_ = os.Chtimes(pathA, clock, clock)
	m.refresh()
	if n, last := drainCount(); n != 1 {
		t.Fatalf("title change broadcast count = %d, want 1", n)
	} else if got := decodeThreadsFrame(t, last)[idA].Title; got != "Renamed Thread A" {
		t.Fatalf("title payload = %q", got)
	}

	// 3b. Cwd change inside throttle window (1s later)
	clock = baseTime.Add(43*time.Second + 200*time.Millisecond)
	f, _ = os.OpenFile(pathA, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.Write(append(mustJSON(map[string]any{"type": "progress", "uuid": uuid(), "sessionId": idA, "cwd": "/tmp/proj-a-moved"}), '\n'))
	_ = f.Close()
	_ = os.Chtimes(pathA, clock, clock)
	m.refresh()
	if n, last := drainCount(); n != 1 {
		t.Fatalf("cwd change broadcast count = %d, want 1", n)
	} else if got := decodeThreadsFrame(t, last)[idA].Cwd; got != "/tmp/proj-a-moved" {
		t.Fatalf("cwd payload = %q", got)
	}

	// 3c. Model change inside throttle window (1s later)
	clock = baseTime.Add(43*time.Second + 500*time.Millisecond)
	f, _ = os.OpenFile(pathA, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.Write(append(mustJSON(map[string]any{"type": "assistant", "uuid": uuid(), "sessionId": idA, "message": map[string]any{"id": "mA3", "model": "claude-sonnet-4-6", "content": []any{map[string]any{"type": "text", "text": "Updated assistant message"}}}}), '\n'))
	_ = f.Close()
	_ = os.Chtimes(pathA, clock, clock)
	m.refresh()
	if n, _ := drainCount(); n != 1 {
		t.Fatalf("model change broadcast count = %d, want 1", n)
	}

	// 4. Sort order change (older thread B jumps above thread A) inside throttle window
	clock = baseTime.Add(44 * time.Second)
	pathB := filepath.Join(projectsDir, "proj", idB+".jsonl")
	f, _ = os.OpenFile(pathB, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.Write(append(mustJSON(map[string]any{"type": "progress", "uuid": uuid(), "sessionId": idB}), '\n'))
	_ = f.Close()
	_ = os.Chtimes(pathB, clock, clock)
	m.refresh()
	if n, _ := drainCount(); n != 1 {
		t.Fatalf("sort reorder broadcast count = %d, want 1", n)
	}

	// 5. Session added inside throttle window
	clock = baseTime.Add(45 * time.Second)
	idC := uuid()
	writeClaudeSessionJSONL(t, projectsDir, "proj", idC, []map[string]any{
		{"type": "user", "uuid": "uC", "sessionId": idC, "cwd": "/tmp/proj-c", "message": map[string]any{"content": "Thread C"}},
	}, clock)
	m.refresh()
	if n, last := drainCount(); n != 1 {
		t.Fatalf("session add broadcast count = %d, want 1", n)
	} else if _, ok := decodeThreadsFrame(t, last)[idC]; !ok {
		t.Fatalf("new session missing from broadcast")
	}

	// 6. Session archived inside throttle window
	clock = baseTime.Add(46 * time.Second)
	if err := m.setPreference(idC, claudePreference{Archived: true}); err != nil {
		t.Fatal(err)
	}
	m.refresh()
	if n, last := drainCount(); n != 1 {
		t.Fatalf("session archive broadcast count = %d, want 1", n)
	} else if _, ok := decodeThreadsFrame(t, last)[idC]; ok {
		t.Fatalf("archived session still present in broadcast")
	}
}
