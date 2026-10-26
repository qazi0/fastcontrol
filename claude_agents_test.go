package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func claudeWorkFixture(t *testing.T) (*ClaudeManager, string, string, string) {
	t.Helper()
	root := uuid()
	dir := t.TempDir()
	parent := filepath.Join(dir, root+".jsonl")
	agent := filepath.Join(dir, root, "subagents", "agent-worker.jsonl")
	if err := os.MkdirAll(filepath.Dir(agent), 0700); err != nil {
		t.Fatal(err)
	}
	writeClaudeWorkRows(t, parent,
		map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "launch", "name": "Agent", "input": map[string]any{"description": "Inspect the transcript"}}}}},
		map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "launch"}}}, "toolUseResult": map[string]any{"status": "async_launched", "agentId": "worker"}},
	)
	writeClaudeWorkRows(t, agent, map[string]any{"type": "user", "isSidechain": true, "uuid": "prompt", "message": map[string]any{"content": "Inspect the latest output"}}, map[string]any{"type": "assistant", "isSidechain": true, "uuid": "reply", "message": map[string]any{"model": "claude-haiku-5-5", "content": []any{map[string]any{"type": "text", "text": "Reading source files"}}}})
	if err := os.WriteFile(strings.TrimSuffix(agent, ".jsonl")+".meta.json", mustJSON(map[string]any{"agentType": "general-purpose", "description": "Inspect the transcript", "toolUseId": "launch"}), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHub(NewIPCClient(""), "")
	m := newClaudeManager(h)
	h.claude = m
	m.preferencesPath = filepath.Join(dir, "private-preferences.json")
	m.entries = map[string]claudeEntry{root: {Path: parent, Info: ThreadInfo{ID: root, Cwd: dir}}}
	m.active = map[string]claudeActive{root: {State: "running"}}
	return m, root, parent, agent
}
func writeClaudeWorkRows(t *testing.T, path string, rows ...map[string]any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, row := range rows {
		if _, err = f.Write(append(mustJSON(row), '\n')); err != nil {
			t.Fatal(err)
		}
	}
}
func TestClaudeAgentTranscriptLifecycleAndReadOnlyControls(t *testing.T) {
	m, root, parent, path := claudeWorkFixture(t)
	list, err := m.listWork(root)
	if err != nil || len(list) != 1 || !list[0].Running || list[0].Activity != "Reading source files" {
		t.Fatalf("agent list: %+v %v", list, err)
	}
	ordinary, err := readClaudeTranscript(path, 15, 1<<20)
	if err != nil || len(ordinary.Turns) != 0 {
		t.Fatal("ordinary reader included sidechains", err)
	}
	c := &Client{out: make(chan []byte, 64), done: make(chan struct{})}
	m.hub.addClient(c)
	for len(c.out) > 0 {
		<-c.out
	}
	if err = m.openWork(c, root, list[0].ID, false); err != nil {
		t.Fatal(err)
	}
	var first struct {
		Meta  Meta
		Turns []Turn
	}
	json.Unmarshal(<-c.out, &first)
	if !first.Meta.ReadOnly || !first.Meta.Running || first.Meta.Model != "claude-haiku-5-5" || len(first.Turns) != 1 {
		t.Fatalf("snapshot %+v", first)
	}
	m.pollWork()
	if len(c.out) != 0 {
		t.Fatal("unchanged poll rebuilt transcript")
	}
	for _, op := range []string{"send", "stop", "wake", "settings", "approve", "answer", "compact", "archive", "unarchive", "rename", "fork"} {
		m.handle(c, mustJSON(map[string]any{"op": op, "id": list[0].ID, "nonce": op}))
		var ack map[string]any
		json.Unmarshal(<-c.out, &ack)
		if ack["ok"] != false || !strings.Contains(ack["msg"].(string), "Read Only") {
			t.Fatalf("%s escaped guard: %+v", op, ack)
		}
	}
	writeClaudeWorkRows(t, path, map[string]any{"type": "assistant", "isSidechain": true, "uuid": "final", "message": map[string]any{"model": "claude-haiku-5-5", "stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": "Review complete"}}}})
	writeClaudeWorkRows(t, parent, map[string]any{"type": "user", "message": map[string]any{"content": "<task-notification><task-id>worker</task-id><status>completed</status></task-notification>"}})
	m.pollWork()
	var final struct {
		Meta  Meta
		Turns []Turn
	}
	json.Unmarshal(<-c.out, &final)
	if final.Meta.Running || final.Meta.AgentState != "completed" || !strings.Contains(string(mustJSON(final.Turns)), "Review complete") {
		t.Fatalf("completion: %+v", final)
	}
	m.mu.Lock()
	old := m.workWatch[c][list[0].ID]
	delete(m.workWatch[c], list[0].ID)
	m.mu.Unlock()
	work, _ := m.resolveWork(root, list[0].ID)
	if err = m.publishWork(c, work, old, false); err != nil {
		t.Fatal(err)
	}
	if len(c.out) != 0 {
		t.Fatal("closed view was re-published")
	}
	if err = m.openWork(c, root, list[0].ID, false); err != nil {
		t.Fatal(err)
	}
	<-c.out
	if err = m.publishWork(c, work, old, false); err != nil {
		t.Fatal(err)
	}
	if len(c.out) != 0 {
		t.Fatal("an old subscription overwrote a reopened view")
	}
	if _, err = m.resolveWork("unrelated", list[0].ID); err == nil {
		t.Fatal("unrelated parent opened an agent")
	}
}
func TestClaudeWorkIndexRetriesPartialLinesAndInvalidatesReplacement(t *testing.T) {
	m, root, parent, _ := claudeWorkFixture(t)
	m.listWork(root)
	before, _ := os.Stat(parent)
	original, _ := os.ReadFile(parent)
	rewritten := bytes.ReplaceAll(original, []byte(`"worker"`), []byte(`"second"`))
	os.WriteFile(parent, rewritten, 0600)
	os.Chtimes(parent, before.ModTime(), before.ModTime())
	changed, _, e := m.workIndex(parent)
	if e != nil || changed["worker"].ID != "" || changed["second"].ID != "second" {
		t.Fatal("same-size rewrite retained stale tasks", e)
	}
	os.WriteFile(parent, original, 0600)
	m.workIndex(parent)
	row := mustJSON(map[string]any{"type": "user", "message": map[string]any{"content": "<task-notification><task-id>worker</task-id><status>failed</status></task-notification>"}})
	f, err := os.OpenFile(parent, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(row)
	f.Close()
	refs, _, err := m.workIndex(parent)
	if err != nil || refs["worker"].State != "running" {
		t.Fatal("partial notification applied", err)
	}
	f, _ = os.OpenFile(parent, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("\n")
	f.Close()
	refs, _, _ = m.workIndex(parent)
	if refs["worker"].State != "failed" {
		t.Fatal("completed line was not applied")
	}
	next := parent + ".next"
	os.WriteFile(next, []byte("{}\n"), 0600)
	if err = os.Rename(next, parent); err != nil {
		t.Fatal(err)
	}
	refs, _, _ = m.workIndex(parent)
	if len(refs) != 0 {
		t.Fatal("replaced transcript retained old tasks")
	}
}

func TestClaudeAgentDiffUsesTheReadOnlySidechain(t *testing.T) {
	m, root, _, path := claudeWorkFixture(t)
	writeClaudeWorkRows(t, path, map[string]any{"type": "assistant", "isSidechain": true, "uuid": "edit-reply", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "edit", "name": "Edit", "input": map[string]any{"file_path": "/private/example.txt", "old_string": "before", "new_string": "after"}}}}})
	list, err := m.listWork(root)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{out: make(chan []byte, 16), done: make(chan struct{})}
	m.hub.addClient(c)
	for len(c.out) > 0 {
		<-c.out
	}
	if err = m.openWork(c, root, list[0].ID, false); err != nil {
		t.Fatal(err)
	}
	<-c.out
	m.handle(c, mustJSON(map[string]any{"op": "diff", "id": list[0].ID, "key": "prompt"}))
	var result map[string]any
	json.Unmarshal(<-c.out, &result)
	if result["t"] != "diff" || !strings.Contains(result["text"].(string), "-before") || !strings.Contains(result["text"].(string), "+after") {
		t.Fatalf("agent diff: %+v", result)
	}
}
func TestClaudeWorkflowChildrenAndBackgroundOutput(t *testing.T) {
	m, root, parent, _ := claudeWorkFixture(t)
	dir := filepath.Join(strings.TrimSuffix(parent, ".jsonl"), "subagents", "workflows", "wf-test")
	os.MkdirAll(dir, 0700)
	worker := filepath.Join(dir, "agent-workflow-child.jsonl")
	writeClaudeWorkRows(t, worker, map[string]any{"type": "assistant", "isSidechain": true, "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Workflow progress"}}}})
	writeClaudeWorkRows(t, parent, map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "workflow"}}}, "toolUseResult": map[string]any{"status": "async_launched", "taskType": "local_workflow", "taskId": "workflow-task", "transcriptDir": filepath.Join("old-folder", "subagents", "workflows", "wf-test"), "summary": "Run workflow"}})
	list, _ := m.listWork(root)
	var workflow ThreadInfo
	for _, a := range list {
		if a.Kind == "workflow" {
			workflow = a
		}
	}
	children, err := m.listWork(workflow.ID)
	if err != nil || len(children) != 1 || children[0].AgentID != "workflow-child" {
		t.Fatalf("workflow children %+v %v", children, err)
	}
	path := filepath.Join(t.TempDir(), "claude-fixture", "project", "tasks", "command-task.output")
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, []byte("Background output\n"), 0600)
	if got := claudeTaskOutput(claudeWorkRef{ID: "command-task", Output: path}); got != path {
		t.Fatalf("output location %q", got)
	}
	if got := claudeTaskOutput(claudeWorkRef{ID: "../command-task", Output: path}); got != "" {
		t.Fatal("path traversal accepted")
	}
}
