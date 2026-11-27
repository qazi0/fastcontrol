package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func (m *GrokManager) handle(c *Client, raw []byte) {
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	op, id := str(v, "op"), str(v, "id")
	ack := func(err error, extra map[string]any) {
		resp := map[string]any{
			"t":     "ack",
			"op":    op,
			"id":    id,
			"nonce": v["nonce"],
			"ok":    err == nil,
		}
		if err != nil {
			resp["msg"] = err.Error()
		}
		for k, x := range extra {
			resp[k] = x
		}
		m.emit(c, resp)
	}

	// Strict read-only ingress guard FIRST on every Grok op.
	if err := m.rejectSubagentControl(id, op); err != nil {
		ack(err, nil)
		return
	}

	switch op {
	case "threads":
		m.mu.Lock()
		cached := append([]byte(nil), m.lastList...)
		m.mu.Unlock()
		if len(cached) > 0 {
			c.push(cached)
			m.requestRefresh()
		} else {
			go func() {
				m.refresh()
				m.mu.Lock()
				b := append([]byte(nil), m.lastList...)
				m.mu.Unlock()
				if len(b) > 0 {
					c.push(b)
				}
			}()
		}

	case "projects":
		go m.handleProjects(c, "")

	case "project":
		go m.handleProjects(c, str(v, "cwd"))

	case "group":
		go func() {
			m.refresh()
			threads, err := m.organizationGroup(id)
			ack(err, map[string]any{"threads": threads})
		}()

	case "models":
		go m.handleModels(c)

	case "open", "more":
		m.mu.Lock()
		_, isWork := m.workWatch[c][id]
		m.mu.Unlock()
		if isWork {
			ack(m.moreWork(c, id, op == "more"), nil)
			return
		}
		gen := m.reserveWatchGen(c, id)
		go m.handleOpen(c, id, op == "more", gen, ack)

	case "close":
		now := m.nowTime()
		var unwatchedRun *grokRun
		m.mu.Lock()
		if m.watch[c] != nil {
			delete(m.watch[c], id)
		}
		if m.watchGen[c] != nil {
			delete(m.watchGen[c], id)
		}
		if m.workWatch[c] != nil {
			delete(m.workWatch[c], id)
		}
		if id != "" && !m.hasWatchersLocked(id) {
			unwatchedRun = m.runs[id]
		}
		m.mu.Unlock()
		if unwatchedRun != nil {
			unwatchedRun.markUnwatched(now)
		}

	case "diff":
		go m.handleDiff(c, id, str(v, "key"), ack)

	case "new":
		go m.handleNew(v, ack)

	case "send":
		go m.handleSend(id, str(v, "text"), stringArray(v["images"]), ack)

	case "compact":
		go m.handleCompact(id, ack)

	case "stop":
		go m.handleStop(id, ack)

	case "wake":
		go m.handleWake(c, id, ack)

	case "settings":
		go m.handleSettings(c, id, raw, ack)

	case "approve":
		feedback := grokFirstNonEmpty(str(v, "feedback"), str(v, "text"))
		go m.handleApprove(id, str(v, "reqId"), str(v, "optionId"), str(v, "decision"), feedback, ack)

	case "answer":
		go m.handleAnswer(id, v, ack)

	case "rename":
		go m.handleRename(id, str(v, "text"), ack)

	case "fork":
		go m.handleFork(id, ack)

	case "archive", "unarchive":
		go m.handleArchive(id, op == "archive", ack)

	case "pin", "unpin":
		go m.handlePin(id, op == "pin", ack)

	case "agents":
		go func() {
			if len(m.snapshotRecords()) == 0 {
				m.refresh()
			}
			agents := grokListAgents(m.snapshotRecords(), m.grokRoot(), id, m.isRunActive)
			ack(nil, map[string]any{"agents": agents})
		}()

	case "agentOpen":
		agent, err := m.openWork(c, str(v, "parent"), id, false)
		extra := map[string]any(nil)
		if err == nil {
			extra = map[string]any{"agent": agent}
		}
		ack(err, extra)

	default:
		ack(errors.New("Unsupported Grok Operation"), nil)
	}
}

func (m *GrokManager) rejectSubagentControl(id, op string) error {
	if !grokSubagentControlOp(op) || strings.TrimSpace(id) == "" {
		return nil
	}
	m.mu.Lock()
	_, known := m.records[id]
	m.mu.Unlock()
	if !known {
		m.refresh()
	}
	return grokRejectSubagentControl(m.snapshotRecords(), id, op)
}

func (m *GrokManager) handleProjects(c *Client, cwd string) {
	records := m.snapshotRecords()
	if len(records) == 0 {
		m.refresh()
		records = m.snapshotRecords()
	}
	if cwd != "" {
		list, _ := grokBuildOrganization(false, m.state, records)
		m.mu.Lock()
		runs := make(map[string]*grokRun, len(m.runs))
		for id, r := range m.runs {
			runs[id] = r
		}
		m.mu.Unlock()
		threads := []ThreadInfo{}
		for _, t := range list {
			if t.Cwd != cwd {
				continue
			}
			if r := runs[t.ID]; r != nil {
				st := r.listState()
				t.Live = st.alive
				t.Running = st.running
				t.Attention = st.attention
				if st.turnAt > 0 {
					t.TurnAt = st.turnAt
				}
			}
			threads = append(threads, t)
		}
		m.emit(c, map[string]any{"t": "project", "cwd": cwd, "threads": threads})
		return
	}
	projects := grokProjects(records, m.state)
	home, _ := os.UserHomeDir()
	m.emit(c, map[string]any{"t": "projects", "projects": projects, "home": home})
}

func (m *GrokManager) organizationGroup(id string) ([]ThreadInfo, error) {
	m.mu.Lock()
	s := m.orgState
	data := m.lastList
	m.mu.Unlock()
	ids, ok := organizationMembers(s, id)
	if !ok {
		return nil, errors.New("This Group Is No Longer Available")
	}
	addOrganizationParents(ids, s)
	var list struct {
		Threads []ThreadInfo `json:"threads"`
	}
	if json.Unmarshal(data, &list) != nil {
		return nil, errors.New("Sessions Are Still Loading")
	}
	threads := []ThreadInfo{}
	for _, t := range list.Threads {
		if ids[t.ID] {
			threads = append(threads, t)
		}
	}
	return threads, nil
}

func normalizeGrokModelsEfforts(models []Model) []Model {
	if len(models) == 0 {
		return models
	}
	out := make([]Model, len(models))
	for i, model := range models {
		model.Efforts = sortGrokEfforts(model.Efforts)
		if def := strings.ToLower(strings.TrimSpace(model.DefaultEffort)); def != "" {
			model.DefaultEffort = def
		}
		out[i] = model
	}
	return out
}

