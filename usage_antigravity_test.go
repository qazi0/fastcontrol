package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeRawFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestConvertAdditiveWireTypes(t *testing.T) {
	// Zero-value additive fields must be omitted so Codex and Claude wire payloads stay unchanged.
	plainMeta := Meta{Title: "Session", Mode: "live", Running: false}
	plainMetaJSON, err := json.Marshal(plainMeta)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plainMetaJSON), `"commands"`) {
		t.Fatalf("empty commands should be omitted: %s", plainMetaJSON)
	}

	plainReq := Request{ID: "r1", Method: "commandExecution", Kind: "command", Title: "Run Command?"}
	plainReqJSON, err := json.Marshal(plainReq)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plainReqJSON), `"options"`) {
		t.Fatalf("empty options should be omitted: %s", plainReqJSON)
	}

	plainQ := Question{ID: "q1", Header: "Choice", Question: "Pick one"}
	plainQJSON, err := json.Marshal(plainQ)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plainQJSON), `"multiSelect"`) || strings.Contains(string(plainQJSON), `"freeText"`) {
		t.Fatalf("false question flags should be omitted: %s", plainQJSON)
	}

	// Populated additive fields serialize and round-trip cleanly.
	richMeta := Meta{
		Provider: "antigravity",
		Title:    "Antigravity Session",
		Mode:     "live",
		Running:  true,
		Commands: []SlashCommand{
			{Name: "plan", Description: "Create a plan", InputHint: "<goal>"},
			{Name: "compact"},
		},
		Requests: []Request{
			{
				ID:     "perm-1",
				Method: "session/request_permission",
				Kind:   "command",
				Title:  "Allow Tool Execution?",
				Options: []RequestOption{
					{ID: "allow", Label: "Allow Once", Kind: "allow_once"},
					{ID: "deny", Label: "Deny", Kind: "reject_once"},
				},
			},
			{
				ID:     "elicit-1",
				Method: "elicitation/create",
				Kind:   "question",
				Title:  "Antigravity Has A Question",
				Questions: []Question{
					{
						ID:          "targets",
						Header:      "Targets",
						Question:    "Select target platforms",
						Options:     []string{"macOS", "Linux", "Windows"},
						MultiSelect: true,
					},
					{
						ID:       "notes",
						Header:   "Notes",
						Question: "Additional instructions",
						FreeText: true,
					},
				},
			},
		},
	}
	encoded, err := json.Marshal(richMeta)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Meta
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Commands) != 2 || decoded.Commands[0].Name != "plan" || decoded.Commands[0].InputHint != "<goal>" || decoded.Commands[1].Description != "" {
		t.Fatalf("unexpected decoded commands: %+v", decoded.Commands)
	}
	if len(decoded.Requests) != 2 || len(decoded.Requests[0].Options) != 2 || decoded.Requests[0].Options[0].Kind != "allow_once" {
		t.Fatalf("unexpected decoded request options: %+v", decoded.Requests)
	}
	if len(decoded.Requests[1].Questions) != 2 || !decoded.Requests[1].Questions[0].MultiSelect || !decoded.Requests[1].Questions[1].FreeText {
		t.Fatalf("unexpected decoded questions: %+v", decoded.Requests[1].Questions)
	}
}

