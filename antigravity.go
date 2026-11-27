package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAntigravityRecencyBroadcastInterval = 15 * time.Second
	defaultAntigravityIdleSessionTimeout       = 12 * time.Minute
	defaultAntigravityModelsCacheTTL           = 5 * time.Minute
	defaultAntigravityPollInterval             = time.Second
	defaultAntigravityWorkPollInterval         = 500 * time.Millisecond
	defaultAntigravityFlushInterval            = 60 * time.Millisecond
	defaultAntigravityCallTimeout              = 30 * time.Second
	antigravityDrainBarrierKind                = "__fc_drain_barrier"
)

type antigravityGate struct {
	mu    sync.Mutex
	users int
}

type antigravityWatchStamp struct {
	mod     time.Time
	size    int64
	running bool
	title   string
	status  string
}

type antigravityWorkWatch struct {
	Gate       *sync.Mutex
	Parent     string
	Window     int
	Generation int
	Mod        time.Time
	Size       int64
	State      string
	Digest     [32]byte
}

type antigravityModelCatalog struct {
	Models        []Model
	Modes         []acpModeInfo
	DefaultModel  string
	DefaultEffort string
	DefaultMode   string
	Tier          *acpTierInfo
	UpdatedAt     time.Time
}

type antigravityPendingPerm struct {
	acpReq acpPermissionRequest
	req    Request
	at     time.Time
}

type antigravityPendingElicit struct {
	acpReq acpElicitationRequest
	req    Request
	at     time.Time
}

type antigravityListState struct {
	alive     bool
	running   bool
	attention bool
	turnAt    int64
	recency   int64
	title     string
	cwd       string
	model     string
	effort    string
	mode      string
	lastMsg   string
	lastErr   string
}