func (m *GrokManager) discoverModels(ctx context.Context) ([]Model, grokDefaults, error) {
	m.modelsMu.Lock()
	defer m.modelsMu.Unlock()

	cachedModels, defaults, _ := grokReadModelsCache(m.grokRoot())
	cachedModels = normalizeGrokModelsEfforts(cachedModels)

	now := m.nowTime()
	cat := m.catalogSnapshot()
	if cat.LiveLoaded && len(cat.Models) > 0 && now.Sub(cat.UpdatedAt) < m.modelsTTL() {
		merged := normalizeGrokModelsEfforts(mergeGrokModels(cat.Models, cachedModels))
		def := defaults
		if def.Model == "" {
			def.Model = cat.DefaultModel
		}
		if def.Model == "" && len(merged) > 0 {
			def.Model = merged[0].Slug
		}
		if def.Effort == "" {
			def.Effort = cat.DefaultEffort
		}
		if def.Effort == "" {
			def.Effort = grokDefaultEffortForModel(merged, def.Model)
		}
		if def.PermissionMode == "" {
			def.PermissionMode = grokFirstNonEmpty(cat.DefaultMode, "default")
		}
		return merged, def, nil
	}

	if err := m.requireACP(); err != nil {
		if len(cachedModels) > 0 {
			return cachedModels, defaults, nil
		}
		return []Model{}, defaults, err
	}
	if err := m.requireAuth(); err != nil {
		if len(cachedModels) > 0 {
			return cachedModels, defaults, nil
		}
		return []Model{}, defaults, err
	}

	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	daemon, err := m.ensureDaemon(probeCtx, grokDaemonKey{alwaysApprove: false})
	if err != nil {
		if len(cachedModels) > 0 {
			return cachedModels, defaults, nil
		}
		return []Model{}, defaults, err
	}

	var liveState *grokModelsState
	if ms, listErr := daemon.client.ListModels(probeCtx); listErr == nil && ms != nil && len(ms.AvailableModels) > 0 {
		liveState = ms
	} else if initRes := daemon.client.InitResult(); initRes != nil && len(initRes.ModelState.AvailableModels) > 0 {
		msCopy := initRes.ModelState
		liveState = &msCopy
	} else if daemon.initRes != nil && len(daemon.initRes.ModelState.AvailableModels) > 0 {
		msCopy := daemon.initRes.ModelState
		liveState = &msCopy
	} else if listErr != nil && len(cachedModels) == 0 {
		return []Model{}, defaults, listErr
	}
	daemon.touchActivity(m.nowTime())

	if liveState != nil {
		m.updateCatalogFromModelsState(liveState)
	}
	cat = m.catalogSnapshot()
	merged := normalizeGrokModelsEfforts(mergeGrokModels(cat.Models, cachedModels))
	if len(merged) > 0 {
		m.mu.Lock()
		m.catalog.Models = append([]Model(nil), merged...)
		if m.catalog.DefaultModel == "" {
			m.catalog.DefaultModel = grokFirstNonEmpty(defaults.Model, merged[0].Slug)
		}
		if m.catalog.DefaultEffort == "" {
			m.catalog.DefaultEffort = grokFirstNonEmpty(defaults.Effort, grokDefaultEffortForModel(merged, m.catalog.DefaultModel))
		}
		if m.catalog.DefaultMode == "" {
			m.catalog.DefaultMode = grokFirstNonEmpty(defaults.PermissionMode, "default")
		}
		m.catalog.LiveLoaded = liveState != nil
		m.catalog.UpdatedAt = m.nowTime()
		m.mu.Unlock()
	}

	def := defaults
	if def.Model == "" {
		def.Model = cat.DefaultModel
	}
	if def.Model == "" && len(merged) > 0 {
		def.Model = merged[0].Slug
	}
	if def.Effort == "" {
		def.Effort = cat.DefaultEffort
	}
	if def.Effort == "" {
		def.Effort = grokDefaultEffortForModel(merged, def.Model)
	}
	if def.PermissionMode == "" {
		def.PermissionMode = grokFirstNonEmpty(cat.DefaultMode, "default")
	}
	return merged, def, nil
}

func grokDefaultEffortForModel(models []Model, slug string) string {
	for _, model := range models {
		if model.Slug == slug && model.DefaultEffort != "" {
			return model.DefaultEffort
		}
	}
	if len(models) > 0 {
		return models[0].DefaultEffort
	}
	return ""
}

func (m *GrokManager) handleModels(c *Client) {
	models, defaults, err := m.discoverModels(m.baseContext())
	if models == nil {
		models = []Model{}
	}
	defaultMode := normalizeGrokPermissionMode(defaults.PermissionMode)
	if defaultMode == "" || !m.validPermissionMode(defaultMode) {
		defaultMode = "default"
	}
	result := map[string]any{
		"t":      "models",
		"models": models,
		"default": map[string]any{
			"model":  defaults.Model,
			"effort": defaults.Effort,
			"mode":   defaultMode,
		},
		"modes":           m.defaultModes(),
		"allowFullAccess": m.allowFullAccess,
	}
	if err != nil {
		result["error"] = err.Error()
	}
	m.emit(c, result)
}

func (m *GrokManager) snapshotRunForEmit(id string, run *grokRun) (Meta, []Turn) {
	meta, turns := run.snapshot()
	pref := m.state.Pref(id)
	if meta.Model == "" && pref.Model != "" {
		meta.Model = pref.Model
	}
	if meta.PermissionMode == "" {
		meta.PermissionMode = grokFirstNonEmpty(normalizeGrokPermissionMode(pref.PermissionMode), "default")
	}
	return meta, turns
}

func (m *GrokManager) buildDiskOpenMeta(id string, rec grokRecord, view *grokTranscript, connecting bool) Meta {
	var meta Meta
	if view != nil {
		meta = view.Meta
	}
	if custom := m.state.Title(id); custom != "" {
		meta.Title = custom
	} else if meta.Title == "" {
		meta.Title = grokRecordTitle(rec, "")
	}
	if meta.Cwd == "" {
		meta.Cwd = rec.Cwd
	}
	if meta.Branch == "" {
		meta.Branch = rec.HeadBranch
	}
	if meta.Model == "" {
		meta.Model = rec.Model
	}
	if meta.Effort == "" {
		meta.Effort = rec.ReasoningEffort
	}
	pref := m.state.Pref(id)
	if pref.Model != "" {
		meta.Model = pref.Model
	}
	if pref.Effort != "" {
		meta.Effort = pref.Effort
	}
	if pref.PermissionMode != "" {
		meta.PermissionMode = normalizeGrokPermissionMode(pref.PermissionMode)
	}
	if meta.PermissionMode == "" {
		meta.PermissionMode = "default"
	}
	meta.Provider = "grok"
	if rec.External {
		meta.Mode = "external"
	} else {
		meta.Mode = "tail"
	}
	meta.Connecting = connecting
	meta.Running = rec.Running
	return meta
}

