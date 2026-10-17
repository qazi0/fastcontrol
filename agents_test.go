package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func agentDatabase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.sqlite")
	rollout := filepath.Join(dir, "child.jsonl")
	data := "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"turn\"}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"item_completed\",\"item\":{\"type\":\"AgentMessage\",\"content\":[{\"text\":\"Reviewing Audio Playback\"}]}}}\n"
	if err := os.WriteFile(rollout, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	source := `{"subagent":{"thread_spawn":{"parent_thread_id":"parent","agent_path":"/root/reviewer","agent_nickname":"Reviewer"}}}`
	schema := `CREATE TABLE threads (id TEXT PRIMARY KEY,name TEXT,title TEXT,first_user_message TEXT,preview TEXT,cwd TEXT,source TEXT,thread_source TEXT,recency_at_ms INTEGER,rollout_path TEXT,is_pinned INTEGER,git_branch TEXT,model TEXT,reasoning_effort TEXT,approval_mode TEXT,sandbox_policy TEXT,archived INTEGER);
CREATE TABLE thread_spawn_edges(parent_thread_id TEXT,child_thread_id TEXT PRIMARY KEY,status TEXT);
INSERT INTO threads VALUES('child','','Review','Prompt','','/same',` + sqlQuote(source) + `,'subagent',20,` + sqlQuote(rollout) + `,0,'','gpt-6.1-sol','low','never','{}',1);
INSERT INTO threads VALUES('edge-child','','Edge Child','Prompt','Preview','/same','exec','subagent',10,` + sqlQuote(rollout) + `,0,'','gpt-6.1-sol','low','never','{}',0);
INSERT INTO threads VALUES('unrelated','','Other','Prompt','Preview','/same','exec','',30,` + sqlQuote(rollout) + `,0,'','gpt-6.1-sol','low','never','{}',0);
INSERT INTO thread_spawn_edges VALUES('parent','child','open'),('parent','edge-child','open');`
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command(sqliteBinary(), path, schema).CombinedOutput(); err != nil {
			t.Fatalf("database fixture: %s %v", out, err)
		}
	} else {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err = db.Exec(schema); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestSubagentsUseParentageAndIncludeArchivedChildren(t *testing.T) {
	db := agentDatabase(t)
	agents, err := listSubagents(db, "parent")
	if err != nil || len(agents) != 2 {
		t.Fatalf("children: %+v %v", agents, err)
	}
	if agents[0].ID != "child" || agents[0].AgentPath != "/root/reviewer" || agents[0].ParentID != "parent" || !agents[0].Running || agents[0].Activity != "Reviewing Audio Playback" {
		t.Fatalf("lost agent metadata: %+v", agents[0])
	}
	if agents, err = listSubagents(db, "parent' OR 1=1 --"); err != nil || len(agents) != 0 {
		t.Fatalf("parent filter escaped: %+v %v", agents, err)
	}
	if _, err = sqliteJSON(db, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
}

func TestSubagentControlsAreRejectedBeforeDispatch(t *testing.T) {
	db := agentDatabase(t)
	agents, err := listSubagents(db, "parent")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHub(NewIPCClient(""), db)
	h.threads = agents
	c := &Client{out: make(chan []byte, 32), done: make(chan struct{})}
	for _, op := range []string{"send", "stop", "wake", "settings", "approve", "answer", "compact", "archive", "unarchive"} {
		h.handle(c, mustJSON(map[string]any{"op": op, "id": "child", "nonce": op}))
		select {
		case data := <-c.out:
			var result map[string]any
			_ = json.Unmarshal(data, &result)
			if result["ok"] != false || result["nonce"] != op || result["msg"] != "Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent" {
				t.Fatalf("control escaped: %s %s", op, data)
			}
		case <-time.After(time.Second):
			t.Fatalf("control was not rejected: %s", op)
		}
	}
	s := newSession(h, agents[0])
	if !s.meta().ReadOnly {
		t.Fatal("subagent meta is writable")
	}
}

func TestClosedSubscriptionCannotBeReaddedByQueuedOpen(t *testing.T) {
	h := NewHub(NewIPCClient(""), "")
	c := &Client{out: make(chan []byte, 8), done: make(chan struct{}), subs: map[string]bool{"child": true}}
	h.addClient(c)
	s := newSession(h, ThreadInfo{ID: "child", Subagent: true})
	h.sessions[s.id] = s
	s.subscribe(c)
	h.handle(c, []byte(`{"op":"close","id":"child"}`))
	(<-s.cmds)()
	(<-s.cmds)()
	if len(s.subs) != 0 {
		t.Fatal("closed viewer was resubscribed")
	}
}

func TestSubagentActivityKeepsExactThreadIdentity(t *testing.T) {
	desktop := desktopItem(map[string]any{"type": "subAgentActivity", "kind": "interacted", "agentPath": "/root/reviewer", "agentThreadId": "child"})
	rollout, ok := rolloutItem(map[string]any{"type": "SubAgentActivity", "kind": "interacted", "agent_path": "/root/reviewer", "agent_thread_id": "child"})
	if !ok || desktop.AgentID != "child" || rollout.AgentID != "child" || desktop.AgentPath != rollout.AgentPath {
		t.Fatal("subagent identity lost")
	}
}