func antigravityListSignature(list []ThreadInfo, groups []OrganizationGroup) [32]byte {
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

func antigravityHasDB(geminiDir string) bool {
	if strings.TrimSpace(geminiDir) == "" {
		geminiDir = antigravityGeminiDir("")
	}
	for _, appDir := range antigravityDefaultAppDirs() {
		if st, err := os.Stat(antigravityDBPath(geminiDir, appDir)); err == nil && st.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// AntigravityManager indexes local Antigravity conversations, manages live ACP
// runs, streams updates to subscribed clients, and handles provider operations.
type AntigravityManager struct {
	hub                      *Hub
	bin                      string
	geminiDir                string
	customGeminiDir          bool
	state                    *antigravityState
	allowFullAccess          bool
	supportsACP              func(string) bool
	dialACP                  func(context.Context, acpClientConfig) (*acpClient, *acpInitResult, error)
	now                      func() time.Time
	recencyBroadcastInterval time.Duration
	idleSessionTimeout       time.Duration
	modelsCacheTTL           time.Duration
	pollInterval             time.Duration
	workPollInterval         time.Duration

	mu               sync.Mutex
	records          map[string]antigravityRecord
	recordSlice      []antigravityRecord
	runs             map[string]*antigravityRun
	watch            map[*Client]map[string]int
	workWatch        map[*Client]map[string]antigravityWorkWatch
	watchedStamp     map[string]antigravityWatchStamp
	gates            map[string]*antigravityGate
	lastList         []byte
	lastBroadcast    []byte
	lastBroadcastSig [32]byte
	lastBroadcastAt  time.Time
	orgState         OrganizationSnapshot
	prevStates       map[string]threadState
	catalog          antigravityModelCatalog
	shuttingDown     bool

	refreshMu  sync.Mutex
	refreshJob atomic.Bool
	modelsMu   sync.Mutex
}

func newAntigravityManager(h *Hub) *AntigravityManager {
	state, err := newAntigravityState(defaultAntigravityStatePath())
	if err != nil {
		state = &antigravityState{
			path: defaultAntigravityStatePath(),
			data: emptyAntigravityStateData(),
		}
	}
	return &AntigravityManager{
		hub:                      h,
		bin:                      agyBinary(),
		geminiDir:                antigravityGeminiDir(""),
		state:                    state,
		supportsACP:              agySupportsACP,
		dialACP:                  dialACPClient,
		recencyBroadcastInterval: defaultAntigravityRecencyBroadcastInterval,
		idleSessionTimeout:       defaultAntigravityIdleSessionTimeout,
		modelsCacheTTL:           defaultAntigravityModelsCacheTTL,
		pollInterval:             defaultAntigravityPollInterval,
		workPollInterval:         defaultAntigravityWorkPollInterval,
		records:                  map[string]antigravityRecord{},
		runs:                     map[string]*antigravityRun{},
		watch:                    map[*Client]map[string]int{},
		workWatch:                map[*Client]map[string]antigravityWorkWatch{},
		watchedStamp:             map[string]antigravityWatchStamp{},
		gates:                    map[string]*antigravityGate{},
		orgState:                 emptyOrganization(),
	}
}

func (m *AntigravityManager) nowTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *AntigravityManager) geminiRoot() string {
	if strings.TrimSpace(m.geminiDir) != "" {
		return m.geminiDir
	}
	return antigravityGeminiDir("")
}

func (m *AntigravityManager) recencyThrottle() time.Duration {
	if m.recencyBroadcastInterval > 0 {
		return m.recencyBroadcastInterval
	}
	return defaultAntigravityRecencyBroadcastInterval
}

func (m *AntigravityManager) idleTimeout() time.Duration {
	if m.idleSessionTimeout > 0 {
		return m.idleSessionTimeout
	}
	return defaultAntigravityIdleSessionTimeout
}

func (m *AntigravityManager) modelsTTL() time.Duration {
	if m.modelsCacheTTL > 0 {
		return m.modelsCacheTTL
	}
	return defaultAntigravityModelsCacheTTL
}

func (m *AntigravityManager) checkSupportsACP(bin string) bool {
	if strings.TrimSpace(bin) == "" {
		return false
	}
	if m.supportsACP != nil {
		return m.supportsACP(bin)
	}
	return agySupportsACP(bin)
}

func (m *AntigravityManager) acpAvailable() bool {
	return m.bin != "" && m.checkSupportsACP(m.bin)
}

func (m *AntigravityManager) available() bool {
	if m.acpAvailable() {
		return true
	}
	m.mu.Lock()
	hasCached := len(m.records) > 0 || len(m.runs) > 0
	m.mu.Unlock()
	if hasCached {
		return true
	}
	return antigravityHasDB(m.geminiRoot())
}

func (m *AntigravityManager) requireACP() error {
	if m.bin == "" {
		return errors.New("Install Antigravity CLI (agy) On This Computer")
	}
	if !m.checkSupportsACP(m.bin) {
		return errors.New("Update Antigravity CLI (agy) To A Version With ACP Support")
	}
	return nil
}

func (m *AntigravityManager) dialClient(ctx context.Context, cfg acpClientConfig) (*acpClient, *acpInitResult, error) {
	if cfg.Binary == "" {
		cfg.Binary = m.bin
	}
	if cfg.AppDataDir == "" {
		cfg.AppDataDir = acpDefaultAppDataDir
	}
	if cfg.GeminiDir == "" && (m.customGeminiDir || (m.geminiDir != "" && m.geminiDir != antigravityGeminiDir(""))) {
		cfg.GeminiDir = m.geminiDir
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = defaultAntigravityCallTimeout
	}
	if m.dialACP != nil {
		return m.dialACP(ctx, cfg)
	}
	return dialACPClient(ctx, cfg)
}

func (m *AntigravityManager) lockThread(id string) func() {
	m.mu.Lock()
	if m.gates == nil {
		m.gates = map[string]*antigravityGate{}
	}
	g := m.gates[id]
	if g == nil {
		g = &antigravityGate{}
		m.gates[id] = g
	}
	g.users++
	m.mu.Unlock()
	g.mu.Lock()
	return func() {
		g.mu.Unlock()
		m.mu.Lock()
		g.users--
		if g.users == 0 {
			delete(m.gates, id)
		}
		m.mu.Unlock()
	}
}

func (m *AntigravityManager) emit(c *Client, value map[string]any) {
	value["provider"] = "antigravity"
	c.push(mustJSON(value))
}

func (m *AntigravityManager) broadcast(id string, value map[string]any) {
	m.mu.Lock()
	var clients []*Client
	for c, ids := range m.watch {
		if _, ok := ids[id]; ok {
			clients = append(clients, c)
		}
	}
	m.mu.Unlock()
	value["provider"] = "antigravity"
	value["id"] = id
	b := mustJSON(value)
	for _, c := range clients {
		c.push(b)
	}
}

func (m *AntigravityManager) event(id, kind, title, text, cwd string) {
	if m.hub == nil {
		return
	}
	payload := map[string]any{
		"t":        "event",
		"provider": "antigravity",
		"id":       id,
		"kind":     kind,
		"title":    title,
		"text":     text,
	}
	if cwd != "" {
		payload["cwd"] = cwd
	}
	b := mustJSON(payload)
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

func (m *AntigravityManager) removeClient(c *Client) {
	m.mu.Lock()
	delete(m.watch, c)
	delete(m.workWatch, c)
	m.mu.Unlock()
}

func (m *AntigravityManager) requestRefresh() {
	if !m.refreshJob.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer m.refreshJob.Store(false)
		m.refresh()
	}()
}

func (m *AntigravityManager) snapshotRecords() []antigravityRecord {
	m.mu.Lock()
	out := append([]antigravityRecord(nil), m.recordSlice...)
	m.mu.Unlock()
	return out
}

func (m *AntigravityManager) isRunActive(id string) bool {
	m.mu.Lock()
	run := m.runs[id]
	m.mu.Unlock()
	return run != nil && run.isRunning()
}

func (m *AntigravityManager) effectiveRecordsLocked(dbRecords []antigravityRecord, runStates map[string]antigravityListState, now time.Time) []antigravityRecord {
	byID := make(map[string]antigravityRecord, len(dbRecords)+len(runStates))
	out := make([]antigravityRecord, 0, len(dbRecords)+len(runStates))
	for _, rec := range dbRecords {
		if st, ok := runStates[rec.ID]; ok {
			rec.Running = st.running
			if st.recency > rec.Recency {
				rec.Recency = st.recency
			}
			if rec.Cwd == "" && st.cwd != "" {
				rec.Cwd = st.cwd
			}
			if st.title != "" && rec.Title == "" {
				rec.Title = st.title
			}
		}
		byID[rec.ID] = rec
		out = append(out, rec)
	}
	for id, st := range runStates {
		if _, ok := byID[id]; ok {
			continue
		}
		recency := st.recency
		if recency == 0 {
			recency = now.UnixMilli()
		}
		rec := antigravityRecord{
			ID:         id,
			Title:      antigravityFirstNonEmpty(st.title, "New Chat"),
			Cwd:        st.cwd,
			Source:     "antigravity",
			AppDataDir: acpDefaultAppDataDir,
			Recency:    recency,
			Updated:    recency / 1000,
			Running:    st.running,
		}
		out = append(out, rec)
	}
	return out
}

func (m *AntigravityManager) refresh() {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	dbRecords, err := antigravityListRecords(ctx, m.geminiRoot(), m.isRunActive)
	cancel()
	if err != nil {
		return
	}

	m.mu.Lock()
	runs := make(map[string]*antigravityRun, len(m.runs))
	for id, run := range m.runs {
		runs[id] = run
	}
	m.mu.Unlock()

	runStates := make(map[string]antigravityListState, len(runs))
	for id, run := range runs {
		runStates[id] = run.listState()
	}

	now := m.nowTime()
	merged := m.effectiveRecordsLocked(dbRecords, runStates, now)

	recMap := make(map[string]antigravityRecord, len(merged))
	for _, rec := range merged {
		recMap[rec.ID] = rec
	}

	list, orgSnap := antigravityBuildOrganization(false, m.state, merged)
	for i := range list {
		t := &list[i]
		if st, ok := runStates[t.ID]; ok {
			t.Live = st.alive
			t.Running = st.running
			t.Attention = st.attention
			if st.alive || st.running {
				t.Active = true
			}
			if st.turnAt > 0 {
				t.TurnAt = st.turnAt
			}
			if st.model != "" {
				t.Model = st.model
			}
			if st.effort != "" {
				t.Effort = st.effort
			}
			if st.lastMsg != "" {
				t.LastMsg = st.lastMsg
			}
			if st.lastErr != "" {
				t.LastErr = st.lastErr
			}
			if st.title != "" && t.DisplayTitle == "" {
				t.Title = clipPlain(st.title, 240)
			}
		}
	}

	b := mustJSON(map[string]any{
		"t":        "threads",
		"provider": "antigravity",
		"threads":  list,
		"groups":   orgSnap.Groups,
	})
	throttle := m.recencyThrottle()

	type pendingEvent struct {
		id, kind, title, text, cwd string
	}
	var externalEvents []pendingEvent

	m.mu.Lock()
	m.records = recMap
	m.recordSlice = merged
	m.lastList = b
	m.orgState = orgSnap

	// Track state transitions for non-managed (external) Antigravity chats.
	nextPrev := make(map[string]threadState, len(list))
	for _, t := range list {
		if t.Subagent {
			continue
		}
		st := threadState{running: t.Running, attention: t.Attention}
		nextPrev[t.ID] = st
		if _, managed := runStates[t.ID]; managed {
			continue
		}
		if m.prevStates != nil {
			old, seen := m.prevStates[t.ID]
			title := antigravityFirstNonEmpty(t.DisplayTitle, t.Title, t.Preview)
			switch {
			case st.running && (!seen || !old.running):
				externalEvents = append(externalEvents, pendingEvent{id: t.ID, kind: "started", title: title, cwd: t.Cwd})
			case !st.running && seen && old.running:
				externalEvents = append(externalEvents, pendingEvent{id: t.ID, kind: "completed", title: title, text: t.LastMsg, cwd: t.Cwd})
			}
		}
	}
	m.prevStates = nextPrev

	shouldBroadcast := false
	if len(m.lastBroadcast) == 0 {
		shouldBroadcast = true
		m.lastBroadcastSig = antigravityListSignature(list, orgSnap.Groups)
	} else if !bytes.Equal(b, m.lastBroadcast) {
		sig := antigravityListSignature(list, orgSnap.Groups)
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

	if shouldBroadcast && m.hub != nil {
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
	for _, ev := range externalEvents {
		m.event(ev.id, ev.kind, ev.title, ev.text, ev.cwd)
	}
}

func (m *AntigravityManager) run(ctx context.Context) {
	go m.followWork(ctx)
	m.refresh()

	interval := m.pollInterval
	if interval <= 0 {
		interval = defaultAntigravityPollInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-tick.C:
			m.refresh()
			m.pollWatchedThreads()
			m.reapIdleSessions()
		}
	}
}

func (m *AntigravityManager) shutdown() {
	m.mu.Lock()
	m.shuttingDown = true
	runs := make([]*antigravityRun, 0, len(m.runs))
	for id, r := range m.runs {
		runs = append(runs, r)
		delete(m.runs, id)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Add(1)
		go func(run *antigravityRun) {
			defer wg.Done()
			run.shutdown(500 * time.Millisecond)
		}(r)
	}
	wg.Wait()
}

func (m *AntigravityManager) pollWatchedThreads() {
	m.mu.Lock()
	watchedIDs := map[string]bool{}
	for _, ids := range m.watch {
		for id := range ids {
			watchedIDs[id] = true
		}
	}
	m.mu.Unlock()

	for id := range watchedIDs {
		m.mu.Lock()
		run := m.runs[id]
		rec, ok := m.records[id]
		prevStamp := m.watchedStamp[id]
		m.mu.Unlock()
		if run != nil || !ok {
			continue
		}
		stamp := m.computeWatchStamp(rec)
		if stamp == prevStamp {
			continue
		}
		m.mu.Lock()
		if m.watchedStamp == nil {
			m.watchedStamp = map[string]antigravityWatchStamp{}
		}
		m.watchedStamp[id] = stamp
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
}

func (m *AntigravityManager) computeWatchStamp(rec antigravityRecord) antigravityWatchStamp {
	stamp := antigravityWatchStamp{
		running: rec.Running,
		title:   antigravityFirstNonEmpty(m.state.Title(rec.ID), rec.Title, rec.AnnotationTitle),
		status:  rec.Status,
	}
	if rec.TranscriptPath != "" {
		if st, err := os.Stat(rec.TranscriptPath); err == nil && st.Mode().IsRegular() {
			stamp.mod = st.ModTime()
			stamp.size = st.Size()
		}
	}
	return stamp
}

func (m *AntigravityManager) reapIdleSessions() {
	now := m.nowTime()
	timeout := m.idleTimeout()
	if timeout <= 0 {
		return
	}

	m.mu.Lock()
	type reapCandidate struct {
		id  string
		run *antigravityRun
	}
	candidates := make([]reapCandidate, 0, len(m.runs))
	for id, run := range m.runs {
		candidates = append(candidates, reapCandidate{id: id, run: run})
	}
	m.mu.Unlock()

	for _, cand := range candidates {
		if !cand.run.canReap(now, timeout) {
			continue
		}
		m.mu.Lock()
		if m.runs[cand.id] == cand.run && cand.run.canReap(now, timeout) {
			delete(m.runs, cand.id)
			m.mu.Unlock()
			go func(r *antigravityRun) {
				r.shutdown(2 * time.Second)
				m.requestRefresh()
			}(cand.run)
		} else {
			m.mu.Unlock()
		}
	}
}

func (m *AntigravityManager) followWork(ctx context.Context) {
	interval := m.workPollInterval
	if interval <= 0 {
		interval = defaultAntigravityWorkPollInterval
	}
	tick := time.NewTicker(interval)
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

func (m *AntigravityManager) pollWork() {
	type sub struct {
		client *Client
		id     string
		watch  antigravityWorkWatch
	}
	var subs []sub
	m.mu.Lock()
	for c, items := range m.workWatch {
		for id, w := range items {
			subs = append(subs, sub{client: c, id: id, watch: w})
		}
	}
	m.mu.Unlock()
	for _, s := range subs {
		_ = m.refreshWork(s.client, s.id, s.watch, false)
	}
}

func (m *AntigravityManager) updateCatalogFromState(state *acpSessionState, initRes *acpInitResult) {
	if state == nil && initRes == nil {
		return
	}
	now := m.nowTime()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyCatalogStateLocked(state, initRes, now)
}

func (m *AntigravityManager) updateCatalogFromConfigOptions(opts []acpConfigOption) {
	if len(opts) == 0 {
		return
	}
	state := &acpSessionState{ConfigOptions: opts}
	now := m.nowTime()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyCatalogStateLocked(state, nil, now)
}

func (m *AntigravityManager) applyCatalogStateLocked(state *acpSessionState, initRes *acpInitResult, now time.Time) {
	cat := m.catalog
	if initRes != nil && initRes.Tier != nil {
		t := *initRes.Tier
		cat.Tier = &t
	}
	if state == nil {
		m.catalog = cat
		return
	}
	if state.Tier != nil {
		t := *state.Tier
		cat.Tier = &t
	}

	var efforts []string
	defaultEffort := cat.DefaultEffort
	if thoughtOpt := state.ConfigOption("thought_level"); thoughtOpt != nil {
		for _, opt := range thoughtOpt.Options {
			if v := strings.TrimSpace(opt.Value); v != "" {
				efforts = append(efforts, v)
			}
		}
		if v := strings.TrimSpace(thoughtOpt.CurrentValue); v != "" {
			defaultEffort = v
		}
	}
	if len(efforts) == 0 && len(cat.Models) > 0 && len(cat.Models[0].Efforts) > 0 {
		efforts = append([]string(nil), cat.Models[0].Efforts...)
	}
	if len(efforts) == 0 {
		efforts = []string{"low", "medium", "high", "xhigh"}
	}

	var models []Model
	if modelOpt := state.ConfigOption("model"); modelOpt != nil && len(modelOpt.Options) > 0 {
		for _, opt := range modelOpt.Options {
			slug := strings.TrimSpace(opt.Value)
			if slug == "" {
				continue
			}
			models = append(models, Model{
				Slug:          slug,
				Name:          antigravityFirstNonEmpty(strings.TrimSpace(opt.Name), slug),
				Description:   strings.TrimSpace(opt.Description),
				DefaultEffort: defaultEffort,
				Efforts:       append([]string(nil), efforts...),
			})
		}
		if v := strings.TrimSpace(modelOpt.CurrentValue); v != "" {
			cat.DefaultModel = v
		}
	} else if len(state.Models.AvailableModels) > 0 {
		for _, info := range state.Models.AvailableModels {
			slug := strings.TrimSpace(info.ModelID)
			if slug == "" {
				continue
			}
			models = append(models, Model{
				Slug:          slug,
				Name:          antigravityFirstNonEmpty(strings.TrimSpace(info.Name), slug),
				Description:   strings.TrimSpace(info.Description),
				DefaultEffort: defaultEffort,
				Efforts:       append([]string(nil), efforts...),
			})
		}
		if v := strings.TrimSpace(state.Models.CurrentModelID); v != "" {
			cat.DefaultModel = v
		}
	}
	if len(models) > 0 {
		cat.Models = models
		cat.UpdatedAt = now
	} else if len(cat.Models) > 0 && state.ConfigOption("thought_level") != nil {
		for i := range cat.Models {
			cat.Models[i].Efforts = append([]string(nil), efforts...)
			cat.Models[i].DefaultEffort = defaultEffort
		}
	}
	if defaultEffort != "" {
		cat.DefaultEffort = defaultEffort
	}

	var modes []acpModeInfo
	if len(state.Modes.AvailableModes) > 0 {
		modes = append(modes, state.Modes.AvailableModes...)
	} else if modeOpt := state.ConfigOption("mode"); modeOpt != nil && len(modeOpt.Options) > 0 {
		for _, opt := range modeOpt.Options {
			id := strings.TrimSpace(opt.Value)
			if id == "" {
				continue
			}
			modes = append(modes, acpModeInfo{
				ID:          id,
				Name:        antigravityFirstNonEmpty(strings.TrimSpace(opt.Name), humanize(strings.ReplaceAll(id, "-", " "))),
				Description: strings.TrimSpace(opt.Description),
			})
		}
	}
	if len(modes) > 0 {
		cat.Modes = modes
	}
	if curMode := strings.TrimSpace(state.CurrentMode()); curMode != "" {
		cat.DefaultMode = curMode
	}
	if cat.DefaultMode == "" {
		cat.DefaultMode = "default"
	}
	m.catalog = cat
}

func defaultAntigravityModes() []acpModeInfo {
	return []acpModeInfo{
		{ID: "default", Name: "Default", Description: "Asks Before Edits And Commands"},
		{ID: "accept-edits", Name: "Accept Edits", Description: "Approves File Edits Automatically"},
		{ID: "auto", Name: "Auto", Description: "Runs Approved Actions Automatically"},
		{ID: "plan", Name: "Plan", Description: "Plans Without Making Edits"},
	}
}

func (m *AntigravityManager) catalogSnapshot() antigravityModelCatalog {
	m.mu.Lock()
	defer m.mu.Unlock()
	cat := m.catalog
	cat.Models = append([]Model(nil), m.catalog.Models...)
	for i := range cat.Models {
		cat.Models[i].Efforts = append([]string(nil), cat.Models[i].Efforts...)
	}
	if len(m.catalog.Modes) > 0 {
		cat.Modes = append([]acpModeInfo(nil), m.catalog.Modes...)
	} else {
		cat.Modes = defaultAntigravityModes()
	}
	if m.catalog.Tier != nil {
		t := *m.catalog.Tier
		cat.Tier = &t
	}
	if cat.DefaultMode == "" {
		cat.DefaultMode = "default"
	}
	return cat
}

func (m *AntigravityManager) validEffort(effort string) bool {
	switch effort {
	case "low", "medium", "high", "xhigh":
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, model := range m.catalog.Models {
		if contains(model.Efforts, effort) {
			return true
		}
	}
	return false
}

func (m *AntigravityManager) validPermissionMode(mode string) bool {
	switch mode {
	case "default", "accept-edits", "auto", "plan":
		return true
	case "full-access":
		return m.allowFullAccess
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, md := range m.catalog.Modes {
		if md.ID == mode {
			if mode == "full-access" {
				return m.allowFullAccess
			}
			return true
		}
	}
	return false
}

// ---- live ACP run ----

type antigravityRun struct {
	manager    *AntigravityManager
	id         string
	appDataDir string
	cwd        string

	client *acpClient

	mu               sync.Mutex
	alive            bool
	running          bool
	loading          bool
	activePrompts    int
	cancelled        bool
	lastActivity     time.Time
	recency          int64
	turnSeq          int64
	activeTurnKey    string
	turnStartedAt    int64
	hasOlder         bool
	diskTurns        []Turn
	diskDiffs        map[string]string
	liveTurns        []Turn
	replayTurns      []Turn
	replayDiffs      map[string]string
	toolDiffs        map[string]string
	turnDiffs        map[string]string
	toolCallTurn     map[string]string
	streamMsgIdx     int
	streamThoughtIdx int
	streamUserIdx    int
	optimisticUsers  []string
	title            string
	model            string
	effort           string
	permissionMode   string
	ctxUsed          int64
	ctxMax           int64
	commands         []SlashCommand
	tier             *acpTierInfo
	lastMsg          string
	lastErr          string
	permissions      map[string]antigravityPendingPerm
	elicitations     map[string]antigravityPendingElicit
	reqOrder         []string
	barriers         map[string]chan struct{}
	lastPublished    map[string][]byte
	lastMeta         []byte
	lastFlush        time.Time
	flushScheduled   bool
	flushTimer       *time.Timer
	closeOnce        sync.Once
	done             chan struct{}
}

func newAntigravityRun(m *AntigravityManager, id, appDataDir, cwd string) *antigravityRun {
	now := m.nowTime()
	if appDataDir == "" {
		appDataDir = acpDefaultAppDataDir
	}
	return &antigravityRun{
		manager:          m,
		id:               id,
		appDataDir:       appDataDir,
		cwd:              cwd,
		alive:            true,
		lastActivity:     now,
		recency:          now.UnixMilli(),
		diskTurns:        []Turn{},
		diskDiffs:        map[string]string{},
		liveTurns:        []Turn{},
		replayTurns:      []Turn{},
		replayDiffs:      map[string]string{},
		toolDiffs:        map[string]string{},
		turnDiffs:        map[string]string{},
		toolCallTurn:     map[string]string{},
		streamMsgIdx:     -1,
		streamThoughtIdx: -1,
		streamUserIdx:    -1,
		permissionMode:   "default",
		permissions:      map[string]antigravityPendingPerm{},
		elicitations:     map[string]antigravityPendingElicit{},
		barriers:         map[string]chan struct{}{},
		lastPublished:    map[string][]byte{},
		done:             make(chan struct{}),
	}
}

func (r *antigravityRun) isAlive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.alive
}

func (r *antigravityRun) isRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

func (r *antigravityRun) canReap(now time.Time, timeout time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.alive || r.running || r.activePrompts > 0 || r.loading {
		return false
	}
	if len(r.permissions) > 0 || len(r.elicitations) > 0 {
		return false
	}
	return now.Sub(r.lastActivity) >= timeout
}

func (r *antigravityRun) listState() antigravityListState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return antigravityListState{
		alive:     r.alive,
		running:   r.running,
		attention: len(r.permissions) > 0 || len(r.elicitations) > 0,
		turnAt:    r.turnStartedAt,
		recency:   r.recency,
		title:     r.title,
		cwd:       r.cwd,
		model:     r.model,
		effort:    r.effort,
		mode:      r.permissionMode,
		lastMsg:   r.lastMsg,
		lastErr:   r.lastErr,
	}
}

func (r *antigravityRun) seedFromTranscript(v *antigravityTranscript) {
	if v == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.diskTurns = cloneTurns(v.Turns)
	r.diskDiffs = map[string]string{}
	for k, d := range v.Diffs {
		r.diskDiffs[k] = d
	}
	r.hasOlder = v.More
	if r.title == "" && v.Meta.Title != "" {
		r.title = v.Meta.Title
	}
	if r.cwd == "" && v.Meta.Cwd != "" {
		r.cwd = v.Meta.Cwd
	}
	if r.model == "" && v.Meta.Model != "" {
		r.model = v.Meta.Model
	}
	if r.ctxUsed == 0 && v.Meta.CtxUsed > 0 {
		r.ctxUsed = v.Meta.CtxUsed
	}
	if r.ctxMax == 0 && v.Meta.CtxMax > 0 {
		r.ctxMax = v.Meta.CtxMax
	}
}

func (r *antigravityRun) applySessionState(state *acpSessionState) {
	if state == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if model := strings.TrimSpace(state.CurrentModel()); model != "" {
		r.model = model
	}
	if thought := strings.TrimSpace(state.CurrentThoughtLevel()); thought != "" {
		r.effort = thought
	}
	if mode := strings.TrimSpace(state.CurrentMode()); mode != "" {
		r.permissionMode = mode
	}
	if state.Tier != nil {
		t := *state.Tier
		r.tier = &t
	}
}

func (r *antigravityRun) mergeOlderDiskHistory(v *antigravityTranscript) {
	if v == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.liveTurns) == 0 {
		r.diskTurns = cloneTurns(v.Turns)
	} else {
		// Retain disk turns that precede the first live turn created in this run.
		cutoff := r.liveTurns[0].StartedAt
		var older []Turn
		for _, t := range v.Turns {
			if cutoff > 0 && t.StartedAt > 0 && t.StartedAt >= cutoff {
				continue
			}
			older = append(older, t)
		}
		if len(older) > len(r.diskTurns) {
			r.diskTurns = cloneTurns(older)
		}
	}
	for k, d := range v.Diffs {
		if _, exists := r.diskDiffs[k]; !exists {
			r.diskDiffs[k] = d
		}
	}
	r.hasOlder = v.More
}

func (r *antigravityRun) snapshot() (Meta, []Turn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *antigravityRun) snapshotLocked() (Meta, []Turn) {
	title := r.title
	if custom := r.manager.state.Title(r.id); custom != "" {
		title = custom
	}
	mode := "live"
	if !r.alive {
		mode = "tail"
	}
	meta := Meta{
		Provider:       "antigravity",
		Mode:           mode,
		Title:          title,
		Cwd:            r.cwd,
		Model:          r.model,
		Effort:         r.effort,
		PermissionMode: r.permissionMode,
		CtxUsed:        r.ctxUsed,
		CtxMax:         r.ctxMax,
		TurnAt:         r.turnStartedAt,
		HasOlder:       r.hasOlder,
		Running:        r.running,
	}
	if len(r.commands) > 0 {
		meta.Commands = append([]SlashCommand(nil), r.commands...)
	}
	if len(r.reqOrder) > 0 {
		meta.Requests = make([]Request, 0, len(r.reqOrder))
		for _, reqID := range r.reqOrder {
			if p, ok := r.permissions[reqID]; ok {
				meta.Requests = append(meta.Requests, cloneRequest(p.req))
			} else if e, ok := r.elicitations[reqID]; ok {
				meta.Requests = append(meta.Requests, cloneRequest(e.req))
			}
		}
	}
	combined := make([]Turn, 0, len(r.diskTurns)+len(r.liveTurns))
	combined = append(combined, r.diskTurns...)
	combined = append(combined, r.liveTurns...)
	return meta, cloneTurns(combined)
}

func cloneTurns(src []Turn) []Turn {
	if len(src) == 0 {
		return []Turn{}
	}
	var out []Turn
	_ = json.Unmarshal(mustJSON(src), &out)
	if out == nil {
		return []Turn{}
	}
	return out
}

func cloneRequest(src Request) Request {
	var out Request
	_ = json.Unmarshal(mustJSON(src), &out)
	return out
}

func (r *antigravityRun) publish(immediate bool) {
	r.mu.Lock()
	sid := r.id
	if sid == "" {
		r.mu.Unlock()
		return
	}
	if !immediate {
		if delay := defaultAntigravityFlushInterval - time.Since(r.lastFlush); delay > 0 {
			if !r.flushScheduled {
				r.flushScheduled = true
				r.flushTimer = time.AfterFunc(delay, func() {
					r.mu.Lock()
					r.flushScheduled = false
					r.flushTimer = nil
					r.mu.Unlock()
					r.publish(true)
				})
			}
			r.mu.Unlock()
			return
		}
	}
	r.lastFlush = time.Now()
	meta, turns := r.snapshotLocked()
	var messages []map[string]any
	for _, t := range turns {
		b := mustJSON(t)
		if !bytes.Equal(b, r.lastPublished[t.Key]) {
			r.lastPublished[t.Key] = b
			messages = append(messages, map[string]any{"t": "turn", "turn": t})
		}
	}
	metaBytes := mustJSON(meta)
	if !bytes.Equal(metaBytes, r.lastMeta) {
		r.lastMeta = metaBytes
		messages = append(messages, map[string]any{"t": "meta", "meta": meta})
	}
	r.mu.Unlock()

	for _, msg := range messages {
		r.manager.broadcast(sid, msg)
	}
}

func (r *antigravityRun) ensureLiveTurnLocked(nowMs int64) *Turn {
	if len(r.liveTurns) > 0 && r.activeTurnKey != "" {
		last := &r.liveTurns[len(r.liveTurns)-1]
		if last.Key == r.activeTurnKey {
			return last
		}
	}
	r.turnSeq++
	key := fmt.Sprintf("live-%d", r.turnSeq)
	r.activeTurnKey = key
	if nowMs == 0 {
		nowMs = r.manager.nowTime().UnixMilli()
	}
	r.turnStartedAt = nowMs
	r.liveTurns = append(r.liveTurns, Turn{
		Key:       key,
		TurnID:    key,
		Status:    "inProgress",
		StartedAt: nowMs,
		Items:     []Item{},
	})
	r.streamMsgIdx = -1
	r.streamThoughtIdx = -1
	r.streamUserIdx = -1
	return &r.liveTurns[len(r.liveTurns)-1]
}

// drainDispatch inserts a FIFO sentinel into client.eventQueue and waits until
// acpClient.dispatchLoop has processed every earlier notification. Never call
// this from inside an ACP callback.
func (r *antigravityRun) drainDispatch(client *acpClient) {
	if client == nil || client.Closed() {
		return
	}
	token := uuid()
	ch := make(chan struct{})
	r.mu.Lock()
	if r.barriers == nil {
		r.barriers = map[string]chan struct{}{}
	}
	r.barriers[token] = ch
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.barriers, token)
		r.mu.Unlock()
	}()

	client.enqueueEvent(acpDispatchEvent{
		update: &acpSessionUpdate{
			Kind: antigravityDrainBarrierKind,
			Text: token,
		},
	})
	select {
	case <-ch:
	case <-client.Done():
	}
}

func (r *antigravityRun) onUpdate(u acpSessionUpdate) {
	if u.Kind == antigravityDrainBarrierKind {
		r.mu.Lock()
		ch := r.barriers[u.Text]
		delete(r.barriers, u.Text)
		r.mu.Unlock()
		if ch != nil {
			close(ch)
		}
		return
	}

	now := r.manager.nowTime()
	nowMs := now.UnixMilli()

	var refreshList bool
	var updatedConfig []acpConfigOption
	var shouldPublish bool

	r.mu.Lock()
	if u.SessionID != "" && r.id != "" && u.SessionID != r.id {
		r.mu.Unlock()
		return
	}
	r.lastActivity = now
	if !r.loading {
		r.recency = nowMs
	}

	switch u.Kind {
	case "usage_update":
		if u.Usage != nil {
			if u.Usage.Used > 0 {
				r.ctxUsed = u.Usage.Used
			}
			if u.Usage.Size > 0 {
				r.ctxMax = u.Usage.Size
			}
			shouldPublish = !r.loading
		}

	case "available_commands_update":
		cmds := make([]SlashCommand, 0, len(u.AvailableCommands))
		for _, c := range u.AvailableCommands {
			name := strings.TrimPrefix(strings.TrimSpace(c.Name), "/")
			if name == "" {
				continue
			}
			cmds = append(cmds, SlashCommand{
				Name:        name,
				Description: strings.TrimSpace(c.Description),
				InputHint:   strings.TrimSpace(c.InputHint),
			})
		}
		r.commands = cmds
		shouldPublish = !r.loading

	case "session_info_update":
		if title := cleanAntigravityTitle(u.Title); title != "" {
			r.title = title
			shouldPublish = !r.loading
			refreshList = true
		}

	case "current_mode_update":
		if mode := strings.TrimSpace(u.CurrentModeID); mode != "" {
			r.permissionMode = mode
			shouldPublish = !r.loading
		}

	case "config_option_update", "config_options_update":
		for _, opt := range u.ConfigOptions {
			val := strings.TrimSpace(opt.CurrentValue)
			switch opt.ID {
			case "model":
				if val != "" {
					r.model = val
				}
			case "thought_level":
				r.effort = val
			case "mode":
				if val != "" {
					r.permissionMode = val
				}
			}
		}
		updatedConfig = append([]acpConfigOption(nil), u.ConfigOptions...)
		shouldPublish = !r.loading

	case "user_message_chunk":
		text := u.Text
		if text == "" && u.Content != nil {
			text = u.Content.Text
		}
		if text != "" {
			if r.loading {
				r.appendReplayChunkLocked("user", text, nowMs)
			} else if !r.consumeOptimisticUserLocked(text) {
				turn := r.ensureLiveTurnLocked(nowMs)
				r.streamThoughtIdx = -1
				r.streamMsgIdx = -1
				if r.streamUserIdx >= 0 && r.streamUserIdx < len(turn.Items) && turn.Items[r.streamUserIdx].K == "user" {
					turn.Items[r.streamUserIdx].Text += text
				} else {
					turn.Items = append(turn.Items, Item{
						ID:   fmt.Sprintf("%s:user:%d", turn.Key, len(turn.Items)),
						K:    "user",
						Text: text,
					})
					r.streamUserIdx = len(turn.Items) - 1
				}
				shouldPublish = true
			}
		}

	case "agent_thought_chunk":
		text := u.Text
		if text == "" && u.Content != nil {
			text = u.Content.Text
		}
		if text != "" {
			if r.loading {
				r.appendReplayChunkLocked("reasoning", text, nowMs)
			} else {
				turn := r.ensureLiveTurnLocked(nowMs)
				r.streamMsgIdx = -1
				r.streamUserIdx = -1
				if r.streamThoughtIdx >= 0 && r.streamThoughtIdx < len(turn.Items) && turn.Items[r.streamThoughtIdx].K == "reasoning" {
					turn.Items[r.streamThoughtIdx].Text += text
				} else {
					turn.Items = append(turn.Items, Item{
						ID:   fmt.Sprintf("%s:thought:%d", turn.Key, len(turn.Items)),
						K:    "reasoning",
						Text: text,
					})
					r.streamThoughtIdx = len(turn.Items) - 1
				}
				shouldPublish = true
			}
		}

	case "agent_message_chunk":
		text := u.Text
		if text == "" && u.Content != nil {
			text = u.Content.Text
		}
		if text != "" {
			if r.loading {
				r.appendReplayChunkLocked("assistant", text, nowMs)
			} else {
				turn := r.ensureLiveTurnLocked(nowMs)
				r.streamThoughtIdx = -1
				r.streamUserIdx = -1
				if r.streamMsgIdx >= 0 && r.streamMsgIdx < len(turn.Items) && turn.Items[r.streamMsgIdx].K == "assistant" {
					turn.Items[r.streamMsgIdx].Text += text
				} else {
					turn.Items = append(turn.Items, Item{
						ID:   fmt.Sprintf("%s:assistant:%d", turn.Key, len(turn.Items)),
						K:    "assistant",
						Text: text,
					})
					r.streamMsgIdx = len(turn.Items) - 1
				}
				r.lastMsg = clipPlain(turn.Items[r.streamMsgIdx].Text, 200)
				shouldPublish = true
			}
		}

	case "tool_call", "tool_call_update":
		if u.ToolCall != nil {
			if r.loading {
				r.applyReplayToolCallLocked(*u.ToolCall, nowMs)
			} else {
				r.applyLiveToolCallLocked(*u.ToolCall, nowMs)
				shouldPublish = true
			}
		}
	}
	r.mu.Unlock()

	if len(updatedConfig) > 0 {
		r.manager.updateCatalogFromConfigOptions(updatedConfig)
	}
	if shouldPublish {
		r.publish(false)
	}
	if refreshList {
		r.manager.requestRefresh()
	}
}

func (r *antigravityRun) consumeOptimisticUserLocked(chunk string) bool {
	if len(r.optimisticUsers) == 0 {
		return false
	}
	want := r.optimisticUsers[0]
	if chunk == want {
		r.optimisticUsers = r.optimisticUsers[1:]
		return true
	}
	if strings.HasPrefix(want, chunk) {
		rest := strings.TrimPrefix(want, chunk)
		if rest == "" {
			r.optimisticUsers = r.optimisticUsers[1:]
		} else {
			r.optimisticUsers[0] = rest
		}
		return true
	}
	return false
}

func (r *antigravityRun) appendReplayChunkLocked(kind, text string, nowMs int64) {
	if kind == "user" {
		if len(r.replayTurns) == 0 || (len(r.replayTurns[len(r.replayTurns)-1].Items) > 0 && r.replayTurns[len(r.replayTurns)-1].Items[len(r.replayTurns[len(r.replayTurns)-1].Items)-1].K != "user") {
			key := fmt.Sprintf("replay-%d", len(r.replayTurns)+1)
			r.replayTurns = append(r.replayTurns, Turn{
				Key:       key,
				TurnID:    key,
				Status:    "completed",
				StartedAt: nowMs,
				Items:     []Item{{ID: key + ":user:0", K: "user", Text: text}},
			})
			return
		}
	}
	if len(r.replayTurns) == 0 {
		key := "replay-1"
		r.replayTurns = append(r.replayTurns, Turn{
			Key:       key,
			TurnID:    key,
			Status:    "completed",
			StartedAt: nowMs,
			Items:     []Item{},
		})
	}
	turn := &r.replayTurns[len(r.replayTurns)-1]
	if n := len(turn.Items); n > 0 && turn.Items[n-1].K == kind {
		turn.Items[n-1].Text += text
		return
	}
	turn.Items = append(turn.Items, Item{
		ID:   fmt.Sprintf("%s:%s:%d", turn.Key, kind, len(turn.Items)),
		K:    kind,
		Text: text,
	})
}

func (r *antigravityRun) applyReplayToolCallLocked(tc acpToolCallInfo, nowMs int64) {
	if len(r.replayTurns) == 0 {
		key := "replay-1"
		r.replayTurns = append(r.replayTurns, Turn{
			Key:       key,
			TurnID:    key,
			Status:    "completed",
			StartedAt: nowMs,
			Items:     []Item{},
		})
	}
	turn := &r.replayTurns[len(r.replayTurns)-1]
	r.upsertToolCallLocked(turn, tc, true)
}

func (r *antigravityRun) applyLiveToolCallLocked(tc acpToolCallInfo, nowMs int64) {
	turn := r.ensureLiveTurnLocked(nowMs)
	r.streamThoughtIdx = -1
	r.streamMsgIdx = -1
	r.streamUserIdx = -1
	r.upsertToolCallLocked(turn, tc, false)
}

func (r *antigravityRun) upsertToolCallLocked(turn *Turn, tc acpToolCallInfo, isReplay bool) {
	itemID := strings.TrimSpace(tc.ToolCallID)
	if itemID == "" {
		itemID = fmt.Sprintf("%s:tool:%d", turn.Key, len(turn.Items))
	}

	idx := -1
	for i := range turn.Items {
		if turn.Items[i].ID == itemID {
			idx = i
			break
		}
	}

	var existing *Item
	if idx >= 0 {
		existing = &turn.Items[idx]
	}
	it, diffText := convertACPToolCall(tc, itemID, existing)
	if idx >= 0 {
		turn.Items[idx] = it
	} else {
		turn.Items = append(turn.Items, it)
	}

	if diffText != "" {
		turn.HasDiff = true
		r.toolDiffs[itemID] = diffText
		r.toolCallTurn[itemID] = turn.Key
		if isReplay {
			r.rebuildTurnDiffLocked(turn, r.replayDiffs)
		} else {
			r.rebuildTurnDiffLocked(turn, r.turnDiffs)
		}
	}
}

func (r *antigravityRun) rebuildTurnDiffLocked(turn *Turn, dst map[string]string) {
	var parts []string
	for _, it := range turn.Items {
		if d := r.toolDiffs[it.ID]; d != "" {
			parts = append(parts, d)
		}
	}
	if len(parts) > 0 {
		joined := strings.Join(parts, "\n")
		if !strings.HasSuffix(joined, "\n") {
			joined += "\n"
		}
		dst[turn.Key] = joined
	}
}

func mapACPToolStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "completed", "complete", "done", "success", "succeeded":
		return "completed"
	case "failed", "error":
		return "failed"
	case "cancelled", "canceled", "declined", "rejected":
		return "declined"
	case "in_progress", "inprogress", "running", "pending":
		return "inProgress"
	default:
		return ""
	}
}

