package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const antigravityTestSchema21 = `
PRAGMA journal_mode = WAL;
CREATE TABLE conversation_summaries (
	conversation_id TEXT PRIMARY KEY,
	title TEXT,
	preview TEXT,
	step_count INTEGER,
	last_modified_time datetime,
	workspace_uris TEXT,
	status TEXT,
	source TEXT,
	project_id TEXT,
	agent_name TEXT,
	parent_conversation_id TEXT,
	nesting_depth INTEGER,
	battle_id TEXT,
	winning_conversation_id TEXT,
	not_fully_idle numeric,
	killed numeric,
	last_user_input_time datetime,
	last_user_input_step_index INTEGER,
	app_data_dir TEXT,
	raw_summary BLOB,
	group_id TEXT
);`

type antigravityTestRow struct {
	id, title, preview, lastMod, workspaceURIs, status, source, projectID, agentName, parentID, lastUserInput, appDataDir, groupID string
	stepCount, nestingDepth, notFullyIdle, killed, lastUserStep                                                                    int
}

func createAntigravityTestDB(t *testing.T, path string, rows []antigravityTestRow) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open test sqlite: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(antigravityTestSchema21); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	stmt, err := db.Prepare(`INSERT INTO conversation_summaries (
		conversation_id, title, preview, step_count, last_modified_time,
		workspace_uris, status, source, project_id, agent_name,
		parent_conversation_id, nesting_depth, battle_id, winning_conversation_id,
		not_fully_idle, killed, last_user_input_time, last_user_input_step_index,
		app_data_dir, raw_summary, group_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, ?, ?, ?, X'', ?)`)
	if err != nil {
		t.Fatalf("prepare insert: %v", err)
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(
			r.id, r.title, r.preview, r.stepCount, r.lastMod,
			r.workspaceURIs, r.status, r.source, r.projectID, r.agentName,
			r.parentID, r.nestingDepth, r.notFullyIdle, r.killed,
			r.lastUserInput, r.lastUserStep, r.appDataDir, r.groupID,
		); err != nil {
			t.Fatalf("insert row %s: %v", r.id, err)
		}
	}
	// Keep WAL file active without forcing a truncating checkpoint so tests
	// exercise real WAL-mode read-only queries.
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if strings.ToLower(mode) != "wal" {
		t.Fatalf("expected wal mode, got %q", mode)
	}
}

func writeTestTranscript(t *testing.T, path string, lines []string, modTime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if !modTime.IsZero() {
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("chtimes transcript: %v", err)
		}
	}
}

func withAntigravityTestClockAndOwners(t *testing.T, now time.Time, probe func([]string) map[string]rolloutOwnerEvidence) {
	t.Helper()
	prevNow := antigravityNow
	prevOwners := antigravityOwners
	antigravityNow = func() time.Time { return now }
	antigravityOwners = &rolloutOwnerCache{
		entries: map[string]rolloutOwnerEvidence{},
		pending: map[string]os.FileInfo{},
		probe:   probe,
	}
	t.Cleanup(func() {
		antigravityNow = prevNow
		antigravityOwners = prevOwners
	})
}

