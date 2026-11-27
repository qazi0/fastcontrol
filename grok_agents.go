package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// grokSubagentMeta mirrors <parentDir>/subagents/<subagent-id>/meta.json.
type grokSubagentMeta struct {
	SubagentID       string          `json:"subagent_id"`
	ParentSessionID  string          `json:"parent_session_id"`
	ChildSessionID   string          `json:"child_session_id"`
	ChildCwd         string          `json:"child_cwd"`
	SubagentType     string          `json:"subagent_type"`
	Description      string          `json:"description"`
	Status           string          `json:"status"`
	EffectiveModelID string          `json:"effective_model_id"`
	StartedAt        json.RawMessage `json:"started_at"`
	CompletedAt      json.RawMessage `json:"completed_at"`
	DurationMs       int64           `json:"duration_ms"`
	Turns            int             `json:"turns"`
	ToolCalls        int             `json:"tool_calls"`
	metaDir          string
}

func grokReadSubagentMetas(parentSessionDir string) []grokSubagentMeta {
	if strings.TrimSpace(parentSessionDir) == "" {
		return nil
	}
	subRoot := filepath.Join(parentSessionDir, "subagents")
	entries, err := os.ReadDir(subRoot)
	if err != nil {
		return nil
	}
	out := make([]grokSubagentMeta, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if !grokValidSessionID(e.Name()) {
			continue
		}
		subDir := filepath.Join(subRoot, e.Name())
		metaPath := filepath.Join(subDir, "meta.json")
		raw, readErr := grokReadRegularFile(metaPath, grokSmallJSONMaxBytes)
		if readErr != nil || len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var m grokSubagentMeta
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		m.SubagentID = grokStorageFirstNonEmpty(strings.TrimSpace(m.SubagentID), e.Name())
		m.ParentSessionID = strings.TrimSpace(m.ParentSessionID)
		m.ChildSessionID = strings.TrimSpace(m.ChildSessionID)
		m.ChildCwd = strings.TrimSpace(m.ChildCwd)
		m.SubagentType = strings.TrimSpace(m.SubagentType)
		m.Description = strings.TrimSpace(m.Description)
		m.Status = strings.TrimSpace(m.Status)
		m.EffectiveModelID = strings.TrimSpace(m.EffectiveModelID)
		m.metaDir = subDir
		out = append(out, m)
	}
	return out
}

func applySubagentMetaToRecord(rec *grokRecord, meta grokSubagentMeta, parentID string) {
	if rec == nil {
		return
	}
	if rec.SessionKind == "" {
		rec.SessionKind = "subagent"
	}
	if rec.ParentSessionID == "" {
		rec.ParentSessionID = parentID
	}
	if rec.SubagentID == "" {
		rec.SubagentID = meta.SubagentID
	}
	if rec.SubagentType == "" {
		rec.SubagentType = meta.SubagentType
	}
	if rec.SubagentStatus == "" {
		rec.SubagentStatus = meta.Status
	}
	if rec.SubagentDir == "" {
		rec.SubagentDir = meta.metaDir
	}
	if rec.AgentName == "" {
		rec.AgentName = grokStorageFirstNonEmpty(meta.SubagentType, meta.Description)
	}
	if rec.Title == "" && meta.Description != "" {
		rec.Title = cleanGrokTitle(meta.Description)
	}
	if rec.Preview == "" && meta.Description != "" {
		rec.Preview = clipPlain(meta.Description, 200)
	}
	if rec.Cwd == "" && meta.ChildCwd != "" {
		rec.Cwd = filepath.Clean(meta.ChildCwd)
	}
	if rec.Model == "" && meta.EffectiveModelID != "" {
		rec.Model = meta.EffectiveModelID
	}
	startedT := parseGrokTimeJSON(meta.StartedAt)
	completedT := parseGrokTimeJSON(meta.CompletedAt)
	if !startedT.IsZero() && rec.Created == 0 {
		rec.Created = startedT.Unix()
	}
	latestT := completedT
	if startedT.After(latestT) {
		latestT = startedT
	}
	if !latestT.IsZero() {
		if latestT.UnixMilli() > rec.Recency {
			rec.Recency = latestT.UnixMilli()
		}
		if latestT.Unix() > rec.Updated {
			rec.Updated = latestT.Unix()
		}
	}
}

