package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type claudePreference struct {
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Permission string `json:"permission,omitempty"`
	Archived   bool   `json:"archived,omitempty"`
}
type claudeEntry struct {
	Info     ThreadInfo
	Meta     Meta
	Path     string
	Modified time.Time
	Size     int64
}
type claudeActive struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	State     string `json:"state"`
}
type ClaudeManager struct {
	hub                        *Hub
	bin, root, preferencesPath string
	mu                         sync.Mutex
	entries                    map[string]claudeEntry
	runs                       map[string]*ClaudeRun
	watch                      map[*Client]map[string]int
	prefs                      map[string]claudePreference
	models                     []Model
	active                     map[string]claudeActive
	lastList                   []byte
	watchedMod                 map[string]time.Time
	refreshMu                  sync.Mutex
	modelsMu                   sync.Mutex
}

func claudeBinary() string {
	home, _ := os.UserHomeDir()
	for _, path := range []string{filepath.Join(home, ".local/bin/claude"), "/opt/homebrew/bin/claude", "/usr/local/bin/claude"} {
		if st, err := os.Stat(path); err == nil && st.Mode()&0111 != 0 {
			return path
		}
	}
	path, _ := exec.LookPath("claude")
	return path
}
func newClaudeManager(h *Hub) *ClaudeManager {
	home, _ := os.UserHomeDir()
	m := &ClaudeManager{hub: h, bin: claudeBinary(), root: filepath.Join(home, ".claude/projects"), preferencesPath: filepath.Join(home, ".fastcontrol/claude-state.json"), entries: map[string]claudeEntry{}, runs: map[string]*ClaudeRun{}, watch: map[*Client]map[string]int{}, prefs: map[string]claudePreference{}, active: map[string]claudeActive{}, watchedMod: map[string]time.Time{}}
	if b, err := os.ReadFile(m.preferencesPath); err == nil {
		_ = json.Unmarshal(b, &m.prefs)
	}
	return m
}
func (m *ClaudeManager) emit(c *Client, value map[string]any) {
	value["provider"] = "claude"
	c.push(mustJSON(value))
}
func (m *ClaudeManager) broadcast(id string, value map[string]any) {
	m.mu.Lock()
	var clients []*Client
	for c, ids := range m.watch {
		if _, ok := ids[id]; ok {
			clients = append(clients, c)
		}
	}
	m.mu.Unlock()
	value["provider"] = "claude"
	value["id"] = id
	b := mustJSON(value)
	for _, c := range clients {
		c.push(b)
	}
}
func (m *ClaudeManager) event(id, kind, title, text string) {
	b := mustJSON(map[string]any{"t": "event", "provider": "claude", "id": id, "kind": kind, "title": title, "text": text})
	m.hub.mu.Lock()
	var clients []*Client
	for c := range m.hub.clients {
		if c.events {
			clients = append(clients, c)
		}
	}
	m.hub.mu.Unlock()
	for _, c := range clients {
		c.push(b)
	}
}
func (m *ClaudeManager) preference(id string) claudePreference {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prefs[id]
}
func (m *ClaudeManager) setPreference(id string, p claudePreference) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prefs[id] = p
	b, err := json.Marshal(m.prefs)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(m.preferencesPath), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.preferencesPath), ".claude-state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), m.preferencesPath)
}
func (m *ClaudeManager) refresh() {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	paths, _ := filepath.Glob(filepath.Join(m.root, "*", "*.jsonl"))
	next := map[string]claudeEntry{}
	m.mu.Lock()
	old := m.entries
	active := m.active
	m.mu.Unlock()
	for _, path := range paths {
		id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if len(id) != 36 {
			continue
		}
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if cached, ok := old[id]; ok && cached.Modified.Equal(st.ModTime()) && cached.Size == st.Size() {
			next[id] = cached
			continue
		}
		v, err := readClaudeTranscript(path, 1, 128<<10)
		if err != nil {
			continue
		}
		info := ThreadInfo{ID: id, Provider: "claude", Title: clipPlain(v.Meta.Title, 120), Preview: clipPlain(v.First, 160), Cwd: v.Meta.Cwd, Model: v.Meta.Model, Effort: v.Meta.Effort, Recency: st.ModTime().UnixMilli(), Branch: v.Meta.Branch, Source: "claude"}
		if len(v.Turns) > 0 {
			for _, it := range v.Turns[len(v.Turns)-1].Items {
				if it.K == "assistant" {
					info.LastMsg = clipPlain(it.Text, 160)
				}
			}
		}
		next[id] = claudeEntry{Info: info, Meta: v.Meta, Path: path, Modified: st.ModTime(), Size: st.Size()}
	}
	m.mu.Lock()
	m.entries = next
	runs := make(map[string]*ClaudeRun, len(m.runs))
	for id, run := range m.runs {
		runs[id] = run
	}
	m.mu.Unlock()
	list := []ThreadInfo{}
	for id, e := range next {
		if m.preference(id).Archived {
			continue
		}
		info := e.Info
		if a, ok := active[id]; ok {
			info.Running = a.State == "running"
			info.Attention = a.State == "blocked"
			info.Active = true
		}
		if r := runs[id]; r != nil {
			meta, _ := r.snapshot()
			info.Running = meta.Running
			info.Attention = len(meta.Requests) > 0
			info.Live = true
			info.TurnAt = meta.TurnAt
		}
		list = append(list, info)
	}
	for id, r := range runs {
		if _, ok := next[id]; !ok {
			meta, _ := r.snapshot()
			list = append(list, ThreadInfo{ID: id, Provider: "claude", Title: meta.Title, Cwd: meta.Cwd, Model: meta.Model, Effort: meta.Effort, Source: "claude", Live: true, Running: meta.Running, Attention: len(meta.Requests) > 0, Recency: time.Now().UnixMilli()})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Recency > list[j].Recency })
	b := mustJSON(map[string]any{"t": "threads", "provider": "claude", "threads": list})
	m.mu.Lock()
	changed := !bytes.Equal(b, m.lastList)
	m.lastList = b
	m.mu.Unlock()
	if changed {
		m.hub.mu.Lock()
		var clients []*Client
		for c := range m.hub.clients {
			if !c.eventsOnly {
				clients = append(clients, c)
			}
		}
		m.hub.mu.Unlock()
		for _, c := range clients {
			c.push(b)
		}
	}
}
func (m *ClaudeManager) refreshActive() {
	if m.bin == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, m.bin, "agents", "--json").Output()
	if err != nil {
		return
	}
	var values []claudeActive
	if json.Unmarshal(out, &values) != nil {
		return
	}
	active := map[string]claudeActive{}
	for _, a := range values {
		active[a.SessionID] = a
	}
	m.mu.Lock()
	previous := m.active
	m.active = active
	managed := make(map[string]bool, len(m.runs))
	for id := range m.runs {
		managed[id] = true
	}
	entries := m.entries
	m.mu.Unlock()
	for id, next := range active {
		if managed[id] {
			continue
		}
		old, known := previous[id]
		if known && old.State != next.State && next.State == "blocked" {
			m.event(id, "attention", entries[id].Info.Title, "Needs Your Attention")
		}
	}
	for id, old := range previous {
		if managed[id] {
			continue
		}
		next, exists := active[id]
		if (old.State == "running" || old.State == "blocked") && (!exists || next.State == "idle") {
			m.event(id, "completed", entries[id].Info.Title, entries[id].Info.LastMsg)
		}
	}
}
func (m *ClaudeManager) run(ctx context.Context) {
	m.refresh()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastActive := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if time.Since(lastActive) > 5*time.Second {
				m.refreshActive()
				lastActive = time.Now()
			}
			m.refresh()
			m.mu.Lock()
			watch := map[string]int{}
			for _, ids := range m.watch {
				for id, window := range ids {
					if window > watch[id] {
						watch[id] = window
					}
				}
			}
			m.mu.Unlock()
			for id, window := range watch {
				m.mu.Lock()
				r := m.runs[id]
				entry, ok := m.entries[id]
				m.mu.Unlock()
				if r != nil || !ok {
					continue
				}
				if !entry.Modified.Equal(m.watchedMod[id]) {
					m.watchedMod[id] = entry.Modified
					m.mu.Lock()
					var clients []*Client
					for c, ids := range m.watch {
						if _, ok := ids[id]; ok {
							clients = append(clients, c)
						}
					}
					m.mu.Unlock()
					for _, c := range clients {
						_ = m.open(c, id, false)
					}
				}
				_ = window
			}
		}
	}
}
func (m *ClaudeManager) removeClient(c *Client) { m.mu.Lock(); delete(m.watch, c); m.mu.Unlock() }
func (m *ClaudeManager) open(c *Client, id string, more bool) error {
	m.mu.Lock()
	if m.watch[c] == nil {
		m.watch[c] = map[string]int{}
	}
	window := m.watch[c][id]
	if window == 0 {
		window = 15
	}
	if more {
		window += 15
	}
	m.watch[c][id] = window
	run := m.runs[id]
	entry, exists := m.entries[id]
	active := m.active[id]
	m.mu.Unlock()
	if run != nil {
		if more && exists {
			size := int64(2 << 20)
			var history *claudeTranscript
			for {
				var err error
				history, err = readClaudeTranscript(entry.Path, window, size)
				if err != nil {
					return err
				}
				if len(history.Turns) >= window || size >= entry.Size || size >= 64<<20 {
					break
				}
				size *= 2
			}
			run.mu.Lock()
			live := map[string]Turn{}
			for _, turn := range run.view.Turns {
				live[turn.Key] = turn
			}
			for i, turn := range history.Turns {
				if current, ok := live[turn.Key]; ok {
					history.Turns[i] = current
					delete(live, turn.Key)
				}
			}
			for _, turn := range run.view.Turns {
				if _, ok := live[turn.Key]; ok {
					history.Turns = append(history.Turns, turn)
				}
			}
			for k, d := range run.view.Diffs {
				history.Diffs[k] = d
			}
			history.Meta = run.view.Meta
			history.Meta.HasOlder = history.More
			history.reindex()
			run.view = history
			run.mu.Unlock()
		}
		meta, turns := run.snapshot()
		m.emit(c, map[string]any{"t": "open", "id": id, "meta": meta, "turns": turns, "more": more})
		return nil
	}
	if !exists {
		return errors.New("Claude Session Not Found")
	}
	size := int64(2 << 20)
	var v *claudeTranscript
	var err error
	for {
		v, err = readClaudeTranscript(entry.Path, window, size)
		if err != nil {
			return err
		}
		if len(v.Turns) >= window || size >= entry.Size || size >= 64<<20 {
			break
		}
		size *= 2
	}
	v.Meta.Model = entry.Meta.Model
	if active.ID != "" {
		v.Meta.Mode = "external"
		v.Meta.Running = active.State == "running" || active.State == "blocked"
	}
	p := m.preference(id)
	if p.Model != "" {
		v.Meta.Model = p.Model
	}
	if p.Effort != "" {
		v.Meta.Effort = p.Effort
	}
	if p.Permission != "" {
		v.Meta.PermissionMode = p.Permission
	}
	m.emit(c, map[string]any{"t": "open", "id": id, "meta": v.Meta, "turns": v.Turns, "more": more})
	return nil
}
func (m *ClaudeManager) projects(c *Client, cwd string) {
	m.mu.Lock()
	var entries []claudeEntry
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	if cwd != "" {
		threads := []ThreadInfo{}
		for _, e := range entries {
			if e.Info.Cwd == cwd && !m.preference(e.Info.ID).Archived {
				threads = append(threads, e.Info)
			}
		}
		sort.Slice(threads, func(i, j int) bool { return threads[i].Recency > threads[j].Recency })
		m.emit(c, map[string]any{"t": "project", "cwd": cwd, "threads": threads})
		return
	}
	groups := map[string]Project{}
	for _, e := range entries {
		if m.preference(e.Info.ID).Archived {
			continue
		}
		p := groups[e.Info.Cwd]
		p.Cwd = e.Info.Cwd
		p.Threads++
		if e.Info.Recency > p.Recency {
			p.Recency = e.Info.Recency
		}
		groups[p.Cwd] = p
	}
	projects := []Project{}
	for _, p := range groups {
		projects = append(projects, p)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Recency > projects[j].Recency })
	home, _ := os.UserHomeDir()
	m.emit(c, map[string]any{"t": "projects", "projects": projects, "home": home})
}

func (m *ClaudeManager) resolvedModel(value string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, model := range m.models {
		if model.Slug == value && model.Resolved != "" {
			return model.Resolved
		}
	}
	return value
}