func classifyACPToolKind(tc acpToolCallInfo, diffText string) string {
	kind := strings.ToLower(strings.TrimSpace(tc.Kind))
	name := strings.TrimSpace(tc.Name)
	lowerName := strings.ToLower(name)

	switch kind {
	case "execute", "command", "terminal", "shell":
		return "cmd"
	case "edit", "write", "delete", "file", "patch":
		return "file"
	case "search":
		return "search"
	}
	if diffText != "" {
		return "file"
	}
	switch lowerName {
	case "run_command", "bash", "execute_command", "terminal", "shell":
		return "cmd"
	case "edit_file", "write_file", "write_to_file", "replace_file_content", "multi_replace_file_content", "edit", "write":
		return "file"
	case "search_web", "grep_search", "find_by_name", "websearch":
		return "search"
	}
	return "tool"
}

func convertACPToolCall(tc acpToolCallInfo, itemID string, prev *Item) (Item, string) {
	diffText := tc.DiffText()
	inferredK := classifyACPToolKind(tc, diffText)

	it := Item{ID: itemID, K: inferredK, Status: "inProgress"}
	if prev != nil {
		it = *prev
		if diffText != "" {
			it.K = "file"
		} else if it.K == "" || (it.K == "tool" && inferredK != "tool") {
			it.K = inferredK
		}
	}

	if st := mapACPToolStatus(tc.Status); st != "" {
		it.Status = st
	} else if it.Status == "" {
		it.Status = "inProgress"
	}

	inp := tc.InputMap()
	switch it.K {
	case "cmd":
		cmdText := antigravityFirstNonEmpty(
			str(inp, "CommandLine"),
			str(inp, "command"),
			str(inp, "cmd"),
			tc.Title,
			tc.Name,
		)
		if cmdText != "" {
			it.Title = clipHead(shellScript(cmdText), 800)
		}
		if out := tc.TextOutput(); out != "" {
			it.Body = clipTail(out, maxOutput)
		} else if len(tc.RawOutput) > 0 && !bytes.Equal(bytes.TrimSpace(tc.RawOutput), []byte("null")) {
			it.Body = clipTail(string(bytes.TrimSpace(tc.RawOutput)), maxOutput)
		}
		if it.Status == "completed" && it.Exit == nil {
			code := 0
			it.Exit = &code
		} else if it.Status == "failed" && it.Exit == nil {
			code := 1
			it.Exit = &code
		}

	case "file":
		var paths []string
		seen := map[string]bool{}
		addPath := func(prefix, p string) {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] {
				return
			}
			seen[p] = true
			paths = append(paths, prefix+" "+p)
		}
		for _, block := range tc.Content {
			if block.Diff != nil && block.Diff.Path != "" {
				prefix := "M"
				if !block.Diff.HasOld && block.Diff.OldText == "" {
					prefix = "A"
				}
				addPath(prefix, block.Diff.Path)
			}
		}
		for _, loc := range tc.Locations {
			addPath("M", loc.Path)
		}
		if p := antigravityFirstNonEmpty(str(inp, "Path"), str(inp, "TargetFile"), str(inp, "file_path"), str(inp, "path")); p != "" {
			addPath("M", p)
		}
		if len(paths) > 0 {
			it.Title = strings.Join(paths, "\n")
		} else if title := antigravityFirstNonEmpty(tc.Title, tc.Name); title != "" {
			it.Title = title
		}
		if diffText != "" {
			it.Body = clipHead(diffText, maxDiff)
		} else if out := tc.TextOutput(); out != "" && it.Body == "" {
			it.Body = clipTail(out, maxOutput)
		}

	case "search":
		if title := antigravityFirstNonEmpty(str(inp, "query"), str(inp, "Query"), str(inp, "Pattern"), tc.Title, tc.Name); title != "" {
			it.Title = title
		}
		if out := tc.TextOutput(); out != "" {
			it.Body = clipTail(out, maxOutput)
		} else if len(inp) > 0 && it.Body == "" {
			it.Body = compactJSON(inp, maxArgs)
		}

	default:
		if title := antigravityFirstNonEmpty(tc.Title, tc.Name); title != "" {
			it.Title = title
		}
		if it.Title == "" {
			it.Title = "Tool"
		}
		if out := tc.TextOutput(); out != "" {
			it.Body = clipTail(out, maxOutput)
		} else if len(tc.RawOutput) > 0 && !bytes.Equal(bytes.TrimSpace(tc.RawOutput), []byte("null")) {
			it.Body = clipTail(string(bytes.TrimSpace(tc.RawOutput)), maxOutput)
		} else if len(inp) > 0 && it.Body == "" {
			it.Body = compactJSON(inp, maxArgs)
		}
	}

	return it, diffText
}