func grokRecordFromSubagentMeta(meta grokSubagentMeta, parentID, fallbackCwd string) grokRecord {
	childID := grokStorageFirstNonEmpty(meta.ChildSessionID, meta.SubagentID)
	cwd := grokStorageFirstNonEmpty(meta.ChildCwd, fallbackCwd)
	if cwd != "" {
		cwd = filepath.Clean(cwd)
	}
	startedT := parseGrokTimeJSON(meta.StartedAt)
	completedT := parseGrokTimeJSON(meta.CompletedAt)
	latestT := completedT
	if startedT.After(latestT) {
		latestT = startedT
	}
	var createdSec, updatedSec, recencyMs, turnAtMs int64
	if !startedT.IsZero() {
		createdSec = startedT.Unix()
		turnAtMs = startedT.UnixMilli()
	}
	if !latestT.IsZero() {
		updatedSec = latestT.Unix()
		recencyMs = latestT.UnixMilli()
		if createdSec == 0 {
			createdSec = updatedSec
		}
	}
	title := cleanGrokTitle(grokStorageFirstNonEmpty(meta.Description, meta.SubagentType, "Subagent"))
	return grokRecord{
		ID:              childID,
		Title:           title,
		Preview:         clipPlain(grokStorageFirstNonEmpty(meta.Description, meta.SubagentType), 200),
		Cwd:             cwd,
		Created:         createdSec,
		Updated:         updatedSec,
		Recency:         recencyMs,
		TurnAt:          turnAtMs,
		Model:           meta.EffectiveModelID,
		NumMessages:     meta.Turns,
		ParentSessionID: parentID,
		SessionKind:     "subagent",
		AgentName:       grokStorageFirstNonEmpty(meta.SubagentType, meta.Description),
		SubagentID:      meta.SubagentID,
		SubagentType:    meta.SubagentType,
		SubagentStatus:  meta.Status,
		SubagentDir:     meta.metaDir,
	}
}

// grokRecordIsSubagent reports whether rec represents a Grok subagent rather
// than a top-level user conversation.
func grokRecordIsSubagent(rec grokRecord) bool {
	return strings.EqualFold(strings.TrimSpace(rec.SessionKind), "subagent") ||
		strings.TrimSpace(rec.ParentSessionID) != ""
}

func grokRecordTitle(rec grokRecord, fallback string) string {
	return grokStorageFirstNonEmpty(
		rec.Title,
		rec.GeneratedTitle,
		rec.SessionSummary,
		rec.AgentName,
		clipPlain(rec.Preview, 120),
		fallback,
	)
}

func grokRecordPreview(rec grokRecord) string {
	return grokStorageFirstNonEmpty(
		rec.Preview,
		rec.LastTurnSummary,
		rec.LastRecap,
		rec.SessionSummary,
		rec.GeneratedTitle,
		rec.Title,
	)
}

func grokAgentState(rec grokRecord, running bool) string {
	if running {
		return "running"
	}
	status := strings.ToLower(strings.TrimSpace(rec.SubagentStatus))
	switch {
	case status == "completed" || status == "complete" || status == "done" || status == "success" || status == "succeeded":
		return "completed"
	case strings.Contains(status, "fail") || strings.Contains(status, "err"):
		return "failed"
	case strings.Contains(status, "cancel") || strings.Contains(status, "stop") || strings.Contains(status, "abort") || strings.Contains(status, "kill"):
		return "stopped"
	case status == "running" || status == "in_progress" || status == "pending" || status == "started":
		// Meta reported running, but live/stale-running check determined it is not active.
		return "saved"
	case rec.NumMessages > 0 || rec.NumChatMessages > 0 || rec.HasSummary:
		return "completed"
	default:
		return "saved"
	}
}

func grokFindParentSessionDir(records []grokRecord, grokHome, parentID string) string {
	for _, rec := range records {
		if rec.ID == parentID && rec.SessionDir != "" {
			return rec.SessionDir
		}
	}
	if strings.TrimSpace(grokHome) == "" {
		return ""
	}
	if !grokValidSessionID(parentID) {
		return ""
	}
	sessionsRoot := filepath.Join(grokHome, "sessions")
	cwdDirs, err := os.ReadDir(sessionsRoot)
	if err != nil {
		return ""
	}
	for _, cwdEntry := range cwdDirs {
		if !cwdEntry.IsDir() || cwdEntry.Type()&os.ModeSymlink != 0 {
			continue
		}
		candidate := filepath.Join(sessionsRoot, cwdEntry.Name(), parentID)
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
	}
	return ""
}

