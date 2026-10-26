package main

// Thread list straight from the Codex state database (WAL mode), opened
// read-only. A query over ~900 threads takes about 6 ms. Running state comes
// from the tail of each recently written rollout file.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ThreadInfo struct {
	Provider  string `json:"provider,omitempty"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Preview   string `json:"preview"`
	Cwd       string `json:"cwd"`
	Source    string `json:"source"`
	Kind      string `json:"kind,omitempty"`
	Recency   int64  `json:"recency"`
	Rollout   string `json:"-"`
	Active    bool   `json:"active"`
	Live      bool   `json:"live"`
	Running   bool   `json:"running"`
	Attention bool   `json:"attention,omitempty"` // waiting on an approval or a question
	Subagent  bool   `json:"subagent,omitempty"`
	ParentID  string `json:"parentId,omitempty"`
	AgentPath string `json:"agentPath,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	Activity  string `json:"activity,omitempty"`
	Pinned    bool   `json:"pinned,omitempty"`
	Exec      bool   `json:"exec,omitempty"` // first turn running headless via codex exec
	LastMsg   string `json:"lastMsg,omitempty"`
	LastErr   string `json:"lastErr,omitempty"`
	TurnAt    int64  `json:"turnAt,omitempty"` // when the current or last turn started (ms)
	Branch    string `json:"branch,omitempty"`

	// Settings recorded in the database, used for threads the desktop has not loaded.
	Model    string `json:"-"`
	Effort   string `json:"-"`
	Approval string `json:"-"`
	Sandbox  string `json:"-"`
}

func stateDBPath() string {
	matches, _ := filepath.Glob(filepath.Join(codexHome(), "state_*.sqlite"))
	best := ""
	for _, m := range matches {
		if m > best {
			best = m
		}
	}
	return best
}

const threadSelect = `select id, coalesce(nullif(name,''), nullif(title,''), substr(first_user_message,1,80)) as title,
 substr(preview,1,160) as preview, cwd, source, coalesce(thread_source,'') as kind,
 recency_at_ms as recency, rollout_path as rollout, is_pinned as pinned,
 coalesce(git_branch,'') as branch, coalesce(model,'') as model, coalesce(reasoning_effort,'') as effort,
 approval_mode as approval, sandbox_policy as sandbox
 from threads`

const threadCols = threadSelect + ` where archived=0 and preview<>''`

const threadQuery = threadCols + ` order by is_pinned desc, recency_at_ms desc limit 400`

// sqlQuote quotes a string literal for the sqlite3 command line.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// listProjectThreads returns every visible thread in one folder, newest first.
func listProjectThreads(db, cwd string) ([]ThreadInfo, error) {
	return queryThreads(db, threadCols+` and cwd=`+sqlQuote(cwd)+` order by recency_at_ms desc limit 500`)
}