func (m *GrokManager) handleOpen(c *Client, id string, more bool, gen uint64, ack func(error, map[string]any)) {
	if strings.TrimSpace(id) == "" {
		m.clearWatchGen(c, id, gen)
		ack(errors.New("Grok Session Not Found"), nil)
		return
	}
	m.mu.Lock()
	run := m.runs[id]
	rec, exists := m.records[id]
	m.mu.Unlock()

	if run == nil && !exists {
		m.refresh()
		m.mu.Lock()
		run = m.runs[id]
		rec, exists = m.records[id]
		m.mu.Unlock()
	}
	if (exists && grokRecordIsSubagent(rec)) || grokIsSubagent(m.snapshotRecords(), id) {
		m.clearWatchGen(c, id, gen)
		ack(errors.New("Open This Subagent From Its Parent Chat"), nil)
		return
	}
	if run == nil && !exists {
		m.clearWatchGen(c, id, gen)
		ack(errors.New("Grok Session Not Found"), nil)
		return
	}

	window := grokDefaultWindow
	if m.hub != nil {
		m.hub.mu.Lock()
		if !m.hub.clients[c] {
			m.hub.mu.Unlock()
			ack(nil, nil)
			return
		}
	}
	m.mu.Lock()
	if m.shuttingDown || m.watchGen[c] == nil || m.watchGen[c][id] != gen {
		m.mu.Unlock()
		if m.hub != nil {
			m.hub.mu.Unlock()
		}
		ack(nil, nil)
		return
	}
	run = m.runs[id]
	rec, exists = m.records[id]
	if exists && grokRecordIsSubagent(rec) {
		delete(m.watchGen[c], id)
		m.mu.Unlock()
		if m.hub != nil {
			m.hub.mu.Unlock()
		}
		ack(errors.New("Open This Subagent From Its Parent Chat"), nil)
		return
	}
	if m.watch == nil {
		m.watch = map[*Client]map[string]int{}
	}
	if m.watch[c] == nil {
		m.watch[c] = map[string]int{}
	}
	if prev := m.watch[c][id]; prev > 0 {
		window = prev
	}
	if more {
		window += grokDefaultWindow
	}
	m.watch[c][id] = window
	m.mu.Unlock()
	if m.hub != nil {
		m.hub.mu.Unlock()
	}

	// Live fast-path: session already active on a pooled daemon.
	if run != nil && run.isAlive() {
		run.touch()
		if more && exists && rec.SessionDir != "" {
			if hist, err := readGrokTranscriptForRecord(rec, window); err == nil {
				run.mergeOlderDiskHistory(hist)
			}
		}
		if exists {
			stamp := m.computeWatchStamp(rec)
			m.mu.Lock()
			if m.watchedStamp == nil {
				m.watchedStamp = map[string]grokWatchStamp{}
			}
			m.watchedStamp[id] = stamp
			m.mu.Unlock()
		}
		if m.isWatchCurrent(c, id, gen) {
			meta, turns := m.snapshotRunForEmit(id, run)
			m.emit(c, map[string]any{"t": "open", "id": id, "meta": meta, "turns": turns, "more": more})
		}
		ack(nil, nil)
		return
	}

	// External TUI session active in active_sessions.json: return Mode "external"
	// cleanly so the UI offers "Take Over" instead of fighting a live terminal TUI.
	if exists && rec.External {
		view, err := m.readRecordTranscript(rec, window)
		if err != nil {
			ack(err, nil)
			return
		}
		meta := m.buildDiskOpenMeta(id, rec, view, false)
		stamp := m.computeWatchStamp(rec)
		m.mu.Lock()
		if m.watchedStamp == nil {
			m.watchedStamp = map[string]grokWatchStamp{}
		}
		m.watchedStamp[id] = stamp
		m.mu.Unlock()
		if m.isWatchCurrent(c, id, gen) {
			m.emit(c, map[string]any{"t": "open", "id": id, "meta": meta, "turns": view.Turns, "more": more})
		}
		ack(nil, nil)
		return
	}

	// Read-only tail fallback when ACP or auth is unavailable on this computer.
	if !m.canDialACP() {
		view, err := m.readRecordTranscript(rec, window)
		if err != nil {
			ack(err, nil)
			return
		}
		meta := m.buildDiskOpenMeta(id, rec, view, false)
		stamp := m.computeWatchStamp(rec)
		m.mu.Lock()
		if m.watchedStamp == nil {
			m.watchedStamp = map[string]grokWatchStamp{}
		}
		m.watchedStamp[id] = stamp
		m.mu.Unlock()
		if m.isWatchCurrent(c, id, gen) {
			m.emit(c, map[string]any{"t": "open", "id": id, "meta": meta, "turns": view.Turns, "more": more})
		}
		ack(nil, nil)
		return
	}

	// ACP + auth available: auto-activate in background, waiting up to
	// openFastWait (150ms) so warm resumes emit a single live frame.
	type openAttachResult struct {
		run *grokRun
		err error
	}
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		ack(errors.New("Relay Is Shutting Down"), nil)
		return
	}
	baseCtx := m.baseContextLocked()
	m.bgWG.Add(1)
	m.mu.Unlock()

	attachCh := make(chan openAttachResult, 1)
	go func() {
		defer m.bgWG.Done()
		attachCtx, cancel := context.WithTimeout(baseCtx, defaultGrokCallTimeout)
		defer cancel()
		r, err := m.ensureRun(attachCtx, id)
		attachCh <- openAttachResult{run: r, err: err}
	}()

	fastTimer := time.NewTimer(m.openFastWaitDuration())
	select {
	case res := <-attachCh:
		if !fastTimer.Stop() {
			select {
			case <-fastTimer.C:
			default:
			}
		}
		if res.err == nil && res.run != nil {
			if window > grokDefaultWindow && exists && rec.SessionDir != "" {
				if hist, err := readGrokTranscriptForRecord(rec, window); err == nil {
					res.run.mergeOlderDiskHistory(hist)
				}
			}
			if exists {
				stamp := m.computeWatchStamp(rec)
				m.mu.Lock()
				if m.watchedStamp == nil {
					m.watchedStamp = map[string]grokWatchStamp{}
				}
				m.watchedStamp[id] = stamp
				m.mu.Unlock()
			}
			if m.isWatchCurrent(c, id, gen) {
				res.run.touch()
				meta, turns := m.snapshotRunForEmit(id, res.run)
				m.emit(c, map[string]any{"t": "open", "id": id, "meta": meta, "turns": turns, "more": more})
			} else {
				m.markRunUnwatchedIfIdle(id, res.run)
			}
			ack(nil, nil)
			return
		}

		// Attach failed quickly: fall back cleanly to disk tail.
		view, err := m.readRecordTranscript(rec, window)
		if err != nil {
			ack(err, nil)
			return
		}
		meta := m.buildDiskOpenMeta(id, rec, view, false)
		stamp := m.computeWatchStamp(rec)
		m.mu.Lock()
		if m.watchedStamp == nil {
			m.watchedStamp = map[string]grokWatchStamp{}
		}
		m.watchedStamp[id] = stamp
		m.mu.Unlock()
		if m.isWatchCurrent(c, id, gen) {
			m.emit(c, map[string]any{"t": "open", "id": id, "meta": meta, "turns": view.Turns, "more": more})
		}
		ack(nil, nil)
		return

	case <-fastTimer.C:
		// Cold/slow attach (>150ms): emit disk transcript immediately with
		// Connecting: true, then upgrade to live once attach completes.
		view, err := m.readRecordTranscript(rec, window)
		if err != nil {
			ack(err, nil)
			return
		}
		meta := m.buildDiskOpenMeta(id, rec, view, true)
		stamp := m.computeWatchStamp(rec)
		m.mu.Lock()
		if m.watchedStamp == nil {
			m.watchedStamp = map[string]grokWatchStamp{}
		}
		m.watchedStamp[id] = stamp
		m.mu.Unlock()
		if m.isWatchCurrent(c, id, gen) {
			m.emit(c, map[string]any{"t": "open", "id": id, "meta": meta, "turns": view.Turns, "more": more})
		}
		ack(nil, nil)

		select {
		case res := <-attachCh:
			if !m.isWatchCurrent(c, id, gen) {
				if res.run != nil {
					m.markRunUnwatchedIfIdle(id, res.run)
				}
				return
			}
			if res.err == nil && res.run != nil {
				m.mu.Lock()
				curWindow := m.watch[c][id]
				m.mu.Unlock()
				if curWindow > grokDefaultWindow && exists && rec.SessionDir != "" {
					if hist, readErr := readGrokTranscriptForRecord(rec, curWindow); readErr == nil {
						res.run.mergeOlderDiskHistory(hist)
					}
				}
				res.run.touch()
				if exists {
					stamp := m.computeWatchStamp(rec)
					m.mu.Lock()
					if m.watchedStamp == nil {
						m.watchedStamp = map[string]grokWatchStamp{}
					}
					m.watchedStamp[id] = stamp
					m.mu.Unlock()
				}
				if m.isWatchCurrent(c, id, gen) {
					liveMeta, turns := m.snapshotRunForEmit(id, res.run)
					m.emit(c, map[string]any{"t": "open", "id": id, "meta": liveMeta, "turns": turns, "more": false})
				}
				return
			}
			fallbackMeta := m.buildDiskOpenMeta(id, rec, view, false)
			if m.isWatchCurrent(c, id, gen) {
				m.emit(c, map[string]any{"t": "meta", "id": id, "meta": fallbackMeta})
			}
		case <-baseCtx.Done():
			return
		}

	case <-baseCtx.Done():
		if !fastTimer.Stop() {
			select {
			case <-fastTimer.C:
			default:
			}
		}
		ack(errors.New("Relay Is Shutting Down"), nil)
		return
	}
}