func (r *antigravityRun) onPermission(req acpPermissionRequest) {
	now := r.manager.nowTime()
	diffText := req.ToolCall.DiffText()
	toolKind := classifyACPToolKind(req.ToolCall, diffText)

	reqKind := "command"
	defaultTitle := "Run Command?"
	if toolKind == "file" {
		reqKind = "file"
		defaultTitle = "Apply File Changes?"
	} else if toolKind != "cmd" {
		defaultTitle = "Permission Required"
	}

	title := antigravityFirstNonEmpty(strings.TrimSpace(req.ToolCall.Title), strings.TrimSpace(req.ToolCall.Name), defaultTitle)
	inp := req.ToolCall.InputMap()
	var body string
	switch reqKind {
	case "file":
		if diffText != "" {
			body = clipHead(diffText, maxDiff)
		} else if p := antigravityFirstNonEmpty(str(inp, "Path"), str(inp, "TargetFile"), str(inp, "file_path"), str(inp, "path")); p != "" {
			body = p
		} else if len(req.ToolCall.Locations) > 0 {
			body = req.ToolCall.Locations[0].Path
		} else {
			body = compactJSON(inp, 1200)
		}
	default:
		cmdText := antigravityFirstNonEmpty(str(inp, "CommandLine"), str(inp, "command"), str(inp, "cmd"))
		if cmdText != "" {
			body = shellScript(cmdText)
		} else if len(inp) > 0 {
			body = compactJSON(inp, 1200)
		} else {
			body = strings.TrimSpace(req.ToolCall.Title)
		}
	}

	options := make([]RequestOption, 0, len(req.Options))
	for _, opt := range req.Options {
		options = append(options, RequestOption{
			ID:    opt.OptionID,
			Label: antigravityFirstNonEmpty(opt.Name, opt.OptionID),
			Kind:  opt.Kind,
		})
	}

	wireReq := Request{
		ID:      req.RequestID,
		Method:  "session/request_permission",
		Kind:    reqKind,
		Title:   title,
		Body:    body,
		Options: options,
	}

	r.mu.Lock()
	if req.SessionID != "" && r.id != "" && req.SessionID != r.id {
		r.mu.Unlock()
		return
	}
	sid := r.id
	r.lastActivity = now
	if _, exists := r.permissions[req.RequestID]; !exists {
		r.reqOrder = append(r.reqOrder, req.RequestID)
	}
	r.permissions[req.RequestID] = antigravityPendingPerm{
		acpReq: req,
		req:    wireReq,
		at:     now,
	}
	if diffText != "" && req.ToolCall.ToolCallID != "" {
		r.toolDiffs[req.ToolCall.ToolCallID] = diffText
		if turn := r.ensureLiveTurnLocked(now.UnixMilli()); turn != nil {
			turn.HasDiff = true
			r.toolCallTurn[req.ToolCall.ToolCallID] = turn.Key
			r.rebuildTurnDiffLocked(turn, r.turnDiffs)
		}
	}
	threadTitle := antigravityFirstNonEmpty(r.manager.state.Title(sid), r.title, "Antigravity")
	cwd := r.cwd
	r.mu.Unlock()

	r.publish(true)
	r.manager.requestRefresh()
	if sid != "" {
		eventText := clipPlain(antigravityFirstNonEmpty(body, title, "Approval Required"), 200)
		r.manager.event(sid, "attention", threadTitle, eventText, cwd)
	}
}

