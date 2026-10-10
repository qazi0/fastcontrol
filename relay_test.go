package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func mustPatches(t *testing.T, s string) []patch {
	t.Helper()
	var ps []patch
	if err := json.Unmarshal([]byte(s), &ps); err != nil {
		t.Fatal(err)
	}
	return ps
}

func TestApplyImmerPatches(t *testing.T) {
	var root any
	_ = json.Unmarshal([]byte(`{"a":{"items":[{"t":"x"},{"t":"y"}]},"n":1}`), &root)
	ps := mustPatches(t, `[
		{"op":"replace","path":["a","items",1,"t"],"value":"Y"},
		{"op":"add","path":["a","items",2],"value":{"t":"z"}},
		{"op":"add","path":["a","items",0],"value":{"t":"w"}},
		{"op":"remove","path":["a","items",1]},
		{"op":"add","path":["a","new"],"value":[1,2,3]},
		{"op":"replace","path":["a","new","length"],"value":1},
		{"op":"remove","path":["n"]}
	]`)
	for _, p := range ps {
		var err error
		if root, err = applyPatch(root, p); err != nil {
			t.Fatal(err)
		}
	}
	var want any
	_ = json.Unmarshal([]byte(`{"a":{"items":[{"t":"w"},{"t":"Y"},{"t":"z"}],"new":[1]}}`), &want)
	if !reflect.DeepEqual(root, want) {
		got, _ := json.Marshal(root)
		t.Fatalf("got %s", got)
	}
}

func TestApplyPatchErrors(t *testing.T) {
	var root any
	_ = json.Unmarshal([]byte(`{"a":[1]}`), &root)
	for _, s := range []string{
		`{"op":"replace","path":["missing","x"],"value":1}`,
		`{"op":"remove","path":["a",5]}`,
	} {
		var p patch
		_ = json.Unmarshal([]byte(s), &p)
		if _, err := applyPatch(root, p); err == nil {
			t.Fatalf("expected error for %s", s)
		}
	}
}

func TestHumanizeAndShellScript(t *testing.T) {
	if got := humanize("exitedReviewMode"); got != "Exited Review Mode" {
		t.Fatal(got)
	}
	if got := shellScript(`/bin/zsh -lc 'echo it'\''s'`); got != "echo it's" {
		t.Fatal(got)
	}
}

func TestRolloutTail(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/r.jsonl"
	lines := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1","started_at":10}}
{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"u","content":[{"type":"text","text":"hi"}]}}}
{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"AgentMessage","id":"a","content":[{"type":"Text","text":"hello"}],"phase":"final"}}}
`
	if err := writeFile(path, lines); err != nil {
		t.Fatal(err)
	}
	v := newRolloutView(path)
	if err := v.load(); err != nil {
		t.Fatal(err)
	}
	if len(v.turns) != 1 || len(v.turns[0].Items) != 2 || !v.running() {
		t.Fatalf("unexpected view: %+v", v.turns)
	}
	if err := appendFile(path, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","duration_ms":5}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	ch, err := v.poll()
	if err != nil || ch == nil || !ch.status[v.turns[0]] || v.running() {
		t.Fatalf("poll did not pick up completion: %v %+v", err, ch)
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }

func appendFile(p, s string) error {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s)
	return err
}