func TestAntigravitySQLiteDualDBDedupURIsAndStaleGuard(t *testing.T) {
	geminiDir := t.TempDir()
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)

	withAntigravityTestClockAndOwners(t, now, func(paths []string) map[string]rolloutOwnerEvidence {
		res := make(map[string]rolloutOwnerEvidence, len(paths))
		for _, p := range paths {
			// Simulate owner alive only for path containing "conv-owner-alive".
			alive := strings.Contains(p, "conv-owner-alive")
			res[p] = rolloutOwnerEvidence{known: true, alive: alive}
		}
		return res
	})

	// Create transcripts with controlled mtimes.
	freshTime := now.Add(-15 * time.Second)
	staleTime := now.Add(-10 * time.Minute)
	writeTestTranscript(t, antigravityTranscriptPath(geminiDir, antigravityAppDirDesktop, "conv-fresh"), []string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","source":"USER_EXPLICIT","created_at":"2026-11-27T13:59:00Z","content":"run tests"}`,
	}, freshTime)
	writeTestTranscript(t, antigravityTranscriptPath(geminiDir, antigravityAppDirDesktop, "conv-stale"), []string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","source":"USER_EXPLICIT","created_at":"2026-11-27T13:40:00Z","content":"abandoned"}`,
	}, staleTime)
	writeTestTranscript(t, antigravityTranscriptPath(geminiDir, antigravityAppDirDesktop, "conv-owner-alive"), []string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","source":"USER_EXPLICIT","created_at":"2026-11-27T13:40:00Z","content":"long wait"}`,
	}, staleTime)
	writeTestTranscript(t, antigravityTranscriptPath(geminiDir, antigravityAppDirDesktop, "conv-killed"), []string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","source":"USER_EXPLICIT","created_at":"2026-11-27T13:59:50Z","content":"killed run"}`,
	}, freshTime)

	// Annotation fallback for empty DB title.
	annPath := antigravityAnnotationPath(geminiDir, antigravityAppDirDesktop, "conv-fresh")
	if err := os.MkdirAll(filepath.Dir(annPath), 0700); err != nil {
		t.Fatalf("mkdir annotations: %v", err)
	}
	if err := os.WriteFile(annPath, []byte("title: \"Annotated Fresh Title\"\n"), 0600); err != nil {
		t.Fatalf("write pbtxt: %v", err)
	}

	desktopDB := antigravityDBPath(geminiDir, antigravityAppDirDesktop)
	createAntigravityTestDB(t, desktopDB, []antigravityTestRow{
		{
			id:            "conv-fresh",
			title:         "",
			preview:       "fresh preview",
			stepCount:     4,
			lastMod:       "2026-11-27 13:59:45.123456+00:00",
			workspaceURIs: `["file:///Users/siraj/My%20Repo/fast%2Bcontrol","https://evil.example/ignore","ssh://host/path","file://remote.host/share"]`,
			status:        "CASCADE_RUN_STATUS_RUNNING",
			projectID:     "gcp-proj-1",
			notFullyIdle:  1,
			killed:        0,
			lastUserInput: "2026-11-27 13:59:00.000000+00:00",
			appDataDir:    "antigravity",
			groupID:       "grp-main",
		},
		{
			id:            "conv-stale",
			title:         "Abandoned Run",
			preview:       "stale preview",
			stepCount:     2,
			lastMod:       "2026-11-27 13:40:00.000000+00:00",
			workspaceURIs: `["file:///Users/siraj/dev/app"]`,
			status:        "CASCADE_RUN_STATUS_RUNNING",
			notFullyIdle:  1,
			killed:        0,
			lastUserInput: "0001-01-01 00:00:00+00:00",
			appDataDir:    "antigravity",
		},
		{
			id:            "conv-owner-alive",
			title:         "Held Open Run",
			preview:       "owner alive",
			stepCount:     3,
			lastMod:       "2026-11-27 13:45:00.000000+00:00",
			workspaceURIs: `["file:///Users/siraj/dev/app"]`,
			status:        "CASCADE_RUN_STATUS_RUNNING",
			notFullyIdle:  0,
			killed:        0,
			lastUserInput: "2026-11-27 13:40:00+00:00",
			appDataDir:    "antigravity",
		},
		{
			id:            "conv-killed",
			title:         "Killed Run",
			preview:       "killed preview",
			stepCount:     1,
			lastMod:       "2026-11-27 13:59:50+00:00",
			workspaceURIs: `["file:///Users/siraj/dev/app"]`,
			status:        "CASCADE_RUN_STATUS_RUNNING",
			notFullyIdle:  1,
			killed:        1,
			lastUserInput: "2026-11-27 13:59:40+00:00",
			appDataDir:    "antigravity",
		},
		{
			id:            "conv-dup-tie",
			title:         "Desktop Preferred",
			preview:       "desktop copy",
			stepCount:     5,
			lastMod:       "2026-11-27 12:00:00.000000+00:00",
			workspaceURIs: `["file:///Users/siraj/dev/app"]`,
			status:        "CASCADE_RUN_STATUS_IDLE",
			appDataDir:    "antigravity",
		},
		{
			id:            "conv-dup-older",
			title:         "Older Desktop Copy",
			preview:       "old desktop",
			stepCount:     1,
			lastMod:       "2026-11-27 10:00:00.000000+00:00",
			workspaceURIs: `["file:///Users/siraj/dev/app"]`,
			status:        "CASCADE_RUN_STATUS_IDLE",
			appDataDir:    "antigravity",
		},
		{
			id:            "sub-1",
			title:         "Worker Subagent",
			preview:       "worker preview",
			stepCount:     6,
			lastMod:       "2026-11-27 13:58:00.000000+00:00",
			workspaceURIs: `["file:///Users/siraj/My%20Repo/fast%2Bcontrol"]`,
			status:        "CASCADE_RUN_STATUS_IDLE",
			agentName:     "verifier",
			parentID:      "conv-fresh",
			nestingDepth:  1,
			appDataDir:    "antigravity",
		},
	})

	cliDB := antigravityDBPath(geminiDir, antigravityAppDirCLI)
	createAntigravityTestDB(t, cliDB, []antigravityTestRow{
		{
			id:            "conv-dup-tie",
			title:         "CLI Duplicate Same Time",
			preview:       "cli copy",
			stepCount:     5,
			lastMod:       "2026-11-27 12:00:00.000000+00:00",
			workspaceURIs: `["file:///Users/siraj/dev/cli"]`,
			status:        "CASCADE_RUN_STATUS_IDLE",
			appDataDir:    "antigravity-cli",
		},
		{
			id:            "conv-dup-older",
			title:         "Newer CLI Copy",
			preview:       "fresh cli",
			stepCount:     9,
			lastMod:       "2026-11-27 13:30:00.000000+00:00",
			workspaceURIs: `["file:///Users/siraj/dev/cli"]`,
			status:        "CASCADE_RUN_STATUS_IDLE",
			source:        "CORTEX_TRAJECTORY_SOURCE_CLI",
			appDataDir:    "antigravity-cli",
		},
		{
			id:            "conv-cli-only",
			title:         "CLI Only Chat",
			preview:       "cli only",
			stepCount:     3,
			lastMod:       "2026-11-27T13:00:00Z",
			workspaceURIs: `["file:///Users/siraj/cli%20space"]`,
			status:        "CASCADE_RUN_STATUS_IDLE",
			source:        "CORTEX_TRAJECTORY_SOURCE_CLI",
			appDataDir:    "",
		},
	})

	liveCheck := func(id string) bool { return id == "conv-cli-only" }
	records, err := antigravityListRecords(context.Background(), geminiDir, liveCheck)
	if err != nil {
		t.Fatalf("antigravityListRecords error: %v", err)
	}

	byID := map[string]antigravityRecord{}
	for _, r := range records {
		byID[r.ID] = r
	}
	if len(byID) != 8 {
		t.Fatalf("expected 8 deduplicated records, got %d", len(byID))
	}

	// Verify URL-decoded Cwd, non-file URI rejection, annotation fallback, and fresh running.
	fresh := byID["conv-fresh"]
	if fresh.Title != "Annotated Fresh Title" {
		t.Fatalf("expected annotation fallback title, got %q", fresh.Title)
	}
	if fresh.Cwd != filepath.Clean("/Users/siraj/My Repo/fast+control") {
		t.Fatalf("unexpected decoded Cwd: %q", fresh.Cwd)
	}
	if len(fresh.WorkspacePaths) != 1 {
		t.Fatalf("expected only 1 valid file:// workspace path, got %v", fresh.WorkspacePaths)
	}
	if !fresh.Running {
		t.Fatalf("expected fresh transcript running=true")
	}
	if fresh.AppDataDir != antigravityAppDirDesktop {
		t.Fatalf("expected appDataDir=%q, got %q", antigravityAppDirDesktop, fresh.AppDataDir)
	}
	if fresh.GroupID != "grp-main" {
		t.Fatalf("expected groupID grp-main, got %q", fresh.GroupID)
	}

	// Stale abandoned CASCADE_RUN_STATUS_RUNNING must clear to Running=false,
	// and Go zero time 0001-01-01 must parse as 0.
	stale := byID["conv-stale"]
	if stale.Running {
		t.Fatalf("expected stale abandoned run to have Running=false")
	}
	if stale.LastUserInputTime != 0 {
		t.Fatalf("expected 0001-01-01 sentinel to parse as 0, got %d", stale.LastUserInputTime)
	}

	// Owner-alive stale transcript must stay Running=true.
	if !byID["conv-owner-alive"].Running {
		t.Fatalf("expected live-owner transcript to remain Running=true")
	}

	// Killed row must have Running=false even with fresh transcript mtime.
	if byID["conv-killed"].Running {
		t.Fatalf("expected killed row to have Running=false")
	}

	// Live ACP override must force Running=true even on IDLE row.
	cliOnly := byID["conv-cli-only"]
	if !cliOnly.Running {
		t.Fatalf("expected liveCheck override to force Running=true")
	}
	if cliOnly.AppDataDir != antigravityAppDirCLI {
		t.Fatalf("expected blank app_data_dir on CLI DB to default to %q, got %q", antigravityAppDirCLI, cliOnly.AppDataDir)
	}
	if cliOnly.Cwd != filepath.Clean("/Users/siraj/cli space") {
		t.Fatalf("unexpected CLI decoded Cwd: %q", cliOnly.Cwd)
	}

	// Deduplication: tie prefers desktop; strictly newer prefers newer row.
	if byID["conv-dup-tie"].Title != "Desktop Preferred" || byID["conv-dup-tie"].AppDataDir != antigravityAppDirDesktop {
		t.Fatalf("expected desktop preference on tie, got %+v", byID["conv-dup-tie"])
	}
	if byID["conv-dup-older"].Title != "Newer CLI Copy" || byID["conv-dup-older"].AppDataDir != antigravityAppDirCLI {
		t.Fatalf("expected newer CLI record to win, got %+v", byID["conv-dup-older"])
	}
}

