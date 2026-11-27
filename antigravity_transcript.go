package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
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
	"unicode/utf8"
)

const (
	antigravityAppDirDesktop = "antigravity"
	antigravityAppDirCLI     = "antigravity-cli"

	antigravityDefaultWindow         = 15
	antigravityDefaultTailBytes      = 2 << 20
	antigravityMaxTailBytes          = 32 << 20
	antigravityMaxLineBytes          = 4 << 20
	antigravityFullTranscriptMaxScan = 32 << 20
	antigravityAnnotationMaxBytes    = 64 << 10
	antigravityStaleRunningGrace     = time.Minute
)

// antigravityNow and antigravityOwners allow deterministic tests without
// mutating global runtime state outside package tests.
var (
	antigravityNow    = time.Now
	antigravityOwners = rolloutOwners
)

// antigravityRecord holds one deduplicated conversation summary row loaded
// read-only from ~/.gemini/<appDataDir>/conversation_summaries.db.
// Updated and Created are Unix seconds; Recency is Unix milliseconds to match
// ThreadInfo.Recency across Codex, Claude, and the web client.
type antigravityRecord struct {
	ID                   string
	Title                string
	Preview              string
	StepCount            int
	Updated              int64
	Created              int64
	Recency              int64
	LastUserInputTime    int64
	Cwd                  string
	WorkspacePaths       []string
	Status               string
	Source               string
	ProjectID            string
	AgentName            string
	ParentConversationID string
	NestingDepth         int
	NotFullyIdle         bool
	Killed               bool
	AppDataDir           string
	SourceDBDir          string
	GroupID              string
	Running              bool
	TranscriptPath       string
	BrainDir             string
	AnnotationTitle      string
}

type antigravityTranscriptOptions struct {
	Limit          int
	TailBytes      int64
	FullPath       string
	AnnotationPath string
	BrainDir       string
	Cwd            string
	Title          string
}

type antigravityTranscript struct {
	Meta      Meta
	Turns     []Turn
	First     string
	More      bool
	Diffs     map[string]string
	Model     string
	InputTok  int64
	CacheTok  int64
	OutputTok int64

	cur     int
	pending [][2]int
}

type antigravityToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type antigravityRawEntry struct {
	StepIndex       *int                  `json:"step_index"`
	Type            string                `json:"type"`
	Status          string                `json:"status"`
	Source          string                `json:"source"`
	CreatedAt       string                `json:"created_at"`
	Content         string                `json:"content"`
	Response        string                `json:"response"`
	Thinking        string                `json:"thinking"`
	Error           string                `json:"error"`
	Model           string                `json:"model"`
	InputTokens     int64                 `json:"input_tokens"`
	CacheReadTokens int64                 `json:"cache_read_tokens"`
	OutputTokens    int64                 `json:"output_tokens"`
	ToolCalls       []antigravityToolCall `json:"tool_calls"`
	TruncatedFields []string              `json:"truncated_fields"`
	lineIndex       int
}

func antigravityGeminiDir(home string) string {
	if strings.TrimSpace(home) == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".gemini")
}

func antigravityDefaultAppDirs() []string {
	return []string{antigravityAppDirDesktop, antigravityAppDirCLI}
}

