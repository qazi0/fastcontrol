package main

import (
	"strings"
	"testing"
)

func TestClaudeTranscriptPreservesToolResultsAndModel(t *testing.T) {
	v := newClaudeTranscript()
	v.apply(map[string]any{"type": "user", "uuid": "u", "cwd": "/tmp/test", "message": map[string]any{"content": "Test"}})
	v.apply(map[string]any{"type": "assistant", "effort": "low", "message": map[string]any{"id": "a", "model": "claude-haiku-5-5", "usage": map[string]any{"input_tokens": float64(20), "cache_read_input_tokens": float64(100)}, "content": []any{map[string]any{"type": "tool_use", "id": "tool", "name": "Bash", "input": map[string]any{"command": "pwd"}}}}})
	v.apply(map[string]any{"type": "user", "toolUseResult": map[string]any{"exitCode": float64(0)}, "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tool", "content": "/tmp/test"}}}})
	v.apply(map[string]any{"type": "assistant", "message": map[string]any{"id": "answer", "model": "claude-haiku-5-5", "content": []any{map[string]any{"type": "text", "text": "DONE"}}}})
	if len(v.Turns) != 1 || len(v.Turns[0].Items) != 3 {
		t.Fatalf("duplicate tool-result turn: %+v", v.Turns)
	}
	tool := v.Turns[0].Items[1]
	if tool.K != "cmd" || tool.Body != "/tmp/test" || tool.Exit == nil || *tool.Exit != 0 {
		t.Fatalf("tool lost: %+v", tool)
	}
	if v.Meta.Model != "claude-haiku-5-5" || v.Meta.CtxUsed != 120 || v.Meta.Effort != "low" {
		t.Fatalf("metadata: %+v", v.Meta)
	}
}

func TestClaudeFileResultDoesNotReplaceDiff(t *testing.T) {
	v := newClaudeTranscript()
	v.ensureTurn("u", 0)
	v.apply(map[string]any{"type": "assistant", "message": map[string]any{"id": "a", "model": "claude-haiku-5-5", "content": []any{map[string]any{"type": "tool_use", "id": "edit", "name": "Edit", "input": map[string]any{"file_path": "/tmp/a", "old_string": "old", "new_string": "new"}}}}})
	v.apply(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "edit", "content": "Success"}}}})
	if !strings.Contains(v.Turns[0].Items[0].Body, "-old") || !strings.Contains(v.Diffs["u"], "+new") {
		t.Fatal("file result discarded diff")
	}
}

func TestClaudeStreamingFinalMessageReconciles(t *testing.T) {
	h := NewHub(NewIPCClient("/nonexistent"), "")
	m := newClaudeManager(h)
	v := newClaudeTranscript()
	v.ensureTurn("u", 0)
	r := &ClaudeRun{manager: m, id: "test", cwd: "/tmp", alive: true, running: true, view: v, options: claudePreference{Model: "claude-haiku-5-5", Effort: "low", Permission: "default"}, permissions: map[string]claudePermission{}, controls: map[string]chan map[string]any{}, blocks: map[int]string{}, arguments: map[int]string{}, blockDefs: map[int]map[string]any{}, lastPublished: map[string][]byte{}}
	r.consume(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"id": "reply", "model": "claude-haiku-5-5"}}})
	r.consume(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_start", "index": float64(0), "content_block": map[string]any{"type": "text", "text": ""}}})
	r.consume(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": float64(0), "delta": map[string]any{"type": "text_delta", "text": "READY"}}})
	r.consume(map[string]any{"type": "assistant", "message": map[string]any{"id": "reply", "model": "claude-haiku-5-5", "content": []any{map[string]any{"type": "text", "text": "READY"}}}})
	meta, turns := r.snapshot()
	if meta.Model != "claude-haiku-5-5" || len(turns[0].Items) != 1 || turns[0].Items[0].Text != "READY" {
		t.Fatalf("stream duplication: %+v", turns)
	}
}
