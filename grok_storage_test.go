package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func withGrokTestSeams(
	t *testing.T,
	now time.Time,
	alive func(int) bool,
	probe func([]string) map[string]rolloutOwnerEvidence,
) {
	t.Helper()
	prevNow := grokNow
	prevAlive := grokProcessAlive
	prevOwners := grokOwners

	if !now.IsZero() {
		grokNow = func() time.Time { return now }
	}
	if alive != nil {
		grokProcessAlive = alive
	}
	if probe != nil {
		grokOwners = &rolloutOwnerCache{
			entries: map[string]rolloutOwnerEvidence{},
			pending: map[string]os.FileInfo{},
			probe:   probe,
		}
	}
	t.Cleanup(func() {
		grokNow = prevNow
		grokProcessAlive = prevAlive
		grokOwners = prevOwners
	})
}

func writeGrokFile(t *testing.T, path string, data []byte, modTime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if !modTime.IsZero() {
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
}

func writeGrokJSON(t *testing.T, path string, v any, modTime time.Time) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	writeGrokFile(t, path, append(b, '\n'), modTime)
}

func writeGrokJSONL(t *testing.T, path string, lines []any, modTime time.Time) {
	t.Helper()
	var buf bytes.Buffer
	for _, line := range lines {
		switch x := line.(type) {
		case string:
			buf.WriteString(x)
			buf.WriteByte('\n')
		default:
			b, err := json.Marshal(x)
			if err != nil {
				t.Fatalf("marshal jsonl line: %v", err)
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
	}
	writeGrokFile(t, path, buf.Bytes(), modTime)
}

func makeTinyPNGBase64(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestGrokHomeAuthAndSessionsDetection(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)
	t.Setenv("GROK_HOME", "")

	// Default fallback to <home>/.grok.
	if got, want := grokHomeDir(""), filepath.Join(fakeHome, ".grok"); got != want {
		t.Fatalf("grokHomeDir(\"\") = %q, want %q", got, want)
	}
	if got, want := grokHomeDir(filepath.Join(fakeHome, "alt")), filepath.Join(fakeHome, "alt", ".grok"); got != want {
		t.Fatalf("grokHomeDir(alt) = %q, want %q", got, want)
	}

	// GROK_HOME override and tilde expansion.
	customGrok := filepath.Join(fakeHome, "custom-grok")
	t.Setenv("GROK_HOME", customGrok)
	if got := grokHomeDir(""); got != filepath.Clean(customGrok) {
		t.Fatalf("grokHomeDir with GROK_HOME = %q, want %q", got, customGrok)
	}
	t.Setenv("GROK_HOME", "~/my-grok")
	if got, want := grokHomeDir(""), filepath.Join(fakeHome, "my-grok"); got != want {
		t.Fatalf("grokHomeDir with ~/ GROK_HOME = %q, want %q", got, want)
	}

	// grokHasAuth: missing, empty, directory, and valid non-empty file.
	grokHome := t.TempDir()
	if grokHasAuth(grokHome) {
		t.Fatalf("expected grokHasAuth=false when auth.json is missing")
	}
	authPath := filepath.Join(grokHome, "auth.json")
	writeGrokFile(t, authPath, nil, time.Time{})
	if grokHasAuth(grokHome) {
		t.Fatalf("expected grokHasAuth=false for empty auth.json")
	}
	if err := os.Remove(authPath); err != nil {
		t.Fatalf("remove empty auth: %v", err)
	}
	if err := os.Mkdir(authPath, 0700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	if grokHasAuth(grokHome) {
		t.Fatalf("expected grokHasAuth=false when auth.json is a directory")
	}
	if err := os.Remove(authPath); err != nil {
		t.Fatalf("remove auth dir: %v", err)
	}
	writeGrokFile(t, authPath, []byte(`{"marker":"ok"}`), time.Time{})
	if !grokHasAuth(grokHome) {
		t.Fatalf("expected grokHasAuth=true for non-empty regular auth.json")
	}

	// grokHasSessions: missing, empty cwd dir, and populated session dir.
	if grokHasSessions(grokHome) {
		t.Fatalf("expected grokHasSessions=false when sessions/ is absent")
	}
	cwdGroup := filepath.Join(grokHome, "sessions", url.PathEscape("/repo/demo"))
	if err := os.MkdirAll(cwdGroup, 0700); err != nil {
		t.Fatalf("mkdir cwd group: %v", err)
	}
	if grokHasSessions(grokHome) {
		t.Fatalf("expected grokHasSessions=false when cwd group has no session subdirectories")
	}
	if err := os.MkdirAll(filepath.Join(cwdGroup, "019a-session-1"), 0700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	if !grokHasSessions(grokHome) {
		t.Fatalf("expected grokHasSessions=true once a session dir exists")
	}
}

func TestGrokLoadRecordsCwdFallbacksSignalsActiveAndStaleGuard(t *testing.T) {
	grokHome := t.TempDir()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)

	withGrokTestSeams(
		t,
		now,
		func(pid int) bool {
			return pid == 4201 || pid == 4202
		},
		func(paths []string) map[string]rolloutOwnerEvidence {
			res := make(map[string]rolloutOwnerEvidence, len(paths))
			for _, p := range paths {
				alive := strings.Contains(p, "sess-owner-alive")
				res[p] = rolloutOwnerEvidence{known: true, alive: alive}
			}
			return res
		},
	)

	// 1. URL-encoded cwd fallback (empty info.cwd, preserves spaces and '+').
	encodedPath := "/Users/siraj/My Repo/fast+control"
	dirEncoded := filepath.Join(grokHome, "sessions", url.PathEscape(encodedPath), "sess-encoded")
	writeGrokJSON(t, filepath.Join(dirEncoded, "summary.json"), map[string]any{
		"info":              map[string]any{"id": "sess-encoded", "cwd": ""},
		"generated_title":   "URL Encoded Workspace",
		"session_summary":   "Fallback summary",
		"title_is_manual":   false,
		"created_at":        "2026-11-27T14:00:00Z",
		"updated_at":        "2026-11-27T14:55:00Z",
		"last_active_at":    "2026-11-27T14:56:00Z",
		"current_model_id":  "grok-4.7",
		"reasoning_effort":  "high",
		"num_messages":      12,
		"num_chat_messages": 4,
		"head_branch":       "work/grok-storage",
		"last_turn_summary": "Indexed Grok sessions",
		"last_recap":        "Recap text",
	}, now.Add(-4*time.Minute))
	writeGrokJSON(t, filepath.Join(dirEncoded, "signals.json"), map[string]any{
		"contextTokensUsed":   48120,
		"contextWindowTokens": 256000,
	}, now.Add(-4*time.Minute))
	// External idle TUI (live PID 4201, turn_started paired with turn_ended).
	writeGrokJSONL(t, filepath.Join(dirEncoded, "events.jsonl"), []any{
		map[string]any{"type": "turn_started", "timestamp": "2026-11-27T14:54:00Z"},
		map[string]any{"type": "turn_ended", "timestamp": "2026-11-27T14:56:00Z"},
	}, now.Add(-4*time.Minute))

	// 2. .cwd file fallback (long slug+hash directory) + live external busy TUI (PID 4202).
	longCwdGroup := filepath.Join(grokHome, "sessions", "very-long-workspace-slug-a1b2c3d4")
	longCwdPath := "/Users/siraj/work/very/deep/nested/monorepo/service-alpha"
	writeGrokFile(t, filepath.Join(longCwdGroup, ".cwd"), []byte(longCwdPath+"\n"), now.Add(-10*time.Minute))
	dirExternalBusy := filepath.Join(longCwdGroup, "sess-ext-busy")
	writeGrokJSON(t, filepath.Join(dirExternalBusy, "summary.json"), map[string]any{
		"info":             map[string]any{"id": "sess-ext-busy", "cwd": ""},
		"generated_title":  "Live TUI Turn",
		"updated_at":       now.Add(-5 * time.Minute).Unix(),
		"current_model_id": "grok-4.7-build-fast",
	}, now.Add(-5*time.Minute))
	writeGrokJSONL(t, filepath.Join(dirExternalBusy, "events.jsonl"), []any{
		map[string]any{"type": "turn_started", "timestamp": "2026-11-27T14:55:00Z"},
	}, now.Add(-5*time.Minute))

	// 3. Dead PID + old unpaired turn_started + no open fd -> must clear stale running ghost!
	dirStale := filepath.Join(grokHome, "sessions", url.PathEscape("/repo/stale"), "sess-stale")
	writeGrokJSON(t, filepath.Join(dirStale, "summary.json"), map[string]any{
		"info":            map[string]any{"id": "sess-stale", "cwd": "/repo/stale"},
		"generated_title": "Crashed CLI Run",
		"updated_at":      "2026-11-27T14:20:00Z",
	}, now.Add(-40*time.Minute))
	writeGrokJSONL(t, filepath.Join(dirStale, "updates.jsonl"), []any{
		map[string]any{"timestamp": "2026-11-27T14:20:00Z", "method": "session/update", "params": map[string]any{"sessionId": "sess-stale", "update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "hello"}}}},
	}, now.Add(-40*time.Minute))
	writeGrokJSONL(t, filepath.Join(dirStale, "events.jsonl"), []any{
		map[string]any{"type": "turn_started", "timestamp": "2026-11-27T14:20:00Z"},
	}, now.Add(-40*time.Minute))

	// 4. Headless fresh unpaired turn_started (within 1 min grace) -> Running=true, External=false.
	dirFresh := filepath.Join(grokHome, "sessions", url.PathEscape("/repo/headless"), "sess-fresh-headless")
	writeGrokJSON(t, filepath.Join(dirFresh, "summary.json"), map[string]any{
		"info":            map[string]any{"id": "sess-fresh-headless", "cwd": "/repo/headless"},
		"generated_title": "Fresh Headless Job",
		"session_kind":    "headless",
		"updated_at":      "2026-11-27T14:59:30Z",
	}, now.Add(-30*time.Second))
	writeGrokJSONL(t, filepath.Join(dirFresh, "events.jsonl"), []any{
		map[string]any{"event": "turn_started", "timestamp": "2026-11-27T14:59:30Z"},
	}, now.Add(-30*time.Second))

	// 5. Stale mtime unpaired turn_started, but rolloutOwners reports live fd -> Running=true.
	dirOwnerAlive := filepath.Join(grokHome, "sessions", url.PathEscape("/repo/long"), "sess-owner-alive")
	writeGrokJSON(t, filepath.Join(dirOwnerAlive, "summary.json"), map[string]any{
		"info":            map[string]any{"id": "sess-owner-alive", "cwd": "/repo/long"},
		"generated_title": "Long Running Command",
		"updated_at":      "2026-11-27T14:40:00Z",
	}, now.Add(-20*time.Minute))
	writeGrokJSONL(t, filepath.Join(dirOwnerAlive, "updates.jsonl"), []any{
		map[string]any{"timestamp": "2026-11-27T14:40:00Z", "method": "session/update", "params": map[string]any{"sessionId": "sess-owner-alive", "update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "build"}}}},
	}, now.Add(-20*time.Minute))
	writeGrokJSONL(t, filepath.Join(dirOwnerAlive, "events.jsonl"), []any{
		map[string]any{"turn_started": map[string]any{"timestamp": "2026-11-27T14:40:00Z"}},
	}, now.Add(-20*time.Minute))

	// 6. Managed liveCheck override on idle session.
	dirLive := filepath.Join(grokHome, "sessions", url.PathEscape("/repo/managed"), "sess-managed-live")
	writeGrokJSON(t, filepath.Join(dirLive, "summary.json"), map[string]any{
		"info":            map[string]any{"id": "sess-managed-live", "cwd": "/repo/managed"},
		"generated_title": "Managed Live Run",
		"updated_at":      "2026-11-27T14:30:00Z",
	}, now.Add(-30*time.Minute))

	// active_sessions.json with two alive PIDs (4201, 4202) and one dead PID (99999).
	writeGrokJSON(t, filepath.Join(grokHome, "active_sessions.json"), []map[string]any{
		{"session_id": "sess-encoded", "pid": 4201, "pid_start": 1001, "cwd": encodedPath, "opened_at": "2026-11-27T14:50:00Z"},
		{"session_id": "sess-ext-busy", "pid": 4202, "pid_start": 1002, "cwd": longCwdPath, "opened_at": "2026-11-27T14:55:00Z"},
		{"session_id": "sess-stale", "pid": 99999, "pid_start": 1003, "cwd": "/repo/stale", "opened_at": "2026-11-27T14:15:00Z"},
	}, now)

	records, err := grokLoadRecords(grokHome, func(id string) bool { return id == "sess-managed-live" })
	if err != nil {
		t.Fatalf("grokLoadRecords error: %v", err)
	}
	byID := map[string]grokRecord{}
	for _, rec := range records {
		byID[rec.ID] = rec
	}
	if len(byID) != 6 {
		t.Fatalf("expected 6 records, got %d", len(byID))
	}

	enc := byID["sess-encoded"]
	if enc.Cwd != filepath.Clean(encodedPath) {
		t.Fatalf("URL-decoded Cwd = %q, want %q", enc.Cwd, filepath.Clean(encodedPath))
	}
	if enc.CtxUsed != 48120 || enc.CtxMax != 256000 {
		t.Fatalf("unexpected signals metrics: used=%d max=%d", enc.CtxUsed, enc.CtxMax)
	}
	if !enc.External || enc.ExternalPID != 4201 || enc.Running {
		t.Fatalf("expected External=true PID=4201 Running=false for idle TUI, got External=%v PID=%d Running=%v", enc.External, enc.ExternalPID, enc.Running)
	}
	if enc.Model != "grok-4.7" || enc.ReasoningEffort != "high" || enc.HeadBranch != "work/grok-storage" || enc.NumMessages != 12 || enc.NumChatMessages != 4 {
		t.Fatalf("unexpected summary fields: %+v", enc)
	}

	extBusy := byID["sess-ext-busy"]
	if extBusy.Cwd != filepath.Clean(longCwdPath) {
		t.Fatalf(".cwd fallback Cwd = %q, want %q", extBusy.Cwd, filepath.Clean(longCwdPath))
	}
	if !extBusy.External || !extBusy.Running {
		t.Fatalf("expected External=true Running=true for busy TUI, got External=%v Running=%v", extBusy.External, extBusy.Running)
	}

	stale := byID["sess-stale"]
	if stale.External || stale.Running {
		t.Fatalf("expected dead PID + stale events to clear External/Running, got External=%v Running=%v", stale.External, stale.Running)
	}

	fresh := byID["sess-fresh-headless"]
	if fresh.External || !fresh.Running || fresh.SessionKind != "headless" {
		t.Fatalf("expected fresh headless turn to be Running=true External=false, got %+v", fresh)
	}

	ownerAlive := byID["sess-owner-alive"]
	if ownerAlive.External || !ownerAlive.Running {
		t.Fatalf("expected live-fd owner to keep Running=true, got External=%v Running=%v", ownerAlive.External, ownerAlive.Running)
	}

	live := byID["sess-managed-live"]
	if !live.Running {
		t.Fatalf("expected liveCheck override to force Running=true")
	}
}

func TestGrokReadModelsCacheAndConfigTOML(t *testing.T) {
	grokHome := t.TempDir()

	// Graceful empty result when neither file exists (e.g. unsigned host).
	emptyModels, emptyDef, err := grokReadModelsCache(grokHome)
	if err != nil {
		t.Fatalf("missing cache should not fail: %v", err)
	}
	if len(emptyModels) != 0 || emptyDef.PermissionMode != "default" {
		t.Fatalf("unexpected empty cache result: models=%v def=%+v", emptyModels, emptyDef)
	}

	// Write realistic config.toml with comments, unrelated tables, and quoted strings.
	cfgTOML := `
# Global Grok CLI config
[cli]
installer = "internal"

[models]
default = "grok-4.7" # preferred coding model
default_reasoning_effort = 'xhigh'

[[subagents.personas.reviewer.inputs]]
name = "default"

[ui]
# Quoted hash inside string must not truncate value
theme = "dark #1"
permission_mode = "auto" # auto-review mode
`
	writeGrokFile(t, filepath.Join(grokHome, "config.toml"), []byte(cfgTOML), time.Time{})

	writeGrokJSON(t, filepath.Join(grokHome, "models_cache.json"), map[string]any{
		"models": []map[string]any{
			{
				"id":                          "grok-4.7",
				"display_name":                "Grok 4.7",
				"description":                 "Flagship coding model",
				"context_window":              500000,
				"default_reasoning_effort":    "high",
				"supported_reasoning_efforts": []string{"low", "medium", "high", "xhigh"},
			},
			{
				"id":                        "grok-4.7-build-fast",
				"displayName":               "Grok 4.7 Fast",
				"contextWindow":             256000,
				"defaultReasoningEffort":    "high",
				"supportedReasoningEfforts": []map[string]any{{"value": "low"}, {"value": "medium"}, {"value": "high"}, {"value": "xhigh"}},
			},
			{
				"id":       "grok-disabled",
				"name":     "Disabled Model",
				"disabled": true,
			},
			{
				"id":                          "grok-4.5",
				"display_name":                "Grok 4.5",
				"context_window":              256000,
				"supported_reasoning_efforts": []string{"low", "medium", "high"},
			},
		},
	}, time.Time{})

	models, defaults, err := grokReadModelsCache(grokHome)
	if err != nil {
		t.Fatalf("grokReadModelsCache error: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("expected 3 enabled models, got %d: %+v", len(models), models)
	}
	if models[0].Slug != "grok-4.7" || models[0].Name != "Grok 4.7" || models[0].DefaultEffort != "high" || len(models[0].Efforts) != 4 {
		t.Fatalf("unexpected model[0]: %+v", models[0])
	}
	if models[1].Slug != "grok-4.7-build-fast" || len(models[1].Efforts) != 4 {
		t.Fatalf("unexpected model[1]: %+v", models[1])
	}
	if models[2].Slug != "grok-4.5" || models[2].DefaultEffort != "high" {
		t.Fatalf("unexpected model[2]: %+v", models[2])
	}
	if defaults.Model != "grok-4.7" || defaults.Effort != "xhigh" || defaults.PermissionMode != "auto" {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}
	if defaults.ContextWindows["grok-4.7"] != 500000 || defaults.ContextWindows["grok-4.7-build-fast"] != 256000 {
		t.Fatalf("unexpected context windows: %+v", defaults.ContextWindows)
	}

	// Verify permission mode normalization ("always-approve" -> "bypassPermissions", "ask" -> "default").
	cfgNorm := parseGrokConfigTOML("[ui]\npermission_mode = \"always-approve\"\n")
	if cfgNorm.PermissionMode != "bypassPermissions" {
		t.Fatalf("expected always-approve -> bypassPermissions, got %q", cfgNorm.PermissionMode)
	}
	cfgAsk := parseGrokConfigTOML("ui.approval_mode = \"ask\"\n")
	if cfgAsk.PermissionMode != "default" {
		t.Fatalf("expected ask -> default, got %q", cfgAsk.PermissionMode)
	}

	// Also verify ACP _meta-nested availableModels shape observed live on m5p (#118).
	acpModels, acpCtx, err := parseGrokModelsCacheJSON([]byte(`{
		"availableModels": [
			{
				"modelId": "grok-4.7",
				"name": "Grok 4.7",
				"_meta": {
					"totalContextTokens": 500000,
					"reasoningEfforts": [
						{"id": "low", "value": "low"},
						{"id": "high", "value": "high", "default": true},
						{"id": "xhigh", "value": "xhigh"}
					]
				}
			}
		]
	}`))
	if err != nil {
		t.Fatalf("parseGrokModelsCacheJSON _meta format: %v", err)
	}
	if len(acpModels) != 1 || acpModels[0].DefaultEffort != "high" || len(acpModels[0].Efforts) != 3 || acpCtx["grok-4.7"] != 500000 {
		t.Fatalf("unexpected _meta parsed models: %+v ctx=%+v", acpModels, acpCtx)
	}
}

func TestSortGrokEffortsAndModelsCacheAscendingOrder(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "descending live grok order",
			in:   []string{"xhigh", "high", "medium", "low"},
			want: []string{"low", "medium", "high", "xhigh"},
		},
		{
			name: "mixed case duplicates and minimal through max",
			in:   []string{" XHIGH ", "high", "LOW", "minimal", "medium", "high", "max", "low", ""},
			want: []string{"minimal", "low", "medium", "high", "xhigh", "max"},
		},
		{
			name: "unknown future labels preserved after known ones",
			in:   []string{"ultra", "xhigh", "turbo", "low", "max", "minimal", "ULTRA"},
			want: []string{"minimal", "low", "xhigh", "max", "ultra", "turbo"},
		},
		{
			name: "empty and whitespace only",
			in:   []string{"", "   "},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sortGrokEfforts(tc.in)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") || len(got) != len(tc.want) {
				t.Fatalf("sortGrokEfforts(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}

	// Regression: live m5p ~/.grok/models_cache.json stores models as an object map
	// with nested info and descending reasoning_efforts (xhigh -> high -> medium -> low).
	grokHome := t.TempDir()
	liveCacheJSON := `{
		"fetched_at": 1764200000,
		"grok_version": "1.0.92",
		"models": {
			"grok-4.7": {
				"info": {
					"id": "grok-4.7",
					"model": "grok-4.7",
					"name": "Grok 4.7",
					"description": "Frontier model",
					"context_window": 256000,
					"hidden": false,
					"reasoning_effort": "high",
					"supports_reasoning_effort": true,
					"reasoning_efforts": [
						{"id": "xhigh", "value": "xhigh", "label": "Extra High", "default": false},
						{"id": "high", "value": "high", "label": "High", "default": true},
						{"id": "medium", "value": "medium", "label": "Medium", "default": false},
						{"id": "low", "value": "low", "label": "Low", "default": false}
					]
				}
			},
			"grok-hidden": {
				"info": {
					"id": "grok-hidden",
					"name": "Hidden Model",
					"hidden": true,
					"reasoning_efforts": [{"value": "xhigh"}, {"value": "low"}]
				}
			},
			"grok-4.5": {
				"info": {
					"id": "grok-4.5",
					"name": "Grok 4.5",
					"context_window": 256000,
					"hidden": false,
					"reasoning_effort": "high",
					"reasoning_efforts": [
						{"id": "high", "value": "high", "default": true},
						{"id": "medium", "value": "medium", "default": false},
						{"id": "low", "value": "low", "default": false}
					]
				}
			}
		}
	}`
	writeGrokFile(t, filepath.Join(grokHome, "models_cache.json"), []byte(liveCacheJSON), time.Time{})

	models, defaults, err := grokReadModelsCache(grokHome)
	if err != nil {
		t.Fatalf("grokReadModelsCache live map format error: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 visible models, got %d: %+v", len(models), models)
	}
	if models[0].Slug != "grok-4.7" || models[0].Name != "Grok 4.7" || models[0].DefaultEffort != "high" {
		t.Fatalf("unexpected models[0]: %+v", models[0])
	}
	want47 := []string{"low", "medium", "high", "xhigh"}
	if strings.Join(models[0].Efforts, ",") != strings.Join(want47, ",") {
		t.Fatalf("grok-4.7 efforts = %v, want %v", models[0].Efforts, want47)
	}
	if models[1].Slug != "grok-4.5" || models[1].DefaultEffort != "high" {
		t.Fatalf("unexpected models[1]: %+v", models[1])
	}
	want45 := []string{"low", "medium", "high"}
	if strings.Join(models[1].Efforts, ",") != strings.Join(want45, ",") {
		t.Fatalf("grok-4.5 efforts = %v, want %v", models[1].Efforts, want45)
	}
	if defaults.ContextWindows["grok-4.7"] != 256000 || defaults.ContextWindows["grok-4.5"] != 256000 {
		t.Fatalf("unexpected context windows: %+v", defaults.ContextWindows)
	}

	// Regression: array-shaped cache with mixed, duplicate, and minimal efforts.
	arrModels, _, err := parseGrokModelsCacheJSON([]byte(`{
		"models": [
			{
				"id": "grok-mixed",
				"display_name": "Grok Mixed",
				"supported_reasoning_efforts": ["XHIGH", "high", "medium", "low", "minimal", "high", " LOW "]
			}
		]
	}`))
	if err != nil {
		t.Fatalf("parseGrokModelsCacheJSON mixed array error: %v", err)
	}
	wantMixed := []string{"minimal", "low", "medium", "high", "xhigh"}
	if len(arrModels) != 1 || strings.Join(arrModels[0].Efforts, ",") != strings.Join(wantMixed, ",") {
		t.Fatalf("mixed array efforts = %+v, want %v", arrModels, wantMixed)
	}
}

func TestGrokTranscriptProjectionDiffsImagesAndPagination(t *testing.T) {
	// Isolate HOME/USERPROFILE so cacheImage writes into t.TempDir().
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	sessionDir := filepath.Join(t.TempDir(), "sess-full")
	validPNGB64 := makeTinyPNGBase64(t)
	dummyHugeB64 := strings.Repeat("QUFB", 400) // Invalid image header -> placeholder, never dumped in text.

	writeGrokJSON(t, filepath.Join(sessionDir, "summary.json"), map[string]any{
		"info":             map[string]any{"id": "sess-full", "cwd": "/repo/app"},
		"generated_title":  "Auto Title",
		"title_is_manual":  false,
		"current_model_id": "grok-4.6",
		"reasoning_effort": "medium",
		"head_branch":      "main",
	}, time.Time{})
	writeGrokJSON(t, filepath.Join(sessionDir, "signals.json"), map[string]any{
		"contextTokensUsed":   32000,
		"contextWindowTokens": 256000,
	}, time.Time{})

	makeLine := func(ts, method string, update map[string]any) map[string]any {
		return map[string]any{
			"timestamp": ts,
			"method":    method,
			"params": map[string]any{
				"sessionId": "sess-full",
				"update":    update,
			},
		}
	}

	lines := []any{
		// Session metadata & commands.
		makeLine("2026-11-27T10:00:00Z", "session/update", map[string]any{
			"sessionUpdate": "available_commands_update",
			"availableCommands": []map[string]any{
				{"name": "/compact", "description": "Compact conversation"},
				{"name": "rewind", "description": "Rewind turn", "input": map[string]any{"hint": "<turn>"}},
			},
		}),
		makeLine("2026-11-27T10:00:01Z", "session/update", map[string]any{
			"sessionUpdate": "current_mode_update",
			"currentModeId": "auto",
		}),
		makeLine("2026-11-27T10:00:02Z", "session/update", map[string]any{
			"sessionUpdate": "config_option_update",
			"configOptions": []map[string]any{
				{"id": "model", "currentValue": "grok-4.7"},
				{"id": "reasoning_effort", "currentValue": "high"},
			},
		}),
		makeLine("2026-11-27T10:00:03Z", "session/update", map[string]any{
			"sessionUpdate": "session_info_update",
			"title":         "Live Session Title",
		}),

		// Turn 1: user message with text + valid PNG image + invalid image fallback.
		makeLine("2026-11-27T10:01:00Z", "session/update", map[string]any{
			"sessionUpdate": "user_message_chunk",
			"content":       map[string]any{"type": "text", "text": "Inspect this screenshot"},
		}),
		makeLine("2026-11-27T10:01:00Z", "session/update", map[string]any{
			"sessionUpdate": "user_message_chunk",
			"content":       map[string]any{"type": "image", "mimeType": "image/png", "data": validPNGB64},
		}),
		makeLine("2026-11-27T10:01:01Z", "session/update", map[string]any{
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"type": "text", "text": "Checking layout "},
		}),
		makeLine("2026-11-27T10:01:02Z", "session/update", map[string]any{
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"type": "text", "text": "and spacing."},
		}),
		makeLine("2026-11-27T10:01:03Z", "session/update", map[string]any{
			"sessionUpdate": "plan",
			"entries": []map[string]any{
				{"content": "Inspect code", "status": "completed"},
				{"content": "Apply fix", "status": "in_progress"},
			},
		}),
		makeLine("2026-11-27T10:01:04Z", "session/update", map[string]any{
			"sessionUpdate": "plan",
			"entries": []map[string]any{
				{"content": "Inspect code", "status": "completed"},
				{"content": "Apply fix", "status": "completed"},
			},
		}),
		makeLine("2026-11-27T10:01:05Z", "session/update", map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "Looks good."},
		}),
		makeLine("2026-11-27T10:01:06Z", "_x.ai/session/update", map[string]any{
			"sessionUpdate": "turn_completed",
			"stop_reason":   "end_turn",
			"elapsed_ms":    6000,
		}),

		// Turn 2: image-only user message with invalid base64 (must not dump base64 into text).
		makeLine("2026-11-27T10:02:00Z", "session/update", map[string]any{
			"sessionUpdate": "user_message_chunk",
			"content":       map[string]any{"type": "image", "mimeType": "image/png", "data": dummyHugeB64},
		}),
		makeLine("2026-11-27T10:02:01Z", "session/update", map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "tc-edit-old",
			"title":         "Edit old.go",
			"kind":          "edit",
			"status":        "completed",
			"content": []map[string]any{
				{"type": "diff", "path": "old.go", "oldText": "a\n", "newText": "b\n"},
			},
		}),
		makeLine("2026-11-27T10:02:02Z", "_x.ai/session/update", map[string]any{
			"sessionUpdate": "turn_completed",
			"stop_reason":   "end_turn",
		}),

		// Turn 3: edit with diff, execute command, search, retry_state, session_recap, usage_update.
		makeLine("2026-11-27T10:03:00Z", "session/update", map[string]any{
			"sessionUpdate": "user_message_chunk",
			"content":       map[string]any{"type": "text", "text": "Patch main.go and run tests"},
		}),
		makeLine("2026-11-27T10:03:01Z", "session/update", map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "tc-edit-1",
			"title":         "Edit main.go",
			"kind":          "edit",
			"status":        "in_progress",
			"locations":     []map[string]any{{"path": "main.go", "line": 10}},
			"rawInput":      map[string]any{"path": "main.go"},
		}),
		makeLine("2026-11-27T10:03:02Z", "session/update", map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    "tc-edit-1",
			"status":        "completed",
			"content": []map[string]any{
				{"type": "content", "content": map[string]any{"type": "text", "text": "Updated main.go"}},
				{"type": "diff", "path": "main.go", "oldText": "line1\noldLine\nline3\n", "newText": "line1\nnewLine\nline3\n"},
			},
		}),
		// Follow-up status-only update must preserve the diff.
		makeLine("2026-11-27T10:03:03Z", "session/update", map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    "tc-edit-1",
			"status":        "completed",
		}),
		makeLine("2026-11-27T10:03:04Z", "session/update", map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "tc-cmd-1",
			"title":         "go test ./...",
			"kind":          "execute",
			"status":        "in_progress",
			"rawInput":      map[string]any{"command": "go test ./..."},
		}),
		makeLine("2026-11-27T10:03:05Z", "session/update", map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    "tc-cmd-1",
			"status":        "failed",
			"rawOutput":     map[string]any{"exit_code": 2, "output": "FAIL package main"},
		}),
		makeLine("2026-11-27T10:03:06Z", "session/update", map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "tc-spawn-1",
			"title":         "Verify tests",
			"kind":          "other",
			"status":        "completed",
			"_meta":         map[string]any{"x.ai/tool": "spawn_subagent"},
			"rawInput":      map[string]any{"description": "Run race detector", "subagent_type": "explore"},
			"rawOutput":     map[string]any{"child_session_id": "sub-child-1"},
		}),
		makeLine("2026-11-27T10:03:07Z", "_x.ai/session/update", map[string]any{
			"sessionUpdate": "retry_state",
			"retrying":      true,
			"attempt":       2,
			"max_attempts":  4,
			"reason":        "rate limit",
		}),
		makeLine("2026-11-27T10:03:08Z", "_x.ai/session/update", map[string]any{
			"sessionUpdate": "session_recap",
			"recap":         "Patched main.go and observed exit 2 on tests.",
		}),
		makeLine("2026-11-27T10:03:09Z", "session/update", map[string]any{
			"sessionUpdate": "usage_update",
			"used":          64000,
			"size":          500000,
		}),
		makeLine("2026-11-27T10:03:10Z", "session/update", map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "Patched "},
		}),
		makeLine("2026-11-27T10:03:11Z", "session/update", map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "main.go."},
		}),
		makeLine("2026-11-27T10:03:12Z", "_x.ai/session/update", map[string]any{
			"sessionUpdate": "turn_completed",
			"stop_reason":   "end_turn",
			"elapsed_ms":    12000,
		}),
	}
	writeGrokJSONL(t, filepath.Join(sessionDir, "updates.jsonl"), lines, time.Time{})

	// Read full transcript (all 3 turns).
	full, err := readGrokTranscript(sessionDir, 15)
	if err != nil {
		t.Fatalf("readGrokTranscript full error: %v", err)
	}
	if full.More || full.Meta.HasOlder {
		t.Fatalf("expected More=false HasOlder=false when all turns fit")
	}
	if full.Meta.Provider != "grok" || full.Meta.Title != "Live Session Title" || full.Meta.Model != "grok-4.7" || full.Meta.Effort != "high" || full.Meta.PermissionMode != "auto" || full.Meta.Branch != "main" {
		t.Fatalf("unexpected full Meta: %+v", full.Meta)
	}
	if full.Meta.CtxUsed != 64000 || full.Meta.CtxMax != 500000 {
		t.Fatalf("expected usage_update CtxUsed=64000 CtxMax=500000, got used=%d max=%d", full.Meta.CtxUsed, full.Meta.CtxMax)
	}
	if len(full.Meta.Commands) != 2 || full.Meta.Commands[0].Name != "compact" || full.Meta.Commands[1].InputHint != "<turn>" {
		t.Fatalf("unexpected commands: %+v", full.Meta.Commands)
	}
	if len(full.Turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(full.Turns))
	}

	// Turn 1 assertions: cached PNG image, merged thought, single in-place plan item, duration.
	t1 := full.Turns[0]
	if t1.DurationMs != 6000 || t1.Status != "completed" {
		t.Fatalf("unexpected turn 1 status/duration: %+v", t1)
	}
	var sawUserImg, sawThought, sawPlan, sawAssistant1 bool
	planCount := 0
	for _, it := range t1.Items {
		switch it.K {
		case "user":
			if it.Text == "Inspect this screenshot" && len(it.Images) == 1 && !strings.Contains(it.Text, validPNGB64) {
				sawUserImg = true
			}
		case "reasoning":
			if it.Text == "Checking layout and spacing." {
				sawThought = true
			}
		case "info":
			if it.Title == "Plan" {
				planCount++
				if strings.Contains(it.Text, "Plan (2/2)") && strings.Contains(it.Text, "[x] Apply fix") {
					sawPlan = true
				}
			}
		case "assistant":
			if it.Text == "Looks good." {
				sawAssistant1 = true
			}
		}
	}
	if !sawUserImg || !sawThought || !sawPlan || planCount != 1 || !sawAssistant1 {
		t.Fatalf("turn 1 check failed: img=%v thought=%v plan=%v(count=%d) assistant=%v items=%+v", sawUserImg, sawThought, sawPlan, planCount, sawAssistant1, t1.Items)
	}

	// Turn 2 assertion: uncacheable base64 image must produce "[Image]" placeholder and NEVER dump base64.
	t2 := full.Turns[1]
	if len(t2.Items) == 0 || t2.Items[0].K != "user" || t2.Items[0].Text != "[Image]" || strings.Contains(t2.Items[0].Text, "QUFB") {
		t.Fatalf("turn 2 image placeholder check failed: %+v", t2.Items)
	}

	// Turn 3 assertions: diff in Diffs[toolCallId] + Turn.HasDiff, cmd exit=2, subagent item, retry, recap, merged assistant text.
	t3 := full.Turns[2]
	if !t3.HasDiff || t3.DurationMs != 12000 {
		t.Fatalf("expected turn 3 HasDiff=true DurationMs=12000, got %+v", t3)
	}
	editDiff := full.Diffs["tc-edit-1"]
	if !strings.Contains(editDiff, "--- main.go") || !strings.Contains(editDiff, "-oldLine") || !strings.Contains(editDiff, "+newLine") {
		t.Fatalf("unexpected Diffs[tc-edit-1]: %q", editDiff)
	}
	if full.Diff("tc-edit-1") != editDiff || !strings.Contains(full.Diff(t3.Key), "+newLine") {
		t.Fatalf("Diff lookup helper mismatch: byTool=%q byTurn=%q", full.Diff("tc-edit-1"), full.Diff(t3.Key))
	}
	var sawFile, sawCmd, sawAgent, sawRetry, sawRecap, sawAssistant3 bool
	for _, it := range t3.Items {
		switch {
		case it.ID == "tc-edit-1" && it.K == "file" && strings.Contains(it.Body, "+newLine") && it.Status == "completed":
			sawFile = true
		case it.ID == "tc-cmd-1" && it.K == "cmd" && it.Status == "failed" && it.Exit != nil && *it.Exit == 2 && strings.Contains(it.Body, "FAIL package main"):
			sawCmd = true
		case it.ID == "tc-spawn-1" && it.K == "agent" && it.AgentID == "sub-child-1" && it.AgentPath == "explore":
			sawAgent = true
		case it.K == "info" && it.Title == "Retry" && strings.Contains(it.Text, "Retrying (2/4): rate limit"):
			sawRetry = true
		case it.K == "info" && it.Title == "Session Recap" && strings.Contains(it.Text, "Patched main.go"):
			sawRecap = true
		case it.K == "assistant" && it.Text == "Patched main.go.":
			sawAssistant3 = true
		}
	}
	if !sawFile || !sawCmd || !sawAgent || !sawRetry || !sawRecap || !sawAssistant3 {
		t.Fatalf("turn 3 check failed: file=%v cmd=%v agent=%v retry=%v recap=%v assistant=%v items=%+v",
			sawFile, sawCmd, sawAgent, sawRetry, sawRecap, sawAssistant3, t3.Items)
	}

	// Pagination: limit=1 returns only turn 3 with More=true, HasOlder=true, and pruned turn 2 diff.
	paged, err := readGrokTranscript(sessionDir, 1)
	if err != nil {
		t.Fatalf("readGrokTranscript paged error: %v", err)
	}
	if !paged.More || !paged.Meta.HasOlder {
		t.Fatalf("expected More=true HasOlder=true on paged transcript")
	}
	if len(paged.Turns) != 1 || paged.Turns[0].Key != t3.Key {
		t.Fatalf("expected only last turn %s, got %+v", t3.Key, paged.Turns)
	}
	if _, leaked := paged.Diffs["tc-edit-old"]; leaked {
		t.Fatalf("expected dropped turn 2 diff tc-edit-old to be pruned from paged Diffs")
	}
	if paged.Diffs["tc-edit-1"] == "" {
		t.Fatalf("expected kept turn 3 diff tc-edit-1 to remain in paged Diffs")
	}

	// Regression check: after pagination trims earlier turns, toolIndex and planIndex
	// must be rebased to the sliced turn indices so live ApplyUpdate mutates in place.
	pagedPlanDir := filepath.Join(t.TempDir(), "sess-paged-plan")
	writeGrokJSONL(t, filepath.Join(pagedPlanDir, "updates.jsonl"), []any{
		makeLine("2026-11-27T10:00:00Z", "session/update", map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "Earlier turn"}}),
		makeLine("2026-11-27T10:00:01Z", "session/update", map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Done earlier"}}),
		makeLine("2026-11-27T10:01:00Z", "session/update", map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "Latest turn"}}),
		makeLine("2026-11-27T10:01:01Z", "session/update", map[string]any{
			"sessionUpdate": "plan",
			"entries":       []map[string]any{{"content": "Step A", "status": "in_progress"}},
		}),
		makeLine("2026-11-27T10:01:02Z", "session/update", map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "tc-live-1",
			"title":         "go test ./...",
			"kind":          "execute",
			"status":        "in_progress",
		}),
	}, time.Time{})
	pagedLive, err := readGrokTranscript(pagedPlanDir, 1)
	if err != nil {
		t.Fatalf("readGrokTranscript pagedPlanDir: %v", err)
	}
	if !pagedLive.More || len(pagedLive.Turns) != 1 {
		t.Fatalf("expected 1 kept turn with More=true, got more=%v len=%d", pagedLive.More, len(pagedLive.Turns))
	}
	if !pagedLive.ApplyUpdate("session/update", json.RawMessage(`{"sessionUpdate":"plan","entries":[{"content":"Step A","status":"completed"}]}`), 0) {
		t.Fatalf("expected live plan update after pagination to apply")
	}
	if !pagedLive.ApplyUpdate("session/update", json.RawMessage(`{"sessionUpdate":"tool_call_update","toolCallId":"tc-live-1","status":"completed","rawOutput":{"exit_code":0,"output":"PASS"}}`), 0) {
		t.Fatalf("expected live tool_call_update after pagination to apply")
	}
	livePlanCount, liveToolCount := 0, 0
	for _, it := range pagedLive.Turns[0].Items {
		if it.ID == pagedLive.Turns[0].Key+":plan" {
			livePlanCount++
			if !strings.Contains(it.Text, "[x] Step A") {
				t.Fatalf("expected plan item updated in place, got %q", it.Text)
			}
		}
		if it.ID == "tc-live-1" {
			liveToolCount++
			if it.Status != "completed" || it.Exit == nil || *it.Exit != 0 || !strings.Contains(it.Body, "PASS") {
				t.Fatalf("expected tool item updated in place, got %+v", it)
			}
		}
	}
	if livePlanCount != 1 || liveToolCount != 1 {
		t.Fatalf("expected in-place updates with zero duplicates after pagination, got planCount=%d toolCount=%d items=%+v", livePlanCount, liveToolCount, pagedLive.Turns[0].Items)
	}

	// Direct live ACP reuse via grokApplySessionUpdate (bare update payload + signals fallback test).
	live := newGrokTranscript()
	if !grokApplySessionUpdate(live, "session/update", json.RawMessage(`{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Live prompt"}}`), 1763640000000) {
		t.Fatalf("expected bare user_message_chunk to apply")
	}
	if !grokApplySessionUpdate(live, "session/update", json.RawMessage(`{"sessionUpdate":"config_option_update","configId":"reasoning_effort","value":{"value":"xhigh"}}`), 0) {
		t.Fatalf("expected wrapped config_option_update value to apply")
	}
	if !grokApplySessionUpdate(live, "_x.ai/session/update", json.RawMessage(`{"sessionUpdate":"turn_completed","stop_reason":"cancelled"}`), 1763640001500) {
		t.Fatalf("expected turn_completed to apply")
	}
	if live.Meta.Effort != "xhigh" || len(live.Turns) != 1 || live.Turns[0].Status != "interrupted" || live.Turns[0].DurationMs != 1500 {
		t.Fatalf("unexpected live projection state: meta=%+v turns=%+v", live.Meta, live.Turns)
	}

	// Verify signals.json fallback when updates.jsonl has no usage_update and manual title precedence.
	manualDir := filepath.Join(t.TempDir(), "sess-manual")
	writeGrokJSON(t, filepath.Join(manualDir, "summary.json"), map[string]any{
		"info":            map[string]any{"id": "sess-manual", "cwd": "/repo/manual"},
		"generated_title": "Pinned Manual Title",
		"title_is_manual": true,
	}, time.Time{})
	writeGrokJSON(t, filepath.Join(manualDir, "signals.json"), map[string]any{
		"contextTokensUsed":   12345,
		"contextWindowTokens": 256000,
	}, time.Time{})
	writeGrokJSONL(t, filepath.Join(manualDir, "updates.jsonl"), []any{
		makeLine("2026-11-27T11:00:00Z", "session/update", map[string]any{"sessionUpdate": "session_info_update", "title": "Overridden Auto Title"}),
		makeLine("2026-11-27T11:00:01Z", "session/update", map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "hi"}}),
	}, time.Time{})
	manualView, err := readGrokTranscript(manualDir, 15)
	if err != nil {
		t.Fatalf("readGrokTranscript manualDir: %v", err)
	}
	if manualView.Meta.Title != "Pinned Manual Title" || manualView.Meta.CtxUsed != 12345 || manualView.Meta.CtxMax != 256000 {
		t.Fatalf("unexpected manual/signals fallback meta: %+v", manualView.Meta)
	}
}