func (r *antigravityRun) onElicitation(req acpElicitationRequest) {
	now := r.manager.nowTime()

	questions := make([]Question, 0, len(req.Fields))
	for _, f := range req.Fields {
		q := Question{
			ID:          f.Name,
			Header:      antigravityFirstNonEmpty(strings.TrimSpace(f.Title), humanize(strings.ReplaceAll(f.Name, "_", " "))),
			Question:    antigravityFirstNonEmpty(strings.TrimSpace(f.Description), strings.TrimSpace(f.Title), f.Name),
			Secret:      f.Secret,
			MultiSelect: f.MultiSelect,
		}
		for _, opt := range f.Options {
			label := antigravityFirstNonEmpty(opt.Label, opt.Value)
			q.Options = append(q.Options, label)
			q.Descriptions = append(q.Descriptions, opt.Description)
		}
		if len(q.Options) == 0 {
			q.FreeText = true
		}
		questions = append(questions, q)
	}
	if len(questions) == 0 {
		questions = append(questions, Question{
			ID:       "response",
			Header:   "Response",
			Question: antigravityFirstNonEmpty(strings.TrimSpace(req.Message), strings.TrimSpace(req.URL), "Provide Your Answer"),
			FreeText: true,
		})
	}

	title := antigravityFirstNonEmpty(clipPlain(strings.TrimSpace(req.Message), 120), "Antigravity Has A Question")
	wireReq := Request{
		ID:        req.RequestID,
		Method:    "elicitation/create",
		Kind:      "question",
		Title:     title,
		Body:      strings.TrimSpace(req.Message),
		Questions: questions,
	}

	r.mu.Lock()
	if req.SessionID != "" && r.id != "" && req.SessionID != r.id {
		r.mu.Unlock()
		return
	}
	sid := r.id
	r.lastActivity = now
	if _, exists := r.elicitations[req.RequestID]; !exists {
		r.reqOrder = append(r.reqOrder, req.RequestID)
	}
	r.elicitations[req.RequestID] = antigravityPendingElicit{
		acpReq: req,
		req:    wireReq,
		at:     now,
	}
	threadTitle := antigravityFirstNonEmpty(r.manager.state.Title(sid), r.title, "Antigravity")
	cwd := r.cwd
	r.mu.Unlock()

	r.publish(true)
	r.manager.requestRefresh()
	if sid != "" {
		eventText := clipPlain(antigravityFirstNonEmpty(req.Message, questions[0].Question, "Input Required"), 200)
		r.manager.event(sid, "attention", threadTitle, eventText, cwd)
	}
}