func TestAntigravityLocalUsageDesktopAndCLIWithDedupAndCaching(t *testing.T) {
	manager := usageFixtureManager(t)
	location := time.FixedZone("PKT", 5*3600)
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, location)
	todayStamp := now.Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	yesterdayStamp := now.AddDate(0, 0, -1).UTC().Format(time.RFC3339Nano)
	oldStamp := now.AddDate(0, 0, -35).UTC().Format(time.RFC3339Nano)

	desktopPath := filepath.Join(manager.home, ".gemini", "antigravity", "brain", "conv-desktop", ".system_generated", "logs", "transcript.jsonl")
	// Also write a transcript_full.jsonl and an unrelated jsonl file that must be ignored.
	fullPath := filepath.Join(manager.home, ".gemini", "antigravity", "brain", "conv-desktop", ".system_generated", "logs", "transcript_full.jsonl")
	cliPath := filepath.Join(manager.home, ".gemini", "antigravity-cli", "brain", "conv-cli", ".system_generated", "logs", "transcript.jsonl")
	// Duplicate conversation ID in CLI dir to verify cross-root deduplication by conversation + step_index.
	dupCLIPath := filepath.Join(manager.home, ".gemini", "antigravity-cli", "brain", "conv-desktop", ".system_generated", "logs", "transcript.jsonl")

	writeUsageFixture(t, desktopPath,
		map[string]any{"step_index": 0, "type": "USER_INPUT", "created_at": todayStamp, "content": "hi"},
		// Partial streaming row followed by larger final row at the same step_index=2.
		map[string]any{"step_index": 2, "type": "PLANNER_RESPONSE", "status": "RUNNING", "created_at": todayStamp, "model": "gemini-4-neon-xhigh", "input_tokens": 1000, "cache_read_tokens": 0, "output_tokens": 50},
		map[string]any{"step_index": 2, "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": todayStamp, "model": "gemini-4-neon-xhigh", "input_tokens": 1500, "cache_read_tokens": 500, "output_tokens": 200},
		// Yesterday entry (counts in Days30, not Today).
		map[string]any{"step_index": 5, "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": yesterdayStamp, "model": "gemini-4-neon-xhigh", "input_tokens": 400, "cache_read_tokens": 600, "output_tokens": 100},
		// Outside 30-day window (ignored).
		map[string]any{"step_index": 1, "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": oldStamp, "model": "gemini-4-neon-xhigh", "input_tokens": 9999, "cache_read_tokens": 0, "output_tokens": 999},
		// Zero-token response (ignored).
		map[string]any{"step_index": 9, "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": todayStamp, "model": "gemini-4-neon-xhigh", "input_tokens": 0, "cache_read_tokens": 0, "output_tokens": 0},
	)
	writeUsageFixture(t, fullPath,
		map[string]any{"step_index": 99, "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": todayStamp, "model": "should-not-read", "input_tokens": 50000, "cache_read_tokens": 0, "output_tokens": 5000},
	)
	writeUsageFixture(t, cliPath,
		map[string]any{"step_index": 1, "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": todayStamp, "model": nil, "input_tokens": 300, "cache_read_tokens": 100, "output_tokens": 50},
	)
	writeUsageFixture(t, dupCLIPath,
		map[string]any{"step_index": 2, "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": todayStamp, "model": "gemini-4-neon-xhigh", "input_tokens": 1500, "cache_read_tokens": 500, "output_tokens": 200},
	)

	report := manager.collect(now)
	if len(report.Providers) != len(usageProviders) || report.Providers[2].Provider != "antigravity" {
		t.Fatalf("expected %d providers with antigravity third: %+v", len(usageProviders), report.Providers)
	}
	p := report.Providers[2]

	// Today: desktop step 2 (input=1500+500=2000, cached=500, out=200 -> 2200) + cli step 1 (input=300+100=400, cached=100, out=50 -> 450) = 2650 tokens, 2 requests.
	if p.Today.Tokens != 2650 || p.Today.Input != 2400 || p.Today.Cached != 600 || p.Today.Output != 250 || p.Today.Requests != 2 {
		t.Fatalf("unexpected Today totals: %+v", p.Today)
	}
	// Days30 adds yesterday step 5 (input=400+600=1000, cached=600, out=100 -> 1100) => 3750 tokens, 3 requests.
	if p.Days30.Tokens != 3750 || p.Days30.Input != 3400 || p.Days30.Cached != 1200 || p.Days30.Output != 350 || p.Days30.Requests != 3 {
		t.Fatalf("unexpected Days30 totals: %+v", p.Days30)
	}
	// Unknown/unverified models stay cleanly in Unpriced with Cost == nil.
	if p.Days30.Unpriced != 3750 || p.Days30.Cost != nil {
		t.Fatalf("expected unpriced tokens with nil cost: %+v", p.Days30)
	}
	if len(p.Models) != 2 || p.Models[0].Model != "gemini-4-neon-xhigh" || p.Models[0].Tokens != 3300 || p.Models[1].Model != "" || p.Models[1].Tokens != 450 {
		t.Fatalf("unexpected per-model breakdown: %+v", p.Models)
	}

	// Verify incremental caching: m.files caches all 3 transcript.jsonl paths and never transcript_full.jsonl.
	if _, ok := manager.files[desktopPath]; !ok {
		t.Fatal("desktop transcript was not cached")
	}
	if _, ok := manager.files[fullPath]; ok {
		t.Fatal("transcript_full.jsonl should not be scanned or cached")
	}

	// Removing cliPath evicts it from cache and updates totals on next collect.
	if err := os.Remove(cliPath); err != nil {
		t.Fatal(err)
	}
	next := manager.collect(now).Providers[2]
	if next.Days30.Tokens != 3300 || next.Days30.Requests != 2 {
		t.Fatalf("removed CLI transcript stayed in totals: %+v", next.Days30)
	}
	if _, ok := manager.files[cliPath]; ok {
		t.Fatal("removed transcript was not evicted from file cache")
	}
}