func TestGrokSubagentsDiscoveryParentageAndReadOnlyIngress(t *testing.T) {
	grokHome := t.TempDir()
	now := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	withGrokTestSeams(t, now, func(int) bool { return false }, func(paths []string) map[string]rolloutOwnerEvidence {
		res := map[string]rolloutOwnerEvidence{}
		for _, p := range paths {
			res[p] = rolloutOwnerEvidence{known: true, alive: false}
		}
		return res
	})

	parentID := "parent-1"
	otherParentID := "parent-2"
	child1ID := "child-session-1"
	child2ID := "child-session-2"
	metaOnlyID := "meta-only-subagent-3"

	cwdEsc := url.PathEscape("/repo/main")
	parentDir := filepath.Join(grokHome, "sessions", cwdEsc, parentID)
	otherParentDir := filepath.Join(grokHome, "sessions", cwdEsc, otherParentID)
	child1Dir := filepath.Join(grokHome, "sessions", cwdEsc, child1ID)
	child2Dir := filepath.Join(grokHome, "sessions", cwdEsc, child2ID)

	writeGrokJSON(t, filepath.Join(parentDir, "summary.json"), map[string]any{
		"info":            map[string]any{"id": parentID, "cwd": "/repo/main"},
		"generated_title": "Main Parent Chat",
		"updated_at":      "2026-11-27T15:55:00Z",
	}, now.Add(-5*time.Minute))
	writeGrokJSON(t, filepath.Join(otherParentDir, "summary.json"), map[string]any{
		"info":            map[string]any{"id": otherParentID, "cwd": "/repo/main"},
		"generated_title": "Other Parent Chat",
		"updated_at":      "2026-11-27T15:50:00Z",
	}, now.Add(-10*time.Minute))

	// Child 1: session_kind == "subagent" + linked via parent's subagents/<id>/meta.json + updates.jsonl.
	writeGrokJSON(t, filepath.Join(child1Dir, "summary.json"), map[string]any{
		"info":              map[string]any{"id": child1ID, "cwd": "/repo/main"},
		"generated_title":   "Audit Storage Layer",
		"session_kind":      "subagent",
		"parent_session_id": parentID,
		"agent_name":        "auditor",
		"current_model_id":  "grok-4.7",
		"updated_at":        "2026-11-27T15:56:00Z",
		"num_messages":      4,
	}, now.Add(-4*time.Minute))
	writeGrokJSONL(t, filepath.Join(child1Dir, "updates.jsonl"), []any{
		map[string]any{"timestamp": "2026-11-27T15:55:10Z", "method": "session/update", "params": map[string]any{"sessionId": child1ID, "update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "Audit grok storage"}}}},
		map[string]any{"timestamp": "2026-11-27T15:56:00Z", "method": "session/update", "params": map[string]any{"sessionId": child1ID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "All invariants verified."}}}},
	}, now.Add(-4*time.Minute))
	writeGrokJSON(t, filepath.Join(parentDir, "subagents", "sub-id-1", "meta.json"), map[string]any{
		"subagent_id":        "sub-id-1",
		"parent_session_id":  parentID,
		"child_session_id":   child1ID,
		"child_cwd":          "/repo/main",
		"subagent_type":      "explore",
		"description":        "Audit storage invariants",
		"status":             "completed",
		"effective_model_id": "grok-4.7",
		"started_at":         "2026-11-27T15:55:10Z",
		"completed_at":       "2026-11-27T15:56:00Z",
		"duration_ms":        50000,
	}, now.Add(-4*time.Minute))

	// Child 2: parent_session_id != "" with fresh active events.jsonl -> Running=true, sorts first!
	writeGrokJSON(t, filepath.Join(child2Dir, "summary.json"), map[string]any{
		"info":              map[string]any{"id": child2ID, "cwd": "/repo/main"},
		"generated_title":   "Active Worker",
		"parent_session_id": parentID,
		"current_model_id":  "grok-4.7-build-fast",
		"updated_at":        "2026-11-27T15:59:45Z",
	}, now.Add(-15*time.Second))
	writeGrokJSONL(t, filepath.Join(child2Dir, "events.jsonl"), []any{
		map[string]any{"type": "turn_started", "timestamp": "2026-11-27T15:59:45Z"},
	}, now.Add(-15*time.Second))

	// Meta-only subagent with output.json and no child session dir.
	writeGrokJSON(t, filepath.Join(parentDir, "subagents", metaOnlyID, "meta.json"), map[string]any{
		"subagent_id":        metaOnlyID,
		"parent_session_id":  parentID,
		"child_cwd":          "/repo/main",
		"subagent_type":      "plan",
		"description":        "Draft migration plan",
		"status":             "failed",
		"effective_model_id": "grok-4.6",
		"started_at":         "2026-11-27T15:50:00Z",
		"completed_at":       "2026-11-27T15:51:00Z",
		"duration_ms":        60000,
	}, now.Add(-9*time.Minute))
	writeGrokJSON(t, filepath.Join(parentDir, "subagents", metaOnlyID, "output.json"), map[string]any{
		"summary": "Encountered test compilation error.",
	}, now.Add(-9*time.Minute))

	// Regression cases for mismatched parentage in meta.json:
	// (a) A real child owned by otherParentID.
	otherChildID := "child-of-other-parent"
	otherChildDir := filepath.Join(grokHome, "sessions", cwdEsc, otherChildID)
	writeGrokJSON(t, filepath.Join(otherChildDir, "summary.json"), map[string]any{
		"info":              map[string]any{"id": otherChildID, "cwd": "/repo/main"},
		"generated_title":   "Other Parent Legitimate Child",
		"session_kind":      "subagent",
		"parent_session_id": otherParentID,
		"updated_at":        "2026-11-27T15:52:00Z",
	}, now.Add(-8*time.Minute))
	// (b) Mismatched meta.json under parentDir claiming parent_session_id = otherParentID.
	writeGrokJSON(t, filepath.Join(parentDir, "subagents", "tampered-wrong-parent", "meta.json"), map[string]any{
		"subagent_id":       "tampered-wrong-parent",
		"parent_session_id": otherParentID,
		"child_session_id":  "ghost-child-wrong-parent",
		"description":       "Wrong parent metadata",
		"status":            "completed",
	}, now.Add(-7*time.Minute))
	// (c) Mismatched meta.json under parentDir referencing otherChildID (whose summary belongs to otherParentID).
	writeGrokJSON(t, filepath.Join(parentDir, "subagents", "tampered-hijack-child", "meta.json"), map[string]any{
		"subagent_id":       "tampered-hijack-child",
		"parent_session_id": parentID,
		"child_session_id":  otherChildID,
		"description":       "Attempted hijack of another parent child",
		"status":            "completed",
	}, now.Add(-7*time.Minute))

	records, err := grokLoadRecords(grokHome, nil)
	if err != nil {
		t.Fatalf("grokLoadRecords: %v", err)
	}

	// List subagents for parentID: running first, then newest Recency, excluding both tampered entries.
	agents := grokListAgents(records, grokHome, parentID, nil)
	if len(agents) != 3 {
		t.Fatalf("expected 3 subagents for %s, got %d: %+v", parentID, len(agents), agents)
	}
	if agents[0].ID != child2ID || !agents[0].Running || agents[0].AgentState != "running" || !agents[0].Subagent || agents[0].ParentID != parentID || agents[0].Model != "grok-4.7-build-fast" {
		t.Fatalf("unexpected agents[0] (running): %+v", agents[0])
	}
	if agents[1].ID != child1ID || agents[1].Running || agents[1].AgentState != "completed" || agents[1].AgentName != "auditor" || agents[1].Model != "grok-4.7" || agents[1].Cwd != filepath.Clean("/repo/main") {
		t.Fatalf("unexpected agents[1]: %+v", agents[1])
	}
	if agents[2].ID != metaOnlyID || agents[2].AgentState != "failed" || agents[2].Model != "grok-4.6" {
		t.Fatalf("unexpected agents[2] (meta-only): %+v", agents[2])
	}

	// Other parent has only its own legitimate child (never mutated by parentDir's tampered meta.json).
	otherAgents := grokListAgents(records, grokHome, otherParentID, nil)
	if len(otherAgents) != 1 || otherAgents[0].ID != otherChildID || otherAgents[0].ParentID != otherParentID {
		t.Fatalf("expected only %s for otherParent, got %+v", otherChildID, otherAgents)
	}
	for _, rec := range records {
		if rec.ID == otherChildID && rec.SubagentID == "tampered-hijack-child" {
			t.Fatalf("expected other parent's child record not to be mutated by mismatched meta.json")
		}
		if rec.ID == "ghost-child-wrong-parent" {
			t.Fatalf("expected mismatched-parent meta.json under parentDir to be ignored")
		}
	}
	if _, err := grokOpenAgentTranscript(records, parentID, otherChildID, 15); err == nil {
		t.Fatalf("expected opening hijacked child from wrong parent to fail")
	}
	if _, err := grokOpenAgentTranscript(records, parentID, "tampered-hijack-child", 15); err == nil {
		t.Fatalf("expected opening hijacked subagent alias from wrong parent to fail")
	}

	// Strict read-only ingress enforcement across all control ops + pin/unpin.
	if grokIsSubagent(records, parentID) {
		t.Fatalf("parent chat must not be classified as subagent")
	}
	for _, subID := range []string{child1ID, "sub-id-1", child2ID, metaOnlyID} {
		if !grokIsSubagent(records, subID) {
			t.Fatalf("expected %q to be classified as subagent", subID)
		}
	}
	for _, op := range []string{"send", "stop", "wake", "settings", "approve", "answer", "compact", "archive", "unarchive", "rename", "fork", "pin", "unpin"} {
		if err := grokRejectSubagentControl(records, child1ID, op); err == nil {
			t.Errorf("expected op %q to be rejected on subagent %s", op, child1ID)
		}
		if err := grokRejectSubagentControl(records, "sub-id-1", op); err == nil {
			t.Errorf("expected op %q to be rejected on subagent alias sub-id-1", op)
		}
		if err := grokRejectSubagentControl(records, parentID, op); err != nil {
			t.Errorf("expected op %q to be allowed on parent chat, got %v", op, err)
		}
	}

	// Parentage validation on open.
	if _, err := grokOpenAgentTranscript(records, otherParentID, child1ID, 15); err == nil {
		t.Fatalf("expected cross-parent open to be rejected")
	}
	if _, err := grokOpenAgentTranscript(records, parentID, parentID, 15); err == nil {
		t.Fatalf("expected opening parent as subagent to be rejected")
	}
	if _, err := grokOpenAgentTranscript(records, parentID, "../escape", 15); err == nil {
		t.Fatalf("expected traversal agentID to be rejected")
	}

	// Valid open by child_session_id and by subagent_id alias both enforce ReadOnly=true and Mode="tail".
	for _, lookupID := range []string{child1ID, "sub-id-1"} {
		view, err := grokOpenAgentTranscript(records, parentID, lookupID, 15)
		if err != nil {
			t.Fatalf("grokOpenAgentTranscript(%s): %v", lookupID, err)
		}
		if !view.Meta.ReadOnly || view.Meta.Mode != "tail" || view.Meta.Provider != "grok" || view.Meta.AgentState != "completed" {
			t.Fatalf("unexpected subagent meta for %s: %+v", lookupID, view.Meta)
		}
		if len(view.Turns) != 1 || len(view.Turns[0].Items) != 2 {
			t.Fatalf("unexpected subagent turns for %s: %+v", lookupID, view.Turns)
		}
	}

	// Meta-only subagent open reads output.json fallback and remains read-only tail.
	metaView, err := grokOpenAgentTranscript(records, parentID, metaOnlyID, 15)
	if err != nil {
		t.Fatalf("open meta-only subagent: %v", err)
	}
	if !metaView.Meta.ReadOnly || metaView.Meta.Mode != "tail" || metaView.Meta.AgentState != "failed" {
		t.Fatalf("unexpected meta-only subagent meta: %+v", metaView.Meta)
	}
	if len(metaView.Turns) != 1 || !strings.Contains(metaView.Turns[0].Items[0].Text, "compilation error") {
		t.Fatalf("expected output.json fallback turn, got %+v", metaView.Turns)
	}
}