func (m *GrokManager) markRunUnwatchedIfIdle(id string, run *grokRun) {
	if run == nil {
		return
	}
	m.mu.Lock()
	watched := m.hasWatchersLocked(id)
	m.mu.Unlock()
	if !watched {
		run.markUnwatched(m.nowTime())
	}
}

func (m *GrokManager) emitTailOpen(c *Client, id string, more bool) error {
	if m.hub != nil {
		m.hub.mu.Lock()
		connected := m.hub.clients[c]
		m.hub.mu.Unlock()
		if !connected {
			return nil
		}
	}
	m.mu.Lock()
	if m.shuttingDown || m.watch[c] == nil {
		m.mu.Unlock()
		return nil
	}
	window, watching := m.watch[c][id]
	rec, exists := m.records[id]
	run := m.runs[id]
	m.mu.Unlock()
	if !watching || !exists || grokRecordIsSubagent(rec) || (run != nil && run.isAlive()) {
		return nil
	}
	if window <= 0 {
		window = grokDefaultWindow
	}
	view, err := m.readRecordTranscript(rec, window)
	if err != nil {
		return err
	}
	meta := m.buildDiskOpenMeta(id, rec, view, false)
	if m.isWatchCurrent(c, id, 0) {
		m.emit(c, map[string]any{"t": "open", "id": id, "meta": meta, "turns": view.Turns, "more": more})
	}
	return nil
}

func (m *GrokManager) emitCurrentOpen(c *Client, id string, more bool) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("Grok Session Not Found")
	}
	m.mu.Lock()
	run := m.runs[id]
	rec, exists := m.records[id]
	m.mu.Unlock()
	if run == nil && !exists {
		m.refresh()
		m.mu.Lock()
		run = m.runs[id]
		rec, exists = m.records[id]
		m.mu.Unlock()
	}
	if (exists && grokRecordIsSubagent(rec)) || grokIsSubagent(m.snapshotRecords(), id) {
		return errors.New("Open This Subagent From Its Parent Chat")
	}
	if run == nil && !exists {
		return errors.New("Grok Session Not Found")
	}

	window := grokDefaultWindow
	if m.hub != nil {
		m.hub.mu.Lock()
		if !m.hub.clients[c] {
			m.hub.mu.Unlock()
			return nil
		}
	}
	m.mu.Lock()
	run = m.runs[id]
	rec, exists = m.records[id]
	if exists && grokRecordIsSubagent(rec) {
		m.mu.Unlock()
		if m.hub != nil {
			m.hub.mu.Unlock()
		}
		return errors.New("Open This Subagent From Its Parent Chat")
	}
	if m.watch == nil {
		m.watch = map[*Client]map[string]int{}
	}
	if m.watch[c] == nil {
		m.watch[c] = map[string]int{}
	}
	if prev := m.watch[c][id]; prev > 0 {
		window = prev
	}
	if more {
		window += grokDefaultWindow
	}
	m.watch[c][id] = window
	m.mu.Unlock()
	if m.hub != nil {
		m.hub.mu.Unlock()
	}

	if run != nil && run.isAlive() {
		run.touch()
		if more && exists && rec.SessionDir != "" {
			if hist, err := readGrokTranscriptForRecord(rec, window); err == nil {
				run.mergeOlderDiskHistory(hist)
			}
		}
		if exists {
			stamp := m.computeWatchStamp(rec)
			m.mu.Lock()
			if m.watchedStamp == nil {
				m.watchedStamp = map[string]grokWatchStamp{}
			}
			m.watchedStamp[id] = stamp
			m.mu.Unlock()
		}
		meta, turns := m.snapshotRunForEmit(id, run)
		m.emit(c, map[string]any{"t": "open", "id": id, "meta": meta, "turns": turns, "more": more})
		return nil
	}

	view, err := m.readRecordTranscript(rec, window)
	if err != nil {
		return err
	}
	meta := m.buildDiskOpenMeta(id, rec, view, false)
	stamp := m.computeWatchStamp(rec)
	m.mu.Lock()
	if m.watchedStamp == nil {
		m.watchedStamp = map[string]grokWatchStamp{}
	}
	m.watchedStamp[id] = stamp
	m.mu.Unlock()

	m.emit(c, map[string]any{"t": "open", "id": id, "meta": meta, "turns": view.Turns, "more": more})
	return nil
}

func (m *GrokManager) readRecordTranscript(rec grokRecord, window int) (*grokTranscript, error) {
	if rec.SessionDir != "" {
		if st, err := os.Stat(rec.SessionDir); err == nil && st.IsDir() {
			return readGrokTranscriptForRecord(rec, window)
		}
	}
	v := newGrokTranscript()
	v.Meta.Title = grokRecordTitle(rec, "")
	v.Meta.Cwd = rec.Cwd
	v.Meta.Branch = rec.HeadBranch
	v.Meta.Model = rec.Model
	v.Meta.Effort = rec.ReasoningEffort
	v.Meta.CtxUsed = rec.CtxUsed
	v.Meta.CtxMax = rec.CtxMax
	v.Meta.Running = rec.Running
	if rec.External {
		v.Meta.Mode = "external"
	} else {
		v.Meta.Mode = "tail"
	}
	return v, nil
}

func (m *GrokManager) handleDiff(c *Client, id, key string, ack func(error, map[string]any)) {
	m.mu.Lock()
	run := m.runs[id]
	rec, hasRec := m.records[id]
	workWatch, isWork := m.workWatch[c][id]
	m.mu.Unlock()

	if isWork {
		view, err := grokOpenAgentTranscript(m.snapshotRecords(), workWatch.Parent, id, workWatch.Window)
		if err != nil {
			ack(err, nil)
			return
		}
		text := clipHead(view.Diff(key), 2<<20)
		m.emit(c, map[string]any{"t": "diff", "id": id, "key": key, "diff": text, "text": text})
		return
	}
	if run == nil && !hasRec && strings.TrimSpace(id) != "" {
		m.refresh()
		m.mu.Lock()
		run = m.runs[id]
		rec, hasRec = m.records[id]
		m.mu.Unlock()
	}
	if (hasRec && grokRecordIsSubagent(rec)) || grokIsSubagent(m.snapshotRecords(), id) {
		ack(errors.New("Open This Subagent From Its Parent Chat"), nil)
		return
	}

	var text string
	if run != nil {
		if d, ok := run.diffByKey(key); ok {
			text = d
		}
	}
	if text == "" && hasRec && rec.SessionDir != "" {
		if view, err := readGrokTranscriptForRecord(rec, 100); err == nil {
			text = view.Diff(key)
		}
	}
	text = clipHead(text, 2<<20)
	m.emit(c, map[string]any{"t": "diff", "id": id, "key": key, "diff": text, "text": text})
}

func buildGrokPromptBlocks(text string, images []string) ([]acpContentBlock, error) {
	text = strings.TrimSpace(text)
	if text == "" && len(images) == 0 {
		return nil, errors.New("Message Is Empty")
	}
	if len(images) > 12 {
		return nil, errors.New("Choose Up To 12 Images")
	}
	if len(images) > 0 {
		if err := checkUploads(images); err != nil {
			return nil, err
		}
	}
	blocks := make([]acpContentBlock, 0, 1+len(images))
	if text != "" {
		blocks = append(blocks, acpTextBlock(text))
	}
	for _, path := range images {
		st, err := os.Lstat(path)
		if err != nil || !st.Mode().IsRegular() || st.Size() > maxImageBytes {
			return nil, errors.New("Uploaded Image Is Too Large Or Unavailable")
		}
		head := make([]byte, 512)
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		n, _ := f.Read(head)
		_ = f.Close()
		if imageMIME(head[:n]) == "" {
			return nil, errors.New("Unsupported Image Format")
		}
		blk, err := acpImageBlockFromFile(path)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, blk)
	}
	return blocks, nil
}