func antigravityValidAppDataDir(dir string) bool {
	if dir == "" || len(dir) > 128 || dir == "." || dir == ".." {
		return false
	}
	for _, r := range dir {
		if r == '/' || r == '\\' || r < 33 || r == 127 {
			return false
		}
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func antigravityValidConversationID(id string) bool {
	if !organizationID(id) || id == "." || id == ".." {
		return false
	}
	return !strings.Contains(id, "..")
}

func antigravityDBPath(geminiDir, appDataDir string) string {
	return filepath.Join(geminiDir, appDataDir, "conversation_summaries.db")
}

func antigravityBrainDir(geminiDir, appDataDir, id string) string {
	return filepath.Join(geminiDir, appDataDir, "brain", id)
}

func antigravityTranscriptPath(geminiDir, appDataDir, id string) string {
	return filepath.Join(antigravityBrainDir(geminiDir, appDataDir, id), ".system_generated", "logs", "transcript.jsonl")
}

func antigravityFullTranscriptPath(geminiDir, appDataDir, id string) string {
	return filepath.Join(antigravityBrainDir(geminiDir, appDataDir, id), ".system_generated", "logs", "transcript_full.jsonl")
}

func antigravityAnnotationPath(geminiDir, appDataDir, id string) string {
	return filepath.Join(geminiDir, appDataDir, "annotations", id+".pbtxt")
}

// antigravityLoadRecords indexes both desktop ("antigravity") and CLI
// ("antigravity-cli") SQLite databases under ~/.gemini read-only.
func antigravityLoadRecords(ctx context.Context, liveCheck func(string) bool) ([]antigravityRecord, error) {
	return antigravityListRecords(ctx, antigravityGeminiDir(""), liveCheck)
}

// antigravityListRecords reads conversation_summaries.db from both
// <geminiDir>/antigravity and <geminiDir>/antigravity-cli (when present),
// deduplicates by conversation_id, resolves brain/transcript/annotation paths,
// and applies stale-running protection.
func antigravityListRecords(ctx context.Context, geminiDir string, liveCheck func(string) bool) ([]antigravityRecord, error) {
	if strings.TrimSpace(geminiDir) == "" {
		geminiDir = antigravityGeminiDir("")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	byID := make(map[string]antigravityRecord, 32)
	var firstErr error
	foundAnyDB := false

	for _, appDir := range antigravityDefaultAppDirs() {
		dbPath := antigravityDBPath(geminiDir, appDir)
		if _, err := os.Stat(dbPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		foundAnyDB = true
		rows, err := readAntigravityDB(ctx, dbPath, appDir)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, rec := range rows {
			if existing, ok := byID[rec.ID]; ok {
				byID[rec.ID] = mergeAntigravityRecord(existing, rec)
			} else {
				byID[rec.ID] = rec
			}
		}
	}

	if !foundAnyDB {
		return []antigravityRecord{}, nil
	}

	records := make([]antigravityRecord, 0, len(byID))
	for _, rec := range byID {
		antigravityResolveRecordPaths(geminiDir, &rec)
		records = append(records, rec)
	}

	antigravityApplyStaleGuard(records, liveCheck, antigravityNow(), antigravityOwners)

	sort.Slice(records, func(i, j int) bool {
		if records[i].Recency != records[j].Recency {
			return records[i].Recency > records[j].Recency
		}
		return records[i].ID < records[j].ID
	})

	if len(records) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return records, nil
}

func readAntigravityDB(ctx context.Context, dbPath, sourceDir string) ([]antigravityRecord, error) {
	var records []antigravityRecord
	err := withReadOnlySQLite(ctx, dbPath, func(ctx context.Context, db *sql.DB) error {
		var tableName string
		err := db.QueryRowContext(ctx, `select name from sqlite_master where type='table' and name='conversation_summaries' limit 1`).Scan(&tableName)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		colRows, err := db.QueryContext(ctx, `pragma table_info('conversation_summaries')`)
		if err != nil {
			return err
		}
		cols := map[string]bool{}
		for colRows.Next() {
			var (
				cid     int
				name    string
				colType string
				notNull int
				dflt    any
				pk      int
			)
			if err := colRows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
				colRows.Close()
				return err
			}
			cols[strings.ToLower(name)] = true
		}
		if err := colRows.Err(); err != nil {
			colRows.Close()
			return err
		}
		colRows.Close()

		if !cols["conversation_id"] {
			return errors.New("conversation_summaries table missing conversation_id column")
		}

		colExpr := func(name, fallback string) string {
			if cols[name] {
				return name
			}
			return fallback + " as " + name
		}

		selects := []string{
			"conversation_id",
			colExpr("title", "''"),
			colExpr("preview", "''"),
			colExpr("step_count", "0"),
			colExpr("last_modified_time", "''"),
			colExpr("workspace_uris", "''"),
			colExpr("status", "''"),
			colExpr("source", "''"),
			colExpr("project_id", "''"),
			colExpr("agent_name", "''"),
			colExpr("parent_conversation_id", "''"),
			colExpr("nesting_depth", "0"),
			colExpr("not_fully_idle", "0"),
			colExpr("killed", "0"),
			colExpr("last_user_input_time", "''"),
			colExpr("app_data_dir", "''"),
			colExpr("group_id", "''"),
		}

		query := "select " + strings.Join(selects, ", ") + " from conversation_summaries"
		if cols["last_modified_time"] {
			query += " order by last_modified_time desc"
		}
		query += fmt.Sprintf(" limit %d", organizationRowLimit)

		sqlRows, err := db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer sqlRows.Close()

		records = make([]antigravityRecord, 0, 32)
		for sqlRows.Next() {
			var (
				id, title, preview, workspaceURIs, status, source, projectID, agentName, parentID, appDataDir, groupID any
				stepCount, nestingDepth, notFullyIdle, killed                                                          any
				lastModified, lastUserInput                                                                            any
			)
			if err := sqlRows.Scan(
				&id, &title, &preview, &stepCount, &lastModified, &workspaceURIs,
				&status, &source, &projectID, &agentName, &parentID, &nestingDepth,
				&notFullyIdle, &killed, &lastUserInput, &appDataDir, &groupID,
			); err != nil {
				return err
			}

			convID := strings.TrimSpace(antigravityScanString(id))
			if !antigravityValidConversationID(convID) {
				continue
			}

			modTime := parseAntigravityTime(lastModified)
			userInputTime := parseAntigravityTime(lastUserInput)

			updated := int64(0)
			recency := int64(0)
			if !modTime.IsZero() {
				updated = modTime.Unix()
				recency = modTime.UnixMilli()
			}
			lastInputSec := int64(0)
			if !userInputTime.IsZero() {
				lastInputSec = userInputTime.Unix()
				if updated == 0 {
					updated = lastInputSec
					recency = userInputTime.UnixMilli()
				}
			}
			created := updated
			if lastInputSec > 0 && (created == 0 || lastInputSec < created) {
				created = lastInputSec
			}

			paths := parseAntigravityWorkspaceURIs(antigravityScanString(workspaceURIs))
			cwd := ""
			if len(paths) > 0 {
				cwd = paths[0]
			}

			resolvedAppDir := strings.TrimSpace(antigravityScanString(appDataDir))
			if !antigravityValidAppDataDir(resolvedAppDir) {
				resolvedAppDir = sourceDir
			}

			rec := antigravityRecord{
				ID:                   convID,
				Title:                strings.TrimSpace(antigravityScanString(title)),
				Preview:              strings.TrimSpace(antigravityScanString(preview)),
				StepCount:            int(antigravityScanInt64(stepCount)),
				Updated:              updated,
				Created:              created,
				Recency:              recency,
				LastUserInputTime:    lastInputSec,
				Cwd:                  cwd,
				WorkspacePaths:       paths,
				Status:               strings.TrimSpace(antigravityScanString(status)),
				Source:               strings.TrimSpace(antigravityScanString(source)),
				ProjectID:            strings.TrimSpace(antigravityScanString(projectID)),
				AgentName:            strings.TrimSpace(antigravityScanString(agentName)),
				ParentConversationID: strings.TrimSpace(antigravityScanString(parentID)),
				NestingDepth:         int(antigravityScanInt64(nestingDepth)),
				NotFullyIdle:         antigravityScanBool(notFullyIdle),
				Killed:               antigravityScanBool(killed),
				AppDataDir:           resolvedAppDir,
				SourceDBDir:          sourceDir,
				GroupID:              strings.TrimSpace(antigravityScanString(groupID)),
			}
			records = append(records, rec)
		}
		return sqlRows.Err()
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// mergeAntigravityRecord deduplicates records sharing a conversation_id.
// It prefers the desktop "antigravity" DB unless the other record has a
// strictly newer last_modified_time (Recency), and backfills blank fields.
func mergeAntigravityRecord(existing, candidate antigravityRecord) antigravityRecord {
	existingDesktop := existing.SourceDBDir == antigravityAppDirDesktop
	candidateDesktop := candidate.SourceDBDir == antigravityAppDirDesktop

	winner, loser := existing, candidate
	switch {
	case candidateDesktop && !existingDesktop:
		if candidate.Recency >= existing.Recency {
			winner, loser = candidate, existing
		}
	case existingDesktop && !candidateDesktop:
		if candidate.Recency > existing.Recency {
			winner, loser = candidate, existing
		}
	default:
		if candidate.Recency > existing.Recency {
			winner, loser = candidate, existing
		}
	}

	if winner.Title == "" {
		winner.Title = loser.Title
	}
	if winner.Preview == "" {
		winner.Preview = loser.Preview
	}
	if winner.Cwd == "" && loser.Cwd != "" {
		winner.Cwd = loser.Cwd
		winner.WorkspacePaths = append([]string(nil), loser.WorkspacePaths...)
	}
	if winner.ParentConversationID == "" {
		winner.ParentConversationID = loser.ParentConversationID
	}
	if winner.NestingDepth == 0 {
		winner.NestingDepth = loser.NestingDepth
	}
	if winner.AgentName == "" {
		winner.AgentName = loser.AgentName
	}
	if winner.ProjectID == "" {
		winner.ProjectID = loser.ProjectID
	}
	if winner.GroupID == "" {
		winner.GroupID = loser.GroupID
	}
	if winner.Source == "" {
		winner.Source = loser.Source
	}
	if winner.AppDataDir == "" {
		winner.AppDataDir = loser.AppDataDir
	}
	if winner.Created == 0 || (loser.Created > 0 && loser.Created < winner.Created) {
		winner.Created = loser.Created
	}
	return winner
}

func antigravityResolveRecordPaths(geminiDir string, rec *antigravityRecord) {
	if rec == nil || !antigravityValidConversationID(rec.ID) {
		return
	}
	candidates := make([]string, 0, 3)
	addDir := func(d string) {
		if !antigravityValidAppDataDir(d) {
			return
		}
		for _, prev := range candidates {
			if prev == d {
				return
			}
		}
		candidates = append(candidates, d)
	}
	addDir(rec.AppDataDir)
	addDir(rec.SourceDBDir)
	addDir(antigravityAppDirDesktop)
	addDir(antigravityAppDirCLI)

	primaryDir := candidates[0]
	rec.BrainDir = antigravityBrainDir(geminiDir, primaryDir, rec.ID)
	rec.TranscriptPath = antigravityTranscriptPath(geminiDir, primaryDir, rec.ID)

	for _, dir := range candidates {
		tPath := antigravityTranscriptPath(geminiDir, dir, rec.ID)
		if st, err := os.Stat(tPath); err == nil && st.Mode().IsRegular() {
			rec.TranscriptPath = tPath
			rec.BrainDir = antigravityBrainDir(geminiDir, dir, rec.ID)
			if rec.AppDataDir == "" {
				rec.AppDataDir = dir
			}
			break
		}
		bDir := antigravityBrainDir(geminiDir, dir, rec.ID)
		if st, err := os.Stat(bDir); err == nil && st.IsDir() {
			rec.BrainDir = bDir
			rec.TranscriptPath = tPath
			if rec.AppDataDir == "" {
				rec.AppDataDir = dir
			}
		}
	}

	for _, dir := range candidates {
		annPath := antigravityAnnotationPath(geminiDir, dir, rec.ID)
		if title := readAntigravityAnnotationTitle(annPath); title != "" {
			rec.AnnotationTitle = title
			if rec.Title == "" {
				rec.Title = title
			}
			break
		}
	}
}

func antigravityRecordIsDBRunning(rec antigravityRecord) bool {
	if rec.Killed {
		return false
	}
	return rec.Status == "CASCADE_RUN_STATUS_RUNNING" || rec.NotFullyIdle
}

// antigravityApplyStaleGuard sets Running on each record while preventing
// abandoned CASCADE_RUN_STATUS_RUNNING or not_fully_idle rows from remaining
// stuck running when their transcript is untouched and has no live owner.
func antigravityApplyStaleGuard(records []antigravityRecord, liveCheck func(string) bool, now time.Time, owners *rolloutOwnerCache) {
	if now.IsZero() {
		now = time.Now()
	}
	staleFiles := map[string]os.FileInfo{}
	staleIndices := make([]int, 0)

	for i := range records {
		rec := &records[i]
		if liveCheck != nil && liveCheck(rec.ID) {
			rec.Running = true
			continue
		}
		if !antigravityRecordIsDBRunning(*rec) {
			rec.Running = false
			continue
		}

		// Candidate is marked running in SQLite; verify freshness or live owner.
		var latestInfo os.FileInfo
		var latestPath string
		if rec.TranscriptPath != "" {
			if st, err := os.Stat(rec.TranscriptPath); err == nil && st.Mode().IsRegular() {
				latestInfo = st
				latestPath = rec.TranscriptPath
			}
			fullPath := filepath.Join(filepath.Dir(rec.TranscriptPath), "transcript_full.jsonl")
			if st, err := os.Stat(fullPath); err == nil && st.Mode().IsRegular() {
				if latestInfo == nil || st.ModTime().After(latestInfo.ModTime()) {
					latestInfo = st
					latestPath = fullPath
				}
			}
		}

		if latestInfo != nil {
			if now.Sub(latestInfo.ModTime()) <= antigravityStaleRunningGrace {
				rec.Running = true
				continue
			}
			if owners != nil {
				staleFiles[latestPath] = latestInfo
				staleIndices = append(staleIndices, i)
			} else {
				rec.Running = false
			}
			continue
		}

		// Transcript file does not exist yet: allow brief grace from DB updated time.
		if rec.Updated > 0 && now.Sub(time.Unix(rec.Updated, 0)) <= antigravityStaleRunningGrace {
			rec.Running = true
		} else {
			rec.Running = false
		}
	}

	if len(staleFiles) == 0 || owners == nil {
		return
	}
	owners.refresh(staleFiles)
	for _, idx := range staleIndices {
		rec := &records[idx]
		path := rec.TranscriptPath
		info := staleFiles[path]
		if info == nil && rec.TranscriptPath != "" {
			fullPath := filepath.Join(filepath.Dir(rec.TranscriptPath), "transcript_full.jsonl")
			if fi := staleFiles[fullPath]; fi != nil {
				path = fullPath
				info = fi
			}
		}
		if info != nil && owners.running(path, info) {
			rec.Running = true
		} else {
			rec.Running = false
		}
	}
}

// parseAntigravityWorkspaceURIs decodes a JSON array of file:// URIs into
// clean local filesystem paths, rejecting non-file or non-local URIs.
func parseAntigravityWorkspaceURIs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var uris []string
	if err := json.Unmarshal([]byte(raw), &uris); err != nil {
		return nil
	}
	out := make([]string, 0, len(uris))
	seen := make(map[string]bool, len(uris))
	for _, item := range uris {
		p, ok := parseAntigravityFileURI(item)
		if !ok || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func parseAntigravityFileURI(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsRune(raw, '\x00') {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "file") {
		return "", false
	}
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		return "", false
	}
	p := u.Path
	if p == "" || strings.ContainsRune(p, '\x00') {
		return "", false
	}
	// Handle Windows drive URIs such as file:///C:/Users/name/repo.
	if len(p) >= 3 && p[0] == '/' && antigravityIsASCIILetter(p[1]) && p[2] == ':' {
		p = p[1:]
		if len(p) == 2 {
			p += "/"
		}
		return filepath.Clean(filepath.FromSlash(p)), true
	}
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	cleaned := filepath.Clean(filepath.FromSlash(p))
	return cleaned, true
}

func antigravityIsASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// parseAntigravityTime parses SQLite datetime strings, RFC3339 timestamps, and
// numeric Unix timestamps (seconds/ms/us/ns). Zero/pre-epoch sentinel values
// such as 0001-01-01 00:00:00+00:00 or 1970-01-01T00:00:00Z return time.Time{}.
func parseAntigravityTime(raw any) time.Time {
	switch v := raw.(type) {
	case nil:
		return time.Time{}
	case time.Time:
		if v.Unix() <= 0 || v.Year() <= 1970 {
			return time.Time{}
		}
		return v.UTC()
	case int64:
		return antigravityUnixToTime(v)
	case int:
		return antigravityUnixToTime(int64(v))
	case uint64:
		if v > (1<<63)-1 {
			return time.Time{}
		}
		return antigravityUnixToTime(int64(v))
	case float64:
		return antigravityFloatUnixToTime(v)
	case []byte:
		return parseAntigravityTimeString(string(v))
	case string:
		return parseAntigravityTimeString(v)
	default:
		return parseAntigravityTimeString(fmt.Sprint(v))
	}
}

func parseAntigravityUnix(raw any) int64 {
	t := parseAntigravityTime(raw)
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func parseAntigravityUnixMilli(raw any) int64 {
	t := parseAntigravityTime(raw)
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func antigravityUnixToTime(n int64) time.Time {
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

func antigravityFloatUnixToTime(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	if f >= 1e11 {
		return antigravityUnixToTime(int64(f))
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

var antigravityTimeLayouts = []string{
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

func parseAntigravityTimeString(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return antigravityUnixToTime(n)
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !strings.ContainsAny(s, "-:T ") {
		return antigravityFloatUnixToTime(f)
	}
	for _, layout := range antigravityTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			if t.Unix() <= 0 || t.Year() <= 1970 {
				return time.Time{}
			}
			return t.UTC()
		}
	}
	return time.Time{}
}

func antigravityScanString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func antigravityScanInt64(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case []byte:
		n, _ := strconv.ParseInt(strings.TrimSpace(string(x)), 10, 64)
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	default:
		return 0
	}
}

func antigravityScanBool(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	case float64:
		return x != 0
	case []byte:
		s := strings.ToLower(strings.TrimSpace(string(x)))
		return s == "1" || s == "true" || s == "t" || s == "yes"
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s == "1" || s == "true" || s == "t" || s == "yes"
	default:
		return false
	}
}

// ---- transcript reader ----

func newAntigravityTranscript() *antigravityTranscript {
	return &antigravityTranscript{
		Meta:  Meta{Provider: "antigravity", Mode: "tail", PermissionMode: "default"},
		Turns: []Turn{},
		Diffs: map[string]string{},
		cur:   -1,
	}
}

func readAntigravityTranscript(path string, limit int) (*antigravityTranscript, error) {
	return readAntigravityTranscriptWithOptions(path, antigravityTranscriptOptions{Limit: limit})
}

func readAntigravityTranscriptForRecord(geminiDir string, rec antigravityRecord, limit int) (*antigravityTranscript, error) {
	if strings.TrimSpace(geminiDir) == "" {
		geminiDir = antigravityGeminiDir("")
	}
	if rec.TranscriptPath == "" || rec.BrainDir == "" {
		antigravityResolveRecordPaths(geminiDir, &rec)
	}
	annPath := ""
	if antigravityValidAppDataDir(rec.AppDataDir) && antigravityValidConversationID(rec.ID) {
		annPath = antigravityAnnotationPath(geminiDir, rec.AppDataDir, rec.ID)
	}
	opts := antigravityTranscriptOptions{
		Limit:          limit,
		FullPath:       filepath.Join(filepath.Dir(rec.TranscriptPath), "transcript_full.jsonl"),
		AnnotationPath: annPath,
		BrainDir:       rec.BrainDir,
		Cwd:            rec.Cwd,
		Title:          rec.Title,
	}
	view, err := readAntigravityTranscriptWithOptions(rec.TranscriptPath, opts)
	if err != nil {
		return nil, err
	}
	if view.Meta.Title == "" && rec.AnnotationTitle != "" {
		view.Meta.Title = rec.AnnotationTitle
	}
	if view.Meta.Cwd == "" {
		view.Meta.Cwd = rec.Cwd
	}
	view.Meta.Running = rec.Running
	return view, nil
}

func readAntigravityTranscriptWithOptions(path string, opts antigravityTranscriptOptions) (*antigravityTranscript, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = antigravityDefaultWindow
	}
	tailBytes := opts.TailBytes
	if tailBytes <= 0 {
		pages := int64((limit + antigravityDefaultWindow - 1) / antigravityDefaultWindow)
		if pages < 1 {
			pages = 1
		}
		tailBytes = pages * antigravityDefaultTailBytes
		if tailBytes > antigravityMaxTailBytes {
			tailBytes = antigravityMaxTailBytes
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("Antigravity Transcript Is Not A Regular File")
	}

	v := newAntigravityTranscript()
	v.Meta.Title = strings.TrimSpace(opts.Title)
	v.Meta.Cwd = opts.Cwd

	start := st.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	v.More = start > 0

	if start > 0 {
		head := make([]byte, 64<<10)
		n, _ := f.ReadAt(head, 0)
		for _, line := range bytes.Split(head[:n], []byte{'\n'}) {
			var entry antigravityRawEntry
			if json.Unmarshal(bytes.TrimSpace(line), &entry) == nil {
				v.applyHeadMetadata(entry)
			}
		}
	}

	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(f, 64<<10)
	if start > 0 {
		_, _, _ = antigravityReadBoundedLine(reader, antigravityMaxLineBytes)
	}

	rawEntries := make([]antigravityRawEntry, 0, 64)
	lineNo := 0
	for {
		line, overlong, readErr := antigravityReadBoundedLine(reader, antigravityMaxLineBytes)
		if len(line) > 0 && !overlong {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var entry antigravityRawEntry
				if json.Unmarshal(trimmed, &entry) == nil {
					entry.lineIndex = lineNo
					rawEntries = append(rawEntries, entry)
				}
			}
		}
		lineNo++
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, readErr
		}
	}

	// Retain only the entries needed for the last `limit` turns before
	// selectively expanding truncated entries from transcript_full.jsonl.
	windowStart := antigravityWindowStartIndex(rawEntries, limit)
	if windowStart > 0 {
		for _, e := range rawEntries[:windowStart] {
			v.applyHeadMetadata(e)
		}
		rawEntries = rawEntries[windowStart:]
		v.More = true
	}

	fullPath := opts.FullPath
	if fullPath == "" {
		fullPath = filepath.Join(filepath.Dir(path), "transcript_full.jsonl")
	}
	antigravityExpandTruncatedEntries(rawEntries, fullPath, opts.BrainDir, start == 0)

	for _, entry := range rawEntries {
		v.applyEntry(entry)
	}

	if len(v.Turns) > limit {
		v.Turns = v.Turns[len(v.Turns)-limit:]
		v.More = true
		kept := make(map[string]bool, len(v.Turns))
		for _, t := range v.Turns {
			kept[t.Key] = true
		}
		for k := range v.Diffs {
			if !kept[k] {
				delete(v.Diffs, k)
			}
		}
	}

	v.Meta.HasOlder = v.More
	if v.Meta.Title == "" && opts.AnnotationPath != "" {
		v.Meta.Title = readAntigravityAnnotationTitle(opts.AnnotationPath)
	}
	if v.Meta.Title == "" {
		v.Meta.Title = v.First
	}
	return v, nil
}

// antigravityReadBoundedLine reads one newline-delimited line capped at maxBytes.
// If a line exceeds maxBytes, the remaining bytes up to '\n' are discarded
// and overlong is reported as true so memory stays strictly bounded.
func antigravityReadBoundedLine(r *bufio.Reader, maxBytes int) ([]byte, bool, error) {
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

// antigravityWindowStartIndex finds the earliest entry index that belongs to
// the last `limit` USER_INPUT turns. Entries before the first USER_INPUT are
// kept only if the total number of turns fits within `limit`.
func antigravityWindowStartIndex(entries []antigravityRawEntry, limit int) int {
	if limit <= 0 || len(entries) == 0 {
		return 0
	}
	turnStarts := make([]int, 0, 16)
	hasLeading := false
	for i, e := range entries {
		if e.Type == "USER_INPUT" {
			turnStarts = append(turnStarts, i)
		} else if len(turnStarts) == 0 && e.Type != "" {
			hasLeading = true
		}
	}
	totalTurns := len(turnStarts)
	if hasLeading {
		totalTurns++
	}
	if totalTurns <= limit {
		return 0
	}
	if len(turnStarts) >= limit {
		return turnStarts[len(turnStarts)-limit]
	}
	return 0
}

func antigravityExpandTruncatedEntries(entries []antigravityRawEntry, fullPath, brainDir string, allowLineFallback bool) {
	if fullPath == "" || len(entries) == 0 {
		return
	}
	byStep := map[int][]int{}
	byLine := map[int]int{}
	needed := 0
	for i := range entries {
		if len(entries[i].TruncatedFields) == 0 {
			continue
		}
		if entries[i].StepIndex != nil {
			step := *entries[i].StepIndex
			if len(byStep[step]) == 0 {
				needed++
			}
			byStep[step] = append(byStep[step], i)
		} else if allowLineFallback {
			byLine[entries[i].lineIndex] = i
			needed++
		}
	}
	if needed == 0 {
		return
	}
	if brainDir != "" && !withinDirectory(brainDir, fullPath) {
		return
	}
	st, err := os.Lstat(fullPath)
	if err != nil || !st.Mode().IsRegular() {
		return
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 64<<10)
	var scanned int64
	lineNo := 0
	for needed > 0 && scanned < antigravityFullTranscriptMaxScan {
		line, overlong, readErr := antigravityReadBoundedLine(reader, antigravityMaxLineBytes)
		scanned += int64(len(line))
		if len(line) > 0 && !overlong {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				matchedIndices := []int(nil)
				var matchedStep *int
				if len(byStep) > 0 {
					var header struct {
						StepIndex *int `json:"step_index"`
					}
					if json.Unmarshal(trimmed, &header) == nil && header.StepIndex != nil {
						if idxs := byStep[*header.StepIndex]; len(idxs) > 0 {
							matchedIndices = idxs
							matchedStep = header.StepIndex
						}
					}
				}
				if len(matchedIndices) == 0 && len(byLine) > 0 {
					if idx, ok := byLine[lineNo]; ok {
						matchedIndices = []int{idx}
						delete(byLine, lineNo)
					}
				}
				if len(matchedIndices) > 0 {
					var fullEntry antigravityRawEntry
					if json.Unmarshal(trimmed, &fullEntry) == nil {
						for _, idx := range matchedIndices {
							antigravityApplyExpandedFields(&entries[idx], fullEntry)
						}
					}
					if matchedStep != nil {
						delete(byStep, *matchedStep)
					}
					needed--
				}
			}
		}
		lineNo++
		if readErr != nil {
			break
		}
	}
}

func antigravityApplyExpandedFields(dst *antigravityRawEntry, full antigravityRawEntry) {
	wanted := make(map[string]bool, len(dst.TruncatedFields))
	for _, field := range dst.TruncatedFields {
		wanted[strings.ToLower(strings.TrimSpace(field))] = true
	}
	if wanted["content"] && full.Content != "" {
		dst.Content = full.Content
	}
	if wanted["response"] && full.Response != "" {
		dst.Response = full.Response
	}
	if wanted["thinking"] && full.Thinking != "" {
		dst.Thinking = full.Thinking
	}
	if wanted["tool_calls"] && len(full.ToolCalls) > 0 {
		dst.ToolCalls = full.ToolCalls
	}
	if wanted["error"] && full.Error != "" {
		dst.Error = full.Error
	}
	// Fallback when truncated_fields used a non-standard field label.
	if full.Content != "" && len(full.Content) > len(dst.Content) {
		dst.Content = full.Content
	}
	if full.Response != "" && len(full.Response) > len(dst.Response) {
		dst.Response = full.Response
	}
	if full.Thinking != "" && len(full.Thinking) > len(dst.Thinking) {
		dst.Thinking = full.Thinking
	}
	if len(full.ToolCalls) > 0 && len(dst.ToolCalls) == 0 {
		dst.ToolCalls = full.ToolCalls
	}
	if full.Error != "" && len(full.Error) > len(dst.Error) {
		dst.Error = full.Error
	}
	dst.TruncatedFields = nil
}

func (v *antigravityTranscript) applyHeadMetadata(entry antigravityRawEntry) {
	if entry.Type == "USER_INPUT" && v.First == "" {
		if text := strings.TrimSpace(entry.Content); text != "" {
			v.First = clipPlain(text, 160)
		}
	}
	if entry.Type == "PLANNER_RESPONSE" {
		if entry.Model != "" {
			v.Model = entry.Model
			v.Meta.Model = entry.Model
		}
		if used := entry.InputTokens + entry.CacheReadTokens; used > 0 {
			v.Meta.CtxUsed = used
		}
	}
}

func (v *antigravityTranscript) startTurn(key string, startedAt int64, status string) *Turn {
	if key == "" {
		key = fmt.Sprintf("turn-%d", len(v.Turns))
	}
	if status == "" {
		status = "completed"
	}
	v.Turns = append(v.Turns, Turn{
		Key:       key,
		TurnID:    key,
		StartedAt: startedAt,
		Status:    status,
		Items:     []Item{},
	})
	v.cur = len(v.Turns) - 1
	v.pending = nil
	return &v.Turns[v.cur]
}

func (v *antigravityTranscript) currentTurn(fallbackKey string, startedAt int64) *Turn {
	if v.cur < 0 || v.cur >= len(v.Turns) {
		return v.startTurn(fallbackKey, startedAt, "completed")
	}
	return &v.Turns[v.cur]
}

func (v *antigravityTranscript) appendItem(it Item) [2]int {
	t := v.currentTurn("earlier", 0)
	t.Items = append(t.Items, it)
	return [2]int{v.cur, len(t.Items) - 1}
}

func (v *antigravityTranscript) applyEntry(entry antigravityRawEntry) {
	stepKey := fmt.Sprintf("line-%d", entry.lineIndex)
	if entry.StepIndex != nil {
		stepKey = fmt.Sprintf("step-%d", *entry.StepIndex)
	}
	tsMs := parseAntigravityUnixMilli(entry.CreatedAt)
	status := mapAntigravityStatus(entry.Status)

	switch entry.Type {
	case "USER_INPUT":
		text := entry.Content
		v.startTurn(stepKey, tsMs, status)
		v.appendItem(Item{
			ID:     stepKey + ":user",
			K:      "user",
			Text:   text,
			Status: status,
		})
		if v.First == "" && strings.TrimSpace(text) != "" {
			v.First = clipPlain(strings.TrimSpace(text), 160)
		}

	case "PLANNER_RESPONSE":
		if entry.Model != "" {
			v.Model = entry.Model
			v.Meta.Model = entry.Model
		}
		v.InputTok += entry.InputTokens
		v.CacheTok += entry.CacheReadTokens
		v.OutputTok += entry.OutputTokens
		if used := entry.InputTokens + entry.CacheReadTokens; used > 0 {
			v.Meta.CtxUsed = used
		}

		turn := v.currentTurn(stepKey, tsMs)
		if turn.StartedAt == 0 && tsMs > 0 {
			turn.StartedAt = tsMs
		}
		if status == "inProgress" || status == "failed" {
			turn.Status = status
		}

		if strings.TrimSpace(entry.Thinking) != "" {
			v.appendItem(Item{
				ID:   stepKey + ":thinking",
				K:    "reasoning",
				Text: entry.Thinking,
			})
		}

		for i, tc := range entry.ToolCalls {
			itemID := fmt.Sprintf("%s:tool:%d", stepKey, i)
			it, fullDiff := antigravityToolCallItem(tc, itemID, status)
			pos := v.appendItem(it)
			v.pending = append(v.pending, pos)
			if it.K == "file" {
				turn := &v.Turns[pos[0]]
				turn.HasDiff = true
				if fullDiff != "" {
					v.Diffs[turn.Key] += fullDiff
					if !strings.HasSuffix(fullDiff, "\n") {
						v.Diffs[turn.Key] += "\n"
					}
				}
			}
		}

		reply := entry.Content
		if reply == "" {
			reply = entry.Response
		}
		if strings.TrimSpace(reply) != "" {
			v.appendItem(Item{
				ID:     stepKey + ":assistant",
				K:      "assistant",
				Text:   reply,
				Status: status,
			})
		}
		if strings.TrimSpace(entry.Error) != "" {
			v.appendItem(Item{
				ID:     stepKey + ":error",
				K:      "error",
				Text:   entry.Error,
				Status: "failed",
			})
		}

	case "GENERIC":
		if len(v.pending) > 0 && (entry.Source == "" || entry.Source == "MODEL" || entry.Source == "TOOL") {
			pos := v.pending[0]
			v.pending = v.pending[1:]
			if pos[0] >= 0 && pos[0] < len(v.Turns) && pos[1] >= 0 && pos[1] < len(v.Turns[pos[0]].Items) {
				item := &v.Turns[pos[0]].Items[pos[1]]
				failed := strings.TrimSpace(entry.Error) != "" || status == "failed"
				if failed {
					item.Status = "failed"
					code := 1
					item.Exit = &code
					errMsg := strings.TrimSpace(entry.Error)
					if errMsg == "" {
						errMsg = strings.TrimSpace(entry.Content)
					}
					if errMsg != "" {
						if item.K == "file" && item.Body != "" {
							item.Body = clipHead(item.Body+"\nerror: "+errMsg, maxDiff)
						} else {
							item.Body = clipTail(errMsg, maxOutput)
						}
					}
				} else {
					item.Status = status
					if item.K == "cmd" {
						if entry.Content != "" {
							item.Body = clipTail(entry.Content, maxOutput)
						}
						if item.Exit == nil && item.Status == "completed" {
							code := 0
							item.Exit = &code
						}
					} else if item.K != "file" && strings.TrimSpace(entry.Content) != "" {
						item.Body = clipTail(entry.Content, maxOutput)
					}
				}
				return
			}
		}

		if strings.TrimSpace(entry.Error) != "" {
			turn := v.currentTurn(stepKey, tsMs)
			if turn.Error == "" {
				turn.Error = clipPlain(entry.Error, 300)
			}
			v.appendItem(Item{
				ID:     stepKey + ":error",
				K:      "error",
				Text:   entry.Error,
				Status: "failed",
			})
		} else if strings.TrimSpace(entry.Content) != "" {
			v.currentTurn(stepKey, tsMs)
			v.appendItem(Item{
				ID:     stepKey + ":info",
				K:      "info",
				Text:   clipHead(strings.TrimSpace(entry.Content), maxOutput),
				Status: status,
			})
		}

	case "SYSTEM_MESSAGE":
		v.currentTurn(stepKey, tsMs)
		if strings.TrimSpace(entry.Error) != "" {
			v.appendItem(Item{
				ID:     stepKey + ":error",
				K:      "error",
				Text:   entry.Error,
				Status: "failed",
			})
		} else if strings.TrimSpace(entry.Content) != "" {
			v.appendItem(Item{
				ID:     stepKey + ":system",
				K:      "info",
				Title:  "System",
				Text:   clipHead(strings.TrimSpace(entry.Content), maxOutput),
				Status: status,
			})
		}

	case "CHECKPOINT":
		v.currentTurn(stepKey, tsMs)
		text := strings.TrimSpace(entry.Content)
		if text == "" {
			text = "Checkpoint"
		}
		v.appendItem(Item{
			ID:     stepKey + ":checkpoint",
			K:      "info",
			Title:  "Checkpoint",
			Text:   clipHead(text, maxOutput),
			Status: status,
		})

	case "ERROR_MESSAGE":
		turn := v.currentTurn(stepKey, tsMs)
		text := strings.TrimSpace(entry.Error)
		if text == "" {
			text = strings.TrimSpace(entry.Content)
		}
		if text == "" {
			text = "Error"
		}
		turn.Status = "failed"
		if turn.Error == "" {
			turn.Error = clipPlain(text, 300)
		}
		v.appendItem(Item{
			ID:     stepKey + ":error",
			K:      "error",
			Text:   text,
			Status: "failed",
		})

	default:
		if strings.TrimSpace(entry.Error) != "" {
			v.currentTurn(stepKey, tsMs)
			v.appendItem(Item{
				ID:     stepKey + ":error",
				K:      "error",
				Text:   entry.Error,
				Status: "failed",
			})
		} else if strings.TrimSpace(entry.Content) != "" {
			v.currentTurn(stepKey, tsMs)
			title := humanize(strings.ToLower(strings.ReplaceAll(entry.Type, "_", " ")))
			v.appendItem(Item{
				ID:     stepKey + ":info",
				K:      "info",
				Title:  title,
				Text:   clipHead(strings.TrimSpace(entry.Content), maxOutput),
				Status: status,
			})
		}
	}
}

func mapAntigravityStatus(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "RUNNING", "IN_PROGRESS", "PENDING", "CASCADE_RUN_STATUS_RUNNING":
		return "inProgress"
	case "ERROR", "FAILED":
		return "failed"
	case "CANCELLED", "CANCELED", "ABORTED":
		return "interrupted"
	default:
		return "completed"
	}
}

func antigravityToolCallItem(tc antigravityToolCall, id, status string) (Item, string) {
	name := strings.TrimSpace(tc.Name)
	args := tc.Args
	if args == nil {
		args = map[string]any{}
	}
	it := Item{
		ID:     id,
		Status: status,
	}
	if it.Status == "" {
		it.Status = "completed"
	}

	switch name {
	case "run_command", "Bash", "bash", "execute_command", "terminal":
		it.K = "cmd"
		cmdText := antigravityFirstNonEmpty(
			str(args, "CommandLine"),
			str(args, "command"),
			str(args, "cmd"),
			str(args, "toolSummary"),
			name,
		)
		it.Title = clipHead(shellScript(cmdText), 800)
		return it, ""

	case "edit_file", "write_file", "replace_file_content", "multi_replace_file_content", "write_to_file", "Edit", "Write":
		it.K = "file"
		path := antigravityFirstNonEmpty(
			str(args, "Path"),
			str(args, "TargetFile"),
			str(args, "file_path"),
			str(args, "path"),
		)
		diff, prefix := buildAntigravityToolDiff(name, path, args)
		if path != "" {
			it.Title = prefix + " " + path
		} else {
			it.Title = name
		}
		it.Body = clipHead(diff, maxDiff)
		return it, diff

	case "search_web", "grep_search", "find_by_name", "WebSearch":
		it.K = "search"
		it.Title = antigravityFirstNonEmpty(
			str(args, "query"),
			str(args, "Query"),
			str(args, "Pattern"),
			name,
		)
		if len(args) > 1 {
			it.Body = compactJSON(args, maxArgs)
		}
		return it, ""

	case "invoke_subagent", "Agent", "Task":
		it.K = "agent"
		it.Title = antigravityFirstNonEmpty(
			str(args, "description"),
			str(args, "Name"),
			str(args, "name"),
			name,
		)
		it.AgentID = antigravityFirstNonEmpty(
			str(args, "conversation_id"),
			str(args, "agent_id"),
			str(args, "resume"),
		)
		it.Body = compactJSON(args, maxArgs)
		return it, ""

	default:
		it.K = "tool"
		it.Title = name
		if it.Title == "" {
			it.Title = "Tool"
		}
		it.Body = compactJSON(args, maxArgs)
		return it, ""
	}
}

func buildAntigravityToolDiff(name, path string, args map[string]any) (string, string) {
	var b strings.Builder
	prefix := "M"
	if path != "" {
		fmt.Fprintf(&b, "--- %s\n+++ %s\n", path, path)
	}
	switch name {
	case "edit_file", "Edit":
		oldText := antigravityFirstNonEmpty(str(args, "Target"), str(args, "old_string"))
		newText := antigravityFirstNonEmpty(str(args, "Replacement"), str(args, "new_string"))
		if oldText != "" {
			b.WriteString(prefixLines(oldText, "-"))
		}
		if newText != "" {
			b.WriteString(prefixLines(newText, "+"))
		}
	case "write_file", "write_to_file", "Write":
		prefix = "A"
		if overwrite, _ := args["Overwrite"].(bool); overwrite {
			prefix = "M"
		}
		if appendMode, _ := args["Append"].(bool); appendMode {
			prefix = "M"
		}
		content := antigravityFirstNonEmpty(str(args, "Content"), str(args, "CodeContent"), str(args, "content"))
		if content != "" {
			b.WriteString(prefixLines(content, "+"))
		}
	case "replace_file_content":
		oldText := str(args, "TargetContent")
		newText := str(args, "ReplacementContent")
		if oldText != "" {
			b.WriteString(prefixLines(oldText, "-"))
		}
		if newText != "" {
			b.WriteString(prefixLines(newText, "+"))
		}
	case "multi_replace_file_content":
		for _, rawChunk := range arrayValue(args["ReplacementChunks"]) {
			cm := mapValue(rawChunk)
			if oldText := str(cm, "TargetContent"); oldText != "" {
				b.WriteString(prefixLines(oldText, "-"))
			}
			if newText := str(cm, "ReplacementContent"); newText != "" {
				b.WriteString(prefixLines(newText, "+"))
			}
		}
	}
	return b.String(), prefix
}

func antigravityFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---- annotation (.pbtxt) title reader ----

func readAntigravityAnnotationTitle(path string) string {
	if path == "" {
		return ""
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > antigravityAnnotationMaxBytes {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return parseAntigravityPbtxtTitle(data)
}

// parseAntigravityPbtxtTitle extracts a top-level `title: "..."` string value
// from a protobuf text-format (.pbtxt) annotation file.
func parseAntigravityPbtxtTitle(data []byte) string {
	s := string(data)
	depth := 0
	i := 0
	n := len(s)

	skipSpaceAndComments := func() {
		for i < n {
			c := s[i]
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				i++
				continue
			}
			if c == '#' {
				for i < n && s[i] != '\n' {
					i++
				}
				continue
			}
			break
		}
	}

	readQuoted := func() (string, bool) {
		if i >= n || (s[i] != '"' && s[i] != '\'') {
			return "", false
		}
		quote := s[i]
		i++
		var out strings.Builder
		for i < n {
			c := s[i]
			if c == quote {
				i++
				return out.String(), true
			}
			if c == '\n' {
				return "", false
			}
			if c == '\\' && i+1 < n {
				i++
				esc := s[i]
				switch esc {
				case 'n':
					out.WriteByte('\n')
					i++
				case 'r':
					out.WriteByte('\r')
					i++
				case 't':
					out.WriteByte('\t')
					i++
				case '\\', '"', '\'':
					out.WriteByte(esc)
					i++
				case 'x':
					if i+2 < n {
						if b, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
							out.WriteByte(byte(b))
							i += 3
							continue
						}
					}
					out.WriteByte(esc)
					i++
				case 'u':
					if i+4 < n {
						if r, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
							out.WriteRune(rune(r))
							i += 5
							continue
						}
					}
					out.WriteByte(esc)
					i++
				case '0', '1', '2', '3', '4', '5', '6', '7':
					end := i + 1
					for end < n && end < i+3 && s[end] >= '0' && s[end] <= '7' {
						end++
					}
					if b, err := strconv.ParseUint(s[i:end], 8, 8); err == nil {
						out.WriteByte(byte(b))
						i = end
						continue
					}
					out.WriteByte(esc)
					i++
				default:
					out.WriteByte(esc)
					i++
				}
				continue
			}
			r, size := utf8.DecodeRuneInString(s[i:])
			out.WriteRune(r)
			i += size
		}
		return "", false
	}

	for {
		skipSpaceAndComments()
		if i >= n {
			return ""
		}
		switch s[i] {
		case '{', '<':
			depth++
			i++
			continue
		case '}', '>':
			if depth > 0 {
				depth--
			}
			i++
			continue
		case '"', '\'':
			_, _ = readQuoted()
			continue
		}

		start := i
		for i < n && (antigravityIsASCIILetter(s[i]) || (s[i] >= '0' && s[i] <= '9') || s[i] == '_') {
			i++
		}
		if i == start {
			i++
			continue
		}
		ident := s[start:i]
		skipSpaceAndComments()
		if i >= n || s[i] != ':' {
			continue
		}
		i++ // consume ':'
		skipSpaceAndComments()

		if depth == 0 && ident == "title" && i < n && (s[i] == '"' || s[i] == '\'') {
			var parts strings.Builder
			for {
				part, ok := readQuoted()
				if !ok {
					break
				}
				parts.WriteString(part)
				saved := i
				skipSpaceAndComments()
				if i >= n || (s[i] != '"' && s[i] != '\'') {
					i = saved
					break
				}
			}
			return cleanAntigravityTitle(parts.String())
		}
	}
}

func cleanAntigravityTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return organizationText(s)
}

// ---- symlink-safe artifact containment ----

// resolveAntigravityArtifactPath resolves a relative, brain/<id>/..., or
// file:// artifact reference for conversationID inside
// <geminiDir>/<appDataDir>/brain/<conversationID> and verifies symlink-safe
// containment so files.go can serve the artifact safely.
func resolveAntigravityArtifactPath(geminiDir, appDataDir, conversationID, rawRef string) (string, error) {
	if strings.TrimSpace(geminiDir) == "" {
		geminiDir = antigravityGeminiDir("")
	}
	if !antigravityValidAppDataDir(appDataDir) {
		return "", errors.New("Invalid Antigravity App Data Directory")
	}
	if !antigravityValidConversationID(conversationID) {
		return "", errors.New("Invalid Antigravity Conversation ID")
	}
	brainDir := antigravityBrainDir(geminiDir, appDataDir, conversationID)
	return resolveAntigravityArtifactInBrain(brainDir, conversationID, rawRef)
}

func resolveAntigravityArtifactInBrain(brainDir, conversationID, rawRef string) (string, error) {
	if !antigravityValidConversationID(conversationID) {
		return "", errors.New("Invalid Antigravity Conversation ID")
	}
	ref := strings.TrimSpace(rawRef)
	if ref == "" || strings.ContainsRune(ref, '\x00') {
		return "", errors.New("Invalid Artifact Path")
	}

	var candidate string
	if strings.Contains(ref, "://") {
		decoded, ok := parseAntigravityFileURI(ref)
		if !ok {
			return "", errors.New("Unsupported Artifact URI")
		}
		candidate = decoded
	} else {
		// Reject non-file schemes like javascript: or data: (allowing Windows drive letters).
		if colon := strings.IndexByte(ref, ':'); colon > 1 && !strings.ContainsAny(ref[:colon], `/\`) {
			return "", errors.New("Unsupported Artifact Scheme")
		}
		if hash := strings.IndexByte(ref, '#'); hash >= 0 {
			ref = ref[:hash]
		}
		if query := strings.IndexByte(ref, '?'); query >= 0 {
			ref = ref[:query]
		}
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return "", errors.New("Invalid Artifact Path")
		}
		if filepath.IsAbs(ref) {
			candidate = filepath.Clean(ref)
		} else {
			slashed := filepath.ToSlash(ref)
			slashed = strings.TrimPrefix(slashed, "./")
			if strings.HasPrefix(slashed, "brain/") {
				expectedPrefix := "brain/" + conversationID + "/"
				if !strings.HasPrefix(slashed, expectedPrefix) {
					return "", errors.New("Artifact Path Escapes Conversation Brain")
				}
				slashed = strings.TrimPrefix(slashed, expectedPrefix)
			}
			candidate = filepath.Join(brainDir, filepath.FromSlash(slashed))
		}
	}

	realBrain, err := filepath.EvalSymlinks(brainDir)
	if err != nil {
		return "", errors.New("Conversation Brain Directory Is Unavailable")
	}
	brainStat, err := os.Stat(realBrain)
	if err != nil || !brainStat.IsDir() {
		return "", errors.New("Conversation Brain Directory Is Unavailable")
	}

	realTarget, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", errors.New("Artifact Is Unavailable")
	}
	rel, err := filepath.Rel(realBrain, realTarget)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("Artifact Path Escapes Conversation Brain")
	}

	st, err := os.Stat(realTarget)
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("Artifact Is Not A Regular File")
	}
	return realTarget, nil
}