// onClose runs inside acpClient.dispatchLoop; NEVER call client.Close() here!
func (r *antigravityRun) onClose(closeErr error) {
	r.mu.Lock()
	sid := r.id
	wasRunning := r.running
	wasCancelled := r.cancelled
	r.alive = false
	r.running = false
	r.activePrompts = 0
	r.permissions = map[string]antigravityPendingPerm{}
	r.elicitations = map[string]antigravityPendingElicit{}
	r.reqOrder = nil
	if r.flushTimer != nil {
		r.flushTimer.Stop()
		r.flushTimer = nil
		r.flushScheduled = false
	}
	if wasRunning && len(r.liveTurns) > 0 {
		last := &r.liveTurns[len(r.liveTurns)-1]
		if last.Status == "inProgress" {
			if wasCancelled {
				last.Status = "interrupted"
			} else {
				last.Status = "failed"
				if closeErr != nil {
					last.Error = clipPlain(closeErr.Error(), 240)
				} else {
					last.Error = "Antigravity Session Stopped"
				}
				r.lastErr = last.Error
			}
		}
	}
	title := antigravityFirstNonEmpty(r.manager.state.Title(sid), r.title, "Antigravity")
	errText := r.lastErr
	cwd := r.cwd
	r.mu.Unlock()

	r.manager.mu.Lock()
	if sid != "" && r.manager.runs[sid] == r {
		delete(r.manager.runs, sid)
	}
	shuttingDown := r.manager.shuttingDown
	r.manager.mu.Unlock()

	r.closeOnce.Do(func() { close(r.done) })

	if !shuttingDown && sid != "" {
		go func() {
			r.publish(true)
			r.manager.requestRefresh()
			if wasRunning && !wasCancelled {
				r.manager.event(sid, "failed", title, antigravityFirstNonEmpty(errText, "Antigravity Session Stopped"), cwd)
			}
		}()
	}
}