func grokSessionOptionsForMode(mode string) grokSessionOptions {
	switch normalizeGrokPermissionMode(mode) {
	case "bypassPermissions":
		return grokSessionOptions{YoloMode: true}
	case "auto":
		return grokSessionOptions{AutoMode: true}
	default:
		return grokSessionOptions{}
	}
}

func (m *GrokManager) handleNew(v map[string]any, ack func(error, map[string]any)) {
	if err := m.requireACP(); err != nil {
		ack(err, nil)
		return
	}
	if err := m.requireAuth(); err != nil {
		ack(err, nil)
		return
	}
	cwd := strings.TrimSpace(str(v, "cwd"))
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		ack(errors.New("Project Folder Is Unavailable"), nil)
		return
	}
	text := str(v, "text")
	images := stringArray(v["images"])
	blocks, err := buildGrokPromptBlocks(text, images)
	if err != nil {
		ack(err, nil)
		return
	}

	model := strings.TrimSpace(str(v, "model"))
	effort := strings.TrimSpace(str(v, "effort"))
	permMode := normalizeGrokPermissionMode(str(v, "permissionMode"))
	if permMode == "" {
		permMode = "default"
	}
	if effort != "" && !m.validEffort(effort) {
		ack(errors.New("Unknown Grok Effort"), nil)
		return
	}
	if !m.validPermissionMode(permMode) {
		ack(errors.New("Unknown Grok Execution Mode"), nil)
		return
	}

	ctx, cancel := context.WithTimeout(m.baseContext(), defaultGrokCallTimeout)
	defer cancel()

	daemon, err := m.ensureDaemon(ctx, grokDaemonKey{
		alwaysApprove: permMode == "bypassPermissions" && m.allowFullAccess,
	})
	if err != nil {
		ack(err, nil)
		return
	}
	client := daemon.client

	daemon.beginNewSession()
	sessionState, err := client.NewSession(ctx, cwd, grokSessionOptionsForMode(permMode))
	if err != nil || sessionState == nil || strings.TrimSpace(sessionState.SessionID) == "" {
		daemon.endNewSession("")
		if err == nil {
			err = errors.New("Grok Did Not Return A Session ID")
		}
		ack(err, nil)
		return
	}
	sid := strings.TrimSpace(sessionState.SessionID)
	daemon.touchActivity(m.nowTime())

	run := newGrokRun(m, sid, cwd)
	run.mu.Lock()
	run.daemon = daemon
	run.client = client
	run.permissionMode = permMode
	if run.transcript != nil {
		run.transcript.Meta.PermissionMode = permMode
	}
	run.mu.Unlock()

	if !daemon.attachRun(sid, run) {
		daemon.endNewSession("")
		ack(errors.New("Grok Daemon Closed"), nil)
		return
	}
	daemon.endNewSession(sid)

	daemon.drainDispatchForSession(ctx, sid)
	run.applySessionState(sessionState)
	m.updateCatalogFromSessionState(sessionState, daemon.initRes)

	if err := m.applySessionPreferences(ctx, run, sid, model, effort, permMode, true, true); err != nil {
		daemon.detachRun(sid, run)
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = client.CloseSession(closeCtx, sid)
		closeCancel()
		ack(err, nil)
		return
	}

	m.mu.Lock()
	if m.shuttingDown || !run.isAlive() {
		shuttingDown := m.shuttingDown
		m.mu.Unlock()
		daemon.detachRun(sid, run)
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = client.CloseSession(closeCtx, sid)
		closeCancel()
		if shuttingDown {
			ack(errors.New("Relay Is Shutting Down"), nil)
		} else {
			ack(errors.New("Grok Session Has Stopped"), nil)
		}
		return
	}
	m.runs[sid] = run
	m.mu.Unlock()

	_ = m.state.SetPref(sid, grokThreadPref{
		Model:          model,
		Effort:         effort,
		PermissionMode: permMode,
	})

	if _, err := run.startPrompt(strings.TrimSpace(text), images, blocks); err != nil {
		ack(err, map[string]any{"thread": sid})
		return
	}

	ack(nil, map[string]any{"thread": sid})
	m.requestRefresh()
}

func (m *GrokManager) ensureRun(ctx context.Context, id string) (*grokRun, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("Grok Session Not Found")
	}
	unlock := m.lockThread(id)
	defer unlock()

	m.mu.Lock()
	if existing := m.runs[id]; existing != nil && existing.isAlive() {
		m.mu.Unlock()
		return existing, nil
	}
	if m.shuttingDown {
		m.mu.Unlock()
		return nil, errors.New("Relay Is Shutting Down")
	}
	rec, exists := m.records[id]
	m.mu.Unlock()

	if !exists {
		m.refresh()
		m.mu.Lock()
		rec, exists = m.records[id]
		m.mu.Unlock()
	}
	if (exists && grokRecordIsSubagent(rec)) || grokIsSubagent(m.snapshotRecords(), id) {
		return nil, errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent")
	}
	if !exists {
		return nil, errors.New("Grok Session Not Found")
	}
	if err := m.requireACP(); err != nil {
		return nil, err
	}
	if err := m.requireAuth(); err != nil {
		return nil, err
	}

	cwd := rec.Cwd
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	} else if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		cwd, _ = os.UserHomeDir()
	}

	pref := m.state.Pref(id)
	permMode := normalizeGrokPermissionMode(pref.PermissionMode)
	if permMode == "" {
		permMode = "default"
	}

	run := newGrokRun(m, id, cwd)
	if hist, err := m.readRecordTranscript(rec, grokDefaultWindow); err == nil {
		run.seedFromTranscript(hist)
	}
	run.mu.Lock()
	run.loading = true
	run.externalRunning = rec.Running
	if run.title == "" {
		run.title = grokRecordTitle(rec, "")
	}
	if run.branch == "" {
		run.branch = rec.HeadBranch
	}
	if permMode != "" {
		run.permissionMode = permMode
		if run.transcript != nil {
			run.transcript.Meta.PermissionMode = permMode
		}
	}
	run.mu.Unlock()

	daemon, err := m.ensureDaemon(ctx, grokDaemonKey{
		alwaysApprove: permMode == "bypassPermissions" && m.allowFullAccess,
	})
	if err != nil {
		return nil, err
	}
	client := daemon.client

	run.mu.Lock()
	run.daemon = daemon
	run.client = client
	run.mu.Unlock()

	if !daemon.attachRun(id, run) {
		return nil, errors.New("Grok Daemon Closed")
	}
	attached := true
	defer func() {
		if attached {
			daemon.detachRun(id, run)
		}
	}()

	opts := grokSessionOptionsForMode(permMode)
	supportsResume := daemon.initRes == nil || daemon.initRes.AgentCapabilities.SessionCapabilities.Resume

	var state *grokSessionState
	if supportsResume {
		state, err = client.ResumeSession(ctx, id, cwd, opts)
		if err != nil {
			loaded, loadErr := client.LoadSession(ctx, id, cwd, opts)
			if loadErr != nil {
				return nil, loadErr
			}
			state = loaded
		}
	} else {
		state, err = client.LoadSession(ctx, id, cwd, opts)
		if err != nil {
			resumed, resumeErr := client.ResumeSession(ctx, id, cwd, opts)
			if resumeErr != nil {
				return nil, err
			}
			state = resumed
		}
	}
	daemon.touchActivity(m.nowTime())

	// Drain all replay notifications emitted during ResumeSession / LoadSession
	// before flipping loading to false.
	daemon.drainDispatchForSession(ctx, id)

	run.mu.Lock()
	run.loading = false
	if run.transcript == nil {
		run.transcript = newGrokTranscript()
	}
	if len(run.transcript.Turns) == 0 && run.replayTranscript != nil && len(run.replayTranscript.Turns) > 0 {
		run.transcript.Turns = cloneTurns(run.replayTranscript.Turns)
		run.mergeDiffMapsLocked(run.replayTranscript)
		run.rebuildTranscriptIndexesLocked()
		if run.replayTranscript.LastMsg != "" {
			run.transcript.LastMsg = run.replayTranscript.LastMsg
			run.lastMsg = clipPlain(run.replayTranscript.LastMsg, 200)
		}
		if run.replayTranscript.LastErr != "" {
			run.transcript.LastErr = run.replayTranscript.LastErr
			run.lastErr = clipPlain(run.replayTranscript.LastErr, 240)
		}
		run.lastPublished = map[string][]byte{}
		for _, t := range run.transcript.Turns {
			run.lastPublished[t.Key] = mustJSON(t)
		}
	}
	run.replayTranscript = nil
	run.mu.Unlock()

	run.applySessionState(state)
	m.updateCatalogFromSessionState(state, daemon.initRes)

	if pref != (grokThreadPref{}) {
		_ = m.applySessionPreferences(ctx, run, id, pref.Model, pref.Effort, permMode, false, false)
	}

	m.mu.Lock()
	if m.shuttingDown || !run.isAlive() {
		shuttingDown := m.shuttingDown
		m.mu.Unlock()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = client.CloseSession(closeCtx, id)
		closeCancel()
		if shuttingDown {
			return nil, errors.New("Relay Is Shutting Down")
		}
		return nil, errors.New("Grok Session Has Stopped")
	}
	m.runs[id] = run
	watched := m.hasWatchersLocked(id)
	attached = false
	m.mu.Unlock()

	if watched {
		run.touch()
	} else {
		run.markUnwatched(m.nowTime())
	}

	m.requestRefresh()
	return run, nil
}

