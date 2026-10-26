package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const claudeWorkPrefix = "claude-work:"

type claudeWorkEntry struct {
	Info                  ThreadInfo
	Root, Path, Directory string
}
type claudeWorkWatch struct {
	Parent             string
	Window, Generation int
	File               os.FileInfo
	State              string
	Digest             [32]byte
}

func claudeWorkID(root, key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%s%s:%x", claudeWorkPrefix, root, sum[:16])
}
func (m *ClaudeManager) workRoot(parent string) (string, claudeEntry, error) {
	root := parent
	if strings.HasPrefix(parent, claudeWorkPrefix) {
		root = strings.SplitN(strings.TrimPrefix(parent, claudeWorkPrefix), ":", 2)[0]
	}
	m.mu.Lock()
	entry, ok := m.entries[root]
	m.mu.Unlock()
	if !ok {
		return "", claudeEntry{}, errors.New("Claude Session Not Found")
	}
	return root, entry, nil
}
func withinDirectory(root, path string) bool {
	root, e := filepath.EvalSymlinks(root)
	if e != nil {
		return false
	}
	path, e = filepath.EvalSymlinks(path)
	if e != nil {
		return false
	}
	rel, e := filepath.Rel(root, path)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func (m *ClaudeManager) workEntries(parent string) ([]claudeWorkEntry, error) {
	root, entry, err := m.workRoot(parent)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(strings.TrimSuffix(entry.Path, ".jsonl"), "subagents")
	parentPath := entry.Path
	filterDirectory := ""
	if parent != root {
		all, e := m.workEntries(root)
		if e != nil {
			return nil, e
		}
		found := false
		for _, work := range all {
			if work.Info.ID == parent {
				found = true
				parentPath = work.Path
				if work.Info.Kind == "workflow" {
					filterDirectory = work.Directory
				}
				break
			}
		}
		if !found {
			return nil, errors.New("Subagent Does Not Belong To This Chat")
		}
		if parentPath == "" && filterDirectory == "" {
			return []claudeWorkEntry{}, nil
		}
	}
	refs, tools, err := m.workIndex(parentPath)
	if parentPath != "" && err != nil {
		return nil, err
	}
	m.mu.Lock()
	active := m.active[root]
	run := m.runs[root]
	m.mu.Unlock()
	// Background workers continue while their owning session is idle.
	parentRunning := active.ID != "" || active.State != ""
	if run != nil {
		run.mu.Lock()
		parentRunning = run.alive
		run.mu.Unlock()
	}
	var out []claudeWorkEntry
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			if os.IsNotExist(e) {
				return nil
			}
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasPrefix(d.Name(), "agent-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(d.Name(), "agent-"), ".jsonl")
		meta := map[string]any{}
		if b, e := organizationReadFile(strings.TrimSuffix(path, ".jsonl")+".meta.json", 64<<10); e == nil {
			_ = json.Unmarshal(b, &meta)
		}
		tool := str(meta, "toolUseId")
		ref, linked := refs[raw]
		if !linked {
			for _, r := range refs {
				if r.ToolID != "" && r.ToolID == tool {
					ref = r
					linked = true
					break
				}
			}
		}
		if parent != root && filterDirectory == "" && !linked && tools[tool].ToolID == "" {
			return nil
		}
		if filterDirectory != "" && !withinDirectory(filterDirectory, path) {
			return nil
		}
		st, e := d.Info()
		if e != nil {
			return nil
		}
		title := str(meta, "description")
		if title == "" {
			title = ref.Title
		}
		if title == "" {
			title = "Subagent"
		}
		state := ref.State
		if state == "" || (state == "running" && !parentRunning) {
			state = "saved"
		}
		summary := m.workSummary(path)
		if (state == "saved" || state == "running") && summary.Finished && st.ModTime().UnixMilli() >= ref.At {
			state = "completed"
		}
		info := ThreadInfo{ID: claudeWorkID(root, path), Provider: "claude", ParentID: parent, Subagent: true, Title: clipPlain(title, 240), AgentName: clipPlain(title, 240), AgentPath: str(meta, "agentType"), AgentID: raw, AgentToolID: tool, AgentState: state, Kind: "agent", Recency: st.ModTime().UnixMilli(), Cwd: entry.Info.Cwd, Rollout: path}
		info.Running = state == "running"
		info.Activity = summary.Activity
		out = append(out, claudeWorkEntry{Info: info, Root: root, Path: path})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, ref := range refs {
		if ref.Kind != "command" && ref.Kind != "workflow" {
			continue
		}
		if ref.Kind == "workflow" && ref.Directory != "" {
			// Native logs can retain the old project path after a folder is moved.
			canonical := filepath.Join(dir, "workflows", filepath.Base(ref.Directory))
			if withinDirectory(dir, canonical) {
				ref.Directory = canonical
			}
		}
		state := ref.State
		if state == "running" && !parentRunning {
			state = "saved"
		}
		info := ThreadInfo{ID: claudeWorkID(root, "task:"+ref.ID), Provider: "claude", ParentID: parent, Subagent: true, Title: ref.Title, AgentName: ref.Title, AgentID: ref.ID, AgentToolID: ref.ToolID, AgentState: state, Kind: ref.Kind, Cwd: entry.Info.Cwd, Running: state == "running"}
		path := claudeTaskOutput(ref)
		if st, e := os.Stat(path); e == nil {
			info.Recency = st.ModTime().UnixMilli()
		}
		out = append(out, claudeWorkEntry{Info: info, Root: root, Path: path, Directory: ref.Directory})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Info.Running != out[j].Info.Running {
			return out[i].Info.Running
		}
		if out[i].Info.Recency != out[j].Info.Recency {
			return out[i].Info.Recency > out[j].Info.Recency
		}
		return out[i].Info.ID < out[j].Info.ID
	})
	return out, nil
}

func (m *ClaudeManager) workSummary(path string) claudeWorkSummary {
	f, err := os.Open(path)
	if err != nil {
		return claudeWorkSummary{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return claudeWorkSummary{}
	}
	m.workMu.Lock()
	cached, ok := m.workSummaries[path]
	m.workMu.Unlock()
	if ok && os.SameFile(cached.File, st) && cached.File.Size() == st.Size() && cached.File.ModTime().Equal(st.ModTime()) {
		return cached
	}
	summary := claudeWorkSummary{File: st}
	start := st.Size() - (64 << 10)
	if start < 0 {
		start = 0
	}
	b, _ := io.ReadAll(io.NewSectionReader(f, start, st.Size()-start))
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		var r map[string]any
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		message := mapValue(r["message"])
		if str(r, "type") == "user" && claudeText(message["content"]) != "" {
			summary.Finished = false
		}
		if str(r, "type") != "assistant" {
			continue
		}
		summary.Finished = str(message, "stop_reason") == "end_turn"
		if model := str(message, "model"); model != "" {
			summary.Model = model
		}
		for _, block := range arrayValue(message["content"]) {
			v := mapValue(block)
			switch str(v, "type") {
			case "text":
				summary.Activity = clipPlain(str(v, "text"), 180)
			case "thinking":
				summary.Activity = "Thinking"
			case "tool_use":
				input := mapValue(v["input"])
				summary.Activity = str(input, "description")
				if summary.Activity == "" {
					summary.Activity = str(input, "command")
				}
				if summary.Activity == "" {
					summary.Activity = str(v, "name")
				}
				summary.Activity = clipPlain(summary.Activity, 180)
			}
		}
	}
	m.workMu.Lock()
	if m.workSummaries == nil {
		m.workSummaries = map[string]claudeWorkSummary{}
	}
	m.workSummaries[path] = summary
	m.workMu.Unlock()
	return summary
}
func claudeTaskOutput(ref claudeWorkRef) string {
	if !organizationID(ref.ID) {
		return ""
	}
	roots := []string{os.TempDir(), "/tmp"}
	if custom := os.Getenv("CLAUDE_CODE_TMPDIR"); custom != "" {
		roots = append(roots, custom)
	}
	for _, root := range roots {
		matches, _ := filepath.Glob(filepath.Join(root, "claude-*", "*", "tasks", ref.ID+".output"))
		for _, path := range matches {
			if withinDirectory(root, path) {
				return path
			}
		}
		if ref.Output != "" && filepath.Base(ref.Output) == ref.ID+".output" && filepath.Base(filepath.Dir(ref.Output)) == "tasks" && strings.HasPrefix(filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(ref.Output)))), "claude-") && withinDirectory(root, ref.Output) {
			return ref.Output
		}
	}
	return ""
}
func (m *ClaudeManager) listWork(parent string) ([]ThreadInfo, error) {
	entries, err := m.workEntries(parent)
	if err != nil {
		return nil, err
	}
	out := make([]ThreadInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Info)
	}
	return out, nil
}
func (m *ClaudeManager) resolveWork(parent, id string) (claudeWorkEntry, error) {
	entries, err := m.workEntries(parent)
	if err != nil {
		return claudeWorkEntry{}, err
	}
	for _, e := range entries {
		if e.Info.ID == id {
			return e, nil
		}
	}
	return claudeWorkEntry{}, errors.New("Subagent Does Not Belong To This Chat")
}
func (m *ClaudeManager) openWork(c *Client, parent, id string, more bool) error {
	work, err := m.resolveWork(parent, id)
	if err != nil {
		return err
	}
	m.hub.mu.Lock()
	if !m.hub.clients[c] {
		m.hub.mu.Unlock()
		return nil
	}
	m.mu.Lock()
	if m.workWatch == nil {
		m.workWatch = map[*Client]map[string]claudeWorkWatch{}
	}
	if m.workWatch[c] == nil {
		m.workWatch[c] = map[string]claudeWorkWatch{}
	}
	w := m.workWatch[c][id]
	w.Parent = parent
	if w.Window == 0 {
		w.Window = 15
	}
	if more {
		w.Window += 15
	}
	w.Generation++
	w.File = nil
	m.workWatch[c][id] = w
	m.mu.Unlock()
	m.hub.mu.Unlock()
	return m.publishWork(c, work, w, more)
}
func (m *ClaudeManager) moreWork(c *Client, id string, more bool) error {
	m.mu.Lock()
	watch, ok := m.workWatch[c][id]
	m.mu.Unlock()
	if !ok {
		return errors.New("Open This Subagent From Its Parent Chat")
	}
	return m.openWork(c, watch.Parent, id, more)
}
func (m *ClaudeManager) publishWork(c *Client, work claudeWorkEntry, w claudeWorkWatch, more bool) error {
	var st os.FileInfo
	var err error
	if work.Path != "" {
		st, err = organizationSourceInfo(work.Path)
		if err != nil {
			return errors.New("This Background Output Is No Longer Available")
		}
		if !st.Mode().IsRegular() {
			return errors.New("This Background Output Is Unavailable")
		}
	}
	state := work.Info.AgentState + ":" + work.Info.Title
	if w.File != nil && st != nil && os.SameFile(w.File, st) && w.File.Size() == st.Size() && w.File.ModTime().Equal(st.ModTime()) && w.State == state {
		return nil
	}
	meta := Meta{Provider: "claude", ReadOnly: true, Mode: "tail", Title: work.Info.Title, Cwd: work.Info.Cwd, Running: work.Info.Running, AgentState: work.Info.AgentState}
	var turns []Turn
	if work.Info.Kind == "agent" {
		size := int64(2 << 20)
		var view *claudeTranscript
		for {
			view, err = readClaudeTranscriptMode(work.Path, w.Window, size, true)
			if err != nil {
				return err
			}
			if len(view.Turns) >= w.Window || size >= st.Size() {
				break
			}
			size *= 2
		}
		meta.Model = view.Meta.Model
		meta.CtxUsed = view.Meta.CtxUsed
		meta.CtxMax = view.Meta.CtxMax
		meta.HasOlder = view.More
		turns = view.Turns
	} else {
		text := work.Info.Title
		if work.Path != "" {
			f, e := os.Open(work.Path)
			if e != nil {
				return e
			}
			start := st.Size() - int64(w.Window)*(64<<10)
			if start < 0 {
				start = 0
			}
			b, e := io.ReadAll(io.NewSectionReader(f, start, st.Size()-start))
			f.Close()
			if e != nil {
				return e
			}
			text = string(bytes.ToValidUTF8(b, []byte("�")))
			meta.HasOlder = start > 0
		}
		turns = []Turn{{Key: "background-output", Status: work.Info.AgentState, Items: []Item{{ID: "output", K: "cmd", Title: work.Info.Title, Body: text}}}}
	}
	b := mustJSON(map[string]any{"t": "open", "provider": "claude", "id": work.Info.ID, "meta": meta, "turns": turns, "more": more})
	digest := sha256.Sum256(b)
	m.hub.mu.Lock()
	m.mu.Lock()
	current, ok := m.workWatch[c][work.Info.ID]
	if !m.hub.clients[c] || !ok || current.Generation != w.Generation {
		m.mu.Unlock()
		m.hub.mu.Unlock()
		return nil
	}
	changed := current.Digest != digest
	current.Digest = digest
	current.File = st
	current.State = state
	m.workWatch[c][work.Info.ID] = current
	m.mu.Unlock()
	m.hub.mu.Unlock()
	if changed {
		c.push(b)
	}
	return nil
}
func (m *ClaudeManager) pollWork() {
	type subscriber struct {
		client *Client
		id     string
		watch  claudeWorkWatch
	}
	var clients []subscriber
	m.mu.Lock()
	for c, items := range m.workWatch {
		for id, w := range items {
			clients = append(clients, subscriber{c, id, w})
		}
	}
	m.mu.Unlock()
	for _, s := range clients {
		work, err := m.resolveWork(s.watch.Parent, s.id)
		if err == nil {
			err = m.publishWork(s.client, work, s.watch, false)
		}
		if err != nil {
			m.unavailableWork(s.client, s.id, s.watch)
		}
	}
}
func (m *ClaudeManager) unavailableWork(c *Client, id string, w claudeWorkWatch) {
	m.hub.mu.Lock()
	m.mu.Lock()
	current, ok := m.workWatch[c][id]
	if !m.hub.clients[c] || !ok || current.Generation != w.Generation || current.State == "unavailable" {
		m.mu.Unlock()
		m.hub.mu.Unlock()
		return
	}
	current.File = nil
	current.State = "unavailable"
	m.workWatch[c][id] = current
	m.mu.Unlock()
	m.hub.mu.Unlock()
	m.emit(c, map[string]any{"t": "meta", "id": id, "meta": Meta{Provider: "claude", Title: "Background work", ReadOnly: true, Mode: "tail", AgentState: "unavailable"}})
}
func (m *ClaudeManager) followWork(ctx context.Context) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.pollWork()
		}
	}
}