func TestGrokStatePersistenceAndOrganization(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), ".fastcontrol", "grok-state.json")
	st, err := newGrokState(statePath)
	if err != nil {
		t.Fatalf("newGrokState: %v", err)
	}

	if err := st.SetPinned("g1", true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	if err := st.SetArchived("g2", true); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	clean, err := st.SetTitle("g1", "  Custom Grok Title  ")
	if err != nil || clean != "Custom Grok Title" {
		t.Fatalf("SetTitle: got (%q, %v)", clean, err)
	}
	if _, err := st.SetTitle("g1", "Bad\nTitle"); err == nil {
		t.Fatalf("expected multiline title to be rejected by sessionTitle")
	}
	if _, err := st.SetTitle("g1", "   "); err == nil {
		t.Fatalf("expected blank title to be rejected by sessionTitle")
	}
	if err := st.SetPref("g1", grokThreadPref{Model: "grok-4.7", Effort: "xhigh", PermissionMode: "auto"}); err != nil {
		t.Fatalf("SetPref: %v", err)
	}

	// Verify file permissions on POSIX (skip on Windows for CI portability).
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
			_ = st.SetPref(id, grokThreadPref{Model: "grok-4.7", Effort: "medium"})
			_ = st.Pref(id)
			_ = st.Snapshot()
		}(i)
	}
	wg.Wait()

	reloaded, err := newGrokState(statePath)
	if err != nil {
		t.Fatalf("reload grok state: %v", err)
	}
	if !reloaded.IsPinned("g1") || !reloaded.IsArchived("g2") || reloaded.Title("g1") != "Custom Grok Title" {
		t.Fatalf("state did not persist across reload: %+v", reloaded.Snapshot())
	}
	pref := reloaded.Pref("g1")
	if pref.Model != "grok-4.7" || pref.Effort != "xhigh" || pref.PermissionMode != "auto" {
		t.Fatalf("unexpected reloaded pref: %+v", pref)
	}

	// OrganizationReader.Grok: pinning, archiving, duplicate folder disambiguation,
	// External->Active mapping, and strict exclusion of subagents.
	records := []grokRecord{
		{ID: "g1", Title: "Orig One", Cwd: filepath.Clean("/work/app"), Recency: 1000, Model: "grok-4.6", ReasoningEffort: "low", External: true, HeadBranch: "main"},
		{ID: "g2", Title: "Archived Two", Cwd: filepath.Clean("/work/app"), Recency: 3000, Model: "grok-4.6"},
		{ID: "g3", Title: "Recent Three", Cwd: filepath.Clean("/personal/app"), Recency: 2000, Model: "grok-4.7", ReasoningEffort: "high"},
		{ID: "sub-1", Title: "Subagent Excluded", Cwd: filepath.Clean("/work/app"), SessionKind: "subagent", ParentSessionID: "g1", Recency: 9000},
		{ID: "sub-2", Title: "ParentLink Excluded", Cwd: filepath.Clean("/work/app"), ParentSessionID: "g1", Recency: 8000},
	}

	reader := NewOrganizationReader(t.TempDir())
	activeThreads, groups := reader.Grok(false, reloaded, records)
	if len(activeThreads) != 2 {
		t.Fatalf("expected 2 active top-level threads (g1 pinned, g3 recent), got %d: %+v", len(activeThreads), activeThreads)
	}
	if activeThreads[0].ID != "g1" || !activeThreads[0].Pinned || !activeThreads[0].Active || activeThreads[0].Running || activeThreads[0].DisplayTitle != "Custom Grok Title" || activeThreads[0].Model != "grok-4.7" || activeThreads[0].Effort != "xhigh" || activeThreads[0].Branch != "main" {
		t.Fatalf("unexpected activeThreads[0]: %+v", activeThreads[0])
	}
	if activeThreads[1].ID != "g3" || activeThreads[1].Pinned || activeThreads[1].Model != "grok-4.7" || activeThreads[1].Effort != "high" {
		t.Fatalf("unexpected activeThreads[1]: %+v", activeThreads[1])
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 disambiguated project groups, got %d: %+v", len(groups), groups)
	}
	// Duplicate basename "app" must disambiguate to "personal/app" and "work/app".
	if groups[0].Name != "personal/app" || groups[1].Name != "work/app" {
		t.Fatalf("expected disambiguated project group names [personal/app, work/app], got [%q, %q]", groups[0].Name, groups[1].Name)
	}

	// Archived view returns only g2.
	archivedThreads, archivedGroups := reader.Grok(true, reloaded, records)
	if len(archivedThreads) != 1 || archivedThreads[0].ID != "g2" {
		t.Fatalf("expected only archived g2, got %+v", archivedThreads)
	}
	if len(archivedGroups) != 1 || archivedGroups[0].Name != "app" {
		t.Fatalf("unexpected archived groups: %+v", archivedGroups)
	}

	// Folder project summaries exclude archived and subagents.
	projects := grokProjects(records, reloaded)
	if len(projects) != 2 || projects[0].Cwd != filepath.Clean("/personal/app") || projects[1].Cwd != filepath.Clean("/work/app") {
		t.Fatalf("unexpected grokProjects: %+v", projects)
	}

	// ClearTitle works and persists.
	if err := reloaded.ClearTitle("g1"); err != nil {
		t.Fatalf("ClearTitle: %v", err)
	}
	if reloaded.Title("g1") != "" {
		t.Fatalf("expected cleared title to be empty")
	}
}