func (m *GrokManager) applySessionPreferences(
	ctx context.Context,
	run *grokRun,
	sid, model, effort, permMode string,
	strict bool,
	freshSession bool,
) error {
	run.mu.Lock()
	client := run.client
	curModel := run.model
	curEffort := run.effort
	curMode := run.permissionMode
	run.mu.Unlock()

	if client == nil {
		return errors.New("Grok Session Is Not Active")
	}
	if model != "" && model != curModel {
		opts, err := client.SetConfigOption(ctx, sid, "model", model)
		if err != nil && isACPMethodNotFound(err) {
			err = client.SetModel(ctx, sid, model)
		}
		if err != nil {
			if strict {
				return err
			}
		} else {
			if len(opts) > 0 {
				m.updateCatalogFromConfigOptions(opts)
			}
			run.mu.Lock()
			run.model = model
			if run.transcript != nil {
				run.transcript.Meta.Model = model
			}
			run.mu.Unlock()
		}
	}
	if effort != "" && effort != curEffort {
		opts, err := client.SetConfigOption(ctx, sid, "reasoning_effort", effort)
		if err != nil {
			if strict {
				return err
			}
		} else {
			if len(opts) > 0 {
				m.updateCatalogFromConfigOptions(opts)
			}
			run.mu.Lock()
			run.effort = effort
			if run.transcript != nil {
				run.transcript.Meta.Effort = effort
			}
			run.mu.Unlock()
		}
	}

	// Never call hanging session/set_mode; use notification-only _x.ai/* methods.
	permMode = normalizeGrokPermissionMode(permMode)
	if permMode != "" {
		if freshSession {
			if permMode == "plan" {
				if err := client.NotifyTogglePlanMode(sid); err != nil && strict {
					return err
				}
			} else if permMode == "acceptEdits" {
				if err := client.NotifyYoloModeChanged(sid, false, false, "acceptEdits"); err != nil && strict {
					return err
				}
			}
		} else if permMode != curMode || permMode == "plan" || permMode == "acceptEdits" {
			if err := applyGrokModeNotifications(client, sid, curMode, permMode); err != nil && strict {
				return err
			}
		}
		run.mu.Lock()
		run.permissionMode = permMode
		if run.transcript != nil {
			run.transcript.Meta.PermissionMode = permMode
		}
		run.mu.Unlock()
	}
	return nil
}

// applyGrokModeNotifications applies live permission mode transitions using
// notification-only `_x.ai/yolo_mode_changed` and `_x.ai/toggle_plan_mode`
// (NEVER calling hanging `session/set_mode`).
func applyGrokModeNotifications(client *grokACPClient, sid, prevMode, nextMode string) error {
	prevMode = normalizeGrokPermissionMode(prevMode)
	nextMode = normalizeGrokPermissionMode(nextMode)
	if nextMode == "" {
		nextMode = "default"
	}
	if prevMode == "" {
		prevMode = "default"
	}

	if nextMode == "plan" {
		if prevMode != "plan" {
			if prevMode != "default" {
				if err := client.NotifyYoloModeChanged(sid, false, false, "plan"); err != nil {
					return err
				}
			}
			return client.NotifyTogglePlanMode(sid)
		}
		return nil
	}

	if prevMode == "plan" {
		if err := client.NotifyTogglePlanMode(sid); err != nil {
			return err
		}
	}
	return client.NotifyYoloModeChanged(
		sid,
		nextMode == "bypassPermissions",
		nextMode == "auto",
		nextMode,
	)
}

func (m *GrokManager) handleSend(id, text string, images []string, ack func(error, map[string]any)) {
	blocks, err := buildGrokPromptBlocks(text, images)
	if err != nil {
		ack(err, nil)
		return
	}
	ctx, cancel := context.WithTimeout(m.baseContext(), defaultGrokCallTimeout)
	run, err := m.ensureRun(ctx, id)
	cancel()
	if err != nil {
		ack(err, nil)
		return
	}
	queued, err := run.startPrompt(strings.TrimSpace(text), images, blocks)
	ack(err, map[string]any{"queued": queued})
}

func (m *GrokManager) handleCompact(id string, ack func(error, map[string]any)) {
	ctx, cancel := context.WithTimeout(m.baseContext(), defaultGrokCallTimeout)
	defer cancel()
	run, err := m.ensureRun(ctx, id)
	if err != nil {
		ack(err, nil)
		return
	}
	run.mu.Lock()
	daemon := run.daemon
	client := run.client
	run.mu.Unlock()
	if client == nil {
		ack(errors.New("Grok Session Is Not Active"), nil)
		return
	}
	err = client.CompactConversation(ctx, id)
	daemon.touchActivity(m.nowTime())
	run.touch()
	ack(err, nil)
	if err == nil {
		m.requestRefresh()
	}
}