func TestAntigravityMalformedTruncatedOversizedAndConfigurableRoots(t *testing.T) {
	manager := usageFixtureManager(t)
	now := time.Now()
	stamp := now.Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)

	customRootA := filepath.Join(t.TempDir(), "root-a")
	customRootB := filepath.Join(t.TempDir(), "root-b")
	manager.antigravityRoots = []string{customRootA, customRootB, customRootA}

	pathA := filepath.Join(customRootA, "brain", "conv-a", ".system_generated", "logs", "transcript.jsonl")
	oversized := `{"step_index":3,"type":"PLANNER_RESPONSE","created_at":"` + stamp + `","content":"` + strings.Repeat("a", (8<<20)+64) + `"}`
	raw := strings.Join([]string{
		`{not valid json`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"bad-timestamp","input_tokens":100,"output_tokens":50}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","created_at":"` + stamp + `","model":"gemini-4-neon","input_tokens":-50,"cache_read_tokens":200,"output_tokens":25}`,
		oversized,
		`{"step_index":4,"type":"PLANNER_RESPONSE","created_at":"` + stamp + `","model":"gemini-4-neon","input_tokens":100,"cache_read_tokens":0,"output_tokens":50`, // truncated last line without newline
	}, "\n")
	writeRawFile(t, pathA, raw)

	pathB := filepath.Join(customRootB, "brain", "conv-b", ".system_generated", "logs", "transcript.jsonl")
	// Valid last line without trailing newline, plus missing step_index fallback key.
	writeRawFile(t, pathB, `{"type":"PLANNER_RESPONSE","created_at":"`+stamp+`","model":"gemini-3.8-flash","input_tokens":100,"cache_read_tokens":50,"output_tokens":10}`)

	p := manager.collect(now).Providers[2]
	// conv-a step 2: negative input clamped to 0, cached=200 => Input=200, Output=25 => 225 tokens.
	// conv-b: Input=150, Cached=50, Output=10 => 160 tokens.
	if p.Days30.Tokens != 385 || p.Days30.Input != 350 || p.Days30.Cached != 250 || p.Days30.Output != 35 || p.Days30.Requests != 2 {
		t.Fatalf("unexpected totals for malformed/custom roots: %+v", p.Days30)
	}
	if p.Skipped != 1 {
		t.Fatalf("expected 1 skipped oversized PLANNER_RESPONSE, got %d", p.Skipped)
	}

	// Verify ANTIGRAVITY_HOME env var with ListSeparator works when override is unset.
	manager2 := usageFixtureManager(t)
	t.Setenv("ANTIGRAVITY_HOME", customRootB+string(os.PathListSeparator)+customRootB)
	p2 := manager2.collect(now).Providers[2]
	if p2.Days30.Tokens != 160 || p2.Days30.Requests != 1 {
		t.Fatalf("unexpected totals from ANTIGRAVITY_HOME: %+v", p2.Days30)
	}
}