func (r *antigravityRun) shutdown(closeSessionTimeout time.Duration) {
	r.mu.Lock()
	if r.flushTimer != nil {
		r.flushTimer.Stop()
		r.flushTimer = nil
		r.flushScheduled = false
	}
	client := r.client
	sid := r.id
	r.mu.Unlock()

	if client != nil && !client.Closed() {
		if sid != "" && closeSessionTimeout > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), closeSessionTimeout)
			_ = client.CloseSession(ctx, sid)
			cancel()
		}
		_ = client.Close()
	}
	r.closeOnce.Do(func() { close(r.done) })
}

func (r *antigravityRun) startPrompt(text string, images []string, blocks []acpContentBlock) (bool, error) {
	now := r.manager.nowTime()
	nowMs := now.UnixMilli()

	r.mu.Lock()
	if !r.alive || r.client == nil {
		r.mu.Unlock()
		return false, errors.New("Antigravity Session Has Stopped")
	}
	steering := r.running
	r.running = true
	r.activePrompts++
	r.cancelled = false
	r.lastActivity = now
	r.recency = nowMs
	r.lastErr = ""

	var turn *Turn
	if steering && len(r.liveTurns) > 0 && r.activeTurnKey != "" {
		turn = &r.liveTurns[len(r.liveTurns)-1]
	} else {
		r.activeTurnKey = ""
		turn = r.ensureLiveTurnLocked(nowMs)
	}
	turn.Status = "inProgress"
	turn.Error = ""
	r.streamMsgIdx = -1
	r.streamThoughtIdx = -1
	r.streamUserIdx = -1
	userItem := Item{
		ID:     fmt.Sprintf("%s:user:%d", turn.Key, len(turn.Items)),
		K:      "user",
		Text:   text,
		Images: append([]string(nil), images...),
	}
	turn.Items = append(turn.Items, userItem)
	if text != "" {
		r.optimisticUsers = append(r.optimisticUsers, text)
	}
	if r.title == "" && text != "" {
		r.title = clipPlain(text, 120)
	}
	title := antigravityFirstNonEmpty(r.manager.state.Title(r.id), r.title, clipPlain(text, 120))
	cwd := r.cwd
	client := r.client
	sid := r.id
	turnKey := turn.Key
	startedAt := turn.StartedAt
	r.mu.Unlock()

	r.publish(true)
	r.manager.requestRefresh()
	if !steering {
		r.manager.event(sid, "started", title, "", cwd)
	}

	go func() {
		res, err := client.Prompt(context.Background(), sid, blocks)
		r.drainDispatch(client)
		r.finishPrompt(turnKey, startedAt, res, err)
	}()
	return false, nil
}

func (r *antigravityRun) finishPrompt(turnKey string, startedAt int64, res *acpPromptResult, promptErr error) {
	now := r.manager.nowTime()
	nowMs := now.UnixMilli()

	var eventKind, eventTitle, eventText, cwd string

	r.mu.Lock()
	r.lastActivity = now
	r.recency = nowMs
	wasRunning := r.running
	if r.activePrompts > 0 {
		r.activePrompts--
	}
	if res != nil && res.Usage != nil {
		if res.Usage.Used > 0 {
			r.ctxUsed = res.Usage.Used
		}
		if res.Usage.Size > 0 {
			r.ctxMax = res.Usage.Size
		}
	}

	stillRunning := r.activePrompts > 0
	if wasRunning && !stillRunning {
		r.running = false
		var target *Turn
		for i := len(r.liveTurns) - 1; i >= 0; i-- {
			if r.liveTurns[i].Key == turnKey {
				target = &r.liveTurns[i]
				break
			}
		}
		if target == nil && len(r.liveTurns) > 0 {
			target = &r.liveTurns[len(r.liveTurns)-1]
		}
		if target != nil {
			if startedAt > 0 && nowMs >= startedAt {
				target.DurationMs = nowMs - startedAt
			}
			stopReason := ""
			if res != nil {
				stopReason = strings.ToLower(strings.TrimSpace(res.StopReason))
			}
			switch {
			case r.cancelled || stopReason == "cancelled" || stopReason == "canceled":
				target.Status = "interrupted"
				target.Error = ""
				eventKind = ""
			case promptErr != nil:
				target.Status = "failed"
				target.Error = clipPlain(promptErr.Error(), 240)
				r.lastErr = target.Error
				eventKind = "failed"
				eventText = target.Error
			default:
				target.Status = "completed"
				eventKind = "completed"
				eventText = r.lastMsg
			}
		}
		r.activeTurnKey = ""
		r.streamMsgIdx = -1
		r.streamThoughtIdx = -1
		r.streamUserIdx = -1
		r.optimisticUsers = nil
		eventTitle = antigravityFirstNonEmpty(r.manager.state.Title(r.id), r.title, "Antigravity")
		cwd = r.cwd
	}
	r.mu.Unlock()

	r.publish(true)
	r.manager.requestRefresh()
	if eventKind != "" {
		r.manager.event(r.id, eventKind, eventTitle, eventText, cwd)
	}
}