func (m *GrokManager) handleStop(id string, ack func(error, map[string]any)) {
	m.mu.Lock()
	run := m.runs[id]
	m.mu.Unlock()
	if run == nil || !run.isAlive() {
		ack(errors.New("Grok Is Not Running"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ack(run.cancelTurn(ctx), nil)
}

func (m *GrokManager) handleWake(c *Client, id string, ack func(error, map[string]any)) {
	ctx, cancel := context.WithTimeout(m.baseContext(), defaultGrokCallTimeout)
	_, err := m.ensureRun(ctx, id)
	cancel()
	if err == nil {
		err = m.emitCurrentOpen(c, id, false)
	}
	ack(err, nil)
}

func (m *GrokManager) handleSettings(c *Client, id string, raw []byte, ack func(error, map[string]any)) {
	var req struct {
		Model          *string `json:"model"`
		Effort         *string `json:"effort"`
		PermissionMode *string `json:"permissionMode"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		ack(errors.New("Invalid Settings Payload"), nil)
		return
	}
	if req.Model == nil && req.Effort == nil && req.PermissionMode == nil {
		ack(errors.New("Nothing To Change"), nil)
		return
	}

	unlock := m.lockThread(id)
	defer unlock()

	m.mu.Lock()
	run := m.runs[id]
	rec, exists := m.records[id]
	m.mu.Unlock()
	if run == nil && !exists {
		m.refresh()
		m.mu.Lock()
		run = m.runs[id]
		rec, exists = m.records[id]
		m.mu.Unlock()
	}
	if (exists && grokRecordIsSubagent(rec)) || grokIsSubagent(m.snapshotRecords(), id) {
		ack(errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent"), nil)
		return
	}
	if run == nil && !exists {
		ack(errors.New("Grok Session Not Found"), nil)
		return
	}

	pref := m.state.Pref(id)
	if req.Model != nil {
		pref.Model = strings.TrimSpace(*req.Model)
	}
	if req.Effort != nil {
		effort := strings.TrimSpace(*req.Effort)
		if effort != "" && !m.validEffort(effort) {
			ack(errors.New("Unknown Grok Effort"), nil)
			return
		}
		pref.Effort = effort
	}
	if req.PermissionMode != nil {
		mode := normalizeGrokPermissionMode(*req.PermissionMode)
		if mode == "" {
			mode = "default"
		}
		if !m.validPermissionMode(mode) {
			ack(errors.New("Unknown Grok Execution Mode"), nil)
			return
		}
		pref.PermissionMode = mode
	}

	if run != nil && run.isAlive() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		run.mu.Lock()
		daemon := run.daemon
		client := run.client
		prevMode := run.permissionMode
		run.mu.Unlock()

		if req.Model != nil && pref.Model != "" {
			opts, err := client.SetConfigOption(ctx, id, "model", pref.Model)
			if err != nil && isACPMethodNotFound(err) {
				err = client.SetModel(ctx, id, pref.Model)
			}
			if err != nil {
				ack(err, nil)
				return
			}
			if len(opts) > 0 {
				m.updateCatalogFromConfigOptions(opts)
			}
			run.mu.Lock()
			run.model = pref.Model
			if run.transcript != nil {
				run.transcript.Meta.Model = pref.Model
			}
			run.mu.Unlock()
		}
		if req.Effort != nil {
			if pref.Effort != "" {
				opts, err := client.SetConfigOption(ctx, id, "reasoning_effort", pref.Effort)
				if err != nil {
					ack(err, nil)
					return
				}
				if len(opts) > 0 {
					m.updateCatalogFromConfigOptions(opts)
				}
			} else {
				// Explicit empty effort resets Grok reasoning_effort back to Default.
				if _, err := client.SetConfigOption(ctx, id, "reasoning_effort", ""); err != nil {
					cat := m.catalogSnapshot()
					def := grokFirstNonEmpty(grokDefaultEffortForModel(cat.Models, pref.Model), cat.DefaultEffort)
					if def != "" {
						_, _ = client.SetConfigOption(ctx, id, "reasoning_effort", def)
					}
				}
			}
			run.mu.Lock()
			run.effort = pref.Effort
			if run.transcript != nil {
				run.transcript.Meta.Effort = pref.Effort
			}
			run.mu.Unlock()
		}
		if req.PermissionMode != nil {
			if err := applyGrokModeNotifications(client, id, prevMode, pref.PermissionMode); err != nil {
				ack(err, nil)
				return
			}
			run.mu.Lock()
			run.permissionMode = pref.PermissionMode
			if run.transcript != nil {
				run.transcript.Meta.PermissionMode = pref.PermissionMode
			}
			run.mu.Unlock()
		}
		daemon.touchActivity(m.nowTime())
		run.publish(true)
	}

	if err := m.state.SetPref(id, pref); err != nil {
		ack(err, nil)
		return
	}
	_ = m.emitCurrentOpen(c, id, false)
	ack(nil, nil)
	m.requestRefresh()
}

func (m *GrokManager) handleApprove(id, reqID, optionID, decision, feedback string, ack func(error, map[string]any)) {
	m.mu.Lock()
	run := m.runs[id]
	m.mu.Unlock()
	if run == nil || !run.isAlive() {
		ack(errors.New("Grok Session Is Not Active"), nil)
		return
	}
	ack(run.approveRequest(reqID, optionID, decision, feedback), nil)
}

func (m *GrokManager) handleAnswer(id string, v map[string]any, ack func(error, map[string]any)) {
	m.mu.Lock()
	run := m.runs[id]
	m.mu.Unlock()
	if run == nil || !run.isAlive() {
		ack(errors.New("Grok Session Is Not Active"), nil)
		return
	}
	action := grokFirstNonEmpty(str(v, "action"), str(v, "decision"))
	answers := map[string][]string{}
	for key, val := range mapValue(v["answers"]) {
		switch typed := val.(type) {
		case string:
			answers[key] = []string{typed}
		default:
			answers[key] = stringArray(val)
		}
	}
	ack(run.answerRequest(str(v, "reqId"), action, answers), nil)
}

func (m *GrokManager) handleRename(id, title string, ack func(error, map[string]any)) {
	unlock := m.lockThread(id)
	defer unlock()

	m.mu.Lock()
	run := m.runs[id]
	rec, exists := m.records[id]
	m.mu.Unlock()
	if run == nil && !exists {
		m.refresh()
		m.mu.Lock()
		run = m.runs[id]
		rec, exists = m.records[id]
		m.mu.Unlock()
	}
	if (exists && grokRecordIsSubagent(rec)) || grokIsSubagent(m.snapshotRecords(), id) {
		ack(errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent"), nil)
		return
	}
	if run == nil && !exists {
		ack(errors.New("Grok Session Not Found"), nil)
		return
	}

	clean, err := m.state.SetTitle(id, title)
	if err != nil {
		ack(err, nil)
		return
	}

	var renameDaemon *grokDaemon
	var renameClient *grokACPClient
	if run != nil && run.isAlive() {
		run.mu.Lock()
		run.title = clean
		if run.transcript != nil {
			run.transcript.Meta.Title = clean
		}
		renameDaemon = run.daemon
		renameClient = run.client
		run.mu.Unlock()
		run.publish(true)
	} else if m.canDialACP() {
		m.mu.Lock()
		for _, d := range m.daemons {
			if d != nil && d.isAlive() {
				renameDaemon = d
				renameClient = d.client
				break
			}
		}
		m.mu.Unlock()
	}
	if renameClient != nil && !renameClient.Closed() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = renameClient.RenameSession(ctx, id, clean)
		cancel()
		renameDaemon.touchActivity(m.nowTime())
	}

	m.refresh()
	ack(nil, map[string]any{"title": clean})
}

func (m *GrokManager) handleFork(id string, ack func(error, map[string]any)) {
	unlock := m.lockThread(id)
	defer unlock()

	m.mu.Lock()
	run := m.runs[id]
	rec, exists := m.records[id]
	m.mu.Unlock()
	if run == nil && !exists {
		m.refresh()
		m.mu.Lock()
		run = m.runs[id]
		rec, exists = m.records[id]
		m.mu.Unlock()
	}
	if (exists && grokRecordIsSubagent(rec)) || grokIsSubagent(m.snapshotRecords(), id) {
		ack(errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent"), nil)
		return
	}
	if run == nil && !exists {
		ack(errors.New("Grok Session Not Found"), nil)
		return
	}

	ctx, cancel := context.WithTimeout(m.baseContext(), defaultGrokCallTimeout)
	defer cancel()

	var daemon *grokDaemon
	var client *grokACPClient
	if run != nil && run.isAlive() {
		run.mu.Lock()
		daemon = run.daemon
		client = run.client
		run.mu.Unlock()
	}
	if client == nil {
		var err error
		daemon, err = m.ensureDaemon(ctx, grokDaemonKey{alwaysApprove: false})
		if err != nil {
			ack(err, nil)
			return
		}
		client = daemon.client
	}

	newID, err := client.ForkSession(ctx, id)
	daemon.touchActivity(m.nowTime())
	if err != nil {
		ack(err, nil)
		return
	}
	if strings.TrimSpace(newID) == "" {
		ack(errors.New("Grok Did Not Return A Forked Session ID"), nil)
		return
	}
	m.refresh()
	ack(nil, map[string]any{"thread": strings.TrimSpace(newID)})
}

func (m *GrokManager) handleArchive(id string, archive bool, ack func(error, map[string]any)) {
	unlock := m.lockThread(id)
	defer unlock()

	m.mu.Lock()
	run := m.runs[id]
	rec, exists := m.records[id]
	m.mu.Unlock()
	if run == nil && !exists {
		m.refresh()
		m.mu.Lock()
		run = m.runs[id]
		rec, exists = m.records[id]
		m.mu.Unlock()
	}
	if (exists && grokRecordIsSubagent(rec)) || grokIsSubagent(m.snapshotRecords(), id) {
		ack(errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent"), nil)
		return
	}
	if run == nil && !exists && (archive || !m.state.IsArchived(id)) {
		ack(errors.New("Grok Session Not Found"), nil)
		return
	}
	if archive {
		if (run != nil && (run.isRunning() || run.isExternalRunning())) || (run == nil && exists && rec.Running) {
			ack(errors.New("Stop This Session Before Archiving"), nil)
			return
		}
	}
	if err := m.state.SetArchived(id, archive); err != nil {
		ack(err, nil)
		return
	}
	if archive && run != nil {
		m.mu.Lock()
		if m.runs[id] == run {
			delete(m.runs, id)
		}
		m.mu.Unlock()
		go run.detachAndCloseSession(2 * time.Second)
	}
	m.refresh()
	ack(nil, nil)
}

func (m *GrokManager) handlePin(id string, pinned bool, ack func(error, map[string]any)) {
	unlock := m.lockThread(id)
	defer unlock()

	m.mu.Lock()
	run := m.runs[id]
	rec, exists := m.records[id]
	m.mu.Unlock()
	if run == nil && !exists {
		m.refresh()
		m.mu.Lock()
		run = m.runs[id]
		rec, exists = m.records[id]
		m.mu.Unlock()
	}
	if (exists && grokRecordIsSubagent(rec)) || grokIsSubagent(m.snapshotRecords(), id) {
		ack(errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent"), nil)
		return
	}
	if run == nil && !exists {
		ack(errors.New("Grok Session Not Found"), nil)
		return
	}
	if err := m.state.SetPinned(id, pinned); err != nil {
		ack(err, nil)
		return
	}
	m.refresh()
	ack(nil, nil)
}

// ---- read-only subagent work following ----

func (m *GrokManager) openWork(c *Client, parent, id string, more bool) (ThreadInfo, error) {
	records := m.snapshotRecords()
	if len(records) == 0 {
		m.refresh()
		records = m.snapshotRecords()
	}
	agents := grokListAgents(records, m.grokRoot(), parent, m.isRunActive)
	var found *ThreadInfo
	for i := range agents {
		if agents[i].ID == id || agents[i].AgentID == id {
			found = &agents[i]
			break
		}
	}
	if found == nil {
		return ThreadInfo{}, errors.New("Subagent Does Not Belong To This Chat")
	}

	if m.hub != nil {
		m.hub.mu.Lock()
		if !m.hub.clients[c] {
			m.hub.mu.Unlock()
			return *found, nil
		}
	}
	m.mu.Lock()
	if m.workWatch == nil {
		m.workWatch = map[*Client]map[string]grokWorkWatch{}
	}
	if m.workWatch[c] == nil {
		m.workWatch[c] = map[string]grokWorkWatch{}
	}
	w := m.workWatch[c][id]
	if w.Gate == nil {
		w.Gate = &sync.Mutex{}
	}
	w.Parent = parent
	if w.Window == 0 {
		w.Window = grokDefaultWindow
	}
	if more {
		w.Window += grokDefaultWindow
	}
	w.Generation++
	w.Mod = time.Time{}
	w.Size = 0
	m.workWatch[c][id] = w
	m.mu.Unlock()
	if m.hub != nil {
		m.hub.mu.Unlock()
	}

	if err := m.refreshWork(c, id, w, more); err != nil {
		return ThreadInfo{}, err
	}
	return *found, nil
}

func (m *GrokManager) moreWork(c *Client, id string, more bool) error {
	m.mu.Lock()
	w, ok := m.workWatch[c][id]
	m.mu.Unlock()
	if !ok {
		return errors.New("Open This Subagent From Its Parent Chat")
	}
	_, err := m.openWork(c, w.Parent, id, more)
	return err
}

func (m *GrokManager) refreshWork(c *Client, id string, w grokWorkWatch, more bool) error {
	w.Gate.Lock()
	defer w.Gate.Unlock()

	m.mu.Lock()
	current, ok := m.workWatch[c][id]
	rec := m.records[id]
	m.mu.Unlock()
	if !ok || current.Generation != w.Generation || current.Gate != w.Gate {
		return nil
	}

	var mod time.Time
	var size int64
	for _, p := range []string{
		rec.UpdatesPath,
		rec.SummaryPath,
		filepath.Join(rec.SubagentDir, "output.json"),
	} {
		if p == "" || p == "output.json" {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			if st.ModTime().After(mod) {
				mod = st.ModTime()
			}
			size += st.Size()
		}
	}
	stateKey := rec.SubagentStatus + ":" + rec.Title + ":" + grokAgentState(rec, rec.Running)
	if !current.Mod.IsZero() && mod.Equal(current.Mod) && size == current.Size && current.State == stateKey && !more {
		return nil
	}

	records := m.snapshotRecords()
	view, err := grokOpenAgentTranscript(records, current.Parent, id, current.Window)
	if err != nil {
		return err
	}
	// Guarantee strict read-only metadata on every subagent payload.
	view.Meta.Provider = "grok"
	view.Meta.ReadOnly = true
	view.Meta.Mode = "tail"

	b := mustJSON(map[string]any{
		"t":        "open",
		"provider": "grok",
		"id":       id,
		"meta":     view.Meta,
		"turns":    view.Turns,
		"more":     more,
	})
	digest := sha256.Sum256(b)

	if m.hub != nil {
		m.hub.mu.Lock()
	}
	m.mu.Lock()
	cur, stillWatched := m.workWatch[c][id]
	connected := m.hub == nil || m.hub.clients[c]
	if !connected || !stillWatched || cur.Generation != w.Generation || cur.Gate != w.Gate {
		m.mu.Unlock()
		if m.hub != nil {
			m.hub.mu.Unlock()
		}
		return nil
	}
	changed := cur.Digest != digest || more
	cur.Digest = digest
	cur.Mod = mod
	cur.Size = size
	cur.State = stateKey
	m.workWatch[c][id] = cur
	m.mu.Unlock()
	if m.hub != nil {
		m.hub.mu.Unlock()
	}

	if changed {
		c.push(b)
	}
	return nil
}
