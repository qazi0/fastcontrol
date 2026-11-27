package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	grokDefaultWindow       = 15
	grokDefaultTailBytes    = 2 << 20
	grokMaxTailBytes        = 32 << 20
	grokMaxLineBytes        = 16 << 20
	grokHeadScanBytes       = 64 << 10
	grokEventsTailBytes     = 256 << 10
	grokSmallJSONMaxBytes   = 2 << 20
	grokCwdFileMaxBytes     = 8 << 10
	grokModelsCacheMaxBytes = 8 << 20
	grokConfigTOMLMaxBytes  = 1 << 20
	grokStaleRunningGrace   = time.Minute
	grokMaxIndexedSessions  = 20000
)

// Injectable seams for deterministic unit tests.
var (
	grokNow          = time.Now
	grokProcessAlive = processAlive
	grokOwners       = rolloutOwners
	grokCacheImage   = cacheImage
)

// grokActiveSession represents one row from <grokHome>/active_sessions.json.
type grokActiveSession struct {
	SessionID string
	PID       int
	PIDStart  int64
	Cwd       string
	OpenedAt  int64
}

// grokDefaults holds default Grok settings parsed from config.toml and models_cache.json.
type grokDefaults struct {
	Model          string           `json:"model,omitempty"`
	Effort         string           `json:"effort,omitempty"`
	PermissionMode string           `json:"permissionMode,omitempty"`
	ContextWindows map[string]int64 `json:"-"`
}

// grokRecord holds one indexed local Grok CLI session loaded read-only from
// <grokHome>/sessions/<encoded-cwd>/<session-id>/.
// Created, Updated, and LastActive are Unix seconds; Recency and TurnAt are
// Unix milliseconds to match ThreadInfo across providers.
type grokRecord struct {
	ID              string
	Title           string
	GeneratedTitle  string
	SessionSummary  string
	TitleIsManual   bool
	Preview         string
	LastTurnSummary string
	LastRecap       string
	Cwd             string
	EncodedCwd      string
	SessionDir      string
	UpdatesPath     string
	EventsPath      string
	SummaryPath     string
	SignalsPath     string
	Created         int64
	Updated         int64
	LastActive      int64
	Recency         int64
	Model           string
	ReasoningEffort string
	NumMessages     int
	NumChatMessages int
	ParentSessionID string
	SessionKind     string // "subagent", "headless", or ""
	AgentName       string
	SubagentID      string
	SubagentType    string
	SubagentStatus  string
	SubagentDir     string
	HeadBranch      string
	HeadCommit      string
	GitRootDir      string
	SandboxProfile  string
	CtxUsed         int64
	CtxMax          int64
	Running         bool
	External        bool
	ExternalPID     int
	TurnAt          int64
	HasSummary      bool
}

// grokTranscript holds the projected fastcontrol conversation view for a Grok
// session. The same projection helpers drive both disk replay from
// updates.jsonl and live ACP streaming in GrokManager.
type grokTranscript struct {
	Meta          Meta
	Turns         []Turn
	First         string
	More          bool
	Diffs         map[string]string
	ToolDiffs     map[string]string
	TurnDiffs     map[string]string
	ConfigOptions []acpConfigOption
	LastMsg       string
	LastErr       string
	Recap         string

	cur         int
	turnSeq     int
	streamKind  string
	streamIdx   int
	toolIndex   map[string][2]int
	toolTurnKey map[string]string
	planIndex   map[int]int
}

func newGrokTranscript() *grokTranscript {
	return &grokTranscript{
		Meta:        Meta{Provider: "grok", Mode: "tail", PermissionMode: "default"},
		Turns:       []Turn{},
		Diffs:       map[string]string{},
		ToolDiffs:   map[string]string{},
		TurnDiffs:   map[string]string{},
		cur:         -1,
		streamIdx:   -1,
		toolIndex:   map[string][2]int{},
		toolTurnKey: map[string]string{},
		planIndex:   map[int]int{},
	}
}

func (v *grokTranscript) ensureInit() {
	if v.Turns == nil {
		v.Turns = []Turn{}
	}
	if v.Diffs == nil {
		v.Diffs = map[string]string{}
	}
	if v.ToolDiffs == nil {
		v.ToolDiffs = map[string]string{}
	}
	if v.TurnDiffs == nil {
		v.TurnDiffs = map[string]string{}
	}
	if v.toolIndex == nil {
		v.toolIndex = map[string][2]int{}
	}
	if v.toolTurnKey == nil {
		v.toolTurnKey = map[string]string{}
	}
	if v.planIndex == nil {
		v.planIndex = map[int]int{}
	}
	if v.cur >= len(v.Turns) {
		v.cur = len(v.Turns) - 1
	}
	if v.Meta.Provider == "" {
		v.Meta.Provider = "grok"
	}
	if v.Meta.Mode == "" {
		v.Meta.Mode = "tail"
	}
	if v.Meta.PermissionMode == "" {
		v.Meta.PermissionMode = "default"
	}
}

