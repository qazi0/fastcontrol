package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func (m *AntigravityManager) handle(c *Client, raw []byte) {
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

	// Strict read-only ingress guard FIRST on every Antigravity op.
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
		ack(m.open(c, id, op == "more"), nil)

	case "close":
		m.mu.Lock()
		delete(m.watch[c], id)
		delete(m.workWatch[c], id)
		m.mu.Unlock()

	case "diff":
		go m.handleDiff(c, id, str(v, "key"), ack)

	case "new":
		go m.handleNew(v, ack)

	case "send":
		go m.handleSend(id, str(v, "text"), stringArray(v["images"]), ack)

	case "compact":
		go m.handleSend(id, "/compact", nil, ack)

	case "stop":
		go m.handleStop(id, ack)

	case "wake":
		go m.handleWake(c, id, ack)

	case "settings":
		go m.handleSettings(c, id, raw, ack)

	case "approve":
		go m.handleApprove(id, str(v, "reqId"), str(v, "optionId"), str(v, "decision"), ack)

	case "answer":
		go m.handleAnswer(id, v, ack)

	case "rename":
		go m.handleRename(id, str(v, "text"), ack)

	case "archive", "unarchive":
		go m.handleArchive(id, op == "archive", ack)

	case "pin", "unpin":
		go m.handlePin(id, op == "pin", ack)

	case "agents":
		go func() {
			if len(m.snapshotRecords()) == 0 {
				m.refresh()
			}
			agents := antigravityListAgents(m.snapshotRecords(), id, m.isRunActive)
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
		ack(errors.New("Unsupported Antigravity Operation"), nil)
	}
}

func (m *AntigravityManager) rejectSubagentControl(id, op string) error {
	if !antigravitySubagentControlOp(op) || strings.TrimSpace(id) == "" {
		return nil
	}
	m.mu.Lock()
	_, known := m.records[id]
	m.mu.Unlock()
	if !known {
		m.refresh()
	}
	return antigravityRejectSubagentControl(m.snapshotRecords(), id, op)
}