// grokListAgents returns read-only subagent ThreadInfo summaries belonging to
// parentID (combining records and <parentDir>/subagents/*/meta.json), sorted
// with running subagents first and newest Recency next.
func grokListAgents(records []grokRecord, grokHome, parentID string, liveCheck func(string) bool) []ThreadInfo {
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		return []ThreadInfo{}
	}

	byChildID := map[string]grokRecord{}
	var parentCwd string
	for _, rec := range records {
		if rec.ID == parentID && parentCwd == "" {
			parentCwd = rec.Cwd
		}
		if !grokRecordIsSubagent(rec) || rec.ParentSessionID != parentID || rec.ID == parentID {
			continue
		}
		byChildID[rec.ID] = rec
	}

	// Merge any subagents discovered directly from <parentDir>/subagents/*/meta.json.
	if parentDir := grokFindParentSessionDir(records, grokHome, parentID); parentDir != "" {
		for _, meta := range grokReadSubagentMetas(parentDir) {
			if meta.ParentSessionID != "" && meta.ParentSessionID != parentID {
				continue
			}
			childID := grokStorageFirstNonEmpty(meta.ChildSessionID, meta.SubagentID)
			if !grokValidSessionID(childID) || childID == parentID {
				continue
			}
			if existing, ok := byChildID[childID]; ok {
				applySubagentMetaToRecord(&existing, meta, parentID)
				byChildID[childID] = existing
				continue
			}
			// Also check if the child session exists in records without parent_session_id populated.
			var matched *grokRecord
			for i := range records {
				if records[i].ID == childID {
					cp := records[i]
					matched = &cp
					break
				}
			}
			if matched != nil {
				if matched.ParentSessionID != "" && matched.ParentSessionID != parentID {
					continue
				}
				applySubagentMetaToRecord(matched, meta, parentID)
				byChildID[childID] = *matched
			} else {
				byChildID[childID] = grokRecordFromSubagentMeta(meta, parentID, parentCwd)
			}
		}
	}

	if len(byChildID) == 0 {
		return []ThreadInfo{}
	}

	out := make([]ThreadInfo, 0, len(byChildID))
	for _, rec := range byChildID {
		running := rec.Running
		if liveCheck != nil && (liveCheck(rec.ID) || (rec.SubagentID != "" && liveCheck(rec.SubagentID))) {
			running = true
		}
		agentName := grokStorageFirstNonEmpty(rec.AgentName, rec.SubagentType, rec.Title, "Subagent")
		title := grokRecordTitle(rec, "Subagent")
		preview := grokRecordPreview(rec)
		state := grokAgentState(rec, running)
		out = append(out, ThreadInfo{
			Provider:   "grok",
			ID:         rec.ID,
			Title:      clipPlain(title, 240),
			Preview:    clipPlain(preview, 160),
			Cwd:        rec.Cwd,
			Source:     "grok",
			Kind:       "agent",
			Recency:    rec.Recency,
			Rollout:    rec.UpdatesPath,
			Active:     running,
			Running:    running,
			Subagent:   true,
			ParentID:   parentID,
			AgentPath:  rec.SubagentType,
			AgentName:  clipPlain(agentName, 240),
			AgentID:    grokStorageFirstNonEmpty(rec.SubagentID, rec.ID),
			AgentState: state,
			TurnAt:     rec.TurnAt,
			Branch:     rec.HeadBranch,
			Model:      rec.Model,
			Effort:     rec.ReasoningEffort,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Running != out[j].Running {
			return out[i].Running
		}
		if out[i].Recency != out[j].Recency {
			return out[i].Recency > out[j].Recency
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// grokIsSubagent reports whether id belongs to a Grok subagent record, used
// for strict read-only ingress enforcement.
func grokIsSubagent(records []grokRecord, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	for _, rec := range records {
		if rec.ID == id || (rec.SubagentID != "" && rec.SubagentID == id) {
			return grokRecordIsSubagent(rec)
		}
	}
	return false
}

// grokSubagentControlOp reports whether op mutates a session and must be
// rejected on Grok subagents.
func grokSubagentControlOp(op string) bool {
	return agentControl(op) || op == "pin" || op == "unpin"
}

// grokRejectSubagentControl enforces read-only ingress for Grok subagents
// across all control operations as well as pin and unpin.
func grokRejectSubagentControl(records []grokRecord, id, op string) error {
	if grokIsSubagent(records, id) && grokSubagentControlOp(op) {
		return errors.New("Subagent Chats Are Read Only. Use The Parent Chat To Direct The Agent")
	}
	return nil
}

// grokOpenAgent validates parentage and returns read-only Meta and Turns.
func grokOpenAgent(records []grokRecord, parentID, agentID string, limit int) (Meta, []Turn, error) {
	view, err := grokOpenAgentTranscript(records, parentID, agentID, limit)
	if err != nil {
		return Meta{}, nil, err
	}
	return view.Meta, view.Turns, nil
}

// grokOpenAgentTranscript validates exact parentage and returns the complete
// read-only transcript view (Meta, Turns, More, Diffs) with Meta.ReadOnly = true
// and Meta.Mode = "tail".
func grokOpenAgentTranscript(records []grokRecord, parentID, agentID string, limit int) (*grokTranscript, error) {
	parentID = strings.TrimSpace(parentID)
	agentID = strings.TrimSpace(agentID)
	if parentID == "" {
		return nil, errors.New("Choose A Parent Chat")
	}
	if !grokValidSessionID(agentID) || agentID == parentID {
		return nil, errors.New("Subagent Does Not Belong To This Chat")
	}

	var agent *grokRecord
	var parentRec *grokRecord
	for i := range records {
		if records[i].ID == parentID {
			parentRec = &records[i]
		}
		if records[i].ID == agentID || (records[i].SubagentID != "" && records[i].SubagentID == agentID) {
			if grokRecordIsSubagent(records[i]) && records[i].ParentSessionID == parentID && records[i].ID != parentID {
				cp := records[i]
				agent = &cp
			}
		}
	}

	// Fallback: check <parentDir>/subagents/*/meta.json when caller passed raw records.
	if agent == nil && parentRec != nil && parentRec.SessionDir != "" {
		for _, meta := range grokReadSubagentMetas(parentRec.SessionDir) {
			if meta.ParentSessionID != "" && meta.ParentSessionID != parentID {
				continue
			}
			if meta.ChildSessionID == agentID || meta.SubagentID == agentID {
				rec := grokRecordFromSubagentMeta(meta, parentID, parentRec.Cwd)
				mismatch := false
				for i := range records {
					if records[i].ID == rec.ID {
						if records[i].ParentSessionID != "" && records[i].ParentSessionID != parentID {
							mismatch = true
							break
						}
						cp := records[i]
						applySubagentMetaToRecord(&cp, meta, parentID)
						rec = cp
						break
					}
				}
				if mismatch {
					continue
				}
				agent = &rec
				break
			}
		}
	}

	if agent == nil || !grokRecordIsSubagent(*agent) || agent.ParentSessionID != parentID || agent.ID == parentID {
		return nil, errors.New("Subagent Does Not Belong To This Chat")
	}

	state := grokAgentState(*agent, agent.Running)
	title := grokRecordTitle(*agent, "Subagent")

	var view *grokTranscript
	if agent.SessionDir != "" {
		if _, err := os.Stat(agent.SessionDir); err == nil {
			v, readErr := readGrokTranscript(agent.SessionDir, limit)
			if readErr != nil && !os.IsNotExist(readErr) {
				return nil, readErr
			}
			view = v
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if view == nil {
		view = newGrokTranscript()
		view.Meta.Title = title
		view.Meta.Cwd = agent.Cwd
		view.Meta.Model = agent.Model
		view.Meta.Effort = agent.ReasoningEffort
		if agent.SubagentDir != "" {
			if outText := grokReadSubagentOutputText(filepath.Join(agent.SubagentDir, "output.json")); outText != "" {
				turn := view.startTurn(agent.TurnAt)
				turn.Status = state
				if state == "saved" || state == "running" {
					turn.Status = "completed"
				}
				turn.Items = append(turn.Items, Item{
					ID:   turn.Key + ":assistant:0",
					K:    "assistant",
					Text: outText,
				})
			}
		}
	}

	// Enforce read-only tail metadata regardless of transcript contents.
	view.Meta.Provider = "grok"
	view.Meta.ReadOnly = true
	view.Meta.Mode = "tail"
	view.Meta.Running = agent.Running
	view.Meta.AgentState = state
	if view.Meta.Title == "" {
		view.Meta.Title = title
	}
	if view.Meta.Cwd == "" {
		view.Meta.Cwd = agent.Cwd
	}
	if view.Meta.Model == "" {
		view.Meta.Model = agent.Model
	}
	if view.Meta.Effort == "" {
		view.Meta.Effort = agent.ReasoningEffort
	}
	if view.Meta.Branch == "" {
		view.Meta.Branch = agent.HeadBranch
	}
	return view, nil
}

func grokReadSubagentOutputText(path string) string {
	raw, err := grokReadRegularFile(path, grokSmallJSONMaxBytes)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(grokStorageFirstNonEmpty(
			str(obj, "output"),
			str(obj, "summary"),
			str(obj, "result"),
			str(obj, "text"),
			str(obj, "content"),
		))
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return ""
}