func sqliteJSONCLI(db, query string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		// mode=ro sees the WAL. If the WAL is unreadable (some sandboxes hide
		// it), fall back to immutable mode, which only sees checkpointed data.
		uri := "file:" + db + "?mode=ro"
		if attempt == 2 {
			uri = "file:" + db + "?immutable=1"
		}
		out, err = exec.CommandContext(ctx, sqliteBinary(), "-json", "-cmd", ".timeout 500", uri, query).Output()
		if err == nil {
			return out, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, err
}

func listThreads(db string) ([]ThreadInfo, error) { return queryThreads(db, threadQuery) }

func queryThreads(db, query string) ([]ThreadInfo, error) {
	out, err := sqliteJSON(db, query)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Preview  string `json:"preview"`
		Cwd      string `json:"cwd"`
		Source   string `json:"source"`
		Kind     string `json:"kind"`
		Recency  int64  `json:"recency"`
		Rollout  string `json:"rollout"`
		Pinned   int    `json:"pinned"`
		Branch   string `json:"branch"`
		Model    string `json:"model"`
		Effort   string `json:"effort"`
		Approval string `json:"approval"`
		Sandbox  string `json:"sandbox"`
	}
	if len(out) > 0 {
		if err := json.Unmarshal(out, &rows); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	res := make([]ThreadInfo, 0, len(rows))
	for _, r := range rows {
		t := ThreadInfo{ID: r.ID, Title: r.Title, Preview: r.Preview, Cwd: r.Cwd, Source: r.Source, Kind: r.Kind, Recency: r.Recency, Rollout: r.Rollout, Pinned: r.Pinned != 0,
			Branch: r.Branch, Model: r.Model, Effort: r.Effort, Approval: r.Approval}
		t.Sandbox = dbSandbox(r.Sandbox)
		t.Subagent = r.Kind == "subagent" || r.Kind == "guardian_review"
		var source struct {
			Subagent struct {
				Spawn struct {
					Parent string `json:"parent_thread_id"`
					Path   string `json:"agent_path"`
					Name   string `json:"agent_nickname"`
				} `json:"thread_spawn"`
			} `json:"subagent"`
		}
		if json.Unmarshal([]byte(r.Source), &source) == nil {
			t.ParentID, t.AgentPath, t.AgentName = source.Subagent.Spawn.Parent, source.Subagent.Spawn.Path, source.Subagent.Spawn.Name
			t.Subagent = t.Subagent || t.ParentID != ""
		}
		if st, err := os.Stat(r.Rollout); err == nil && now.Sub(st.ModTime()) < 15*time.Minute {
			t.Active = true
			rs := tailStatus(r.Rollout, st)
			t.Running, t.LastMsg, t.LastErr, t.TurnAt = rs.running, rs.lastMsg, rs.lastErr, rs.turnAt
			if t.Subagent {
				t.Activity = rs.activity
			}
		}
		res = append(res, t)
	}
	return res, nil
}

// ---- running state from rollout tails ----

type rolloutStatus struct {
	size     int64
	mod      time.Time
	running  bool
	lastMsg  string
	lastErr  string
	turnAt   int64
	activity string
}

var (
	statusMu    sync.Mutex
	statusCache = map[string]rolloutStatus{}
)

// tailStatus reports whether the last turn in a rollout is still running,
// reading only the last 256 KB and caching by size and mtime.
func tailStatus(path string, st os.FileInfo) rolloutStatus {
	statusMu.Lock()
	c, ok := statusCache[path]
	statusMu.Unlock()
	if ok && c.size == st.Size() && c.mod.Equal(st.ModTime()) {
		return c
	}
	rs := rolloutStatus{size: st.Size(), mod: st.ModTime()}
	f, err := os.Open(path)
	if err != nil {
		return rs
	}
	defer f.Close()
	const n = 256 << 10
	start := st.Size() - n
	if start < 0 {
		start = 0
	}
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return rs
	}
	lines := bytes.Split(buf, []byte("\n"))
	if start > 0 {
		lines = lines[1:]
	}
	for i := len(lines) - 1; i >= -1; i-- {
		var l []byte
		if i < 0 {
			if start == 0 {
				break
			}
			l = previousTurnMarker(f, start)
		} else {
			l = lines[i]
		}
		if rs.activity == "" {
			rs.activity = rolloutActivity(l)
		}
		if !bytes.Contains(l, []byte(`"event_msg"`)) {
			continue
		}
		var kind string
		switch {
		case bytes.Contains(l, []byte(`"type":"task_started"`)):
			kind = "started"
		case bytes.Contains(l, []byte(`"type":"task_complete"`)):
			kind = "complete"
		case bytes.Contains(l, []byte(`"type":"turn_aborted"`)):
			kind = "aborted"
		default:
			continue
		}
		var e struct {
			Payload struct {
				LastAgentMessage string `json:"last_agent_message"`
				StartedAt        int64  `json:"started_at"`
				Error            *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"payload"`
		}
		_ = json.Unmarshal(l, &e)
		rs.running = kind == "started"
		rs.lastMsg = clipPlain(e.Payload.LastAgentMessage, 300)
		rs.turnAt = e.Payload.StartedAt * 1000
		switch {
		case e.Payload.Error != nil:
			rs.lastErr = clipPlain(errorText(e.Payload.Error.Message), 300)
		case kind == "aborted":
			rs.lastErr = "Stopped"
		}
		break
	}
	statusMu.Lock()
	statusCache[path] = rs
	statusMu.Unlock()
	return rs
}

// ---- projects and models ----

type Project struct {
	Cwd     string `json:"cwd"`
	Threads int    `json:"threads"`
	Recency int64  `json:"recency"`
}

func listProjects(db string) ([]Project, error) {
	out, err := sqliteJSON(db, `select cwd, count(*) as threads, max(recency_at_ms) as recency from threads
	 where archived=0 and preview<>'' and coalesce(thread_source,'') not in ('subagent','guardian_review')
	 group by cwd order by recency desc limit 40`)
	if err != nil {
		return nil, err
	}
	var ps []Project
	if len(out) > 0 {
		err = json.Unmarshal(out, &ps)
	}
	return ps, err
}

type Model struct {
	Resolved      string   `json:"resolved,omitempty"`
	Slug          string   `json:"slug"`
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	DefaultEffort string   `json:"defaultEffort"`
	Efforts       []string `json:"efforts"`
}

func listModels() []Model {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".codex", "models_cache.json"))
	if err != nil {
		return nil
	}
	var c struct {
		Models []struct {
			Slug       string `json:"slug"`
			Name       string `json:"display_name"`
			Desc       string `json:"description"`
			Default    string `json:"default_reasoning_level"`
			Visibility string `json:"visibility"`
			Priority   int    `json:"priority"`
			Levels     []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	sort.SliceStable(c.Models, func(i, j int) bool { return c.Models[i].Priority < c.Models[j].Priority })
	var out []Model
	for _, m := range c.Models {
		if m.Visibility != "list" {
			continue
		}
		mm := Model{Slug: m.Slug, Name: m.Name, Description: m.Desc, DefaultEffort: m.Default}
		for _, l := range m.Levels {
			mm.Efforts = append(mm.Efforts, l.Effort)
		}
		out = append(out, mm)
	}
	return out
}

// errorText pulls the human message out of the JSON error bodies Codex records.
func errorText(s string) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(s), &e) == nil {
		if e.Error.Message != "" {
			return e.Error.Message
		}
		if e.Message != "" {
			return e.Message
		}
	}
	return s
}

// findRollout locates a thread's rollout before the database lists it
// (a codex exec run writes the file first).
func findRollout(id string) string {
	m, _ := filepath.Glob(filepath.Join(codexHome(), "sessions", "*", "*", "*", "rollout-*-"+id+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}

// dbSandbox maps the database's sandbox policy to the desktop's names:
// "disabled" is full access, and a managed policy is read-only unless some
// filesystem entry grants writes.
func dbSandbox(raw string) string {
	var sp struct {
		Type       string `json:"type"`
		FileSystem struct {
			Entries []struct {
				Access string `json:"access"`
			} `json:"entries"`
		} `json:"file_system"`
	}
	if json.Unmarshal([]byte(raw), &sp) != nil {
		return ""
	}
	switch sp.Type {
	case "disabled", "danger-full-access", "dangerFullAccess":
		return "dangerFullAccess"
	case "managed":
		for _, e := range sp.FileSystem.Entries {
			if e.Access == "write" || e.Access == "read-write" || e.Access == "readWrite" {
				return "workspaceWrite"
			}
		}
		return "readOnly"
	}
	return sp.Type
}