func (r *antigravityRun) cancelTurn(ctx context.Context) error {
	r.mu.Lock()
	if !r.alive || r.client == nil {
		r.mu.Unlock()
		return errors.New("Antigravity Is Not Running")
	}
	r.cancelled = true
	r.permissions = map[string]antigravityPendingPerm{}
	r.elicitations = map[string]antigravityPendingElicit{}
	r.reqOrder = nil
	r.lastActivity = r.manager.nowTime()
	client := r.client
	sid := r.id
	r.mu.Unlock()

	err := client.Cancel(ctx, sid)
	r.publish(true)
	r.manager.requestRefresh()
	return err
}

func (r *antigravityRun) removeRequestLocked(reqID string) {
	delete(r.permissions, reqID)
	delete(r.elicitations, reqID)
	filtered := r.reqOrder[:0]
	for _, id := range r.reqOrder {
		if id != reqID {
			filtered = append(filtered, id)
		}
	}
	r.reqOrder = filtered
}

func resolvePermissionOption(options []acpPermissionOption, optionID, decision string) (string, bool, error) {
	optionID = strings.TrimSpace(optionID)
	decision = strings.TrimSpace(decision)

	if optionID != "" {
		if len(options) > 0 {
			for _, opt := range options {
				if opt.OptionID == optionID {
					return optionID, false, nil
				}
			}
			return "", false, errors.New("Unknown Approval Option")
		}
		return optionID, false, nil
	}

	if decision == "cancel" {
		return "", true, nil
	}

	findByKind := func(kinds ...string) string {
		for _, want := range kinds {
			for _, opt := range options {
				if strings.EqualFold(strings.TrimSpace(opt.Kind), want) {
					return opt.OptionID
				}
			}
		}
		return ""
	}
	findContains := func(sub string) string {
		for _, opt := range options {
			if strings.Contains(strings.ToLower(opt.Kind), sub) {
				return opt.OptionID
			}
		}
		return ""
	}

	switch decision {
	case "accept":
		if id := findByKind("allow_once", "allow_always"); id != "" {
			return id, false, nil
		}
		if id := findContains("allow"); id != "" {
			return id, false, nil
		}
		if len(options) > 0 {
			return options[0].OptionID, false, nil
		}
		return "", false, errors.New("No Approval Option Available")

	case "acceptForSession":
		if id := findByKind("allow_always", "allow_once"); id != "" {
			return id, false, nil
		}
		if id := findContains("allow"); id != "" {
			return id, false, nil
		}
		if len(options) > 0 {
			return options[0].OptionID, false, nil
		}
		return "", false, errors.New("No Approval Option Available")

	case "decline":
		if id := findByKind("reject_once", "reject_always"); id != "" {
			return id, false, nil
		}
		if id := findContains("reject"); id != "" {
			return id, false, nil
		}
		return "", true, nil

	default:
		return "", false, errors.New("Unknown Approval Decision")
	}
}

func (r *antigravityRun) approvePermission(reqID, optionID, decision string) error {
	reqID = strings.TrimSpace(reqID)
	r.mu.Lock()
	perm, ok := r.permissions[reqID]
	client := r.client
	r.mu.Unlock()
	if !ok {
		return errors.New("Request Is No Longer Pending")
	}
	chosen, cancelled, err := resolvePermissionOption(perm.acpReq.Options, optionID, decision)
	if err != nil {
		return err
	}
	if err := client.RespondPermission(reqID, chosen, cancelled); err != nil {
		return err
	}
	r.mu.Lock()
	r.removeRequestLocked(reqID)
	r.lastActivity = r.manager.nowTime()
	r.mu.Unlock()
	r.publish(true)
	r.manager.requestRefresh()
	return nil
}

func resolveElicitationOptionValue(options []acpElicitationOption, input string) string {
	for _, opt := range options {
		if opt.Value == input {
			return opt.Value
		}
	}
	for _, opt := range options {
		if opt.Label == input {
			return opt.Value
		}
	}
	return input
}

func coerceElicitationScalar(fieldType, raw string) any {
	switch strings.ToLower(strings.TrimSpace(fieldType)) {
	case "boolean":
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
		return raw
	case "integer":
		if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
			return n
		}
		return raw
	case "number":
		if f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
			return f
		}
		return raw
	default:
		return raw
	}
}

func (r *antigravityRun) answerElicitation(reqID, action string, answers map[string][]string) error {
	reqID = strings.TrimSpace(reqID)
	action = strings.ToLower(strings.TrimSpace(action))

	r.mu.Lock()
	elicit, ok := r.elicitations[reqID]
	client := r.client
	r.mu.Unlock()
	if !ok {
		return errors.New("Request Is No Longer Pending")
	}

	if action == "cancel" || action == "decline" {
		if err := client.RespondElicitation(reqID, action, nil); err != nil {
			return err
		}
		r.mu.Lock()
		r.removeRequestLocked(reqID)
		r.lastActivity = r.manager.nowTime()
		r.mu.Unlock()
		r.publish(true)
		r.manager.requestRefresh()
		return nil
	}

	if action != "" && action != "accept" {
		return errors.New("Unknown Elicitation Action")
	}

	content := map[string]any{}
	if len(elicit.acpReq.Fields) > 0 {
		for _, f := range elicit.acpReq.Fields {
			rawVals := answers[f.Name]
			cleaned := make([]string, 0, len(rawVals))
			for _, item := range rawVals {
				if strings.TrimSpace(item) != "" {
					cleaned = append(cleaned, item)
				}
			}
			if f.Required && len(cleaned) == 0 {
				return errors.New("Answer Every Required Question")
			}
			if len(cleaned) == 0 {
				continue
			}
			if f.MultiSelect || strings.EqualFold(f.Type, "array") {
				mapped := make([]string, 0, len(cleaned))
				for _, item := range cleaned {
					mapped = append(mapped, resolveElicitationOptionValue(f.Options, item))
				}
				content[f.Name] = mapped
			} else {
				val := resolveElicitationOptionValue(f.Options, cleaned[0])
				content[f.Name] = coerceElicitationScalar(f.Type, val)
			}
		}
	} else {
		for k, vals := range answers {
			if len(vals) == 1 {
				content[k] = vals[0]
			} else if len(vals) > 1 {
				content[k] = append([]string(nil), vals...)
			}
		}
	}

	if err := client.RespondElicitation(reqID, "accept", content); err != nil {
		return err
	}
	r.mu.Lock()
	r.removeRequestLocked(reqID)
	r.lastActivity = r.manager.nowTime()
	r.mu.Unlock()
	r.publish(true)
	r.manager.requestRefresh()
	return nil
}

func (r *antigravityRun) diffByKey(key string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key == "" {
		var out strings.Builder
		for _, t := range r.diskTurns {
			if d := r.diskDiffs[t.Key]; d != "" {
				out.WriteString(d)
				if !strings.HasSuffix(d, "\n") {
					out.WriteByte('\n')
				}
			}
		}
		seenTurns := map[string]bool{}
		for _, t := range r.liveTurns {
			seenTurns[t.Key] = true
			if d := r.turnDiffs[t.Key]; d != "" {
				out.WriteString(d)
				if !strings.HasSuffix(d, "\n") {
					out.WriteByte('\n')
				}
			}
		}
		// Include any orphan tool diffs whose turn key was not in liveTurns.
		var orphanKeys []string
		for tid := range r.toolDiffs {
			tk := r.toolCallTurn[tid]
			if tk == "" || !seenTurns[tk] {
				orphanKeys = append(orphanKeys, tid)
			}
		}
		sort.Strings(orphanKeys)
		for _, tid := range orphanKeys {
			d := r.toolDiffs[tid]
			out.WriteString(d)
			if !strings.HasSuffix(d, "\n") {
				out.WriteByte('\n')
			}
		}
		return out.String(), out.Len() > 0
	}
	if d, ok := r.toolDiffs[key]; ok && d != "" {
		return d, true
	}
	if d, ok := r.turnDiffs[key]; ok && d != "" {
		return d, true
	}
	if d, ok := r.diskDiffs[key]; ok && d != "" {
		return d, true
	}
	return "", false
}
