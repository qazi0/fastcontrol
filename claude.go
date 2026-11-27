package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const defaultClaudeRecencyBroadcastInterval = 15 * time.Second

type claudePreference struct {
	Title      string `json:"title,omitempty"`
	ParentID   string `json:"parentId,omitempty"`
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
	ID              string `json:"id,omitempty"`
	PID             int    `json:"pid,omitempty"`
	SessionID       string `json:"sessionId,omitempty"`
	HostSessionID   string `json:"hostSessionId,omitempty"`
	Cwd             string `json:"cwd,omitempty"`
	Kind            string `json:"kind,omitempty"`
	Name            string `json:"name,omitempty"`
	State           string `json:"state,omitempty"`
	Status          string `json:"status,omitempty"`
	StartedAt       int64  `json:"startedAt,omitempty"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt,omitempty"`
	UpdatedAt       int64  `json:"updatedAt,omitempty"`
}

func (a *claudeActive) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID              string          `json:"id"`
		PID             json.RawMessage `json:"pid"`
		SessionID       string          `json:"sessionId"`
		HostSessionID   string          `json:"hostSessionId"`
		Cwd             string          `json:"cwd"`
		Kind            string          `json:"kind"`
		Name            string          `json:"name"`
		State           string          `json:"state"`
		Status          string          `json:"status"`
		StartedAt       json.RawMessage `json:"startedAt"`
		StatusUpdatedAt json.RawMessage `json:"statusUpdatedAt"`
		UpdatedAt       json.RawMessage `json:"updatedAt"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*a = claudeActive{
		ID:              strings.TrimSpace(raw.ID),
		PID:             parseClaudePID(raw.PID),
		SessionID:       strings.TrimSpace(raw.SessionID),
		HostSessionID:   strings.TrimSpace(raw.HostSessionID),
		Cwd:             strings.TrimSpace(raw.Cwd),
		Kind:            strings.TrimSpace(raw.Kind),
		Name:            strings.TrimSpace(raw.Name),
		State:           normalizeClaudeState(raw.State),
		Status:          strings.TrimSpace(raw.Status),
		StartedAt:       parseClaudeMillis(raw.StartedAt),
		StatusUpdatedAt: parseClaudeMillis(raw.StatusUpdatedAt),
		UpdatedAt:       parseClaudeMillis(raw.UpdatedAt),
	}
	if status := normalizeClaudeState(raw.Status); status != "" {
		a.State = status
	}
	if a.ID == "" {
		a.ID = a.derivedOwnerID()
	}
	return nil
}

func normalizeClaudeState(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "":
		return ""
	case "running", "busy":
		return "running"
	case "blocked", "waiting", "needs_attention", "needs-attention", "permission":
		return "blocked"
	case "idle":
		return "idle"
	default:
		return s
	}
}

func parseClaudePID(raw json.RawMessage) int {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0
	}
	var n int
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return n
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil && f > 0 && f == float64(int(f)) {
		return int(f)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if parsed, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}

func normalizeClaudeMillis(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if n >= 1_000_000_000 && n < 100_000_000_000 {
		return n * 1000
	}
	return n
}

func parseClaudeMillis(raw json.RawMessage) int64 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return normalizeClaudeMillis(n)
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil && f > 0 {
		return normalizeClaudeMillis(int64(f))
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.TrimSpace(s)
		if parsed, err := strconv.ParseInt(s, 10, 64); err == nil && parsed > 0 {
			return normalizeClaudeMillis(parsed)
		}
		if ts := claudeTime(s); ts > 0 {
			return ts
		}
	}
	return 0
}

func (a claudeActive) derivedOwnerID() string {
	if a.PID > 0 {
		return "pid:" + strconv.Itoa(a.PID)
	}
	if a.HostSessionID != "" {
		return "host:" + a.HostSessionID
	}
	if a.SessionID != "" {
		return "session:" + a.SessionID
	}
	return ""
}

func (a claudeActive) ownerID() string {
	if id := strings.TrimSpace(a.ID); id != "" {
		return id
	}
	return a.derivedOwnerID()
}

func (a claudeActive) backgroundID() string {
	id := strings.TrimSpace(a.ID)
	if id == "" || strings.HasPrefix(id, "pid:") || strings.HasPrefix(id, "host:") || strings.HasPrefix(id, "session:") {
		return ""
	}
	return id
}

func (a claudeActive) normalizedState() string {
	if s := normalizeClaudeState(a.State); s != "" {
		return s
	}
	return normalizeClaudeState(a.Status)
}

func (a claudeActive) present() bool {
	return a.ownerID() != "" || a.normalizedState() != ""
}

func (a claudeActive) isRunning() bool {
	return a.normalizedState() == "running"
}

func (a claudeActive) isAttention() bool {
	return a.normalizedState() == "blocked"
}

func (a claudeActive) isBusy() bool {
	s := a.normalizedState()
	return s == "running" || s == "blocked"
}

func (a claudeActive) effectiveTurnAt() int64 {
	if !a.isBusy() {
		return 0
	}
	if a.StatusUpdatedAt > 0 {
		return a.StatusUpdatedAt
	}
	if a.StartedAt > 0 {
		return a.StartedAt
	}
	return a.UpdatedAt
}

func (a claudeActive) recencyStamp() int64 {
	best := a.StatusUpdatedAt
	if a.UpdatedAt > best {
		best = a.UpdatedAt
	}
	if a.StartedAt > best {
		best = a.StartedAt
	}
	return best
}

func (a claudeActive) watchKey() string {
	if !a.present() {
		return ""
	}
	return a.ownerID() + ":" + a.normalizedState() + ":" + strconv.FormatInt(a.effectiveTurnAt(), 10)
}

func mergeClaudeActiveSession(base, session claudeActive) claudeActive {
	if session.SessionID != "" && base.SessionID != "" && session.SessionID != base.SessionID {
		return base
	}
	merged := base
	if merged.SessionID == "" {
		merged.SessionID = session.SessionID
	}
	if merged.PID == 0 {
		merged.PID = session.PID
	}
	if merged.HostSessionID == "" {
		merged.HostSessionID = session.HostSessionID
	}
	if merged.Cwd == "" {
		merged.Cwd = session.Cwd
	}
	if merged.Kind == "" {
		merged.Kind = session.Kind
	}
	if merged.Name == "" {
		merged.Name = session.Name
	}
	if merged.StartedAt == 0 {
		merged.StartedAt = session.StartedAt
	}
	if session.UpdatedAt > merged.UpdatedAt {
		merged.UpdatedAt = session.UpdatedAt
	}
	baseState := merged.normalizedState()
	sessionState := session.normalizedState()
	if baseState == "" && sessionState != "" {
		merged.State = sessionState
		if merged.Status == "" {
			merged.Status = session.Status
		}
		if session.StatusUpdatedAt > merged.StatusUpdatedAt {
			merged.StatusUpdatedAt = session.StatusUpdatedAt
		}
	} else if sessionState == "" || sessionState == baseState {
		if session.StatusUpdatedAt > merged.StatusUpdatedAt {
			merged.StatusUpdatedAt = session.StatusUpdatedAt
		}
	}
	if merged.ID == "" {
		merged.ID = merged.derivedOwnerID()
	}
	return merged
}

func preferClaudeActive(existing, candidate claudeActive) claudeActive {
	if !existing.present() {
		return candidate
	}
	if !candidate.present() {
		return existing
	}
	if existing.PID > 0 && existing.PID == candidate.PID {
		return mergeClaudeActiveSession(existing, candidate)
	}
	if candidate.isBusy() != existing.isBusy() {
		if candidate.isBusy() {
			return candidate
		}
		return existing
	}
	if candidate.recencyStamp() > existing.recencyStamp() {
		return candidate
	}
	return existing
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return platformProcessAlive(pid)
}

func signalExternalPID(pid int) error {
	if pid <= 0 {
		return errors.New("Invalid Process ID")
	}
	if pid == os.Getpid() {
		return errors.New("Refusing To Stop Relay Process")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		defer p.Release()
		return p.Kill()
	}
	err = p.Signal(syscall.SIGTERM)
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func claudeListSignature(list []ThreadInfo, groups []OrganizationGroup) [32]byte {
	stripped := make([]ThreadInfo, len(list))
	extra := make([]string, len(list))
	for i, t := range list {
		t.Recency = 0
		stripped[i] = t
		if t.Model != "" || t.Effort != "" {
			extra[i] = t.Model + "\x00" + t.Effort
		}
	}
	return sha256.Sum256(mustJSON(struct {
		Threads []ThreadInfo        `json:"threads"`
		Groups  []OrganizationGroup `json:"groups"`
		Extra   []string            `json:"extra"`
	}{Threads: stripped, Groups: groups, Extra: extra}))
}

type ClaudeManager struct {
	hub                        *Hub
	bin, root, preferencesPath string
	sessionsDir                string
	pidAlive                   func(int) bool
	stopExternalFn             func(context.Context, claudeActive) error
	now                        func() time.Time
	recencyBroadcastInterval   time.Duration
	mu                         sync.Mutex
	entries                    map[string]claudeEntry
	runs                       map[string]*ClaudeRun
	watch                      map[*Client]map[string]int
	prefs                      map[string]claudePreference
	prefGates                  map[string]*claudePreferenceGate // guarded by mu
	models                     []Model
	modelsAt                   time.Time // model catalog publication time, guarded by mu
	active                     map[string]claudeActive
	lastList                   []byte
	lastBroadcast              []byte
	lastBroadcastSig           [32]byte
	lastBroadcastAt            time.Time
	orgState                   OrganizationSnapshot // immutable after publication under mu
	watchedMod                 map[string]time.Time
	watchedActive              map[string]string
	refreshMu                  sync.Mutex
	activeMu                   sync.Mutex
	refreshJob                 atomic.Bool
	modelsMu                   sync.Mutex
	workMu                     sync.Mutex // index lookup only; each index owns its append cursor
	workIndexes                map[string]*claudeWorkIndex
	workSummaries              map[string]claudeWorkSummary           // immutable values guarded by workMu
	workWatch                  map[*Client]map[string]claudeWorkWatch // guarded by mu
}

func (m *ClaudeManager) nowTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *ClaudeManager) recencyThrottle() time.Duration {
	if m.recencyBroadcastInterval > 0 {
		return m.recencyBroadcastInterval
	}
	return defaultClaudeRecencyBroadcastInterval
}

func (m *ClaudeManager) isPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if m.pidAlive != nil {
		return m.pidAlive(pid)
	}
	return processAlive(pid)
}

func (m *ClaudeManager) claudeSessionsDir() string {
	if m.sessionsDir != "" {
		return m.sessionsDir
	}
	if m.root != "" {
		return filepath.Join(filepath.Dir(m.root), "sessions")
	}
	return ""
}

func claudeBinary() string {
	home, _ := os.UserHomeDir()
	for _, path := range []string{filepath.Join(home, ".local", "bin", "claude.exe"), filepath.Join(home, ".local/bin/claude"), "/opt/homebrew/bin/claude", "/usr/local/bin/claude"} {
		if st, err := os.Stat(path); err == nil && (runtime.GOOS == "windows" || st.Mode()&0111 != 0) {
			return path
		}
	}
	path, _ := exec.LookPath("claude")
	return path
}
func newClaudeManager(h *Hub) *ClaudeManager {
	home, _ := os.UserHomeDir()
	m := &ClaudeManager{
		hub:                      h,
		bin:                      claudeBinary(),
		root:                     filepath.Join(home, ".claude/projects"),
		sessionsDir:              filepath.Join(home, ".claude/sessions"),
		preferencesPath:          filepath.Join(home, ".fastcontrol/claude-state.json"),
		entries:                  map[string]claudeEntry{},
		runs:                     map[string]*ClaudeRun{},
		watch:                    map[*Client]map[string]int{},
		prefs:                    map[string]claudePreference{},
		active:                   map[string]claudeActive{},
		watchedMod:               map[string]time.Time{},
		watchedActive:            map[string]string{},
		recencyBroadcastInterval: defaultClaudeRecencyBroadcastInterval,
	}
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
		if v.ParentID != "" {
			info.ParentID, info.Relation = v.ParentID, "fork"
		}
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
		if a, ok := active[id]; ok && a.present() {
			info.Running = a.isRunning()
			info.Attention = a.isAttention()
			info.Active = true
			if info.Running || info.Attention {
				info.TurnAt = a.effectiveTurnAt()
			}
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
	now := m.nowTime()
	for id, r := range runs {
		if _, ok := next[id]; !ok {
			meta, _ := r.snapshot()
			recency := meta.TurnAt
			if recency == 0 {
				recency = now.UnixMilli()
			}
			list = append(list, ThreadInfo{ID: id, Provider: "claude", Title: meta.Title, Cwd: meta.Cwd, Model: meta.Model, Effort: meta.Effort, Source: "claude", Live: true, Running: meta.Running, Attention: len(meta.Requests) > 0, TurnAt: meta.TurnAt, Recency: recency})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Recency != list[j].Recency {
			return list[i].Recency > list[j].Recency
		}
		return list[i].ID < list[j].ID
	})
	organization := emptyOrganization()
	if m.hub.organization != nil && len(list) > 0 {
		ids := make([]string, len(list))
		for i := range list {
			ids[i] = list[i].ID
		}
		organization, _ = m.hub.organization.Claude(ids)
	}
	for _, t := range list {
		pref := m.preference(t.ID)
		p, exists := organization.Placements[t.ID]
		if pref.Title != "" {
			p.DisplayTitle = pref.Title
		}
		if pref.ParentID != "" && !p.Detached {
			p.ParentID, p.Relation = pref.ParentID, "fork"
		} else if p.ParentID == "" && !p.Detached && t.Relation == "fork" {
			p.ParentID, p.Relation = t.ParentID, "fork"
		}
		if exists || pref.Title != "" || pref.ParentID != "" || p.ParentID != "" {
			organization.Placements[t.ID] = p
		}
	}
	list = applyThreadOrganization(list, organization)
	b := mustJSON(map[string]any{"t": "threads", "provider": "claude", "threads": list, "groups": organization.Groups})
	throttle := m.recencyThrottle()
	m.mu.Lock()
	m.lastList = b
	m.orgState = organization
	shouldBroadcast := false
	if len(m.lastBroadcast) == 0 {
		shouldBroadcast = true
		m.lastBroadcastSig = claudeListSignature(list, organization.Groups)
	} else if !bytes.Equal(b, m.lastBroadcast) {
		sig := claudeListSignature(list, organization.Groups)
		if sig != m.lastBroadcastSig || now.Sub(m.lastBroadcastAt) >= throttle {
			shouldBroadcast = true
			m.lastBroadcastSig = sig
		}
	}
	if shouldBroadcast {
		m.lastBroadcast = b
		m.lastBroadcastAt = now
	}
	m.mu.Unlock()
	if shouldBroadcast {
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

func (m *ClaudeManager) readClaudeSessionFiles() (map[int]claudeActive, bool) {
	dir := m.claudeSessionsDir()
	if dir == "" {
		return nil, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	byPID := map[int]claudeActive{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		pidText := strings.TrimSuffix(name, ".json")
		if pidText == "" {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 0 || strconv.Itoa(pid) != pidText {
			continue
		}
		if !m.isPIDAlive(pid) {
			continue
		}
		path := filepath.Join(dir, name)
		st, err := os.Lstat(path)
		if err != nil || !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > 64<<10 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var sf claudeActive
		if json.Unmarshal(data, &sf) != nil {
			continue
		}
		if sf.PID == 0 {
			sf.PID = pid
		}
		if sf.PID != pid {
			continue
		}
		if sf.ID == "" {
			sf.ID = sf.derivedOwnerID()
		}
		if sf.SessionID == "" {
			continue
		}
		byPID[pid] = sf
	}
	return byPID, true
}

func (m *ClaudeManager) refreshActive() {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	var values []claudeActive
	agentsOK := false
	if m.bin != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		out, err := exec.CommandContext(ctx, m.bin, "agents", "--json").Output()
		cancel()
		if err == nil && json.Unmarshal(out, &values) == nil {
			agentsOK = true
		}
	}
	sessionByPID, sessionsOK := m.readClaudeSessionFiles()
	if !agentsOK && !sessionsOK {
		return
	}
	active := map[string]claudeActive{}
	if agentsOK {
		for _, a := range values {
			if a.SessionID == "" {
				continue
			}
			if a.PID > 0 {
				if !m.isPIDAlive(a.PID) {
					continue
				}
				if sf, ok := sessionByPID[a.PID]; ok {
					a = mergeClaudeActiveSession(a, sf)
				}
			}
			if a.ID == "" {
				a.ID = a.derivedOwnerID()
			}
			if !a.present() {
				continue
			}
			active[a.SessionID] = preferClaudeActive(active[a.SessionID], a)
		}
	}
	for _, sf := range sessionByPID {
		if sf.SessionID == "" || !sf.present() {
			continue
		}
		if existing, ok := active[sf.SessionID]; ok {
			if existing.PID == sf.PID {
				active[sf.SessionID] = mergeClaudeActiveSession(existing, sf)
			} else {
				active[sf.SessionID] = preferClaudeActive(existing, sf)
			}
		} else {
			active[sf.SessionID] = sf
		}
	}
	m.mu.Lock()
	previous := m.active
	if !agentsOK {
		for id, old := range previous {
			if !old.present() {
				continue
			}
			if _, ok := active[id]; ok {
				continue
			}
			if old.PID == 0 || m.isPIDAlive(old.PID) {
				active[id] = old
			}
		}
	}
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
		if known && !old.isAttention() && next.isAttention() {
			m.event(id, "attention", entries[id].Info.Title, "Needs Your Attention")
		}
	}
	for id, old := range previous {
		if managed[id] {
			continue
		}
		next, exists := active[id]
		if old.isBusy() && (!exists || !next.isBusy()) {
			m.event(id, "completed", entries[id].Info.Title, entries[id].Info.LastMsg)
		}
	}
}
func (m *ClaudeManager) run(ctx context.Context) {
	go m.followWork(ctx)
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
				active := m.active[id]
				m.mu.Unlock()
				if r != nil || !ok {
					continue
				}
				activeKey := active.watchKey()
				if !entry.Modified.Equal(m.watchedMod[id]) || activeKey != m.watchedActive[id] {
					m.watchedMod[id] = entry.Modified
					if m.watchedActive == nil {
						m.watchedActive = map[string]string{}
					}
					m.watchedActive[id] = activeKey
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
func (m *ClaudeManager) removeClient(c *Client) {
	m.mu.Lock()
	delete(m.watch, c)
	delete(m.workWatch, c)
	m.mu.Unlock()
}
func (m *ClaudeManager) open(c *Client, id string, more bool) error {
	m.hub.mu.Lock()
	if !m.hub.clients[c] {
		m.hub.mu.Unlock()
		return nil
	}
	m.mu.Lock()
	run := m.runs[id]
	entry, exists := m.entries[id]
	if run == nil && !exists {
		m.mu.Unlock()
		m.hub.mu.Unlock()
		return errors.New("Claude Session Not Found")
	}
	if m.watch == nil {
		m.watch = map[*Client]map[string]int{}
	}
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
	active := m.active[id]
	m.mu.Unlock()
	m.hub.mu.Unlock()
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
	if active.present() {
		v.Meta.Mode = "external"
		v.Meta.Running = active.isBusy()
		if turnAt := active.effectiveTurnAt(); turnAt > 0 {
			v.Meta.TurnAt = turnAt
		}
	}
	p := m.preference(id)
	if p.Model != "" {
		if resolved := m.resolvedModel(p.Model); strings.HasPrefix(resolved, "claude-") {
			v.Meta.Model = resolved
		}
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