func TestAntigravityOlderSQLiteSchemaMissingOptionalColumns(t *testing.T) {
	geminiDir := t.TempDir()
	dbPath := antigravityDBPath(geminiDir, antigravityAppDirDesktop)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT,
			last_modified_time TEXT
		);
		INSERT INTO conversation_summaries VALUES ('legacy-1', 'Legacy Chat', '1763657544');
	`)
	if err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}

	records, err := antigravityListRecords(context.Background(), geminiDir, nil)
	if err != nil {
		t.Fatalf("list legacy db: %v", err)
	}
	if len(records) != 1 || records[0].ID != "legacy-1" || records[0].Title != "Legacy Chat" {
		t.Fatalf("unexpected legacy records: %+v", records)
	}
	if records[0].Updated != 1763657544 || records[0].Recency != 1763657544000 {
		t.Fatalf("unexpected unix timestamp parsing: updated=%d recency=%d", records[0].Updated, records[0].Recency)
	}
}

func TestAntigravityTimestampAndURIParsing(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
	}{
		{"sqlite_frac_offset", "2025-11-20 16:52:24.960249+00:00", 1763657544},
		{"rfc3339", "2025-11-20T16:52:24Z", 1763657544},
		{"rfc3339_offset", "2025-11-20T11:52:24-05:00", 1763657544},
		{"unix_sec_int", int64(1763657544), 1763657544},
		{"unix_ms_int", int64(1763657544960), 1763657544},
		{"unix_sec_str", "1763657544", 1763657544},
		{"go_zero_sentinel", "0001-01-01 00:00:00+00:00", 0},
		{"epoch_zero_sentinel", "1970-01-01T00:00:00Z", 0},
		{"invalid", "not-a-date", 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		if got := parseAntigravityUnix(tc.in); got != tc.want {
			t.Errorf("%s: parseAntigravityUnix(%v) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}

	uris := parseAntigravityWorkspaceURIs(`["file:///Users/alice/My%20Docs","file://localhost/tmp/work","file:///C:/Users/bob/proj","https://example.com/a","javascript:alert(1)","/bare/path","file://evil.host/share"]`)
	if len(uris) != 3 {
		t.Fatalf("expected 3 valid local paths, got %v", uris)
	}
	if uris[0] != filepath.Clean("/Users/alice/My Docs") {
		t.Errorf("unexpected decoded path 0: %q", uris[0])
	}
	if uris[1] != filepath.Clean("/tmp/work") {
		t.Errorf("unexpected localhost path 1: %q", uris[1])
	}
	if uris[2] != filepath.Clean(filepath.FromSlash("C:/Users/bob/proj")) {
		t.Errorf("unexpected windows drive path 2: %q", uris[2])
	}
}

func TestAntigravityTranscriptSelectiveExpansionAndPagination(t *testing.T) {
	geminiDir := t.TempDir()
	convID := "conv-transcript-1"
	brainDir := antigravityBrainDir(geminiDir, antigravityAppDirDesktop, convID)
	tPath := antigravityTranscriptPath(geminiDir, antigravityAppDirDesktop, convID)
	fullPath := antigravityFullTranscriptPath(geminiDir, antigravityAppDirDesktop, convID)
	annPath := antigravityAnnotationPath(geminiDir, antigravityAppDirDesktop, convID)

	longAnswer := "Full expanded response: " + strings.Repeat("alpha ", 80)
	longThinking := "Full expanded reasoning: " + strings.Repeat("think ", 60)
	fullReplacement := "line1\nline2-updated\n"

	compactLines := []string{
		`{"step_index":0,"type":"SYSTEM_MESSAGE","status":"DONE","source":"SYSTEM","created_at":"2026-11-27T10:00:00Z","content":"Session initialized"}`,
		`{"step_index":1,"type":"USER_INPUT","status":"DONE","source":"USER_EXPLICIT","created_at":"2026-11-27T10:01:00Z","content":"First turn question"}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","source":"MODEL","created_at":"2026-11-27T10:01:05Z","model":"gemini-4-neon","input_tokens":1000,"cache_read_tokens":200,"output_tokens":50,"response":"Legacy response fallback"}`,
		`{"step_index":3,"type":"CHECKPOINT","status":"DONE","source":"SYSTEM","created_at":"2026-11-27T10:01:10Z","content":"Saved milestone"}`,
		`{"step_index":4,"type":"USER_INPUT","status":"DONE","source":"USER_EXPLICIT","created_at":"2026-11-27T10:02:00Z","content":"Edit file and run command"}`,
		`{"step_index":5,"type":"PLANNER_RESPONSE","status":"DONE","source":"MODEL","created_at":"2026-11-27T10:02:05Z","model":"gemini-4-neon-high","input_tokens":2500,"cache_read_tokens":500,"output_tokens":300,"thinking":"trunc...","content":"trunc...","truncated_fields":["thinking","content","tool_calls"],"tool_calls":[{"name":"edit_file","args":{"Path":"/repo/a.go","Target":"old","Replacement":"trunc..."}}]}`,
		`{"step_index":6,"type":"GENERIC","status":"DONE","source":"MODEL","created_at":"2026-11-27T10:02:06Z","content":"Applied edit"}`,
		`{"step_index":7,"type":"GENERIC","status":"ERROR","source":"MODEL","created_at":"2026-11-27T10:02:07Z","content":"exit 1","error":"command failed: exit status 1"}`,
		`{"step_index":8,"type":"ERROR_MESSAGE","status":"ERROR","source":"SYSTEM","created_at":"2026-11-27T10:02:08Z","content":"Transient rate limit"}`,
	}

	fullStep5, err := json.Marshal(map[string]any{
		"step_index":        5,
		"type":              "PLANNER_RESPONSE",
		"status":            "DONE",
		"source":            "MODEL",
		"created_at":        "2026-11-27T10:02:05Z",
		"model":             "gemini-4-neon-high",
		"input_tokens":      2500,
		"cache_read_tokens": 500,
		"output_tokens":     300,
		"thinking":          longThinking,
		"content":           longAnswer,
		"tool_calls": []map[string]any{
			{
				"name": "edit_file",
				"args": map[string]any{"Path": "/repo/a.go", "Target": "line1\nline2\n", "Replacement": fullReplacement},
			},
			{
				"name": "run_command",
				"args": map[string]any{"CommandLine": "go test ./..."},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal full step: %v", err)
	}
	fullLines := []string{
		`{"step_index":1,"type":"USER_INPUT","content":"First turn question"}`,
		string(fullStep5),
	}

	writeTestTranscript(t, tPath, compactLines, time.Time{})
	writeTestTranscript(t, fullPath, fullLines, time.Time{})
	if err := os.MkdirAll(filepath.Dir(annPath), 0700); err != nil {
		t.Fatalf("mkdir annotations: %v", err)
	}
	if err := os.WriteFile(annPath, []byte("# comment\nnested { title: \"Ignore Me\" }\ntitle: \"My \\\"Quoted\\\" \" \"Title\"\n"), 0600); err != nil {
		t.Fatalf("write pbtxt: %v", err)
	}

	// Read full history (limit 10 -> More=false).
	fullView, err := readAntigravityTranscriptWithOptions(tPath, antigravityTranscriptOptions{
		Limit:          10,
		FullPath:       fullPath,
		AnnotationPath: annPath,
		BrainDir:       brainDir,
		Cwd:            "/repo",
	})
	if err != nil {
		t.Fatalf("read full transcript: %v", err)
	}
	if fullView.More || fullView.Meta.HasOlder {
		t.Fatalf("expected More=false when all turns fit in limit")
	}
	if fullView.Meta.Title != `My "Quoted" Title` {
		t.Fatalf("unexpected pbtxt title: %q", fullView.Meta.Title)
	}
	if fullView.Meta.Model != "gemini-4-neon-high" {
		t.Fatalf("unexpected model: %q", fullView.Meta.Model)
	}
	if fullView.Meta.CtxUsed != 3000 {
		t.Fatalf("expected CtxUsed=3000 (input+cache_read), got %d", fullView.Meta.CtxUsed)
	}
	if fullView.InputTok != 3500 || fullView.CacheTok != 700 || fullView.OutputTok != 350 {
		t.Fatalf("unexpected token totals: in=%d cache=%d out=%d", fullView.InputTok, fullView.CacheTok, fullView.OutputTok)
	}

	// Expect 3 turns: leading SYSTEM_MESSAGE turn + 2 USER_INPUT turns.
	if len(fullView.Turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(fullView.Turns))
	}

	// Turn 1 (first USER_INPUT): verify legacy `response` fallback and CHECKPOINT.
	var sawLegacyResp, sawCheckpoint bool
	for _, it := range fullView.Turns[1].Items {
		if it.K == "assistant" && it.Text == "Legacy response fallback" {
			sawLegacyResp = true
		}
		if it.K == "info" && it.Title == "Checkpoint" && it.Text == "Saved milestone" {
			sawCheckpoint = true
		}
	}
	if !sawLegacyResp || !sawCheckpoint {
		t.Fatalf("missing legacy response (%v) or checkpoint (%v) in turn 1: %+v", sawLegacyResp, sawCheckpoint, fullView.Turns[1].Items)
	}

	// Turn 2 (second USER_INPUT): verify expanded thinking, expanded file diff,
	// expanded cmd + attached GENERIC error, expanded assistant reply, and ERROR_MESSAGE.
	t2 := fullView.Turns[2]
	if !t2.HasDiff {
		t.Fatalf("expected turn 2 HasDiff=true")
	}
	if !strings.Contains(fullView.Diffs[t2.Key], "+line2-updated") {
		t.Fatalf("expected expanded diff in Diffs[%s], got %q", t2.Key, fullView.Diffs[t2.Key])
	}
	var sawThinking, sawFile, sawCmdFailed, sawAssistant, sawErrMsg bool
	for _, it := range t2.Items {
		switch {
		case it.K == "reasoning" && it.Text == longThinking:
			sawThinking = true
		case it.K == "file" && it.Title == "M /repo/a.go" && strings.Contains(it.Body, "+line2-updated"):
			sawFile = true
		case it.K == "cmd" && it.Title == "go test ./..." && it.Status == "failed" && it.Exit != nil && *it.Exit == 1 && strings.Contains(it.Body, "command failed"):
			sawCmdFailed = true
		case it.K == "assistant" && it.Text == longAnswer:
			sawAssistant = true
		case it.K == "error" && it.Text == "Transient rate limit":
			sawErrMsg = true
		}
	}
	if !sawThinking || !sawFile || !sawCmdFailed || !sawAssistant || !sawErrMsg {
		t.Fatalf("missing expanded items: thinking=%v file=%v cmdFailed=%v assistant=%v errMsg=%v; items=%+v",
			sawThinking, sawFile, sawCmdFailed, sawAssistant, sawErrMsg, t2.Items)
	}

	// Pagination: limit=1 returns only last turn with More=true & HasOlder=true.
	pagedView, err := readAntigravityTranscriptWithOptions(tPath, antigravityTranscriptOptions{
		Limit:    1,
		FullPath: fullPath,
		BrainDir: brainDir,
	})
	if err != nil {
		t.Fatalf("read paged transcript: %v", err)
	}
	if !pagedView.More || !pagedView.Meta.HasOlder {
		t.Fatalf("expected More=true and HasOlder=true when paginated")
	}
	if len(pagedView.Turns) != 1 || pagedView.Turns[0].Key != "step-4" {
		t.Fatalf("expected only last turn step-4, got %+v", pagedView.Turns)
	}
}

func TestAntigravityArtifactContainment(t *testing.T) {
	geminiDir := t.TempDir()
	convID := "conv-art-1"
	otherID := "conv-art-2"
	brainDir := antigravityBrainDir(geminiDir, antigravityAppDirDesktop, convID)
	otherBrain := antigravityBrainDir(geminiDir, antigravityAppDirDesktop, otherID)

	planPath := filepath.Join(brainDir, "implementation_plan.md")
	nestedImg := filepath.Join(brainDir, "images", "shot.png")
	secretOutside := filepath.Join(geminiDir, "secret.txt")
	otherFile := filepath.Join(otherBrain, "private.md")

	for _, p := range []string{planPath, nestedImg, secretOutside, otherFile} {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte("ok"), 0600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	// Create symlink inside brain pointing outside.
	escapeLink := filepath.Join(brainDir, "escape.md")
	symlinkCreated := os.Symlink(secretOutside, escapeLink) == nil

	// Valid relative path.
	got, err := resolveAntigravityArtifactPath(geminiDir, antigravityAppDirDesktop, convID, "implementation_plan.md#L1-L5")
	if err != nil {
		t.Fatalf("valid relative artifact failed: %v", err)
	}
	wantReal, _ := filepath.EvalSymlinks(planPath)
	if got != wantReal {
		t.Fatalf("got %q, want %q", got, wantReal)
	}

	// Valid brain/<id>/... path.
	got, err = resolveAntigravityArtifactPath(geminiDir, antigravityAppDirDesktop, convID, "brain/"+convID+"/images/shot.png")
	if err != nil {
		t.Fatalf("valid brain/<id> artifact failed: %v", err)
	}
	wantImg, _ := filepath.EvalSymlinks(nestedImg)
	if got != wantImg {
		t.Fatalf("got %q, want %q", got, wantImg)
	}

	// Valid file:// URI inside brain.
	fileURI := sqliteFileURI(planPath, "")
	fileURI = strings.TrimSuffix(fileURI, "?")
	got, err = resolveAntigravityArtifactPath(geminiDir, antigravityAppDirDesktop, convID, fileURI)
	if err != nil {
		t.Fatalf("valid file:// artifact failed: %v", err)
	}
	if got != wantReal {
		t.Fatalf("got %q, want %q", got, wantReal)
	}

	// Reject traversals, cross-conversation brain refs, outside absolutes, directories, non-file schemes.
	rejectRefs := []string{
		"../../secret.txt",
		"../../../etc/passwd",
		"brain/" + otherID + "/private.md",
		"brain/" + convID + "/../" + otherID + "/private.md",
		secretOutside,
		"images",
		"https://example.com/plan.md",
		"javascript:alert(1)",
		"",
	}
	if symlinkCreated {
		rejectRefs = append(rejectRefs, "escape.md", "brain/"+convID+"/escape.md")
	}
	for _, ref := range rejectRefs {
		if resolved, err := resolveAntigravityArtifactPath(geminiDir, antigravityAppDirDesktop, convID, ref); err == nil {
			t.Errorf("expected artifact ref %q to be rejected, but resolved to %q", ref, resolved)
		}
	}
}

func TestAntigravitySubagentsListOpenAndReadOnlyIngress(t *testing.T) {
	geminiDir := t.TempDir()
	parentID := "parent-conv"
	otherParent := "other-conv"
	agent1ID := "subagent-1"
	agent2ID := "subagent-2"

	agent1Transcript := antigravityTranscriptPath(geminiDir, antigravityAppDirDesktop, agent1ID)
	writeTestTranscript(t, agent1Transcript, []string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":"2026-11-27T11:00:00Z","content":"Verify SQLite reader"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","model":"gemini-4-neon","input_tokens":500,"output_tokens":100,"content":"All checks passed"}`,
	}, time.Time{})

	records := []antigravityRecord{
		{
			ID:         parentID,
			Title:      "Main Chat",
			Cwd:        "/repo/main",
			Recency:    3000,
			AppDataDir: antigravityAppDirDesktop,
		},
		{
			ID:                   agent1ID,
			Title:                "Verify Storage",
			Preview:              "Checking WAL mode",
			Cwd:                  "/repo/main",
			Status:               "CASCADE_RUN_STATUS_IDLE",
			AgentName:            "verifier",
			ParentConversationID: parentID,
			NestingDepth:         1,
			Recency:              2000,
			StepCount:            2,
			TranscriptPath:       agent1Transcript,
			BrainDir:             antigravityBrainDir(geminiDir, antigravityAppDirDesktop, agent1ID),
		},
		{
			ID:                   agent2ID,
			Title:                "Active Worker",
			Preview:              "Running tests",
			Cwd:                  "/repo/main",
			Status:               "CASCADE_RUN_STATUS_RUNNING",
			AgentName:            "tester",
			ParentConversationID: parentID,
			NestingDepth:         1,
			Running:              true,
			Recency:              1500,
		},
		{
			ID:                   "subagent-other",
			Title:                "Other Parent Worker",
			ParentConversationID: otherParent,
			NestingDepth:         1,
			Recency:              4000,
		},
	}

	// List subagents: running first, then newest recency, only matching parentID.
	agents := antigravityListAgents(records, parentID, nil)
	if len(agents) != 2 {
		t.Fatalf("expected 2 subagents for %s, got %d", parentID, len(agents))
	}
	if agents[0].ID != agent2ID || !agents[0].Running || agents[0].AgentState != "running" || !agents[0].Subagent || agents[0].ParentID != parentID || agents[0].AgentName != "tester" {
		t.Fatalf("unexpected first (running) subagent: %+v", agents[0])
	}
	if agents[1].ID != agent1ID || agents[1].Running || agents[1].AgentState != "completed" || agents[1].AgentName != "verifier" || agents[1].Cwd != "/repo/main" {
		t.Fatalf("unexpected second subagent: %+v", agents[1])
	}

	// Subagent detection and strict read-only ingress enforcement.
	if antigravityIsSubagent(records, parentID) {
		t.Fatalf("parent chat must not be classified as subagent")
	}
	if !antigravityIsSubagent(records, agent1ID) || !antigravityIsSubagent(records, agent2ID) {
		t.Fatalf("subagents must be classified as subagents")
	}
	for _, op := range []string{"send", "stop", "wake", "settings", "approve", "answer", "compact", "archive", "unarchive", "rename", "fork", "pin", "unpin"} {
		if err := antigravityRejectSubagentControl(records, agent1ID, op); err == nil {
			t.Errorf("expected op %q to be rejected on subagent", op)
		}
		if err := antigravityRejectSubagentControl(records, parentID, op); err != nil {
			t.Errorf("expected op %q to be allowed on parent chat, got %v", op, err)
		}
	}

	// Parentage validation on open.
	if _, _, err := antigravityOpenAgent(records, otherParent, agent1ID, 15); err == nil {
		t.Fatalf("expected parentage mismatch to fail")
	}
	if _, _, err := antigravityOpenAgent(records, parentID, parentID, 15); err == nil {
		t.Fatalf("expected opening parent as subagent to fail")
	}

	// Valid open returns read-only tail Meta and parsed turns.
	meta, turns, err := antigravityOpenAgent(records, parentID, agent1ID, 15)
	if err != nil {
		t.Fatalf("open valid subagent failed: %v", err)
	}
	if !meta.ReadOnly || meta.Mode != "tail" || meta.Provider != "antigravity" || meta.AgentState != "completed" || meta.Model != "gemini-4-neon" {
		t.Fatalf("unexpected subagent meta: %+v", meta)
	}
	if len(turns) != 1 || len(turns[0].Items) != 2 {
		t.Fatalf("unexpected subagent turns: %+v", turns)
	}
}

func TestAntigravityStatePersistenceAndOrganization(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), ".fastcontrol", "antigravity-state.json")
	st, err := newAntigravityState(statePath)
	if err != nil {
		t.Fatalf("newAntigravityState: %v", err)
	}

	if err := st.SetPinned("c1", true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	if err := st.SetArchived("c2", true); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	clean, err := st.SetTitle("c1", "  Custom Title  ")
	if err != nil || clean != "Custom Title" {
		t.Fatalf("SetTitle: got (%q, %v)", clean, err)
	}
	if _, err := st.SetTitle("c1", "Bad\nTitle"); err == nil {
		t.Fatalf("expected multiline title to be rejected by sessionTitle")
	}
	if _, err := st.SetTitle("c1", "   "); err == nil {
		t.Fatalf("expected blank title to be rejected by sessionTitle")
	}
	if err := st.SetPref("c1", antigravityThreadPref{Model: "gemini-4-neon", Effort: "high", PermissionMode: "accept-edits"}); err != nil {
		t.Fatalf("SetPref: %v", err)
	}

	// Verify file permissions (0600 on Unix) and persistence across reload.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(statePath)
		if err != nil {
			t.Fatalf("stat state file: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("expected state file mode 0600, got %o", perm)
		}
	}

	// Concurrent reads/writes under -race.
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("concurrent-%d", idx)
			_ = st.SetPinned(id, idx%2 == 0)
			_ = st.IsPinned(id)
			_ = st.SetPref(id, antigravityThreadPref{Model: "gemini-4-neon", Effort: "low"})
			_ = st.Pref(id)
			_ = st.Snapshot()
		}(i)
	}
	wg.Wait()

	reloaded, err := newAntigravityState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if !reloaded.IsPinned("c1") || !reloaded.IsArchived("c2") || reloaded.Title("c1") != "Custom Title" {
		t.Fatalf("state did not persist cleanly across reload: %+v", reloaded.Snapshot())
	}
	pref := reloaded.Pref("c1")
	if pref.Model != "gemini-4-neon" || pref.Effort != "high" || pref.PermissionMode != "accept-edits" {
		t.Fatalf("unexpected reloaded pref: %+v", pref)
	}

	// OrganizationReader.Antigravity: pinning, archiving, renaming, workspace grouping,
	// manual GroupID, and excluding subagents.
	records := []antigravityRecord{
		{ID: "c1", Title: "Orig One", Cwd: "/work/repo-a", WorkspacePaths: []string{"/work/repo-a"}, Recency: 1000, GroupID: "sprint-1"},
		{ID: "c2", Title: "Archived Two", Cwd: "/work/repo-a", WorkspacePaths: []string{"/work/repo-a"}, Recency: 3000},
		{ID: "c3", Title: "Recent Three", Cwd: "/personal/repo-b", WorkspacePaths: []string{"/personal/repo-b"}, Recency: 2000},
		{ID: "sub-x", Title: "Subagent Excluded", Cwd: "/work/repo-a", ParentConversationID: "c1", NestingDepth: 1, Recency: 5000},
	}

	reader := NewOrganizationReader(t.TempDir())
	activeThreads, groups := reader.Antigravity(false, reloaded, records)
	if len(activeThreads) != 2 {
		t.Fatalf("expected 2 active top-level threads (c1 pinned, c3 recent), got %d: %+v", len(activeThreads), activeThreads)
	}
	// Pinned thread c1 must sort first despite lower Recency than c3.
	if activeThreads[0].ID != "c1" || !activeThreads[0].Pinned || activeThreads[0].DisplayTitle != "Custom Title" || activeThreads[0].Model != "gemini-4-neon" || activeThreads[0].Effort != "high" {
		t.Fatalf("unexpected pinned thread[0]: %+v", activeThreads[0])
	}
	if activeThreads[1].ID != "c3" || activeThreads[1].Pinned {
		t.Fatalf("unexpected thread[1]: %+v", activeThreads[1])
	}
	if activeThreads[0].ProjectID == "" || len(activeThreads[0].GroupIDs) != 2 {
		t.Fatalf("expected project group + manual group on c1, got project=%q groups=%v", activeThreads[0].ProjectID, activeThreads[0].GroupIDs)
	}
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups (2 workspace projects + 1 manual group), got %d: %+v", len(groups), groups)
	}

	// Archived list returns only c2.
	archivedThreads, archivedGroups := reader.Antigravity(true, reloaded, records)
	if len(archivedThreads) != 1 || archivedThreads[0].ID != "c2" {
		t.Fatalf("expected only archived c2, got %+v", archivedThreads)
	}
	if len(archivedGroups) != 1 || archivedGroups[0].Kind != "project" {
		t.Fatalf("unexpected archived groups: %+v", archivedGroups)
	}

	// Folder project summaries exclude archived and subagents.
	projects := antigravityProjects(records, reloaded)
	if len(projects) != 2 || projects[0].Cwd != "/personal/repo-b" || projects[1].Cwd != "/work/repo-a" {
		t.Fatalf("unexpected antigravityProjects: %+v", projects)
	}
}

func TestAntigravityLiveReadOnlyDBSmoke(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skipping live local Antigravity DB check in CI")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("home directory unavailable")
	}
	geminiDir := antigravityGeminiDir(home)
	dbPath := antigravityDBPath(geminiDir, antigravityAppDirDesktop)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skip("live Antigravity conversation_summaries.db not present")
	}

	records, err := antigravityListRecords(context.Background(), geminiDir, nil)
	if err != nil {
		t.Fatalf("live read-only antigravityListRecords failed: %v", err)
	}
	if len(records) == 0 {
		t.Skip("live Antigravity DB has 0 rows")
	}

	// Verify organization and transcript parsing without logging any private text.
	reader := NewOrganizationReader(home)
	threads, groups := reader.Antigravity(false, nil, records)
	for _, th := range threads {
		if th.ID == "" || th.Provider != "antigravity" {
			t.Fatalf("invalid thread summary in live DB: id_present=%v provider=%q", th.ID != "", th.Provider)
		}
	}
	for _, g := range groups {
		if g.ID == "" || g.Name == "" {
			t.Fatalf("invalid group in live DB: id_present=%v name_present=%v", g.ID != "", g.Name != "")
		}
	}

	// Parse up to 3 existing live transcripts read-only to confirm schema compatibility.
	checked := 0
	for _, rec := range records {
		if rec.TranscriptPath == "" {
			continue
		}
		if _, err := os.Stat(rec.TranscriptPath); err != nil {
			continue
		}
		view, err := readAntigravityTranscriptForRecord(geminiDir, rec, 15)
		if err != nil {
			t.Fatalf("live read-only transcript parse failed: %v", err)
		}
		if view.Meta.Provider != "antigravity" {
			t.Fatalf("unexpected transcript provider: %q", view.Meta.Provider)
		}
		for _, turn := range view.Turns {
			for _, it := range turn.Items {
				if it.K == "user" && (strings.Contains(it.Text, "<USER_REQUEST>") || strings.Contains(it.Text, "<ADDITIONAL_METADATA>")) {
					t.Fatalf("live read-only transcript leaked raw Antigravity XML wrapper in user item")
				}
			}
		}
		checked++
		if checked >= 3 {
			break
		}
	}
}

func TestCleanAntigravityUserText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "full_wrapped_prompt_with_metadata_skill_and_settings",
			in: "<USER_REQUEST>\nFix the login bug in auth.go\n</USER_REQUEST>\n" +
				"<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-27T20:00:00Z\n" +
				"<SKILL>\nname: auth-helper\n</SKILL>\n</ADDITIONAL_METADATA>\n" +
				"<USER_SETTINGS_CHANGE>\nModel set to gemini-4-neon\n</USER_SETTINGS_CHANGE>",
			want: "Fix the login bug in auth.go",
		},
		{
			name: "case_insensitive_and_inline_consecutive_tags",
			in:   `<user_request id="1">Hello world</user_request><additional_metadata>local time</additional_metadata>`,
			want: "Hello world",
		},
		{
			name: "begin_end_chat_standalone_lines",
			in:   "BEGIN CHAT\nCheck the failing build\nEND CHAT\n<ADDITIONAL_METADATA>\nlocal time\n</ADDITIONAL_METADATA>",
			want: "Check the failing build",
		},
		{
			name: "begin_end_chat_decorated_and_xml_tags",
			in:   "--- BEGIN CHAT ---\n<begin_chat>\n<USER_REQUEST>\nDeploy release v0.2.19\n</USER_REQUEST>\n<end_chat>\n--- END CHAT ---",
			want: "Deploy release v0.2.19",
		},
		{
			name: "single_line_begin_end_chat_tags",
			in:   "<begin_chat>Quick question<end_chat>",
			want: "Quick question",
		},
		{
			name: "truncated_preview_missing_closing_tag",
			in:   "<USER_REQUEST>\nBuild a dashboard for metrics and telemetry",
			want: "Build a dashboard for metrics and telemetry",
		},
		{
			name: "truncated_preview_partial_closing_tag",
			in:   "<USER_REQUEST>\nBuild a dashboard</USER_REQU",
			want: "Build a dashboard",
		},
		{
			name: "truncated_preview_partial_metadata_tag_with_ellipsis",
			in:   "<USER_REQUEST>\nBuild a dashboard\n</USER_REQUEST>\n<ADDITIONAL_ME…",
			want: "Build a dashboard",
		},
		{
			name: "multiple_context_and_metadata_blocks",
			in: "<ATTACHED_FILES>\na.go\n</ATTACHED_FILES>\n" +
				"<CURRENT_EDITOR>\nmain.go\n</CURRENT_EDITOR>\n" +
				"<OPEN_FILES>\napp.js\n</OPEN_FILES>\n" +
				"<USER_CONTEXT>\nworkspace\n</USER_CONTEXT>\n" +
				"<CONTEXT>\nextra\n</CONTEXT>\n" +
				"<EPHEMERAL_MESSAGE>\nnote\n</EPHEMERAL_MESSAGE>\n" +
				"Actual prompt line",
			want: "Actual prompt line",
		},
		{
			name: "system_only_wakeup_with_preamble_returns_empty",
			in: "The following is a <SYSTEM_MESSAGE> not actually sent by the user. It is provided by the system as important information to pay attention to.\n\n" +
				"<SYSTEM_MESSAGE>\n[Message] background task finished\n</SYSTEM_MESSAGE>",
			want: "",
		},
		{
			name: "settings_and_metadata_only_returns_empty",
			in: "<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-27T20:00:00Z\n</ADDITIONAL_METADATA>\n" +
				"<USER_SETTINGS_CHANGE>\neffort=high\n</USER_SETTINGS_CHANGE>",
			want: "",
		},
		{
			name: "fenced_code_with_xml_unwrapped_preserved",
			in: "Here is an example:\n```xml\n<USER_REQUEST>demo</USER_REQUEST>\n<ADDITIONAL_METADATA>meta</ADDITIONAL_METADATA>\nBEGIN CHAT\nhello\nEND CHAT\n```",
			want: "Here is an example:\n```xml\n<USER_REQUEST>demo</USER_REQUEST>\n<ADDITIONAL_METADATA>meta</ADDITIONAL_METADATA>\nBEGIN CHAT\nhello\nEND CHAT\n```",
		},
		{
			name: "fenced_code_with_xml_inside_user_request_preserved",
			in: "<USER_REQUEST>\nStrip this:\n```xml\n<USER_REQUEST>demo</USER_REQUEST>\n<ADDITIONAL_METADATA>meta</ADDITIONAL_METADATA>\nBEGIN CHAT\nhello\nEND CHAT\n```\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nreal meta\n</ADDITIONAL_METADATA>",
			want: "Strip this:\n```xml\n<USER_REQUEST>demo</USER_REQUEST>\n<ADDITIONAL_METADATA>meta</ADDITIONAL_METADATA>\nBEGIN CHAT\nhello\nEND CHAT\n```",
		},
		{
			name: "inline_backticks_and_plain_html_preserved",
			in:   "<USER_REQUEST>\nStrip `<USER_REQUEST>` and `</USER_REQUEST>` from <div>markup</div>.\n</USER_REQUEST>\n<ADDITIONAL_METADATA>meta</ADDITIONAL_METADATA>",
			want: "Strip `<USER_REQUEST>` and `</USER_REQUEST>` from <div>markup</div>.",
		},
		{
			name: "plain_prose_mentioning_begin_and_end_chat_preserved",
			in:   "Let's begin chat testing and end chat logging with <span>tags</span>.",
			want: "Let's begin chat testing and end chat logging with <span>tags</span>.",
		},
		{
			name: "tilde_code_fence_preserved",
			in:   "~~~\n<USER_REQUEST>inside tilde</USER_REQUEST>\n~~~",
			want: "~~~\n<USER_REQUEST>inside tilde</USER_REQUEST>\n~~~",
		},
		{
			name: "long_multiline_metadata_block_followed_by_settings_and_trailing_spaces",
			in: "<USER_REQUEST>\nShort prompt\n</USER_REQUEST>\n" +
				"<ADDITIONAL_METADATA>\n" + strings.Repeat("line of workspace metadata\n", 40) + "</ADDITIONAL_METADATA>   \n" +
				"<USER_SETTINGS_CHANGE>\n" + strings.Repeat("setting=value\n", 10) + "</USER_SETTINGS_CHANGE>\n",
			want: "Short prompt",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanAntigravityUserText(tc.in); got != tc.want {
				t.Fatalf("cleanAntigravityUserText(%q)\ngot:  %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAntigravityTranscriptAndDBStripXMLEnvelopes(t *testing.T) {
	geminiDir := t.TempDir()
	convID := "conv-xml-clean"
	tPath := antigravityTranscriptPath(geminiDir, antigravityAppDirDesktop, convID)
	annPath := antigravityAnnotationPath(geminiDir, antigravityAppDirDesktop, convID)

	if err := os.MkdirAll(filepath.Dir(annPath), 0700); err != nil {
		t.Fatalf("mkdir annotations: %v", err)
	}
	if err := os.WriteFile(annPath, []byte("title: \"Clean Annotation Fallback\"\n"), 0600); err != nil {
		t.Fatalf("write annotation: %v", err)
	}

	wrappedTurn1 := "<USER_REQUEST>\nFix Antigravity XML tags\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-27T20:00:00Z\n<SKILL>\nname: helper\n</SKILL>\n</ADDITIONAL_METADATA>\n<USER_SETTINGS_CHANGE>\nmode=default\n</USER_SETTINGS_CHANGE>"
	systemOnlyTurn := "The following is a <SYSTEM_MESSAGE> not actually sent by the user. It is provided by the system as important information to pay attention to.\n\n<SYSTEM_MESSAGE>\n[Message] wakeup\n</SYSTEM_MESSAGE>"
	beginEndTurn := "BEGIN CHAT\nKeep fenced example:\n```xml\n<USER_REQUEST>literal</USER_REQUEST>\n```\nEND CHAT\n<ADDITIONAL_METADATA>\ntime\n</ADDITIONAL_METADATA>"

	line1, _ := json.Marshal(map[string]any{"step_index": 0, "type": "USER_INPUT", "status": "DONE", "source": "USER_EXPLICIT", "created_at": "2026-11-27T10:00:00Z", "content": wrappedTurn1})
	line2, _ := json.Marshal(map[string]any{"step_index": 1, "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": "2026-11-27T10:00:05Z", "content": "Working on it."})
	line3, _ := json.Marshal(map[string]any{"step_index": 2, "type": "USER_INPUT", "status": "DONE", "source": "SYSTEM", "created_at": "2026-11-27T10:01:00Z", "content": systemOnlyTurn})
	line4, _ := json.Marshal(map[string]any{"step_index": 3, "type": "USER_INPUT", "status": "DONE", "source": "USER_EXPLICIT", "created_at": "2026-11-27T10:02:00Z", "content": beginEndTurn})
	line5, _ := json.Marshal(map[string]any{"step_index": 4, "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": "2026-11-27T10:02:05Z", "content": "Done."})

	writeTestTranscript(t, tPath, []string{string(line1), string(line2), string(line3), string(line4), string(line5)}, time.Time{})

	// DB rows with wrapped title, truncated preview, and system-only title falling back to annotation.
	desktopDB := antigravityDBPath(geminiDir, antigravityAppDirDesktop)
	createAntigravityTestDB(t, desktopDB, []antigravityTestRow{
		{
			id:            convID,
			title:         "<SYSTEM_MESSAGE>\ninternal wakeup\n</SYSTEM_MESSAGE>",
			preview:       "<USER_REQUEST>\nFix Antigravity XML tags and truncated tail</USER_REQ",
			stepCount:     5,
			lastMod:       "2026-11-27 10:02:05+00:00",
			workspaceURIs: `["file:///Users/siraj/dev/fastcontrol"]`,
			status:        "CASCADE_RUN_STATUS_IDLE",
			appDataDir:    antigravityAppDirDesktop,
		},
		{
			id:            "conv-xml-title",
			title:         "<USER_REQUEST>\nWrapped DB Title\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\ntime\n</ADDITIONAL_METADATA>",
			preview:       "<begin_chat>\nWrapped preview text\n<end_chat>",
			stepCount:     2,
			lastMod:       "2026-11-27 10:01:00+00:00",
			workspaceURIs: `["file:///Users/siraj/dev/fastcontrol"]`,
			status:        "CASCADE_RUN_STATUS_IDLE",
			appDataDir:    antigravityAppDirDesktop,
		},
	})

	records, err := antigravityListRecords(context.Background(), geminiDir, nil)
	if err != nil {
		t.Fatalf("antigravityListRecords: %v", err)
	}
	byID := map[string]antigravityRecord{}
	for _, r := range records {
		byID[r.ID] = r
	}
	if byID[convID].Title != "Clean Annotation Fallback" {
		t.Fatalf("expected system-only DB title to fall back to annotation, got %q", byID[convID].Title)
	}
	if byID[convID].Preview != "Fix Antigravity XML tags and truncated tail" {
		t.Fatalf("expected cleaned truncated preview, got %q", byID[convID].Preview)
	}
	if byID["conv-xml-title"].Title != "Wrapped DB Title" || byID["conv-xml-title"].Preview != "Wrapped preview text" {
		t.Fatalf("expected cleaned DB title/preview, got title=%q preview=%q", byID["conv-xml-title"].Title, byID["conv-xml-title"].Preview)
	}

	// Verify ThreadInfo carries clean Title/Preview.
	reader := NewOrganizationReader(t.TempDir())
	threads, _ := reader.Antigravity(false, nil, records)
	for _, th := range threads {
		if strings.Contains(th.Title, "<USER_REQUEST>") || strings.Contains(th.Preview, "<USER_REQUEST>") || strings.Contains(th.Preview, "<begin_chat>") {
			t.Fatalf("ThreadInfo leaked XML wrapper: %+v", th)
		}
	}

	// Verify transcript parsing: 2 real turns (system-only wakeup skipped cleanly), clean First & Meta.Title.
	view, err := readAntigravityTranscriptWithOptions(tPath, antigravityTranscriptOptions{Limit: 10})
	if err != nil {
		t.Fatalf("readAntigravityTranscriptWithOptions: %v", err)
	}
	if view.First != "Fix Antigravity XML tags" {
		t.Fatalf("expected clean First, got %q", view.First)
	}
	if view.Meta.Title != "Fix Antigravity XML tags" {
		t.Fatalf("expected clean fallback Meta.Title, got %q", view.Meta.Title)
	}
	if len(view.Turns) != 2 {
		t.Fatalf("expected 2 turns (system-only wakeup skipped), got %d: %+v", len(view.Turns), view.Turns)
	}
	if view.Turns[0].Items[0].K != "user" || view.Turns[0].Items[0].Text != "Fix Antigravity XML tags" {
		t.Fatalf("unexpected turn 0 user item: %+v", view.Turns[0].Items[0])
	}
	wantTurn1User := "Keep fenced example:\n```xml\n<USER_REQUEST>literal</USER_REQUEST>\n```"
	if view.Turns[1].Items[0].K != "user" || view.Turns[1].Items[0].Text != wantTurn1User {
		t.Fatalf("unexpected turn 1 user item:\ngot:  %q\nwant: %q", view.Turns[1].Items[0].Text, wantTurn1User)
	}
}