// Diff returns the unified diff for a toolCallId, a turn key, or all kept turns
// in chronological order when key is empty (without duplicating entries).
func (v *grokTranscript) Diff(key string) string {
	if v == nil {
		return ""
	}
	key = strings.TrimSpace(key)
	if key != "" {
		if d := v.ToolDiffs[key]; d != "" {
			return d
		}
		if d := v.TurnDiffs[key]; d != "" {
			return d
		}
		return v.Diffs[key]
	}
	var b strings.Builder
	seen := map[string]bool{}
	for _, t := range v.Turns {
		if d := v.TurnDiffs[t.Key]; d != "" {
			seen[t.Key] = true
			b.WriteString(d)
			if !strings.HasSuffix(d, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	keys := make([]string, 0, len(v.ToolDiffs))
	for k := range v.ToolDiffs {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		d := v.ToolDiffs[k]
		if d == "" {
			continue
		}
		b.WriteString(d)
		if !strings.HasSuffix(d, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// grokHomeDir resolves the Grok home directory, honoring GROK_HOME when set
// and otherwise falling back to <home>/.grok.
func grokHomeDir(home string) string {
	if env := strings.TrimSpace(os.Getenv("GROK_HOME")); env != "" {
		if env == "~" || strings.HasPrefix(env, "~/") || strings.HasPrefix(env, `~\`) {
			base := strings.TrimSpace(home)
			if base == "" {
				base, _ = os.UserHomeDir()
			}
			if base != "" {
				if env == "~" {
					return filepath.Clean(base)
				}
				return filepath.Clean(filepath.Join(base, env[2:]))
			}
		}
		return filepath.Clean(env)
	}
	home = strings.TrimSpace(home)
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ".grok"
	}
	return filepath.Join(home, ".grok")
}

// grokHasAuth reports whether <grokHome>/auth.json exists as a non-empty
// regular file without ever reading or logging its contents.
func grokHasAuth(grokHome string) bool {
	if strings.TrimSpace(grokHome) == "" {
		grokHome = grokHomeDir("")
	}
	st, err := os.Stat(filepath.Join(grokHome, "auth.json"))
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

// grokHasSessions quickly checks whether <grokHome>/sessions contains at least
// one session directory.
func grokHasSessions(grokHome string) bool {
	if strings.TrimSpace(grokHome) == "" {
		grokHome = grokHomeDir("")
	}
	sessionsRoot := filepath.Join(grokHome, "sessions")
	cwdDirs, err := os.ReadDir(sessionsRoot)
	if err != nil {
		return false
	}
	for _, cwdEntry := range cwdDirs {
		if !cwdEntry.IsDir() || cwdEntry.Type()&os.ModeSymlink != 0 {
			continue
		}
		children, err := os.ReadDir(filepath.Join(sessionsRoot, cwdEntry.Name()))
		if err != nil {
			continue
		}
		for _, child := range children {
			if !child.IsDir() || child.Type()&os.ModeSymlink != 0 {
				continue
			}
			if grokValidSessionID(child.Name()) {
				return true
			}
		}
	}
	return false
}

func grokValidSessionID(id string) bool {
	if !organizationID(id) || id == "." || id == ".." {
		return false
	}
	return !strings.Contains(id, "..")
}

// grokLoadRecords indexes all local Grok CLI sessions under
// <grokHome>/sessions/<encoded-cwd>/<session-id>/ read-only, enriches context
// metrics from signals.json and subagent metadata from subagents/*/meta.json,
// detects live external TUI sessions via PID-verified active_sessions.json,
// and applies an events.jsonl + mtime + rolloutOwners stale-running guard.
func grokLoadRecords(grokHome string, liveCheck func(string) bool) ([]grokRecord, error) {
	if strings.TrimSpace(grokHome) == "" {
		grokHome = grokHomeDir("")
	}
	sessionsRoot := filepath.Join(grokHome, "sessions")
	cwdDirs, err := os.ReadDir(sessionsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return []grokRecord{}, nil
		}
		return nil, err
	}

	activeMap := grokReadActiveSessions(grokHome)
	byID := make(map[string]grokRecord, 32)
	scanned := 0

	for _, cwdEntry := range cwdDirs {
		if scanned >= grokMaxIndexedSessions {
			break
		}
		if !cwdEntry.IsDir() || cwdEntry.Type()&os.ModeSymlink != 0 {
			continue
		}
		encodedCwd := cwdEntry.Name()
		cwdGroupDir := filepath.Join(sessionsRoot, encodedCwd)
		fallbackCwd := grokResolveEncodedCwd(cwdGroupDir, encodedCwd)

		sessionEntries, readErr := os.ReadDir(cwdGroupDir)
		if readErr != nil {
			continue
		}
		for _, sessEntry := range sessionEntries {
			if scanned >= grokMaxIndexedSessions {
				break
			}
			if !sessEntry.IsDir() || sessEntry.Type()&os.ModeSymlink != 0 {
				continue
			}
			dirID := sessEntry.Name()
			if !grokValidSessionID(dirID) {
				continue
			}
			sessionDir := filepath.Join(cwdGroupDir, dirID)
			rec, ok := grokLoadOneRecord(sessionDir, encodedCwd, dirID, fallbackCwd)
			if !ok {
				continue
			}
			scanned++
			if prev, exists := byID[rec.ID]; exists {
				byID[rec.ID] = mergeGrokRecord(prev, rec)
			} else {
				byID[rec.ID] = rec
			}
		}
	}

	// Enrich subagent relationships from <parentDir>/subagents/*/meta.json so
	// both child session directories and meta-only subagents carry exact linkage.
	parentSnapshot := make([]grokRecord, 0, len(byID))
	for _, rec := range byID {
		parentSnapshot = append(parentSnapshot, rec)
	}
	for _, parent := range parentSnapshot {
		metas := grokReadSubagentMetas(parent.SessionDir)
		for _, meta := range metas {
			if meta.ParentSessionID != "" && meta.ParentSessionID != parent.ID {
				continue
			}
			childID := grokStorageFirstNonEmpty(meta.ChildSessionID, meta.SubagentID)
			if !grokValidSessionID(childID) || childID == parent.ID {
				continue
			}
			if existing, ok := byID[childID]; ok {
				if existing.ParentSessionID != "" && existing.ParentSessionID != parent.ID {
					continue
				}
				applySubagentMetaToRecord(&existing, meta, parent.ID)
				byID[childID] = existing
			} else {
				byID[childID] = grokRecordFromSubagentMeta(meta, parent.ID, parent.Cwd)
			}
		}
	}

	records := make([]grokRecord, 0, len(byID))
	for _, rec := range byID {
		if active, ok := activeMap[rec.ID]; ok {
			rec.External = true
			rec.ExternalPID = active.PID
			if rec.Cwd == "" && active.Cwd != "" {
				rec.Cwd = active.Cwd
			}
		}
		records = append(records, rec)
	}

	grokApplyRunningGuard(records, liveCheck, grokNow(), grokOwners)

	sort.Slice(records, func(i, j int) bool {
		if records[i].Recency != records[j].Recency {
			return records[i].Recency > records[j].Recency
		}
		return records[i].ID < records[j].ID
	})
	return records, nil
}

type grokSummaryWire struct {
	Info struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"info"`
	ID              string          `json:"id"`
	SessionID       string          `json:"session_id"`
	Cwd             string          `json:"cwd"`
	GeneratedTitle  string          `json:"generated_title"`
	SessionSummary  string          `json:"session_summary"`
	TitleIsManual   bool            `json:"title_is_manual"`
	CreatedAt       json.RawMessage `json:"created_at"`
	UpdatedAt       json.RawMessage `json:"updated_at"`
	LastActiveAt    json.RawMessage `json:"last_active_at"`
	CurrentModelID  string          `json:"current_model_id"`
	ReasoningEffort string          `json:"reasoning_effort"`
	NumMessages     int             `json:"num_messages"`
	NumChatMessages int             `json:"num_chat_messages"`
	ParentSessionID string          `json:"parent_session_id"`
	SessionKind     string          `json:"session_kind"`
	AgentName       string          `json:"agent_name"`
	HeadBranch      string          `json:"head_branch"`
	HeadCommit      string          `json:"head_commit"`
	GitRootDir      string          `json:"git_root_dir"`
	SandboxProfile  string          `json:"sandbox_profile"`
	LastTurnSummary string          `json:"last_turn_summary"`
	LastRecap       string          `json:"last_recap"`
}

type grokSignalsWire struct {
	ContextTokensUsed        int64 `json:"contextTokensUsed"`
	ContextWindowTokens      int64 `json:"contextWindowTokens"`
	ContextTokensUsedSnake   int64 `json:"context_tokens_used"`
	ContextWindowTokensSnake int64 `json:"context_window_tokens"`
}

func (s grokSignalsWire) used() int64 {
	if s.ContextTokensUsed > 0 {
		return s.ContextTokensUsed
	}
	return s.ContextTokensUsedSnake
}

func (s grokSignalsWire) window() int64 {
	if s.ContextWindowTokens > 0 {
		return s.ContextWindowTokens
	}
	return s.ContextWindowTokensSnake
}

func grokReadRegularFile(path string, maxBytes int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxBytes {
		return nil, errors.New("file is not regular or exceeds limit")
	}
	return os.ReadFile(path)
}

func grokReadSummaryFile(path string) (grokSummaryWire, bool) {
	b, err := grokReadRegularFile(path, grokSmallJSONMaxBytes)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return grokSummaryWire{}, false
	}
	var w grokSummaryWire
	if json.Unmarshal(b, &w) != nil {
		return grokSummaryWire{}, false
	}
	return w, true
}

func grokReadSignalsFile(path string) (grokSignalsWire, bool) {
	b, err := grokReadRegularFile(path, grokSmallJSONMaxBytes)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return grokSignalsWire{}, false
	}
	var w grokSignalsWire
	if json.Unmarshal(b, &w) != nil {
		return grokSignalsWire{}, false
	}
	return w, true
}

func grokLoadOneRecord(sessionDir, encodedCwd, dirID, fallbackCwd string) (grokRecord, bool) {
	summaryPath := filepath.Join(sessionDir, "summary.json")
	updatesPath := filepath.Join(sessionDir, "updates.jsonl")
	eventsPath := filepath.Join(sessionDir, "events.jsonl")
	signalsPath := filepath.Join(sessionDir, "signals.json")

	summary, hasSummary := grokReadSummaryFile(summaryPath)
	updatesStat, hasUpdates := grokStatRegular(updatesPath)
	eventsStat, hasEvents := grokStatRegular(eventsPath)
	if !hasSummary && !hasUpdates && !hasEvents {
		return grokRecord{}, false
	}

	id := grokStorageFirstNonEmpty(strings.TrimSpace(summary.Info.ID), strings.TrimSpace(summary.ID), strings.TrimSpace(summary.SessionID), dirID)
	if !grokValidSessionID(id) {
		id = dirID
	}

	cwd := grokStorageFirstNonEmpty(strings.TrimSpace(summary.Info.Cwd), strings.TrimSpace(summary.Cwd), fallbackCwd)
	if cwd != "" {
		cwd = filepath.Clean(cwd)
	}

	createdT := parseGrokTimeJSON(summary.CreatedAt)
	updatedT := parseGrokTimeJSON(summary.UpdatedAt)
	lastActiveT := parseGrokTimeJSON(summary.LastActiveAt)

	latestT := updatedT
	if lastActiveT.After(latestT) {
		latestT = lastActiveT
	}
	if latestT.IsZero() {
		latestT = createdT
	}
	if hasUpdates && updatesStat.ModTime().After(latestT) {
		latestT = updatesStat.ModTime().UTC()
	}
	if hasEvents && eventsStat.ModTime().After(latestT) {
		latestT = eventsStat.ModTime().UTC()
	}

	var createdSec, updatedSec, lastActiveSec, recencyMs int64
	if !createdT.IsZero() {
		createdSec = createdT.Unix()
	}
	if !updatedT.IsZero() {
		updatedSec = updatedT.Unix()
	}
	if !lastActiveT.IsZero() {
		lastActiveSec = lastActiveT.Unix()
	}
	if !latestT.IsZero() {
		recencyMs = latestT.UnixMilli()
		if updatedSec == 0 {
			updatedSec = latestT.Unix()
		}
		if createdSec == 0 {
			createdSec = updatedSec
		}
	}

	genTitle := cleanGrokTitle(summary.GeneratedTitle)
	sessSummary := cleanGrokTitle(summary.SessionSummary)
	lastTurn := strings.TrimSpace(summary.LastTurnSummary)
	lastRecap := strings.TrimSpace(summary.LastRecap)

	title := grokStorageFirstNonEmpty(genTitle, sessSummary)
	preview := grokStorageFirstNonEmpty(lastTurn, lastRecap, sessSummary, genTitle)

	rec := grokRecord{
		ID:              id,
		Title:           title,
		GeneratedTitle:  genTitle,
		SessionSummary:  sessSummary,
		TitleIsManual:   summary.TitleIsManual,
		Preview:         clipPlain(preview, 200),
		LastTurnSummary: lastTurn,
		LastRecap:       lastRecap,
		Cwd:             cwd,
		EncodedCwd:      encodedCwd,
		SessionDir:      sessionDir,
		UpdatesPath:     updatesPath,
		EventsPath:      eventsPath,
		SummaryPath:     summaryPath,
		SignalsPath:     signalsPath,
		Created:         createdSec,
		Updated:         updatedSec,
		LastActive:      lastActiveSec,
		Recency:         recencyMs,
		Model:           strings.TrimSpace(summary.CurrentModelID),
		ReasoningEffort: strings.TrimSpace(summary.ReasoningEffort),
		NumMessages:     summary.NumMessages,
		NumChatMessages: summary.NumChatMessages,
		ParentSessionID: strings.TrimSpace(summary.ParentSessionID),
		SessionKind:     strings.TrimSpace(summary.SessionKind),
		AgentName:       strings.TrimSpace(summary.AgentName),
		HeadBranch:      strings.TrimSpace(summary.HeadBranch),
		HeadCommit:      strings.TrimSpace(summary.HeadCommit),
		GitRootDir:      strings.TrimSpace(summary.GitRootDir),
		SandboxProfile:  strings.TrimSpace(summary.SandboxProfile),
		HasSummary:      hasSummary,
	}

	if sig, ok := grokReadSignalsFile(signalsPath); ok {
		rec.CtxUsed = sig.used()
		rec.CtxMax = sig.window()
	}
	return rec, true
}

func mergeGrokRecord(existing, candidate grokRecord) grokRecord {
	winner, loser := existing, candidate
	if (!existing.HasSummary && candidate.HasSummary) ||
		(existing.HasSummary == candidate.HasSummary && candidate.Recency > existing.Recency) {
		winner, loser = candidate, existing
	}
	if winner.Title == "" {
		winner.Title = loser.Title
	}
	if winner.GeneratedTitle == "" {
		winner.GeneratedTitle = loser.GeneratedTitle
	}
	if winner.SessionSummary == "" {
		winner.SessionSummary = loser.SessionSummary
	}
	if winner.Preview == "" {
		winner.Preview = loser.Preview
	}
	if winner.Cwd == "" {
		winner.Cwd = loser.Cwd
	}
	if winner.Model == "" {
		winner.Model = loser.Model
	}
	if winner.ReasoningEffort == "" {
		winner.ReasoningEffort = loser.ReasoningEffort
	}
	if winner.ParentSessionID == "" {
		winner.ParentSessionID = loser.ParentSessionID
	}
	if winner.SessionKind == "" {
		winner.SessionKind = loser.SessionKind
	}
	if winner.AgentName == "" {
		winner.AgentName = loser.AgentName
	}
	if winner.HeadBranch == "" {
		winner.HeadBranch = loser.HeadBranch
	}
	if winner.CtxUsed == 0 {
		winner.CtxUsed = loser.CtxUsed
	}
	if winner.CtxMax == 0 {
		winner.CtxMax = loser.CtxMax
	}
	if winner.Created == 0 || (loser.Created > 0 && loser.Created < winner.Created) {
		winner.Created = loser.Created
	}
	return winner
}

func grokResolveEncodedCwd(cwdGroupDir, encodedCwd string) string {
	if raw, err := grokReadRegularFile(filepath.Join(cwdGroupDir, ".cwd"), grokCwdFileMaxBytes); err == nil {
		if line := strings.TrimSpace(string(raw)); line != "" && !strings.ContainsRune(line, '\x00') {
			return filepath.Clean(line)
		}
	}
	if decoded, err := url.PathUnescape(encodedCwd); err == nil {
		decoded = strings.TrimSpace(decoded)
		if decoded != "" && !strings.ContainsRune(decoded, '\x00') {
			if strings.HasPrefix(decoded, "/") || strings.Contains(decoded, `\`) || (len(decoded) >= 2 && decoded[1] == ':') {
				return filepath.Clean(decoded)
			}
		}
	}
	return ""
}

func grokStatRegular(path string) (os.FileInfo, bool) {
	if strings.TrimSpace(path) == "" {
		return nil, false
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	return st, true
}

// grokReadActiveSessions reads <grokHome>/active_sessions.json and returns
// only entries whose PID is confirmed alive via grokProcessAlive.
func grokReadActiveSessions(grokHome string) map[string]grokActiveSession {
	out := map[string]grokActiveSession{}
	path := filepath.Join(grokHome, "active_sessions.json")
	b, err := grokReadRegularFile(path, grokSmallJSONMaxBytes)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return out
	}

	type rawActive struct {
		SessionID      string          `json:"session_id"`
		SessionIDCamel string          `json:"sessionId"`
		PID            int             `json:"pid"`
		PIDStart       int64           `json:"pid_start"`
		PIDStartCamel  int64           `json:"pidStart"`
		Cwd            string          `json:"cwd"`
		OpenedAt       json.RawMessage `json:"opened_at"`
		OpenedAtCamel  json.RawMessage `json:"openedAt"`
	}

	var rows []rawActive
	if json.Unmarshal(b, &rows) != nil {
		var wrapper struct {
			Sessions []rawActive `json:"sessions"`
		}
		if json.Unmarshal(b, &wrapper) != nil {
			return out
		}
		rows = wrapper.Sessions
	}

	aliveFn := grokProcessAlive
	if aliveFn == nil {
		aliveFn = processAlive
	}
	for _, row := range rows {
		sid := grokStorageFirstNonEmpty(strings.TrimSpace(row.SessionID), strings.TrimSpace(row.SessionIDCamel))
		if !grokValidSessionID(sid) || row.PID <= 0 {
			continue
		}
		if !aliveFn(row.PID) {
			continue
		}
		pidStart := row.PIDStart
		if pidStart == 0 {
			pidStart = row.PIDStartCamel
		}
		openedT := parseGrokTimeJSON(row.OpenedAt)
		if openedT.IsZero() {
			openedT = parseGrokTimeJSON(row.OpenedAtCamel)
		}
		var openedMs int64
		if !openedT.IsZero() {
			openedMs = openedT.UnixMilli()
		}
		entry := grokActiveSession{
			SessionID: sid,
			PID:       row.PID,
			PIDStart:  pidStart,
			Cwd:       strings.TrimSpace(row.Cwd),
			OpenedAt:  openedMs,
		}
		if prev, exists := out[sid]; !exists || entry.OpenedAt >= prev.OpenedAt {
			out[sid] = entry
		}
	}
	return out
}

type grokEventsTailState struct {
	hasTurnStarted bool
	inFlight       bool
	turnAtMs       int64
}

// grokInspectEventsTail inspects the tail of <sessionDir>/events.jsonl for an
// unpaired turn_started without a subsequent turn_ended.
func grokInspectEventsTail(eventsPath string) grokEventsTailState {
	st, ok := grokStatRegular(eventsPath)
	if !ok || st.Size() == 0 {
		return grokEventsTailState{}
	}
	f, err := os.Open(eventsPath)
	if err != nil {
		return grokEventsTailState{}
	}
	defer f.Close()

	start := st.Size() - grokEventsTailBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return grokEventsTailState{}
	}
	lines := bytes.Split(buf, []byte{'\n'})
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}

	var state grokEventsTailState
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		kind, tsMs := parseGrokEventLine(line)
		switch kind {
		case "turn_started":
			state.hasTurnStarted = true
			state.inFlight = true
			state.turnAtMs = tsMs
			return state
		case "turn_ended", "turn_completed", "turn_cancelled", "turn_canceled", "turn_failed", "turn_aborted":
			state.inFlight = false
			if state.turnAtMs == 0 && tsMs > 0 {
				state.turnAtMs = tsMs
			}
			return state
		}
	}
	return state
}

func parseGrokEventLine(line []byte) (string, int64) {
	var raw map[string]any
	if json.Unmarshal(line, &raw) != nil || len(raw) == 0 {
		return "", 0
	}
	tsMs := grokExtractTimestampMs(raw)
	for _, key := range []string{"type", "event", "kind", "event_type", "name"} {
		if s := strings.ToLower(strings.TrimSpace(str(raw, key))); s != "" {
			return s, tsMs
		}
	}
	for _, nested := range []string{"event", "payload", "data"} {
		if m, ok := raw[nested].(map[string]any); ok {
			if tsMs == 0 {
				tsMs = grokExtractTimestampMs(m)
			}
			for _, key := range []string{"type", "kind", "name", "event_type"} {
				if s := strings.ToLower(strings.TrimSpace(str(m, key))); s != "" {
					return s, tsMs
				}
			}
			for k := range m {
				lk := strings.ToLower(strings.TrimSpace(k))
				if isGrokTurnLifecycleKey(lk) {
					return lk, tsMs
				}
			}
		}
	}
	for k := range raw {
		lk := strings.ToLower(strings.TrimSpace(k))
		if isGrokTurnLifecycleKey(lk) {
			if sub, ok := raw[k].(map[string]any); ok && tsMs == 0 {
				tsMs = grokExtractTimestampMs(sub)
			}
			return lk, tsMs
		}
	}
	return "", tsMs
}

func isGrokTurnLifecycleKey(k string) bool {
	switch k {
	case "turn_started", "turn_ended", "turn_completed", "turn_cancelled", "turn_canceled", "turn_failed", "turn_aborted":
		return true
	}
	return false
}

func grokExtractTimestampMs(m map[string]any) int64 {
	if m == nil {
		return 0
	}
	if us, ok := num(m, "timestamp_us"); ok && us > 0 {
		return us / 1000
	}
	for _, key := range []string{"timestamp_ms", "timestampMs", "agentTimestampMs", "started_at", "startedAt", "timestamp", "ts"} {
		if v, ok := m[key]; ok {
			if t := parseGrokTime(v); !t.IsZero() {
				return t.UnixMilli()
			}
		}
	}
	return 0
}

func grokApplyRunningGuard(records []grokRecord, liveCheck func(string) bool, now time.Time, owners *rolloutOwnerCache) {
	if now.IsZero() {
		now = time.Now()
	}
	staleFiles := map[string]os.FileInfo{}
	staleIndices := make([]int, 0)
	stalePaths := map[int]string{}

	for i := range records {
		rec := &records[i]
		if liveCheck != nil && liveCheck(rec.ID) {
			rec.Running = true
			continue
		}

		ev := grokInspectEventsTail(rec.EventsPath)
		if ev.turnAtMs > 0 {
			rec.TurnAt = ev.turnAtMs
		}
		if !ev.inFlight {
			rec.Running = false
			continue
		}

		// events.jsonl shows an unpaired turn_started.
		// If active_sessions.json verified a live external PID, trust it as running.
		if rec.External {
			rec.Running = true
			continue
		}

		var latestInfo os.FileInfo
		var latestPath string
		if st, ok := grokStatRegular(rec.UpdatesPath); ok {
			latestInfo = st
			latestPath = rec.UpdatesPath
		}
		if st, ok := grokStatRegular(rec.EventsPath); ok {
			if latestInfo == nil || st.ModTime().After(latestInfo.ModTime()) {
				latestInfo = st
				latestPath = rec.EventsPath
			}
		}

		if latestInfo != nil {
			if now.Sub(latestInfo.ModTime()) <= grokStaleRunningGrace {
				rec.Running = true
				continue
			}
			if owners != nil {
				staleFiles[latestPath] = latestInfo
				staleIndices = append(staleIndices, i)
				stalePaths[i] = latestPath
			} else {
				rec.Running = false
			}
			continue
		}
		rec.Running = false
	}

	if len(staleFiles) == 0 || owners == nil {
		return
	}
	owners.refresh(staleFiles)
	for _, idx := range staleIndices {
		path := stalePaths[idx]
		info := staleFiles[path]
		if info != nil && owners.running(path, info) {
			records[idx].Running = true
		} else {
			records[idx].Running = false
		}
	}
}

// ---- models_cache.json & config.toml ----

// grokReadModelsCache reads <grokHome>/models_cache.json and
// <grokHome>/config.toml (without external TOML dependencies) and returns the
// available Grok models alongside configured defaults.
func grokReadModelsCache(grokHome string) ([]Model, grokDefaults, error) {
	if strings.TrimSpace(grokHome) == "" {
		grokHome = grokHomeDir("")
	}

	defaults := grokDefaults{
		ContextWindows: map[string]int64{},
	}

	cfgPath := filepath.Join(grokHome, "config.toml")
	if cfgBytes, err := grokReadRegularFile(cfgPath, grokConfigTOMLMaxBytes); err == nil {
		parsedCfg := parseGrokConfigTOML(string(cfgBytes))
		defaults.Model = parsedCfg.Model
		defaults.Effort = parsedCfg.Effort
		defaults.PermissionMode = parsedCfg.PermissionMode
	}

	cachePath := filepath.Join(grokHome, "models_cache.json")
	raw, err := grokReadRegularFile(cachePath, grokModelsCacheMaxBytes)
	if err != nil {
		if os.IsNotExist(err) {
			if defaults.PermissionMode == "" {
				defaults.PermissionMode = "default"
			}
			return []Model{}, defaults, nil
		}
		return nil, defaults, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if defaults.PermissionMode == "" {
			defaults.PermissionMode = "default"
		}
		return []Model{}, defaults, nil
	}

	models, ctxWindows, err := parseGrokModelsCacheJSON(raw)
	if err != nil {
		return nil, defaults, err
	}
	defaults.ContextWindows = ctxWindows

	if defaults.Model == "" && len(models) > 0 {
		defaults.Model = models[0].Slug
	}
	if defaults.Effort == "" {
		for _, m := range models {
			if m.Slug == defaults.Model && m.DefaultEffort != "" {
				defaults.Effort = m.DefaultEffort
				break
			}
		}
		if defaults.Effort == "" && len(models) > 0 {
			defaults.Effort = models[0].DefaultEffort
		}
	}
	if defaults.PermissionMode == "" {
		defaults.PermissionMode = "default"
	}
	return models, defaults, nil
}

func grokEffortRank(effort string) int {
	switch effort {
	case "minimal":
		return 0
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "xhigh":
		return 4
	case "max":
		return 5
	default:
		return 100
	}
}

// sortGrokEfforts normalizes (trimmed lowercase), deduplicates, and sorts Grok
// reasoning effort values strictly ascending by intelligence level while
// preserving any unknown future labels in their original order after known ones.
func sortGrokEfforts(efforts []string) []string {
	if len(efforts) == 0 {
		return nil
	}
	out := make([]string, 0, len(efforts))
	seen := make(map[string]bool, len(efforts))
	for _, raw := range efforts {
		val := strings.ToLower(strings.TrimSpace(raw))
		if val == "" || seen[val] {
			continue
		}
		seen[val] = true
		out = append(out, val)
	}
	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool {
		return grokEffortRank(out[i]) < grokEffortRank(out[j])
	})
	return out
}

type grokRawModelItem struct {
	key string
	raw json.RawMessage
}

func parseGrokRawModelList(raw json.RawMessage) ([]grokRawModelItem, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	switch trimmed[0] {
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, err
		}
		items := make([]grokRawModelItem, 0, len(arr))
		for _, elem := range arr {
			items = append(items, grokRawModelItem{raw: elem})
		}
		return items, nil
	case '{':
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		delim, ok := tok.(json.Delim)
		if !ok || delim != '{' {
			return nil, errors.New("expected JSON object for Grok models")
		}
		var items []grokRawModelItem
		for dec.More() {
			kTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := kTok.(string)
			if !ok {
				return nil, errors.New("expected string key in Grok models map")
			}
			var val json.RawMessage
			if err := dec.Decode(&val); err != nil {
				return nil, err
			}
			items = append(items, grokRawModelItem{key: key, raw: val})
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return items, nil
	default:
		return nil, errors.New("unsupported Grok models container")
	}
}

func parseGrokModelsCacheJSON(raw []byte) ([]Model, map[string]int64, error) {
	var rawItems []grokRawModelItem
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return []Model{}, map[string]int64{}, nil
	}
	if trimmed[0] == '[' {
		items, err := parseGrokRawModelList(trimmed)
		if err != nil {
			return nil, nil, err
		}
		rawItems = items
	} else {
		var envelope struct {
			Models          json.RawMessage `json:"models"`
			AvailableModels json.RawMessage `json:"availableModels"`
			Data            json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(trimmed, &envelope); err != nil {
			return nil, nil, err
		}
		for _, field := range []json.RawMessage{envelope.Models, envelope.AvailableModels, envelope.Data} {
			if len(bytes.TrimSpace(field)) == 0 || bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
				continue
			}
			items, err := parseGrokRawModelList(field)
			if err != nil {
				return nil, nil, err
			}
			if len(items) > 0 {
				rawItems = items
				break
			}
		}
	}

	models := make([]Model, 0, len(rawItems))
	ctxWindows := make(map[string]int64, len(rawItems))
	seen := map[string]bool{}

	for _, entry := range rawItems {
		var outer map[string]any
		if json.Unmarshal(entry.raw, &outer) != nil || len(outer) == 0 {
			continue
		}
		if disabled, _ := outer["disabled"].(bool); disabled {
			continue
		}
		if hidden, _ := outer["hidden"].(bool); hidden {
			continue
		}
		m := outer
		if infoMap, ok := outer["info"].(map[string]any); ok && len(infoMap) > 0 {
			m = infoMap
			if disabled, _ := m["disabled"].(bool); disabled {
				continue
			}
			if hidden, _ := m["hidden"].(bool); hidden {
				continue
			}
		}
		slug := grokStorageFirstNonEmpty(
			strings.TrimSpace(str(m, "id")),
			strings.TrimSpace(str(m, "model_id")),
			strings.TrimSpace(str(m, "modelId")),
			strings.TrimSpace(str(m, "slug")),
			strings.TrimSpace(str(m, "model")),
			strings.TrimSpace(entry.key),
		)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true

		metaMap, _ := m["_meta"].(map[string]any)
		name := grokStorageFirstNonEmpty(
			strings.TrimSpace(str(m, "display_name")),
			strings.TrimSpace(str(m, "displayName")),
			strings.TrimSpace(str(m, "name")),
			slug,
		)
		desc := strings.TrimSpace(str(m, "description"))
		efforts, flaggedDefault := extractGrokEfforts(m)
		if len(efforts) == 0 && metaMap != nil {
			efforts, flaggedDefault = extractGrokEfforts(metaMap)
		}
		efforts = sortGrokEfforts(efforts)
		defaultEffort := strings.ToLower(grokStorageFirstNonEmpty(
			strings.TrimSpace(str(m, "default_reasoning_effort")),
			strings.TrimSpace(str(m, "defaultReasoningEffort")),
			strings.TrimSpace(str(m, "default_reasoning_level")),
			strings.TrimSpace(str(m, "defaultEffort")),
			strings.TrimSpace(str(m, "reasoning_effort")),
			strings.TrimSpace(str(m, "reasoningEffort")),
			strings.TrimSpace(str(metaMap, "default_reasoning_effort")),
			strings.TrimSpace(str(metaMap, "defaultReasoningEffort")),
			strings.TrimSpace(str(metaMap, "reasoningEffort")),
			flaggedDefault,
		))
		if defaultEffort == "" && len(efforts) > 0 {
			for _, candidate := range []string{"high", "medium"} {
				for _, e := range efforts {
					if e == candidate {
						defaultEffort = e
						break
					}
				}
				if defaultEffort != "" {
					break
				}
			}
			if defaultEffort == "" {
				defaultEffort = efforts[0]
			}
		}

		ctxWin := extractGrokContextWindow(m)
		if ctxWin == 0 && metaMap != nil {
			ctxWin = extractGrokContextWindow(metaMap)
		}
		if ctxWin > 0 {
			ctxWindows[slug] = ctxWin
		}

		models = append(models, Model{
			Slug:          slug,
			Name:          name,
			Description:   desc,
			DefaultEffort: defaultEffort,
			Efforts:       efforts,
		})
	}
	return models, ctxWindows, nil
}

func extractGrokContextWindow(m map[string]any) int64 {
	if m == nil {
		return 0
	}
	for _, key := range []string{
		"context_window",
		"contextWindow",
		"totalContextTokens",
		"total_context_tokens",
		"max_context_tokens",
		"maxContextTokens",
		"context_length",
		"contextLength",
	} {
		if n, ok := num(m, key); ok && n > 0 {
			return n
		}
	}
	return 0
}

func extractGrokEfforts(m map[string]any) ([]string, string) {
	var rawVals []any
	for _, key := range []string{
		"supported_reasoning_efforts",
		"supportedReasoningEfforts",
		"supported_reasoning_levels",
		"reasoning_efforts",
		"reasoningEfforts",
		"efforts",
	} {
		if arr := arrayValue(m[key]); len(arr) > 0 {
			rawVals = arr
			break
		}
	}
	if len(rawVals) == 0 {
		return nil, ""
	}
	out := make([]string, 0, len(rawVals))
	flaggedDefault := ""
	for _, item := range rawVals {
		var val string
		switch x := item.(type) {
		case string:
			val = strings.ToLower(strings.TrimSpace(x))
		case map[string]any:
			val = strings.ToLower(grokStorageFirstNonEmpty(
				strings.TrimSpace(str(x, "effort")),
				strings.TrimSpace(str(x, "value")),
				strings.TrimSpace(str(x, "id")),
				strings.TrimSpace(str(x, "name")),
			))
			if isDef, _ := x["default"].(bool); isDef && val != "" && flaggedDefault == "" {
				flaggedDefault = val
			}
		}
		if val != "" {
			out = append(out, val)
		}
	}
	return sortGrokEfforts(out), flaggedDefault
}

// parseGrokConfigTOML extracts [models] default, default_reasoning_effort, and
// [ui] permission_mode (plus deprecated approval_mode) from config.toml using
// a lightweight scoped section/key parser with zero external dependencies.
func parseGrokConfigTOML(content string) grokDefaults {
	var out grokDefaults
	var fallbackApproval string
	section := ""

	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(stripTOMLComment(rawLine))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[[") && strings.HasSuffix(line, "]]") {
			section = strings.TrimSpace(line[2 : len(line)-2])
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		valRaw := strings.TrimSpace(line[eq+1:])
		if key == "" || valRaw == "" {
			continue
		}
		fullKey := key
		if section != "" {
			fullKey = section + "." + key
		}
		val, ok := parseTOMLStringValue(valRaw)
		if !ok {
			continue
		}
		switch fullKey {
		case "models.default":
			out.Model = val
		case "models.default_reasoning_effort":
			out.Effort = strings.ToLower(strings.TrimSpace(val))
		case "ui.permission_mode":
			out.PermissionMode = normalizeGrokPermissionMode(val)
		case "ui.approval_mode":
			fallbackApproval = normalizeGrokPermissionMode(val)
		}
	}
	if out.PermissionMode == "" && fallbackApproval != "" {
		out.PermissionMode = fallbackApproval
	}
	return out
}

func stripTOMLComment(line string) string {
	inDouble := false
	inSingle := false
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '#':
			return line[:i]
		}
	}
	return line
}

func parseTOMLStringValue(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false
	}
	if raw[0] == '"' {
		if unq, err := strconv.Unquote(raw); err == nil {
			return strings.TrimSpace(unq), true
		}
		if len(raw) >= 2 && raw[len(raw)-1] == '"' {
			return strings.TrimSpace(raw[1 : len(raw)-1]), true
		}
		return "", false
	}
	if raw[0] == '\'' {
		if len(raw) >= 2 && raw[len(raw)-1] == '\'' {
			return strings.TrimSpace(raw[1 : len(raw)-1]), true
		}
		return "", false
	}
	// Bare unquoted token (defensive fallback).
	if strings.ContainsAny(raw, "[]{},") {
		return "", false
	}
	return strings.TrimSpace(raw), true
}

func normalizeGrokPermissionMode(raw string) string {
	s := strings.TrimSpace(raw)
	switch strings.ToLower(s) {
	case "":
		return ""
	case "default", "ask":
		return "default"
	case "acceptedits", "accept-edits", "accept_edits":
		return "acceptEdits"
	case "auto", "auto-review", "auto_review":
		return "auto"
	case "dontask", "dont-ask", "dont_ask":
		return "dontAsk"
	case "plan":
		return "plan"
	case "bypasspermissions", "bypass-permissions", "bypass_permissions", "always-approve", "always_approve", "yolo":
		return "bypassPermissions"
	default:
		return s
	}
}

// ---- Transcript reader & reusable ACP update projection ----

type grokUpdateLine struct {
	Timestamp   json.RawMessage `json:"timestamp"`
	TimestampUs int64           `json:"timestamp_us"`
	Method      string          `json:"method"`
	Params      json.RawMessage `json:"params"`
}

// readGrokTranscript reads <sessionDir>/updates.jsonl (tail-bounded and
// line-bounded), plus summary.json and signals.json, and projects the session
// into fastcontrol Turns, Items, Meta, and Diffs.
func readGrokTranscript(sessionDir string, limit int) (*grokTranscript, error) {
	sessionDir = strings.TrimSpace(sessionDir)
	if sessionDir == "" {
		return nil, errors.New("Grok Session Directory Is Empty")
	}
	if limit <= 0 {
		limit = grokDefaultWindow
	}

	stDir, err := os.Stat(sessionDir)
	if err != nil {
		return nil, err
	}
	if !stDir.IsDir() {
		return nil, errors.New("Grok Session Path Is Not A Directory")
	}

	v := newGrokTranscript()

	summaryPath := filepath.Join(sessionDir, "summary.json")
	summary, hasSummary := grokReadSummaryFile(summaryPath)
	if hasSummary {
		if cwd := grokStorageFirstNonEmpty(strings.TrimSpace(summary.Info.Cwd), strings.TrimSpace(summary.Cwd)); cwd != "" {
			v.Meta.Cwd = filepath.Clean(cwd)
		}
		if branch := strings.TrimSpace(summary.HeadBranch); branch != "" {
			v.Meta.Branch = branch
		}
		if model := strings.TrimSpace(summary.CurrentModelID); model != "" {
			v.Meta.Model = model
		}
		if effort := strings.TrimSpace(summary.ReasoningEffort); effort != "" {
			v.Meta.Effort = effort
		}
		if title := grokStorageFirstNonEmpty(cleanGrokTitle(summary.GeneratedTitle), cleanGrokTitle(summary.SessionSummary)); title != "" {
			v.Meta.Title = title
		}
		if recap := strings.TrimSpace(summary.LastRecap); recap != "" {
			v.Recap = recap
		}
	}

	updatesPath := filepath.Join(sessionDir, "updates.jsonl")
	f, err := os.Open(updatesPath)
	if err != nil {
		if os.IsNotExist(err) && hasSummary {
			applyGrokSignalsFile(v, filepath.Join(sessionDir, "signals.json"))
			return v, nil
		}
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("Grok Updates File Is Not A Regular File")
	}

	pages := int64((limit + grokDefaultWindow - 1) / grokDefaultWindow)
	if pages < 1 {
		pages = 1
	}
	tailBytes := pages * grokDefaultTailBytes
	if tailBytes > grokMaxTailBytes {
		tailBytes = grokMaxTailBytes
	}

	start := st.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	v.More = start > 0

	if start > 0 {
		head := make([]byte, grokHeadScanBytes)
		n, _ := f.ReadAt(head, 0)
		for _, line := range bytes.Split(head[:n], []byte{'\n'}) {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 || trimmed[0] != '{' {
				continue
			}
			var rec grokUpdateLine
			if json.Unmarshal(trimmed, &rec) == nil && len(bytes.TrimSpace(rec.Params)) > 0 {
				v.applyHeadMetadata(rec.Method, rec.Params)
			}
		}
	}

	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(f, 64<<10)
	if start > 0 {
		_, _, _ = grokReadBoundedLine(reader, grokMaxLineBytes)
	}

	for {
		line, overlong, readErr := grokReadBoundedLine(reader, grokMaxLineBytes)
		if len(line) > 0 && !overlong {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 && trimmed[0] == '{' {
				var rec grokUpdateLine
				if json.Unmarshal(trimmed, &rec) == nil && len(bytes.TrimSpace(rec.Params)) > 0 {
					tsMs := grokLineTimestampMs(rec)
					grokApplySessionUpdate(v, rec.Method, rec.Params, tsMs)
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, readErr
		}
	}

	if len(v.Turns) > limit {
		v.Turns = v.Turns[len(v.Turns)-limit:]
		v.More = true
		v.pruneDiffsToKeptTurns()
	}

	applyGrokSignalsFile(v, filepath.Join(sessionDir, "signals.json"))

	// Manual titles recorded in summary.json always win over auto-generated ones.
	if hasSummary && summary.TitleIsManual {
		if manual := grokStorageFirstNonEmpty(cleanGrokTitle(summary.GeneratedTitle), cleanGrokTitle(summary.SessionSummary)); manual != "" {
			v.Meta.Title = manual
		}
	}
	if v.Meta.Title == "" {
		v.Meta.Title = v.First
	}
	v.Meta.HasOlder = v.More
	return v, nil
}

// readGrokTranscriptForRecord opens rec's transcript and fills in record-level
// fallback metadata (Cwd, Title, Branch, Model, Effort, Running, External Mode).
func readGrokTranscriptForRecord(rec grokRecord, limit int) (*grokTranscript, error) {
	view, err := readGrokTranscript(rec.SessionDir, limit)
	if err != nil {
		return nil, err
	}
	if view.Meta.Cwd == "" {
		view.Meta.Cwd = rec.Cwd
	}
	if view.Meta.Title == "" {
		view.Meta.Title = grokRecordTitle(rec, "")
	}
	if view.Meta.Branch == "" {
		view.Meta.Branch = rec.HeadBranch
	}
	if view.Meta.Model == "" {
		view.Meta.Model = rec.Model
	}
	if view.Meta.Effort == "" {
		view.Meta.Effort = rec.ReasoningEffort
	}
	if view.Meta.CtxUsed == 0 && rec.CtxUsed > 0 {
		view.Meta.CtxUsed = rec.CtxUsed
	}
	if view.Meta.CtxMax == 0 && rec.CtxMax > 0 {
		view.Meta.CtxMax = rec.CtxMax
	}
	view.Meta.Running = rec.Running
	if rec.TurnAt > 0 && view.Meta.TurnAt == 0 {
		view.Meta.TurnAt = rec.TurnAt
	}
	if rec.External {
		view.Meta.Mode = "external"
	} else {
		view.Meta.Mode = "tail"
	}
	return view, nil
}

func applyGrokSignalsFile(v *grokTranscript, path string) {
	sig, ok := grokReadSignalsFile(path)
	if !ok {
		return
	}
	if v.Meta.CtxUsed == 0 && sig.used() > 0 {
		v.Meta.CtxUsed = sig.used()
	}
	if v.Meta.CtxMax == 0 && sig.window() > 0 {
		v.Meta.CtxMax = sig.window()
	}
}

func grokLineTimestampMs(rec grokUpdateLine) int64 {
	if rec.TimestampUs > 0 {
		return rec.TimestampUs / 1000
	}
	if t := parseGrokTimeJSON(rec.Timestamp); !t.IsZero() {
		return t.UnixMilli()
	}
	return 0
}

func grokReadBoundedLine(r *bufio.Reader, maxBytes int) ([]byte, bool, error) {
	var buf []byte
	overlong := false
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 && !overlong {
			if len(buf)+len(chunk) > maxBytes {
				overlong = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buf, overlong, err
	}
}

func (v *grokTranscript) pruneDiffsToKeptTurns() {
	keptTurns := make(map[string]bool, len(v.Turns))
	keptItems := make(map[string]bool, len(v.Turns)*2)
	v.toolIndex = make(map[string][2]int, len(v.Turns)*2)
	v.toolTurnKey = make(map[string]string, len(v.Turns)*2)
	v.planIndex = make(map[int]int, len(v.Turns))
	for ti, t := range v.Turns {
		keptTurns[t.Key] = true
		for ii, it := range t.Items {
			keptItems[it.ID] = true
			if it.ID == t.Key+":plan" {
				v.planIndex[ti] = ii
			} else if it.K == "cmd" || it.K == "file" || it.K == "search" || it.K == "agent" || it.K == "tool" {
				v.toolIndex[it.ID] = [2]int{ti, ii}
				v.toolTurnKey[it.ID] = t.Key
			}
		}
	}
	for k := range v.TurnDiffs {
		if !keptTurns[k] {
			delete(v.TurnDiffs, k)
		}
	}
	for k := range v.ToolDiffs {
		if !keptItems[k] {
			delete(v.ToolDiffs, k)
		}
	}
	for k := range v.Diffs {
		if !keptTurns[k] && !keptItems[k] {
			delete(v.Diffs, k)
		}
	}
	v.cur = len(v.Turns) - 1
	v.streamKind = ""
	v.streamIdx = -1
}

func (v *grokTranscript) applyHeadMetadata(method string, paramsRaw json.RawMessage) {
	updateRaw, _ := grokUnwrapUpdatePayload(paramsRaw)
	if len(updateRaw) == 0 {
		return
	}
	kind := grokUpdateKind(updateRaw)
	switch kind {
	case "user_message_chunk":
		if v.First == "" {
			text, _ := grokExtractMessageChunk(updateRaw)
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				v.First = clipPlain(trimmed, 160)
			}
		}
	case "session_info_update":
		var info struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(updateRaw, &info) == nil {
			if title := cleanGrokTitle(info.Title); title != "" && v.Meta.Title == "" {
				v.Meta.Title = title
			}
		}
	case "current_mode_update":
		var m struct {
			CurrentModeID string `json:"currentModeId"`
			ModeID        string `json:"modeId"`
		}
		if json.Unmarshal(updateRaw, &m) == nil {
			if mode := normalizeGrokPermissionMode(grokStorageFirstNonEmpty(m.CurrentModeID, m.ModeID)); mode != "" {
				v.Meta.PermissionMode = mode
			}
		}
	case "config_option_update", "config_options_update":
		grokApplyConfigOptionsUpdate(v, updateRaw)
	case "available_commands_update":
		grokApplyAvailableCommandsUpdate(v, updateRaw)
	}
	_ = method
}

// ApplyUpdate projects a single ACP session/update or _x.ai/session/update
// notification onto this transcript.
func (v *grokTranscript) ApplyUpdate(method string, paramsRaw json.RawMessage, tsMs int64) bool {
	return grokApplySessionUpdate(v, method, paramsRaw, tsMs)
}

// grokApplySessionUpdate projects one ACP session/update or
// _x.ai/session/update payload into v's Turns, Items, Meta, and Diffs.
// paramsRaw may be either the full JSON-RPC params object
// ({sessionId, _meta?, update: {...}}) or a bare update object.
func grokApplySessionUpdate(v *grokTranscript, method string, paramsRaw json.RawMessage, tsMs int64) bool {
	if v == nil || len(bytes.TrimSpace(paramsRaw)) == 0 {
		return false
	}
	v.ensureInit()

	updateRaw, metaTsMs := grokUnwrapUpdatePayload(paramsRaw)
	if len(updateRaw) == 0 {
		return false
	}
	if tsMs <= 0 && metaTsMs > 0 {
		tsMs = metaTsMs
	}

	kind := grokUpdateKind(updateRaw)
	if kind == "" {
		return false
	}

	switch kind {
	case "user_message_chunk":
		text, images := grokExtractMessageChunk(updateRaw)
		if text == "" && len(images) == 0 {
			return false
		}
		// Start a new turn whenever the previous stream was not a user chunk.
		if v.cur < 0 || v.streamKind != "user" {
			v.startTurn(tsMs)
		}
		turn := &v.Turns[v.cur]
		if v.streamKind == "user" && v.streamIdx >= 0 && v.streamIdx < len(turn.Items) && turn.Items[v.streamIdx].K == "user" {
			if text != "" {
				if turn.Items[v.streamIdx].Text != "" && text == "[Image]" {
					// Avoid noisy placeholder when text already accompanies an image.
				} else {
					turn.Items[v.streamIdx].Text += text
				}
			}
			if len(images) > 0 {
				turn.Items[v.streamIdx].Images = appendUniqueStrings(turn.Items[v.streamIdx].Images, images)
			}
		} else {
			itemText := text
			if itemText == "[Image]" && len(images) > 0 {
				itemText = ""
			}
			turn.Items = append(turn.Items, Item{
				ID:     fmt.Sprintf("%s:user:%d", turn.Key, len(turn.Items)),
				K:      "user",
				Text:   itemText,
				Images: images,
			})
			v.streamKind = "user"
			v.streamIdx = len(turn.Items) - 1
		}
		if v.First == "" {
			if trimmed := strings.TrimSpace(turn.Items[v.streamIdx].Text); trimmed != "" {
				v.First = clipPlain(trimmed, 160)
			}
		}
		return true

	case "agent_thought_chunk":
		text, _ := grokExtractMessageChunk(updateRaw)
		if text == "" {
			return false
		}
		turn := v.ensureCurrentTurn(tsMs)
		if v.streamKind == "reasoning" && v.streamIdx >= 0 && v.streamIdx < len(turn.Items) && turn.Items[v.streamIdx].K == "reasoning" {
			turn.Items[v.streamIdx].Text += text
		} else {
			turn.Items = append(turn.Items, Item{
				ID:   fmt.Sprintf("%s:thought:%d", turn.Key, len(turn.Items)),
				K:    "reasoning",
				Text: text,
			})
			v.streamKind = "reasoning"
			v.streamIdx = len(turn.Items) - 1
		}
		return true

	case "agent_message_chunk":
		text, images := grokExtractMessageChunk(updateRaw)
		if text == "" && len(images) == 0 {
			return false
		}
		turn := v.ensureCurrentTurn(tsMs)
		if v.streamKind == "assistant" && v.streamIdx >= 0 && v.streamIdx < len(turn.Items) && turn.Items[v.streamIdx].K == "assistant" {
			turn.Items[v.streamIdx].Text += text
			if len(images) > 0 {
				turn.Items[v.streamIdx].Images = appendUniqueStrings(turn.Items[v.streamIdx].Images, images)
			}
		} else {
			turn.Items = append(turn.Items, Item{
				ID:     fmt.Sprintf("%s:assistant:%d", turn.Key, len(turn.Items)),
				K:      "assistant",
				Text:   text,
				Images: images,
			})
			v.streamKind = "assistant"
			v.streamIdx = len(turn.Items) - 1
		}
		if msg := strings.TrimSpace(turn.Items[v.streamIdx].Text); msg != "" {
			v.LastMsg = clipPlain(msg, 300)
		}
		return true

	case "tool_call", "tool_call_update":
		v.streamKind = ""
		v.streamIdx = -1
		return grokApplyToolCallUpdate(v, updateRaw, tsMs)

	case "plan":
		v.streamKind = ""
		v.streamIdx = -1
		return grokApplyPlanUpdate(v, updateRaw, tsMs)

	case "usage_update":
		return grokApplyUsageUpdate(v, updateRaw)

	case "available_commands_update":
		return grokApplyAvailableCommandsUpdate(v, updateRaw)

	case "current_mode_update":
		var m struct {
			CurrentModeID string `json:"currentModeId"`
			ModeID        string `json:"modeId"`
		}
		if json.Unmarshal(updateRaw, &m) != nil {
			return false
		}
		if mode := normalizeGrokPermissionMode(grokStorageFirstNonEmpty(m.CurrentModeID, m.ModeID)); mode != "" {
			v.Meta.PermissionMode = mode
			return true
		}
		return false

	case "config_option_update", "config_options_update":
		return grokApplyConfigOptionsUpdate(v, updateRaw)

	case "session_info_update":
		var info struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(updateRaw, &info) != nil {
			return false
		}
		if title := cleanGrokTitle(info.Title); title != "" {
			v.Meta.Title = title
			return true
		}
		return false

	case "turn_completed":
		v.streamKind = ""
		v.streamIdx = -1
		return grokApplyTurnCompleted(v, updateRaw, tsMs)

	case "retry_state":
		v.streamKind = ""
		v.streamIdx = -1
		return grokApplyRetryState(v, updateRaw, tsMs)

	case "session_recap":
		v.streamKind = ""
		v.streamIdx = -1
		return grokApplySessionRecap(v, updateRaw, tsMs)

	case "subagent_spawned", "subagent_finished":
		v.streamKind = ""
		v.streamIdx = -1
		return grokApplySubagentLifecycle(v, kind, updateRaw, tsMs)

	case "model_changed":
		var mc struct {
			ModelID        string `json:"modelId"`
			Model          string `json:"model"`
			CurrentModelID string `json:"currentModelId"`
		}
		if json.Unmarshal(updateRaw, &mc) != nil {
			return false
		}
		if model := strings.TrimSpace(grokStorageFirstNonEmpty(mc.ModelID, mc.CurrentModelID, mc.Model)); model != "" {
			v.Meta.Model = model
			return true
		}
		return false

	case "auto_compact", "context_compacted":
		v.streamKind = ""
		v.streamIdx = -1
		turn := v.ensureCurrentTurn(tsMs)
		turn.Items = append(turn.Items, Item{
			ID:    fmt.Sprintf("%s:compact:%d", turn.Key, len(turn.Items)),
			K:     "info",
			Title: "Context Compacted",
			Text:  "Context Compacted",
		})
		return true

	default:
		// Recognize _x.ai/session/update wrappers if method indicates an x.ai update
		// with an unknown subtype: safely ignore without corrupting turns.
		_ = method
		return false
	}
}

func (v *grokTranscript) startTurn(startedAt int64) *Turn {
	v.turnSeq++
	key := fmt.Sprintf("turn-%d", v.turnSeq)
	v.Turns = append(v.Turns, Turn{
		Key:       key,
		TurnID:    key,
		StartedAt: startedAt,
		Status:    "completed",
		Items:     []Item{},
	})
	v.cur = len(v.Turns) - 1
	v.streamKind = ""
	v.streamIdx = -1
	if startedAt > 0 {
		v.Meta.TurnAt = startedAt
	}
	return &v.Turns[v.cur]
}

func (v *grokTranscript) ensureCurrentTurn(startedAt int64) *Turn {
	if v.cur < 0 || v.cur >= len(v.Turns) {
		return v.startTurn(startedAt)
	}
	t := &v.Turns[v.cur]
	if t.StartedAt == 0 && startedAt > 0 {
		t.StartedAt = startedAt
	}
	return t
}

func grokUnwrapUpdatePayload(paramsRaw json.RawMessage) (json.RawMessage, int64) {
	trimmed := bytes.TrimSpace(paramsRaw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, 0
	}
	var env struct {
		Update json.RawMessage `json:"update"`
		Meta   struct {
			AgentTimestampMs int64 `json:"agentTimestampMs"`
		} `json:"_meta"`
	}
	if json.Unmarshal(trimmed, &env) != nil {
		return nil, 0
	}
	if len(bytes.TrimSpace(env.Update)) > 0 && !bytes.Equal(bytes.TrimSpace(env.Update), []byte("null")) {
		return bytes.TrimSpace(env.Update), env.Meta.AgentTimestampMs
	}
	return trimmed, env.Meta.AgentTimestampMs
}

func grokUpdateKind(updateRaw json.RawMessage) string {
	var header struct {
		SessionUpdate string `json:"sessionUpdate"`
		Type          string `json:"type"`
		Kind          string `json:"kind"`
	}
	if json.Unmarshal(updateRaw, &header) != nil {
		return ""
	}
	return strings.TrimSpace(grokStorageFirstNonEmpty(header.SessionUpdate, header.Type, header.Kind))
}

func grokExtractMessageChunk(updateRaw json.RawMessage) (string, []string) {
	var raw struct {
		Content json.RawMessage `json:"content"`
		Text    string          `json:"text"`
	}
	if json.Unmarshal(updateRaw, &raw) != nil {
		return "", nil
	}
	var texts []string
	var images []string
	hasImageBlock := false

	processBlock := func(blk acpContentBlock) {
		typ := strings.ToLower(strings.TrimSpace(blk.Type))
		switch typ {
		case "image", "input_image", "local_image":
			hasImageBlock = true
			img := grokResolveInlineImage(blk)
			if img != "" {
				images = appendUniqueStrings(images, []string{img})
			}
		default:
			if blk.Text != "" {
				texts = append(texts, blk.Text)
			}
		}
	}

	contentBytes := bytes.TrimSpace(raw.Content)
	if len(contentBytes) > 0 && !bytes.Equal(contentBytes, []byte("null")) {
		switch contentBytes[0] {
		case '{':
			var blk acpContentBlock
			if json.Unmarshal(contentBytes, &blk) == nil {
				processBlock(blk)
			}
		case '[':
			var blks []acpContentBlock
			if json.Unmarshal(contentBytes, &blks) == nil {
				for _, blk := range blks {
					processBlock(blk)
				}
			}
		case '"':
			var s string
			if json.Unmarshal(contentBytes, &s) == nil && s != "" {
				texts = append(texts, s)
			}
		}
	}
	if len(texts) == 0 && raw.Text != "" {
		texts = append(texts, raw.Text)
	}

	joined := strings.Join(texts, "")
	if joined == "" && hasImageBlock && len(images) == 0 {
		joined = "[Image]"
	}
	return joined, images
}

func grokResolveInlineImage(blk acpContentBlock) string {
	if data := strings.TrimSpace(blk.Data); data != "" {
		cacheFn := grokCacheImage
		if cacheFn == nil {
			cacheFn = cacheImage
		}
		if cached := cacheFn(data); cached != "" {
			return cached
		}
	}
	if uri := strings.TrimSpace(blk.URI); uri != "" {
		if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
			return uri
		}
		if p, ok := parseAntigravityFileURI(uri); ok {
			return p
		}
		if filepath.IsAbs(uri) {
			return filepath.Clean(uri)
		}
	}
	return ""
}

func grokApplyToolCallUpdate(v *grokTranscript, updateRaw json.RawMessage, tsMs int64) bool {
	tc := parseACPToolCallInfo(updateRaw)

	// Extract Grok-specific _meta["x.ai/tool"] name if standard Name is empty.
	var grokMeta struct {
		Meta map[string]any `json:"_meta"`
	}
	if json.Unmarshal(updateRaw, &grokMeta) == nil && len(grokMeta.Meta) > 0 {
		if tc.Name == "" {
			switch toolMeta := grokMeta.Meta["x.ai/tool"].(type) {
			case string:
				tc.Name = strings.TrimSpace(toolMeta)
			case map[string]any:
				tc.Name = grokStorageFirstNonEmpty(strings.TrimSpace(str(toolMeta, "name")), strings.TrimSpace(str(toolMeta, "toolName")))
			}
		}
	}

	turn := v.ensureCurrentTurn(tsMs)
	itemID := strings.TrimSpace(tc.ToolCallID)
	if itemID == "" {
		itemID = fmt.Sprintf("%s:tool:%d", turn.Key, len(turn.Items))
	}

	var existing *Item
	targetTurnIdx := v.cur
	targetItemIdx := -1
	if pos, ok := v.toolIndex[itemID]; ok {
		if pos[0] >= 0 && pos[0] < len(v.Turns) && pos[1] >= 0 && pos[1] < len(v.Turns[pos[0]].Items) && v.Turns[pos[0]].Items[pos[1]].ID == itemID {
			targetTurnIdx = pos[0]
			targetItemIdx = pos[1]
			existing = &v.Turns[targetTurnIdx].Items[targetItemIdx]
		}
	}
	if targetItemIdx < 0 {
		for i := range turn.Items {
			if turn.Items[i].ID == itemID {
				targetTurnIdx = v.cur
				targetItemIdx = i
				existing = &turn.Items[i]
				break
			}
		}
	}

	it, diffText := convertGrokToolCall(tc, itemID, existing)
	targetTurn := &v.Turns[targetTurnIdx]
	if targetItemIdx >= 0 {
		targetTurn.Items[targetItemIdx] = it
	} else {
		targetTurn.Items = append(targetTurn.Items, it)
		targetItemIdx = len(targetTurn.Items) - 1
	}
	v.toolIndex[itemID] = [2]int{targetTurnIdx, targetItemIdx}
	v.toolTurnKey[itemID] = targetTurn.Key

	if diffText != "" {
		targetTurn.HasDiff = true
		v.ToolDiffs[itemID] = diffText
		v.Diffs[itemID] = diffText
		v.rebuildTurnDiff(targetTurn)
	} else if it.K == "file" && v.ToolDiffs[itemID] != "" {
		targetTurn.HasDiff = true
	}
	return true
}

func (v *grokTranscript) rebuildTurnDiff(turn *Turn) {
	if turn == nil {
		return
	}
	var parts []string
	for _, it := range turn.Items {
		if d := v.ToolDiffs[it.ID]; d != "" {
			parts = append(parts, d)
		}
	}
	if len(parts) == 0 {
		return
	}
	joined := strings.Join(parts, "\n")
	if !strings.HasSuffix(joined, "\n") {
		joined += "\n"
	}
	v.TurnDiffs[turn.Key] = joined
	v.Diffs[turn.Key] = joined
}

func classifyGrokToolKind(tc acpToolCallInfo, diffText string) string {
	name := strings.ToLower(strings.TrimSpace(tc.Name))
	if name == "spawn_subagent" || name == "agent" || name == "task" || name == "invoke_subagent" {
		return "agent"
	}
	if diffText != "" {
		return "file"
	}
	switch strings.ToLower(strings.TrimSpace(tc.Kind)) {
	case "execute", "command", "terminal", "shell":
		return "cmd"
	case "edit", "write", "delete", "file", "patch":
		return "file"
	case "search":
		return "search"
	case "read", "fetch", "think", "other":
		return "tool"
	}
	switch name {
	case "bash", "run_command", "execute_command", "shell", "terminal":
		return "cmd"
	case "edit_file", "write_file", "edit", "write", "apply_patch", "patch_file", "create_file", "delete_file":
		return "file"
	case "grep", "grep_search", "search", "web_search", "search_web", "find_files", "ripgrep":
		return "search"
	}
	return "tool"
}

func convertGrokToolCall(tc acpToolCallInfo, itemID string, prev *Item) (Item, string) {
	diffText := tc.DiffText()
	inferredK := classifyGrokToolKind(tc, diffText)

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
	rawOutMap := grokParseRawOutputMap(tc.RawOutput)
	rawOutText := grokFormatRawOutputText(tc.RawOutput, rawOutMap)
	textOut := tc.TextOutput()

	switch it.K {
	case "cmd":
		cmdText := grokStorageFirstNonEmpty(
			str(inp, "command"),
			str(inp, "CommandLine"),
			str(inp, "cmd"),
			tc.Title,
			tc.Name,
		)
		if cmdText != "" {
			it.Title = clipHead(shellScript(cmdText), 800)
		}
		if textOut != "" {
			it.Body = clipTail(textOut, maxOutput)
		} else if rawOutText != "" {
			it.Body = clipTail(rawOutText, maxOutput)
		}
		if code, ok := grokExitCodeFromMap(rawOutMap); ok {
			it.Exit = &code
		} else if it.Status == "completed" && it.Exit == nil {
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
		if p := grokStorageFirstNonEmpty(str(inp, "path"), str(inp, "file_path"), str(inp, "Path"), str(inp, "TargetFile")); p != "" {
			addPath("M", p)
		}
		if len(paths) > 0 {
			it.Title = strings.Join(paths, "\n")
		} else if title := grokStorageFirstNonEmpty(tc.Title, tc.Name); title != "" {
			it.Title = title
		}
		if diffText != "" {
			it.Body = clipHead(diffText, maxDiff)
		} else if textOut != "" && it.Body == "" {
			it.Body = clipTail(textOut, maxOutput)
		} else if rawOutText != "" && it.Body == "" {
			it.Body = clipTail(rawOutText, maxOutput)
		}

	case "search":
		if title := grokStorageFirstNonEmpty(str(inp, "query"), str(inp, "pattern"), str(inp, "Pattern"), str(inp, "Query"), tc.Title, tc.Name); title != "" {
			it.Title = title
		}
		if textOut != "" {
			it.Body = clipTail(textOut, maxOutput)
		} else if rawOutText != "" {
			it.Body = clipTail(rawOutText, maxOutput)
		} else if len(inp) > 0 && it.Body == "" {
			it.Body = compactJSON(inp, maxArgs)
		}

	case "agent":
		if title := grokStorageFirstNonEmpty(str(inp, "description"), tc.Title, tc.Name, "Subagent"); title != "" {
			it.Title = clipPlain(title, 240)
		}
		if agentID := grokStorageFirstNonEmpty(
			str(rawOutMap, "child_session_id"),
			str(rawOutMap, "childSessionId"),
			str(rawOutMap, "subagent_id"),
			str(rawOutMap, "subagentId"),
			str(rawOutMap, "session_id"),
			str(inp, "resume_from"),
		); agentID != "" {
			it.AgentID = agentID
		}
		if agentPath := grokStorageFirstNonEmpty(str(inp, "subagent_type"), str(rawOutMap, "subagent_type")); agentPath != "" {
			it.AgentPath = agentPath
		}
		if textOut != "" {
			it.Body = clipTail(textOut, maxOutput)
		} else if rawOutText != "" {
			it.Body = clipTail(rawOutText, maxOutput)
		} else if len(inp) > 0 && it.Body == "" {
			it.Body = compactJSON(inp, maxArgs)
		}

	default:
		if title := grokStorageFirstNonEmpty(tc.Title, tc.Name); title != "" {
			it.Title = title
		}
		if it.Title == "" {
			it.Title = "Tool"
		}
		if textOut != "" {
			it.Body = clipTail(textOut, maxOutput)
		} else if rawOutText != "" {
			it.Body = clipTail(rawOutText, maxOutput)
		} else if len(inp) > 0 && it.Body == "" {
			it.Body = compactJSON(inp, maxArgs)
		}
	}

	return it, diffText
}

func grokParseRawOutputMap(raw json.RawMessage) map[string]any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(trimmed, &m) == nil {
		return m
	}
	return nil
}

func grokFormatRawOutputText(raw json.RawMessage, parsed map[string]any) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if parsed != nil {
		if s := grokStorageFirstNonEmpty(str(parsed, "output"), str(parsed, "stdout"), str(parsed, "text"), str(parsed, "content"), str(parsed, "error")); s != "" {
			if stderr := strings.TrimSpace(str(parsed, "stderr")); stderr != "" && !strings.Contains(s, stderr) {
				return s + "\n" + stderr
			}
			return s
		}
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s
		}
	}
	return string(trimmed)
}

func grokExitCodeFromMap(m map[string]any) (int, bool) {
	if m == nil {
		return 0, false
	}
	for _, k := range []string{"exit_code", "exitCode", "code"} {
		if n, ok := num(m, k); ok {
			return int(n), true
		}
	}
	return 0, false
}

func grokApplyPlanUpdate(v *grokTranscript, updateRaw json.RawMessage, tsMs int64) bool {
	var raw struct {
		Entries []struct {
			Content  string `json:"content"`
			Text     string `json:"text"`
			Title    string `json:"title"`
			Status   string `json:"status"`
			Priority string `json:"priority"`
		} `json:"entries"`
		Items []struct {
			Content string `json:"content"`
			Text    string `json:"text"`
			Title   string `json:"title"`
			Status  string `json:"status"`
		} `json:"items"`
		Text string `json:"text"`
	}
	if json.Unmarshal(updateRaw, &raw) != nil {
		return false
	}

	var lines []string
	doneCount := 0
	total := 0
	appendEntry := func(text, status string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		total++
		marker := "[ ]"
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "completed", "complete", "done":
			marker = "[x]"
			doneCount++
		case "in_progress", "inprogress", "running", "active":
			marker = "[~]"
		case "failed", "error", "cancelled", "canceled":
			marker = "[-]"
		}
		lines = append(lines, marker+" "+text)
	}

	for _, e := range raw.Entries {
		appendEntry(grokStorageFirstNonEmpty(e.Content, e.Text, e.Title), e.Status)
	}
	if len(lines) == 0 {
		for _, e := range raw.Items {
			appendEntry(grokStorageFirstNonEmpty(e.Content, e.Text, e.Title), e.Status)
		}
	}

	var rendered string
	if len(lines) > 0 {
		header := fmt.Sprintf("Plan (%d/%d)", doneCount, total)
		rendered = header + "\n" + strings.Join(lines, "\n")
	} else if t := strings.TrimSpace(raw.Text); t != "" {
		rendered = "Plan\n" + t
	}
	if rendered == "" {
		return false
	}

	turn := v.ensureCurrentTurn(tsMs)
	if idx, ok := v.planIndex[v.cur]; ok && idx >= 0 && idx < len(turn.Items) && turn.Items[idx].ID == turn.Key+":plan" {
		turn.Items[idx].Text = clipHead(rendered, maxOutput)
		return true
	}
	turn.Items = append(turn.Items, Item{
		ID:    turn.Key + ":plan",
		K:     "info",
		Title: "Plan",
		Text:  clipHead(rendered, maxOutput),
	})
	v.planIndex[v.cur] = len(turn.Items) - 1
	return true
}

func grokApplyUsageUpdate(v *grokTranscript, updateRaw json.RawMessage) bool {
	var u struct {
		Used                     int64 `json:"used"`
		Size                     int64 `json:"size"`
		ContextTokensUsed        int64 `json:"contextTokensUsed"`
		ContextWindowTokens      int64 `json:"contextWindowTokens"`
		ContextTokensUsedSnake   int64 `json:"context_tokens_used"`
		ContextWindowTokensSnake int64 `json:"context_window_tokens"`
		Usage                    *struct {
			Used                int64 `json:"used"`
			Size                int64 `json:"size"`
			ContextTokensUsed   int64 `json:"contextTokensUsed"`
			ContextWindowTokens int64 `json:"contextWindowTokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(updateRaw, &u) != nil {
		return false
	}
	used := firstPositiveInt64(u.Used, u.ContextTokensUsed, u.ContextTokensUsedSnake)
	size := firstPositiveInt64(u.Size, u.ContextWindowTokens, u.ContextWindowTokensSnake)
	if u.Usage != nil {
		if used == 0 {
			used = firstPositiveInt64(u.Usage.Used, u.Usage.ContextTokensUsed)
		}
		if size == 0 {
			size = firstPositiveInt64(u.Usage.Size, u.Usage.ContextWindowTokens)
		}
	}
	changed := false
	if used > 0 {
		v.Meta.CtxUsed = used
		changed = true
	}
	if size > 0 {
		v.Meta.CtxMax = size
		changed = true
	}
	return changed
}

func grokApplyAvailableCommandsUpdate(v *grokTranscript, updateRaw json.RawMessage) bool {
	var u struct {
		AvailableCommands []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputHint   string          `json:"inputHint"`
			Input       json.RawMessage `json:"input"`
		} `json:"availableCommands"`
	}
	if json.Unmarshal(updateRaw, &u) != nil {
		return false
	}
	cmds := make([]SlashCommand, 0, len(u.AvailableCommands))
	for _, item := range u.AvailableCommands {
		name := strings.TrimPrefix(strings.TrimSpace(item.Name), "/")
		if name == "" {
			continue
		}
		hint := strings.TrimSpace(item.InputHint)
		if hint == "" && len(bytes.TrimSpace(item.Input)) > 0 {
			var inputObj struct {
				Hint string `json:"hint"`
			}
			if json.Unmarshal(item.Input, &inputObj) == nil && inputObj.Hint != "" {
				hint = strings.TrimSpace(inputObj.Hint)
			} else {
				var inputStr string
				if json.Unmarshal(item.Input, &inputStr) == nil {
					hint = strings.TrimSpace(inputStr)
				}
			}
		}
		cmds = append(cmds, SlashCommand{
			Name:        name,
			Description: strings.TrimSpace(item.Description),
			InputHint:   hint,
		})
	}
	v.Meta.Commands = cmds
	return true
}

func grokApplyConfigOptionsUpdate(v *grokTranscript, updateRaw json.RawMessage) bool {
	var u struct {
		ConfigOptions json.RawMessage `json:"configOptions"`
		ConfigID      string          `json:"configId"`
		Value         json.RawMessage `json:"value"`
	}
	if json.Unmarshal(updateRaw, &u) != nil {
		return false
	}
	opts := parseACPConfigOptions(u.ConfigOptions)
	if len(opts) == 0 && strings.TrimSpace(u.ConfigID) != "" {
		val := grokExtractConfigValue(u.Value)
		opts = []acpConfigOption{{
			ID:           strings.TrimSpace(u.ConfigID),
			CurrentValue: val,
		}}
	}
	if len(opts) == 0 {
		return false
	}
	v.ConfigOptions = opts
	changed := false
	for _, opt := range opts {
		val := strings.TrimSpace(opt.CurrentValue)
		switch opt.ID {
		case "model":
			if val != "" {
				v.Meta.Model = val
				changed = true
			}
		case "reasoning_effort", "thought_level":
			if val != "" {
				v.Meta.Effort = val
				changed = true
			}
		case "mode", "permission_mode":
			if mode := normalizeGrokPermissionMode(val); mode != "" {
				v.Meta.PermissionMode = mode
				changed = true
			}
		}
	}
	return changed
}

func grokExtractConfigValue(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return strings.TrimSpace(s)
	}
	var wrapped struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(trimmed, &wrapped) == nil {
		return strings.TrimSpace(wrapped.Value)
	}
	return ""
}

func grokApplyTurnCompleted(v *grokTranscript, updateRaw json.RawMessage, tsMs int64) bool {
	var u struct {
		StopReason      string `json:"stop_reason"`
		StopReasonCamel string `json:"stopReason"`
		ElapsedMs       int64  `json:"elapsed_ms"`
		ElapsedMsCamel  int64  `json:"elapsedMs"`
		DurationMs      int64  `json:"duration_ms"`
		DurationMsCamel int64  `json:"durationMs"`
		Error           string `json:"error"`
		Usage           *struct {
			Used                     int64 `json:"used"`
			Size                     int64 `json:"size"`
			ContextTokensUsed        int64 `json:"contextTokensUsed"`
			ContextWindowTokens      int64 `json:"contextWindowTokens"`
			ContextTokensUsedSnake   int64 `json:"context_tokens_used"`
			ContextWindowTokensSnake int64 `json:"context_window_tokens"`
			InputTokens              int64 `json:"inputTokens"`
			CachedReadTokens         int64 `json:"cachedReadTokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(updateRaw, &u) != nil {
		return false
	}
	changed := false
	if u.Usage != nil {
		used := firstPositiveInt64(u.Usage.Used, u.Usage.ContextTokensUsed, u.Usage.ContextTokensUsedSnake)
		if used == 0 && (u.Usage.InputTokens > 0 || u.Usage.CachedReadTokens > 0) {
			used = u.Usage.InputTokens + u.Usage.CachedReadTokens
		}
		size := firstPositiveInt64(u.Usage.Size, u.Usage.ContextWindowTokens, u.Usage.ContextWindowTokensSnake)
		if used > 0 {
			v.Meta.CtxUsed = used
			changed = true
		}
		if size > 0 {
			v.Meta.CtxMax = size
			changed = true
		}
	}
	if v.cur < 0 || v.cur >= len(v.Turns) {
		return changed
	}
	turn := &v.Turns[v.cur]
	dur := firstPositiveInt64(u.ElapsedMs, u.ElapsedMsCamel, u.DurationMs, u.DurationMsCamel)
	if dur == 0 && tsMs > 0 && turn.StartedAt > 0 && tsMs >= turn.StartedAt {
		dur = tsMs - turn.StartedAt
	}
	if dur > 0 {
		turn.DurationMs = dur
	}

	stop := strings.ToLower(strings.TrimSpace(grokStorageFirstNonEmpty(u.StopReason, u.StopReasonCamel)))
	errMsg := strings.TrimSpace(u.Error)
	switch {
	case stop == "cancelled" || stop == "canceled" || stop == "interrupted":
		turn.Status = "interrupted"
	case errMsg != "" || stop == "error" || stop == "failed":
		turn.Status = "failed"
		if errMsg != "" {
			turn.Error = clipPlain(errMsg, 300)
			v.LastErr = turn.Error
		}
	default:
		turn.Status = "completed"
	}
	return true
}

func grokApplyRetryState(v *grokTranscript, updateRaw json.RawMessage, tsMs int64) bool {
	var u struct {
		Retrying    *bool  `json:"retrying"`
		Attempt     int    `json:"attempt"`
		MaxAttempts int    `json:"max_attempts"`
		MaxCamel    int    `json:"maxAttempts"`
		Reason      string `json:"reason"`
		Message     string `json:"message"`
		Error       string `json:"error"`
	}
	if json.Unmarshal(updateRaw, &u) != nil {
		return false
	}
	maxAtt := u.MaxAttempts
	if maxAtt == 0 {
		maxAtt = u.MaxCamel
	}
	reason := strings.TrimSpace(grokStorageFirstNonEmpty(u.Reason, u.Message, u.Error))
	if u.Retrying != nil && !*u.Retrying && reason == "" {
		return false
	}
	var label string
	switch {
	case u.Attempt > 0 && maxAtt > 0:
		label = fmt.Sprintf("Retrying (%d/%d)", u.Attempt, maxAtt)
	case u.Attempt > 0:
		label = fmt.Sprintf("Retrying (Attempt %d)", u.Attempt)
	default:
		label = "Retrying"
	}
	if reason != "" {
		label += ": " + clipPlain(reason, 200)
	}
	turn := v.ensureCurrentTurn(tsMs)
	turn.Items = append(turn.Items, Item{
		ID:    fmt.Sprintf("%s:retry:%d", turn.Key, len(turn.Items)),
		K:     "info",
		Title: "Retry",
		Text:  label,
	})
	return true
}

func grokApplySessionRecap(v *grokTranscript, updateRaw json.RawMessage, tsMs int64) bool {
	var u struct {
		Recap   string `json:"recap"`
		Summary string `json:"summary"`
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if json.Unmarshal(updateRaw, &u) != nil {
		return false
	}
	text := strings.TrimSpace(grokStorageFirstNonEmpty(u.Recap, u.Summary, u.Text, u.Content))
	if text == "" {
		return false
	}
	v.Recap = text
	turn := v.ensureCurrentTurn(tsMs)
	turn.Items = append(turn.Items, Item{
		ID:    fmt.Sprintf("%s:recap:%d", turn.Key, len(turn.Items)),
		K:     "info",
		Title: "Session Recap",
		Text:  clipHead(text, maxOutput),
	})
	return true
}

func grokApplySubagentLifecycle(v *grokTranscript, kind string, updateRaw json.RawMessage, tsMs int64) bool {
	var u struct {
		SubagentID      string `json:"subagent_id"`
		SubagentIDCamel string `json:"subagentId"`
		ChildSessionID  string `json:"child_session_id"`
		ChildCamel      string `json:"childSessionId"`
		SubagentType    string `json:"subagent_type"`
		TypeCamel       string `json:"subagentType"`
		Description     string `json:"description"`
		Status          string `json:"status"`
		Summary         string `json:"summary"`
	}
	if json.Unmarshal(updateRaw, &u) != nil {
		return false
	}
	agentID := grokStorageFirstNonEmpty(
		strings.TrimSpace(u.ChildSessionID),
		strings.TrimSpace(u.ChildCamel),
		strings.TrimSpace(u.SubagentID),
		strings.TrimSpace(u.SubagentIDCamel),
	)
	if agentID == "" {
		return false
	}
	status := mapACPToolStatus(u.Status)
	if status == "" {
		if kind == "subagent_spawned" {
			status = "inProgress"
		} else {
			status = "completed"
		}
	}
	turn := v.ensureCurrentTurn(tsMs)
	for i := range turn.Items {
		if turn.Items[i].K == "agent" && (turn.Items[i].AgentID == agentID || turn.Items[i].ID == "subagent:"+agentID) {
			turn.Items[i].Status = status
			if u.Description != "" && turn.Items[i].Title == "" {
				turn.Items[i].Title = clipPlain(u.Description, 240)
			}
			if u.Summary != "" {
				turn.Items[i].Body = clipTail(u.Summary, maxOutput)
			}
			return true
		}
	}
	title := grokStorageFirstNonEmpty(strings.TrimSpace(u.Description), strings.TrimSpace(u.SubagentType), strings.TrimSpace(u.TypeCamel), "Subagent")
	turn.Items = append(turn.Items, Item{
		ID:        "subagent:" + agentID,
		K:         "agent",
		Title:     clipPlain(title, 240),
		Body:      clipTail(strings.TrimSpace(u.Summary), maxOutput),
		Status:    status,
		AgentID:   agentID,
		AgentPath: grokStorageFirstNonEmpty(strings.TrimSpace(u.SubagentType), strings.TrimSpace(u.TypeCamel)),
	})
	return true
}

// ---- Time & string helpers ----

func parseGrokTimeJSON(raw json.RawMessage) time.Time {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return time.Time{}
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return parseGrokTimeString(s)
	}
	var n int64
	if json.Unmarshal(trimmed, &n) == nil {
		return grokUnixToTime(n)
	}
	var f float64
	if json.Unmarshal(trimmed, &f) == nil {
		return grokFloatUnixToTime(f)
	}
	return time.Time{}
}

func parseGrokTime(raw any) time.Time {
	switch v := raw.(type) {
	case nil:
		return time.Time{}
	case time.Time:
		if v.Unix() <= 0 || v.Year() <= 1970 {
			return time.Time{}
		}
		return v.UTC()
	case int64:
		return grokUnixToTime(v)
	case int:
		return grokUnixToTime(int64(v))
	case float64:
		return grokFloatUnixToTime(v)
	case []byte:
		return parseGrokTimeString(string(v))
	case string:
		return parseGrokTimeString(v)
	default:
		return parseGrokTimeString(fmt.Sprint(v))
	}
}

func grokUnixToTime(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	switch {
	case n >= 100_000_000_000_000_000: // nanoseconds
		return time.Unix(0, n).UTC()
	case n >= 100_000_000_000_000: // microseconds
		return time.UnixMicro(n).UTC()
	case n >= 100_000_000_000: // milliseconds
		return time.UnixMilli(n).UTC()
	default: // seconds
		return time.Unix(n, 0).UTC()
	}
}

func grokFloatUnixToTime(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	if f >= 1e11 {
		return grokUnixToTime(int64(f))
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

var grokTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05.999999999-0700",
	"2006-01-02 15:04:05-0700",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
}

func parseGrokTimeString(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return grokUnixToTime(n)
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !strings.ContainsAny(s, "-:T ") {
		return grokFloatUnixToTime(f)
	}
	for _, layout := range grokTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			if t.Unix() <= 0 || t.Year() <= 1970 {
				return time.Time{}
			}
			return t.UTC()
		}
	}
	return time.Time{}
}

func grokStorageFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstPositiveInt64(values ...int64) int64 {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func cleanGrokTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return organizationText(s)
}

func appendUniqueStrings(dst, src []string) []string {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]bool, len(dst))
	for _, s := range dst {
		seen[s] = true
	}
	for _, s := range src {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		dst = append(dst, s)
	}
	return dst
}