func TestParseAntigravityQuotaTSV(t *testing.T) {
	liveSample := strings.Join([]string{
		"Gemini Models\tWeekly Limit Remaining\t82%\t2026-12-03T07:01:49Z",
		"Gemini Models\tFive Hour Limit Remaining\t86.3%\t2026-11-27T17:01:21Z",
		"Claude and GPT models\tWeekly Limit Remaining\t51%\t2026-12-04T04:41:08Z",
		"Claude and GPT models\tFive Hour Limit Remaining\t100%\t1970-01-01T00:00:00Z",
		"",
		"Ignored banner line without tabs",
		"Bad Row\tWeekly Limit Remaining\tnot-a-number%\t2026-12-04T04:41:08Z",
	}, "\r\n")

	windows := parseAntigravityQuotaTSV(liveSample)
	if len(windows) != 4 {
		t.Fatalf("expected 4 quota windows, got %d: %+v", len(windows), windows)
	}

	want := []struct {
		label     string
		remaining float64
		used      float64
		minutes   int64
		resetZero bool
	}{
		{"Gemini Weekly", 82, 18, 10080, false},
		{"Gemini 5-Hour", 86.3, 13.7, 300, false},
		{"Claude And GPT Weekly", 51, 49, 10080, false},
		{"Claude And GPT 5-Hour", 100, 0, 300, true},
	}
	for i, w := range want {
		got := windows[i]
		if got.Label != w.label || got.Remaining != w.remaining || got.Used != w.used || got.Minutes != w.minutes {
			t.Fatalf("window[%d] mismatch: got %+v, want %+v", i, got, w)
		}
		if w.resetZero && got.ResetAt != 0 {
			t.Fatalf("window[%d] expected ResetAt=0 for epoch sentinel, got %d", i, got.ResetAt)
		}
		if !w.resetZero && got.ResetAt <= 0 {
			t.Fatalf("window[%d] expected positive ResetAt, got %d", i, got.ResetAt)
		}
	}

	// Clamping out-of-range percentages and negative Unix timestamps.
	clamped := parseAntigravityQuotaTSV("Gemini Models\tWeekly Limit Remaining\t135%\t1969-12-31T23:59:59Z\nGemini Models\tFive Hour Limit Remaining\t-12%\t1970-01-01T00:00:00Z\n")
	if len(clamped) != 2 || clamped[0].Remaining != 100 || clamped[0].Used != 0 || clamped[0].ResetAt != 0 || clamped[1].Remaining != 0 || clamped[1].Used != 100 || clamped[1].ResetAt != 0 {
		t.Fatalf("unexpected clamped windows: %+v", clamped)
	}
}