func (m *AntigravityManager) handleProjects(c *Client, cwd string) {
	records := m.snapshotRecords()
	if len(records) == 0 {
		m.refresh()
		records = m.snapshotRecords()
	}
	if cwd != "" {
		list, _ := antigravityBuildOrganization(false, m.state, records)
		m.mu.Lock()
		runs := make(map[string]*antigravityRun, len(m.runs))
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
	projects := antigravityProjects(records, m.state)
	home, _ := os.UserHomeDir()
	m.emit(c, map[string]any{"t": "projects", "projects": projects, "home": home})
}

func (m *AntigravityManager) organizationGroup(id string) ([]ThreadInfo, error) {
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

func (m *AntigravityManager) discoverModels(ctx context.Context) error {
	m.modelsMu.Lock()
	defer m.modelsMu.Unlock()

	now := m.nowTime()
	m.mu.Lock()
	fresh := len(m.catalog.Models) > 0 && now.Sub(m.catalog.UpdatedAt) < m.modelsTTL()
	m.mu.Unlock()
	if fresh {
		return nil
	}

	if err := m.requireACP(); err != nil {
		return err
	}

	records := m.snapshotRecords()
	if len(records) == 0 {
		m.refresh()
		records = m.snapshotRecords()
	}

	// Pick an existing idle non-subagent conversation so we can probe via
	// session/load + session/close without creating a stray conversation.
	var probeRec *antigravityRecord
	m.mu.Lock()
	activeRunIDs := make(map[string]bool, len(m.runs))
	for id := range m.runs {
		activeRunIDs[id] = true
	}
	m.mu.Unlock()

	for i := range records {
		rec := records[i]
		if antigravityRecordIsSubagent(rec) || rec.Running || activeRunIDs[rec.ID] {
			continue
		}
		if probeRec == nil || (probeRec.AppDataDir != acpDefaultAppDataDir && rec.AppDataDir == acpDefaultAppDataDir) {
			cp := rec
			probeRec = &cp
			if rec.AppDataDir == acpDefaultAppDataDir {
				break
			}
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	appDir := acpDefaultAppDataDir
	if probeRec != nil && antigravityValidAppDataDir(probeRec.AppDataDir) {
		appDir = probeRec.AppDataDir
	}

	client, initRes, err := m.dialClient(probeCtx, acpClientConfig{
		AppDataDir:       appDir,
		DisableTelemetry: true,
		CallTimeout:      15 * time.Second,
	})
	if err != nil {
		return err
	}
	defer client.Close()

	var state *acpSessionState
	if probeRec != nil {
		cwd := probeRec.Cwd
		if cwd == "" {
			cwd = os.TempDir()
		} else if st, statErr := os.Stat(cwd); statErr != nil || !st.IsDir() {
			cwd = os.TempDir()
		}
		loaded, loadErr := client.LoadSession(probeCtx, probeRec.ID, cwd)
		if loadErr == nil && loaded != nil {
			state = loaded
			_ = client.CloseSession(probeCtx, probeRec.ID)
		}
	}

	if state == nil {
		created, newErr := client.NewSession(probeCtx, os.TempDir(), nil)
		if newErr != nil {
			return newErr
		}
		state = created
		if created.SessionID != "" {
			_ = client.CloseSession(probeCtx, created.SessionID)
		}
	}

	m.updateCatalogFromState(state, initRes)
	return nil
}

func (m *AntigravityManager) handleModels(c *Client) {
	err := m.discoverModels(context.Background())
	cat := m.catalogSnapshot()
	models := cat.Models
	if models == nil {
		models = []Model{}
	}
	modes := make([]map[string]any, 0, len(cat.Modes))
	for _, md := range cat.Modes {
		if md.ID == "full-access" && !m.allowFullAccess {
			continue
		}
		modes = append(modes, map[string]any{
			"id":          md.ID,
			"name":        md.Name,
			"description": md.Description,
		})
	}
	result := map[string]any{
		"t":      "models",
		"models": models,
		"default": map[string]any{
			"model":  cat.DefaultModel,
			"effort": cat.DefaultEffort,
			"mode":   cat.DefaultMode,
		},
		"modes":           modes,
		"allowFullAccess": m.allowFullAccess,
	}
	if cat.Tier != nil {
		result["tier"] = cat.Tier
	}
	if err != nil {
		result["error"] = err.Error()
	}
	m.emit(c, result)
}

func (m *AntigravityManager) open(c *Client, id string, more bool) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("Antigravity Session Not Found")
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
	if run == nil && !exists {
		return errors.New("Antigravity Session Not Found")
	}
	if exists && antigravityRecordIsSubagent(rec) {
		return errors.New("Open This Subagent From Its Parent Chat")
	}

	window := antigravityDefaultWindow
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
	if exists && antigravityRecordIsSubagent(rec) {
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
		window += antigravityDefaultWindow
	}
	m.watch[c][id] = window
	m.mu.Unlock()
	if m.hub != nil {
		m.hub.mu.Unlock()
	}

	if run != nil {
		if more && exists && rec.TranscriptPath != "" {
			if hist, err := readAntigravityTranscriptForRecord(m.geminiRoot(), rec, window); err == nil {
				run.mergeOlderDiskHistory(hist)
			}
		}
		meta, turns := run.snapshot()
		pref := m.state.Pref(id)
		if meta.Model == "" && pref.Model != "" {
			meta.Model = pref.Model
		}
		if meta.PermissionMode == "" {
			meta.PermissionMode = antigravityFirstNonEmpty(pref.PermissionMode, "default")
		}
		m.emit(c, map[string]any{"t": "open", "id": id, "meta": meta, "turns": turns, "more": more})
		return nil
	}

	view, err := m.readRecordTranscript(rec, window)
	if err != nil {
		return err
	}
	if custom := m.state.Title(id); custom != "" {
		view.Meta.Title = custom
	}
	pref := m.state.Pref(id)
	if pref.Model != "" {
		view.Meta.Model = pref.Model
	}
	if pref.Effort != "" {
		view.Meta.Effort = pref.Effort
	}
	if pref.PermissionMode != "" {
		view.Meta.PermissionMode = pref.PermissionMode
	}
	view.Meta.Provider = "antigravity"
	view.Meta.Mode = "tail"
	view.Meta.Running = rec.Running

	stamp := m.computeWatchStamp(rec)
	m.mu.Lock()
	if m.watchedStamp == nil {
		m.watchedStamp = map[string]antigravityWatchStamp{}
	}
	m.watchedStamp[id] = stamp
	m.mu.Unlock()

	m.emit(c, map[string]any{"t": "open", "id": id, "meta": view.Meta, "turns": view.Turns, "more": more})
	return nil
}

func (m *AntigravityManager) readRecordTranscript(rec antigravityRecord, window int) (*antigravityTranscript, error) {
	if rec.TranscriptPath != "" {
		if st, err := os.Stat(rec.TranscriptPath); err == nil && st.Mode().IsRegular() {
			return readAntigravityTranscriptForRecord(m.geminiRoot(), rec, window)
		}
	}
	v := newAntigravityTranscript()
	v.Meta.Title = antigravityFirstNonEmpty(rec.Title, rec.AnnotationTitle, clipPlain(rec.Preview, 120))
	v.Meta.Cwd = rec.Cwd
	v.Meta.Running = rec.Running
	return v, nil
}

func (m *AntigravityManager) handleDiff(c *Client, id, key string, ack func(error, map[string]any)) {
	m.mu.Lock()
	run := m.runs[id]
	rec, hasRec := m.records[id]
	workWatch, isWork := m.workWatch[c][id]
	m.mu.Unlock()

	if isWork {
		view, err := antigravityOpenAgentTranscript(m.snapshotRecords(), workWatch.Parent, id, workWatch.Window)
		if err != nil {
			ack(err, nil)
			return
		}
		text := selectTranscriptDiff(view.Diffs, key)
		text = clipHead(text, 2<<20)
		m.emit(c, map[string]any{"t": "diff", "id": id, "key": key, "diff": text, "text": text})
		return
	}
	if hasRec && antigravityRecordIsSubagent(rec) {
		ack(errors.New("Open This Subagent From Its Parent Chat"), nil)
		return
	}

	var text string
	if run != nil {
		if d, ok := run.diffByKey(key); ok {
			text = d
		}
	}
	if text == "" && hasRec && rec.TranscriptPath != "" {
		if view, err := readAntigravityTranscriptForRecord(m.geminiRoot(), rec, 100); err == nil {
			text = selectTranscriptDiff(view.Diffs, key)
		}
	}
	text = clipHead(text, 2<<20)
	m.emit(c, map[string]any{"t": "diff", "id": id, "key": key, "diff": text, "text": text})
}

func selectTranscriptDiff(diffs map[string]string, key string) string {
	if len(diffs) == 0 {
		return ""
	}
	if key != "" {
		return diffs[key]
	}
	keys := make([]string, 0, len(diffs))
	for k := range diffs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, k := range keys {
		d := diffs[k]
		if d == "" {
			continue
		}
		out.WriteString(d)
		if !strings.HasSuffix(d, "\n") {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func buildAntigravityPromptBlocks(text string, images []string) ([]acpContentBlock, error) {
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

func (m *AntigravityManager) handleNew(v map[string]any, ack func(error, map[string]any)) {
	if err := m.requireACP(); err != nil {
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
	blocks, err := buildAntigravityPromptBlocks(text, images)
	if err != nil {
		ack(err, nil)
		return
	}

	model := strings.TrimSpace(str(v, "model"))
	effort := strings.TrimSpace(str(v, "effort"))
	permMode := strings.TrimSpace(str(v, "permissionMode"))
	if permMode == "" {
		permMode = "default"
	}
	if effort != "" && !m.validEffort(effort) {
		ack(errors.New("Unknown Antigravity Effort"), nil)
		return
	}
	if !m.validPermissionMode(permMode) {
		ack(errors.New("Unknown Antigravity Execution Mode"), nil)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run := newAntigravityRun(m, "", acpDefaultAppDataDir, cwd)
	client, initRes, err := m.dialClient(ctx, acpClientConfig{
		AppDataDir:                 acpDefaultAppDataDir,
		Cwd:                        cwd,
		DangerouslySkipPermissions: permMode == "full-access" && m.allowFullAccess,
		DisableTelemetry:           true,
		CallTimeout:                defaultAntigravityCallTimeout,
		OnUpdate:                   run.onUpdate,
		OnPermission:               run.onPermission,
		OnElicitation:              run.onElicitation,
		OnClose:                    run.onClose,
	})
	if err != nil {
		ack(err, nil)
		return
	}

	run.mu.Lock()
	run.client = client
	run.mu.Unlock()

	sessionState, err := client.NewSession(ctx, cwd, nil)
	if err != nil || sessionState == nil || strings.TrimSpace(sessionState.SessionID) == "" {
		_ = client.Close()
		if err == nil {
			err = errors.New("Antigravity Did Not Return A Session ID")
		}
		ack(err, nil)
		return
	}
	sid := strings.TrimSpace(sessionState.SessionID)

	run.mu.Lock()
	run.id = sid
	run.mu.Unlock()
	run.drainDispatch(client)
	run.applySessionState(sessionState)
	m.updateCatalogFromState(sessionState, initRes)

	if err := m.applySessionPreferences(ctx, run, sid, model, effort, permMode, true); err != nil {
		_ = client.Close()
		ack(err, nil)
		return
	}

	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		_ = client.Close()
		ack(errors.New("Relay Is Shutting Down"), nil)
		return
	}
	m.runs[sid] = run
	m.mu.Unlock()

	_ = m.state.SetPref(sid, antigravityThreadPref{
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

func (m *AntigravityManager) ensureRun(ctx context.Context, id string) (*antigravityRun, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("Antigravity Session Not Found")
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
	if !exists {
		return nil, errors.New("Antigravity Session Not Found")
	}
	if antigravityRecordIsSubagent(rec) {
		return nil, errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent")
	}
	if err := m.requireACP(); err != nil {
		return nil, err
	}

	appDir := rec.AppDataDir
	if !antigravityValidAppDataDir(appDir) {
		appDir = acpDefaultAppDataDir
	}
	cwd := rec.Cwd
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	} else if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		cwd, _ = os.UserHomeDir()
	}

	pref := m.state.Pref(id)
	run := newAntigravityRun(m, id, appDir, cwd)
	if hist, err := m.readRecordTranscript(rec, antigravityDefaultWindow); err == nil {
		run.seedFromTranscript(hist)
	}
	run.mu.Lock()
	run.loading = true
	if run.title == "" {
		run.title = antigravityFirstNonEmpty(rec.Title, rec.AnnotationTitle, clipPlain(rec.Preview, 120))
	}
	run.mu.Unlock()

	client, initRes, err := m.dialClient(ctx, acpClientConfig{
		AppDataDir:                 appDir,
		Cwd:                        cwd,
		DangerouslySkipPermissions: pref.PermissionMode == "full-access" && m.allowFullAccess,
		DisableTelemetry:           true,
		CallTimeout:                defaultAntigravityCallTimeout,
		OnUpdate:                   run.onUpdate,
		OnPermission:               run.onPermission,
		OnElicitation:              run.onElicitation,
		OnClose:                    run.onClose,
	})
	if err != nil {
		return nil, err
	}

	run.mu.Lock()
	run.client = client
	run.mu.Unlock()

	state, err := client.LoadSession(ctx, id, cwd)
	if err != nil {
		// Fall back to session/resume if session/load is unavailable.
		resumed, resumeErr := client.ResumeSession(ctx, id, cwd)
		if resumeErr != nil {
			_ = client.Close()
			return nil, err
		}
		state = resumed
	}

	// Wait for dispatchLoop to drain all replay notifications emitted during
	// session/load or session/resume before flipping loading to false.
	run.drainDispatch(client)

	run.mu.Lock()
	run.loading = false
	if len(run.diskTurns) == 0 && len(run.replayTurns) > 0 {
		run.diskTurns = cloneTurns(run.replayTurns)
		for k, d := range run.replayDiffs {
			run.diskDiffs[k] = d
		}
	}
	run.replayTurns = nil
	run.replayDiffs = map[string]string{}
	run.mu.Unlock()

	run.applySessionState(state)
	m.updateCatalogFromState(state, initRes)

	// Re-apply saved preferences if they differ from the loaded session's defaults.
	if pref != (antigravityThreadPref{}) {
		_ = m.applySessionPreferences(ctx, run, id, pref.Model, pref.Effort, pref.PermissionMode, false)
	}

	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		_ = client.Close()
		return nil, errors.New("Relay Is Shutting Down")
	}
	m.runs[id] = run
	m.mu.Unlock()

	m.requestRefresh()
	return run, nil
}

func (m *AntigravityManager) applySessionPreferences(ctx context.Context, run *antigravityRun, sid, model, effort, permMode string, strict bool) error {
	run.mu.Lock()
	client := run.client
	curModel := run.model
	curEffort := run.effort
	curMode := run.permissionMode
	run.mu.Unlock()

	if client == nil {
		return errors.New("Antigravity Session Is Not Active")
	}
	if model != "" && model != curModel {
		if err := client.SetConfigOption(ctx, sid, "model", model); err != nil {
			if strict {
				return err
			}
		} else {
			run.mu.Lock()
			run.model = model
			run.mu.Unlock()
		}
	}
	if effort != "" && effort != curEffort {
		if err := client.SetConfigOption(ctx, sid, "thought_level", effort); err != nil {
			if strict {
				return err
			}
		} else {
			run.mu.Lock()
			run.effort = effort
			run.mu.Unlock()
		}
	}
	if permMode != "" && permMode != "full-access" && permMode != curMode {
		if err := client.SetMode(ctx, sid, permMode); err != nil {
			if strict {
				return err
			}
		} else {
			run.mu.Lock()
			run.permissionMode = permMode
			run.mu.Unlock()
		}
	} else if permMode == "full-access" && m.allowFullAccess {
		run.mu.Lock()
		run.permissionMode = permMode
		run.mu.Unlock()
	}
	return nil
}

func (m *AntigravityManager) handleSend(id, text string, images []string, ack func(error, map[string]any)) {
	blocks, err := buildAntigravityPromptBlocks(text, images)
	if err != nil {
		ack(err, nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	run, err := m.ensureRun(ctx, id)
	cancel()
	if err != nil {
		ack(err, nil)
		return
	}
	queued, err := run.startPrompt(strings.TrimSpace(text), images, blocks)
	ack(err, map[string]any{"queued": queued})
}

func (m *AntigravityManager) handleStop(id string, ack func(error, map[string]any)) {
	m.mu.Lock()
	run := m.runs[id]
	m.mu.Unlock()
	if run == nil || !run.isAlive() {
		ack(errors.New("Antigravity Is Not Running"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ack(run.cancelTurn(ctx), nil)
}

func (m *AntigravityManager) handleWake(c *Client, id string, ack func(error, map[string]any)) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err := m.ensureRun(ctx, id)
	cancel()
	if err == nil {
		err = m.open(c, id, false)
	}
	ack(err, nil)
}

func (m *AntigravityManager) handleSettings(c *Client, id string, raw []byte, ack func(error, map[string]any)) {
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
	if run == nil && !exists {
		ack(errors.New("Antigravity Session Not Found"), nil)
		return
	}
	if exists && antigravityRecordIsSubagent(rec) {
		ack(errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent"), nil)
		return
	}

	pref := m.state.Pref(id)
	if req.Model != nil {
		pref.Model = strings.TrimSpace(*req.Model)
	}
	if req.Effort != nil {
		effort := strings.TrimSpace(*req.Effort)
		if effort != "" && !m.validEffort(effort) {
			ack(errors.New("Unknown Antigravity Effort"), nil)
			return
		}
		pref.Effort = effort
	}
	if req.PermissionMode != nil {
		mode := strings.TrimSpace(*req.PermissionMode)
		if mode == "" {
			mode = "default"
		}
		if !m.validPermissionMode(mode) {
			ack(errors.New("Unknown Antigravity Execution Mode"), nil)
			return
		}
		pref.PermissionMode = mode
	}

	if run != nil && run.isAlive() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		run.mu.Lock()
		client := run.client
		run.mu.Unlock()

		if req.Model != nil && pref.Model != "" {
			if err := client.SetConfigOption(ctx, id, "model", pref.Model); err != nil {
				ack(err, nil)
				return
			}
			run.mu.Lock()
			run.model = pref.Model
			run.mu.Unlock()
		}
		if req.Effort != nil {
			if pref.Effort != "" {
				if err := client.SetConfigOption(ctx, id, "thought_level", pref.Effort); err != nil {
					ack(err, nil)
					return
				}
			} else {
				// Explicit empty effort resets Antigravity Intelligence back to Default.
				if err := client.SetConfigOption(ctx, id, "thought_level", ""); err != nil {
					if def := m.catalogSnapshot().DefaultEffort; def != "" {
						_ = client.SetConfigOption(ctx, id, "thought_level", def)
					}
				}
			}
			run.mu.Lock()
			run.effort = pref.Effort
			run.mu.Unlock()
		}
		if req.PermissionMode != nil {
			if pref.PermissionMode != "full-access" {
				if err := client.SetMode(ctx, id, pref.PermissionMode); err != nil {
					ack(err, nil)
					return
				}
			}
			run.mu.Lock()
			run.permissionMode = pref.PermissionMode
			run.mu.Unlock()
		}
		run.publish(true)
	}

	if err := m.state.SetPref(id, pref); err != nil {
		ack(err, nil)
		return
	}
	_ = m.open(c, id, false)
	ack(nil, nil)
	m.requestRefresh()
}

func (m *AntigravityManager) handleApprove(id, reqID, optionID, decision string, ack func(error, map[string]any)) {
	m.mu.Lock()
	run := m.runs[id]
	m.mu.Unlock()
	if run == nil || !run.isAlive() {
		ack(errors.New("Antigravity Session Is Not Active"), nil)
		return
	}
	ack(run.approvePermission(reqID, optionID, decision), nil)
}

func (m *AntigravityManager) handleAnswer(id string, v map[string]any, ack func(error, map[string]any)) {
	m.mu.Lock()
	run := m.runs[id]
	m.mu.Unlock()
	if run == nil || !run.isAlive() {
		ack(errors.New("Antigravity Session Is Not Active"), nil)
		return
	}
	action := antigravityFirstNonEmpty(str(v, "action"), str(v, "decision"))
	answers := map[string][]string{}
	for key, val := range mapValue(v["answers"]) {
		switch typed := val.(type) {
		case string:
			answers[key] = []string{typed}
		default:
			answers[key] = stringArray(val)
		}
	}
	ack(run.answerElicitation(str(v, "reqId"), action, answers), nil)
}

func (m *AntigravityManager) handleRename(id, title string, ack func(error, map[string]any)) {
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
	if run == nil && !exists {
		ack(errors.New("Antigravity Session Not Found"), nil)
		return
	}
	if exists && antigravityRecordIsSubagent(rec) {
		ack(errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent"), nil)
		return
	}

	clean, err := m.state.SetTitle(id, title)
	if err != nil {
		ack(err, nil)
		return
	}
	if run != nil {
		run.mu.Lock()
		run.title = clean
		run.mu.Unlock()
		run.publish(true)
	}
	m.refresh()
	ack(nil, map[string]any{"title": clean})
}

func (m *AntigravityManager) handleArchive(id string, archive bool, ack func(error, map[string]any)) {
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
	if exists && antigravityRecordIsSubagent(rec) {
		ack(errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent"), nil)
		return
	}
	if run == nil && !exists && (archive || !m.state.IsArchived(id)) {
		ack(errors.New("Antigravity Session Not Found"), nil)
		return
	}
	if archive {
		if (run != nil && run.isRunning()) || (run == nil && exists && rec.Running) {
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
		go run.shutdown(2 * time.Second)
	}
	m.refresh()
	ack(nil, nil)
}

func (m *AntigravityManager) handlePin(id string, pinned bool, ack func(error, map[string]any)) {
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
	if run == nil && !exists {
		ack(errors.New("Antigravity Session Not Found"), nil)
		return
	}
	if exists && antigravityRecordIsSubagent(rec) {
		ack(errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent"), nil)
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

func (m *AntigravityManager) openWork(c *Client, parent, id string, more bool) (ThreadInfo, error) {
	records := m.snapshotRecords()
	if len(records) == 0 {
		m.refresh()
		records = m.snapshotRecords()
	}
	agents := antigravityListAgents(records, parent, m.isRunActive)
	var found *ThreadInfo
	for i := range agents {
		if agents[i].ID == id {
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
		m.workWatch = map[*Client]map[string]antigravityWorkWatch{}
	}
	if m.workWatch[c] == nil {
		m.workWatch[c] = map[string]antigravityWorkWatch{}
	}
	w := m.workWatch[c][id]
	if w.Gate == nil {
		w.Gate = &sync.Mutex{}
	}
	w.Parent = parent
	if w.Window == 0 {
		w.Window = antigravityDefaultWindow
	}
	if more {
		w.Window += antigravityDefaultWindow
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

func (m *AntigravityManager) moreWork(c *Client, id string, more bool) error {
	m.mu.Lock()
	w, ok := m.workWatch[c][id]
	m.mu.Unlock()
	if !ok {
		return errors.New("Open This Subagent From Its Parent Chat")
	}
	_, err := m.openWork(c, w.Parent, id, more)
	return err
}

func (m *AntigravityManager) refreshWork(c *Client, id string, w antigravityWorkWatch, more bool) error {
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
	if rec.TranscriptPath != "" {
		if st, err := os.Stat(rec.TranscriptPath); err == nil && st.Mode().IsRegular() {
			mod = st.ModTime()
			size = st.Size()
		}
	}
	stateKey := rec.Status + ":" + rec.Title + ":" + antigravityAgentState(rec, rec.Running)
	if !current.Mod.IsZero() && mod.Equal(current.Mod) && size == current.Size && current.State == stateKey && !more {
		return nil
	}

	records := m.snapshotRecords()
	view, err := antigravityOpenAgentTranscript(records, current.Parent, id, current.Window)
	if err != nil {
		return err
	}
	// Guarantee strict read-only metadata on every subagent payload.
	view.Meta.Provider = "antigravity"
	view.Meta.ReadOnly = true
	view.Meta.Mode = "tail"

	b := mustJSON(map[string]any{
		"t":        "open",
		"provider": "antigravity",
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