func TestFetchAntigravityQuotaErrorHandling(t *testing.T) {
	ctx := context.Background()

	t.Run("missing binary", func(t *testing.T) {
		p := ProviderUsage{Provider: "antigravity", Windows: []UsageWindow{}}
		err := fetchAntigravityQuotaWith(ctx, "/tmp", &p, func(string) string { return "" }, func(context.Context, string) (string, string, error) {
			t.Fatal("command should not run when binary is missing")
			return "", "", nil
		})
		if err == nil || !strings.Contains(err.Error(), "agy") || len(p.Windows) != 0 {
			t.Fatalf("expected missing binary error, got err=%v windows=%+v", err, p.Windows)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		p := ProviderUsage{Provider: "antigravity", Windows: []UsageWindow{}}
		err := fetchAntigravityQuotaWith(ctx, "/tmp", &p, func(string) string { return "/bin/agy" }, func(context.Context, string) (string, string, error) {
			return "", "", context.DeadlineExceeded
		})
		if err == nil || !strings.Contains(err.Error(), "Timed Out") {
			t.Fatalf("expected timeout error, got %v", err)
		}
	})

	t.Run("unsigned cli stderr", func(t *testing.T) {
		p := ProviderUsage{Provider: "antigravity", Windows: []UsageWindow{}}
		err := fetchAntigravityQuotaWith(ctx, "/tmp", &p, func(string) string { return "/bin/agy" }, func(context.Context, string) (string, string, error) {
			return "", "Error: not signed in to ~/.gemini/antigravity-cli", errors.New("exit status 1")
		})
		if err == nil || !strings.Contains(err.Error(), "Sign In") {
			t.Fatalf("expected sign-in error, got %v", err)
		}
	})

	t.Run("unsigned cli stdout exit zero", func(t *testing.T) {
		p := ProviderUsage{Provider: "antigravity", Windows: []UsageWindow{}}
		err := fetchAntigravityQuotaWith(ctx, "/tmp", &p, func(string) string { return "/bin/agy" }, func(context.Context, string) (string, string, error) {
			return "Please log in to continue\n", "", nil
		})
		if err == nil || !strings.Contains(err.Error(), "Sign In") {
			t.Fatalf("expected sign-in error on auth stdout, got %v", err)
		}
	})

	t.Run("non-zero generic failure", func(t *testing.T) {
		p := ProviderUsage{Provider: "antigravity", Windows: []UsageWindow{}}
		err := fetchAntigravityQuotaWith(ctx, "/tmp", &p, func(string) string { return "/bin/agy" }, func(context.Context, string) (string, string, error) {
			return "", "network unreachable", errors.New("exit status 1")
		})
		if err == nil || !strings.Contains(err.Error(), "Could Not Read") {
			t.Fatalf("expected generic quota error, got %v", err)
		}
	})

	t.Run("empty stdout", func(t *testing.T) {
		p := ProviderUsage{Provider: "antigravity", Windows: []UsageWindow{}}
		err := fetchAntigravityQuotaWith(ctx, "/tmp", &p, func(string) string { return "/bin/agy" }, func(context.Context, string) (string, string, error) {
			return "\n", "", nil
		})
		if err == nil || !strings.Contains(err.Error(), "Did Not Report") {
			t.Fatalf("expected empty windows error, got %v", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		p := ProviderUsage{Provider: "antigravity", Windows: []UsageWindow{}}
		err := fetchAntigravityQuotaWith(ctx, "/tmp", &p, func(string) string { return "/bin/agy" }, func(context.Context, string) (string, string, error) {
			return "Gemini Models\tWeekly Limit Remaining\t80%\t2026-12-03T07:01:49Z\n", "", nil
		})
		if err != nil || len(p.Windows) != 1 || p.Windows[0].Label != "Gemini Weekly" || p.Windows[0].Used != 20 {
			t.Fatalf("expected parsed window, got err=%v windows=%+v", err, p.Windows)
		}
	})
}

func TestFindAntigravityUsageBinaryAndCommandTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell script test")
	}
	home := t.TempDir()
	t.Setenv("PATH", "")

	localBin := filepath.Join(home, ".local", "bin", "agy")
	if err := os.MkdirAll(filepath.Dir(localBin), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf 'Gemini Models\\tWeekly Limit Remaining\\t75%%\\t2026-12-03T07:01:49Z\\n'\n"
	if err := os.WriteFile(localBin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if found := findAntigravityUsageBinary(home); found != localBin {
		t.Fatalf("expected %s, got %q", localBin, found)
	}

	p := ProviderUsage{Provider: "antigravity", Windows: []UsageWindow{}}
	if err := fetchAntigravityQuota(context.Background(), home, &p); err != nil {
		t.Fatalf("fetchAntigravityQuota failed: %v", err)
	}
	if len(p.Windows) != 1 || p.Windows[0].Label != "Gemini Weekly" || p.Windows[0].Remaining != 75 || p.Windows[0].Used != 25 {
		t.Fatalf("unexpected quota windows from fake agy: %+v", p.Windows)
	}

	// Verify bounded context timeout terminates a hanging agy process cleanly.
	hangBin := filepath.Join(t.TempDir(), "agy-hang")
	if err := os.WriteFile(hangBin, []byte("#!/bin/sh\nsleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := runAntigravityUsageCommand(ctx, hangBin)
	if err == nil {
		t.Fatal("expected timeout error from hanging binary")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("hanging binary did not terminate promptly: %v", time.Since(start))
	}
}

func TestAntigravityUsageServeAndThreeProviderRefreshLifecycle(t *testing.T) {
	m := usageFixtureManager(t)
	now := time.Now()
	stamp := now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	writeUsageFixture(t, filepath.Join(m.home, ".gemini", "antigravity", "brain", "conv-1", ".system_generated", "logs", "transcript.jsonl"),
		map[string]any{"step_index": 1, "type": "PLANNER_RESPONSE", "created_at": stamp, "model": "gemini-4-neon-xhigh", "input_tokens": 500, "cache_read_tokens": 200, "output_tokens": 100},
	)

	// Simulate Antigravity quota returning a non-fatal QuotaError while Codex and Claude succeed.
	var agyCalls atomic.Int32
	m.quota = func(_ string, p *ProviderUsage) {
		switch p.Provider {
		case "codex":
			p.Windows = []UsageWindow{{Label: "Weekly", Used: 20, Remaining: 80}}
			p.QuotaAt = now.Unix()
		case "claude":
			p.Windows = []UsageWindow{{Label: "Weekly", Used: 35, Remaining: 65}}
			p.QuotaAt = now.Unix()
		case "antigravity":
			agyCalls.Add(1)
			p.QuotaError = "Sign In to Antigravity CLI on This Computer to Read Subscription Limits"
		}
	}

	// GET /usage?provider=antigravity must succeed (HTTP 200) and return non-fatal quotaError.
	req := httptest.NewRequest(http.MethodGet, "/usage?provider=antigravity", nil)
	rec := httptest.NewRecorder()
	m.serve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for antigravity provider, got %d: %s", rec.Code, rec.Body.String())
	}
	var ctxReport UsageReport
	if err := json.Unmarshal(rec.Body.Bytes(), &ctxReport); err != nil {
		t.Fatal(err)
	}
	if len(ctxReport.Providers) != 1 || ctxReport.Providers[0].Provider != "antigravity" || ctxReport.Providers[0].QuotaError == "" {
		t.Fatalf("unexpected antigravity context quota response: %+v", ctxReport)
	}

	// Unknown provider must still return 400.
	badRec := httptest.NewRecorder()
	m.serve(badRec, httptest.NewRequest(http.MethodGet, "/usage?provider=other", nil))
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown provider, got %d", badRec.Code)
	}

	// Full 3-provider concurrent refresh under -race preserves Codex/Claude quotas and Antigravity local tokens despite Antigravity QuotaError.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := m.get(context.Background(), false)
			if err != nil {
				t.Errorf("m.get failed: %v", err)
				return
			}
			var report UsageReport
			if err := json.Unmarshal(data, &report); err != nil {
				t.Errorf("unmarshal failed: %v", err)
				return
			}
			if !validUsageProviders(report) {
				t.Errorf("invalid 3-provider report: %+v", report.Providers)
			}
			if report.Refreshing {
				t.Errorf("completed report still marked refreshing")
			}
			if len(report.Providers[0].Windows) != 1 || len(report.Providers[1].Windows) != 1 {
				t.Errorf("codex/claude quotas missing: %+v", report.Providers)
			}
			agy := report.Providers[2]
			if agy.QuotaError == "" || agy.Days30.Tokens != 800 || agy.Today.Tokens != 800 || agy.Days30.Unpriced != 800 {
				t.Errorf("antigravity local tokens or non-fatal quotaError wrong: %+v", agy)
			}
		}()
	}
	wg.Wait()

	// Legacy 2-provider usage-cache.json on disk must be ignored cleanly rather than crashing or truncating providers.
	legacyCache := UsageReport{
		Host: hostName(),
		Day:  time.Now().Format("2006-01-02"),
		Providers: []ProviderUsage{
			{Provider: "codex", Windows: []UsageWindow{}},
			{Provider: "claude", Windows: []UsageWindow{}},
		},
	}
	if validUsageProviders(legacyCache) {
		t.Fatal("2-provider report must not satisfy validUsageProviders")
	}
	if err := os.WriteFile(m.cachePath(), mustJSON(legacyCache), 0600); err != nil {
		t.Fatal(err)
	}
	reloaded := newUsageManager(m.home)
	if len(reloaded.cached) != 0 {
		t.Fatal("stale 2-provider disk cache should have been ignored")
	}
}

func TestFindAntigravityUsageBinaryEnvOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", "")

	binName := "agy-override"
	if runtime.GOOS == "windows" {
		binName = "agy-override.cmd"
	}
	override := filepath.Join(t.TempDir(), binName)
	if err := os.WriteFile(override, []byte("stub"), 0700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AGY_BIN", override)
	t.Setenv("ANTIGRAVITY_CLI", "")
	if got := findAntigravityUsageBinary(home); got != filepath.Clean(override) {
		t.Fatalf("expected AGY_BIN override %q, got %q", override, got)
	}

	t.Setenv("AGY_BIN", "")
	t.Setenv("ANTIGRAVITY_CLI", override)
	if got := findAntigravityUsageBinary(home); got != filepath.Clean(override) {
		t.Fatalf("expected ANTIGRAVITY_CLI override %q, got %q", override, got)
	}
}
