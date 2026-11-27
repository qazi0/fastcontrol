package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestGrokLocalUsageTurnsSessionFallbackCostDedupAndCaching(t *testing.T) {
	manager := usageFixtureManager(t)
	location := time.FixedZone("PKT", 5*3600)
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, location)
	todayStamp := now.Add(-15 * time.Minute).UTC().Format(time.RFC3339Nano)
	yesterdayStamp := now.AddDate(0, 0, -1).UTC().Format(time.RFC3339Nano)
	oldStamp := now.AddDate(0, 0, -35).UTC().Format(time.RFC3339Nano)

	grokRoot := filepath.Join(t.TempDir(), "custom-grok-home")
	manager.grokHome = grokRoot

	// Session 1: turns[] with priced turn today, unpriced turn today, yesterday turn, old turn, and duplicate turn.
	sess1Path := filepath.Join(grokRoot, "sessions", "%2FUsers%2Fdev%2Fproj-a", "sess-1", "usage.json")
	writeJSONFixture(t, sess1Path, map[string]any{
		"sessionId": "sess-1",
		"updatedAt": todayStamp,
		"session": map[string]any{
			"primaryModelId": "grok-4.7",
		},
		"turns": []map[string]any{
			{
				"turnNumber":          1,
				"endedAt":             todayStamp,
				"inputTokens":         10000,
				"cachedReadTokens":    8000,
				"cacheCreationTokens": 500,
				"outputTokens":        1200,
				"reasoningTokens":     400,
				"totalTokens":         11200,
				"costUsdTicks":        150000000, // $0.015
				"primaryModelId":      "grok-4.7",
			},
			// Duplicate turn 1 with smaller token count must not overwrite the larger row.
			{
				"turnNumber":     1,
				"endedAt":        todayStamp,
				"inputTokens":    2000,
				"outputTokens":   100,
				"totalTokens":    2100,
				"costUsdTicks":   10000000,
				"primaryModelId": "grok-4.7",
			},
			// Turn 2: zero costUsdTicks must stay in Unpriced (never invent rates),
			// and single-key turn.modelUsage wins over session.primaryModelId when turn modelId is omitted.
			{
				"turnNumber":       2,
				"endedAt":          todayStamp,
				"inputTokens":      1000,
				"cachedReadTokens": 200,
				"outputTokens":     300,
				"totalTokens":      1300,
				"costUsdTicks":     0,
				"modelUsage": map[string]any{
					"grok-4.7-build-fast": map[string]any{
						"inputTokens":      800,
						"cachedReadTokens": 200,
						"outputTokens":     300,
					},
				},
			},
			// Turn 3: yesterday turn with uncached-only inputTokens disambiguated via totalTokens.
			{
				"turnNumber":       3,
				"endedAt":          yesterdayStamp,
				"inputTokens":      700,
				"cachedReadTokens": 4000,
				"outputTokens":     300,
				"totalTokens":      5000,
				"costUsdTicks":     50000000, // $0.005
				"primaryModelId":   "grok-4.7",
			},
			// Turn 4: outside 30-day window (ignored).
			{
				"turnNumber":     4,
				"endedAt":        oldStamp,
				"inputTokens":    99999,
				"outputTokens":   9999,
				"totalTokens":    109998,
				"costUsdTicks":   900000000,
				"primaryModelId": "grok-4.7",
			},
		},
	})

	// Cross-directory duplicate copy of sess-1 turn 1 must deduplicate by sessionId + turnNumber.
	dupPath := filepath.Join(grokRoot, "sessions", "%2FUsers%2Fdev%2Fproj-copy", "sess-1", "usage.json")
	writeJSONFixture(t, dupPath, map[string]any{
		"sessionId": "sess-1",
		"updatedAt": todayStamp,
		"turns": []map[string]any{
			{
				"turnNumber":          1,
				"endedAt":             todayStamp,
				"inputTokens":         10000,
				"cachedReadTokens":    8000,
				"cacheCreationTokens": 500,
				"outputTokens":        1200,
				"totalTokens":         11200,
				"costUsdTicks":        150000000,
				"primaryModelId":      "grok-4.7",
			},
		},
	})

	// Session 2: empty turns[] falls back to session aggregate.
	sess2Path := filepath.Join(grokRoot, "sessions", "%2FUsers%2Fdev%2Fproj-b", "sess-2", "usage.json")
	writeJSONFixture(t, sess2Path, map[string]any{
		"sessionId": "sess-2",
		"updatedAt": todayStamp,
		"turns":     []any{},
		"session": map[string]any{
			"inputTokens":         2000,
			"cachedReadTokens":    1000,
			"cacheCreationTokens": 0,
			"outputTokens":        500,
			"totalTokens":         2500,
			"costUsdTicks":        25000000, // $0.0025
			"primaryModelId":      "grok-4.6",
		},
	})

	// Session 3: multi-model turn using modelUsage map with exact per-model costUsdTicks.
	sess3Path := filepath.Join(grokRoot, "sessions", "%2FUsers%2Fdev%2Fproj-b", "sess-3", "usage.json")
	writeJSONFixture(t, sess3Path, map[string]any{
		"sessionId": "sess-3",
		"updatedAt": yesterdayStamp,
		"turns": []map[string]any{
			{
				"turnNumber":   1,
				"endedAt":      yesterdayStamp,
				"inputTokens":  1500,
				"outputTokens": 500,
				"totalTokens":  2000,
				"costUsdTicks": 30000000,
				"modelUsage": map[string]any{
					"grok-4.7": map[string]any{
						"inputTokens":  1000,
						"outputTokens": 300,
						"totalTokens":  1300,
						"costUsdTicks": 20000000, // $0.002
					},
					"grok-4.7-build-fast": map[string]any{
						"inputTokens":  500,
						"outputTokens": 200,
						"totalTokens":  700,
						"costUsdTicks": 10000000, // $0.001
					},
				},
			},
		},
	})

	// Session 4: malformed JSON must increment Skipped without breaking other sessions.
	badPath := filepath.Join(grokRoot, "sessions", "%2FUsers%2Fdev%2Fproj-b", "sess-bad", "usage.json")
	writeRawFile(t, badPath, `{not valid json`)

	// Session 5: negative/invalid numbers clamped cleanly; zero-token turn ignored.
	clampPath := filepath.Join(grokRoot, "sessions", "%2FUsers%2Fdev%2Fproj-b", "sess-clamp", "usage.json")
	writeJSONFixture(t, clampPath, map[string]any{
		"sessionId": "sess-clamp",
		"updatedAt": todayStamp,
		"turns": []map[string]any{
			{
				"turnNumber":       1,
				"endedAt":          todayStamp,
				"inputTokens":      -500,
				"cachedReadTokens": 100,
				"outputTokens":     -10,
				"costUsdTicks":     -999,
				"primaryModelId":   "grok-4.5",
			},
			{
				"turnNumber":     2,
				"endedAt":        todayStamp,
				"inputTokens":    0,
				"outputTokens":   0,
				"totalTokens":    0,
				"costUsdTicks":   0,
				"primaryModelId": "grok-4.5",
			},
		},
	})

	report := manager.collect(now)
	if len(report.Providers) != 4 || report.Providers[3].Provider != "grok" {
		t.Fatalf("expected 4 providers with grok fourth: %+v", report.Providers)
	}
	p := report.Providers[3]

	// Today:
	// - sess-1 turn 1: in=10000, cached=8000, writes=500, out=1200 -> 11200, $0.015
	// - sess-1 turn 2: in=1000, cached=200, writes=0, out=300 -> 1300, unpriced
	// - sess-2 session: in=2000, cached=1000, writes=0, out=500 -> 2500, $0.0025
	// - sess-clamp turn 1: in=100 (clamped rawIn=0 + cached=100), cached=100, out=0 -> 100, unpriced
	// Today total: in=13100, cached=9300, writes=500, out=2000, tokens=15100, requests=4, cost=$0.0175, unpriced=1400.
	if p.Today.Tokens != 15100 || p.Today.Input != 13100 || p.Today.Cached != 9300 || p.Today.Writes != 500 || p.Today.Output != 2000 || p.Today.Requests != 4 {
		t.Fatalf("unexpected Today totals: %+v", p.Today)
	}
	if p.Today.Unpriced != 1400 || p.Today.Cost == nil || math.Abs(*p.Today.Cost-0.0175) > 1e-9 {
		t.Fatalf("unexpected Today cost/unpriced: cost=%v unpriced=%d", p.Today.Cost, p.Today.Unpriced)
	}

	// Days30 adds:
	// - sess-1 turn 3 (yesterday): in=4700, cached=4000, out=300 -> 5000, $0.005
	// - sess-3 turn 1 grok-4.7: in=1000, out=300 -> 1300, $0.002
	// - sess-3 turn 1 grok-4.7-build-fast: in=500, out=200 -> 700, $0.001
	// Days30 total: tokens=22100, input=19300, cached=13300, writes=500, output=2800, requests=7, cost=$0.0255, unpriced=1400.
	if p.Days30.Tokens != 22100 || p.Days30.Input != 19300 || p.Days30.Cached != 13300 || p.Days30.Writes != 500 || p.Days30.Output != 2800 || p.Days30.Requests != 7 {
		t.Fatalf("unexpected Days30 totals: %+v", p.Days30)
	}
	if p.Days30.Unpriced != 1400 || p.Days30.Cost == nil || math.Abs(*p.Days30.Cost-0.0255) > 1e-9 {
		t.Fatalf("unexpected Days30 cost/unpriced: cost=%v unpriced=%d", p.Days30.Cost, p.Days30.Unpriced)
	}
	if p.Skipped != 1 {
		t.Fatalf("expected 1 skipped malformed usage.json, got %d", p.Skipped)
	}
	if p.LocalError != "" {
		t.Fatalf("unexpected LocalError on malformed JSON: %q", p.LocalError)
	}

	// Verify per-model breakdown sorted by tokens descending.
	byModel := map[string]UsageModel{}
	for _, m := range p.Models {
		byModel[m.Model] = m
	}
	if m := byModel["grok-4.7"]; m.Tokens != 17500 || m.Cost == nil || math.Abs(*m.Cost-0.022) > 1e-9 || m.Unpriced != 0 {
		t.Fatalf("unexpected grok-4.7 model totals: %+v", m)
	}
	if m := byModel["grok-4.6"]; m.Tokens != 2500 || m.Cost == nil || math.Abs(*m.Cost-0.0025) > 1e-9 {
		t.Fatalf("unexpected grok-4.6 model totals: %+v", m)
	}
	if m := byModel["grok-4.7-build-fast"]; m.Tokens != 2000 || m.Cost == nil || math.Abs(*m.Cost-0.001) > 1e-9 || m.Unpriced != 1300 {
		t.Fatalf("unexpected grok-4.7-build-fast model totals: %+v", m)
	}
	if m := byModel["grok-4.5"]; m.Tokens != 100 || m.Cost != nil || m.Unpriced != 100 {
		t.Fatalf("unexpected grok-4.5 model totals: %+v", m)
	}

	// Verify file cache tracks usage.json and evicts removed files.
	if _, ok := manager.files[sess2Path]; !ok {
		t.Fatal("sess2 usage.json was not cached")
	}
	if err := os.Remove(sess2Path); err != nil {
		t.Fatal(err)
	}
	next := manager.collect(now).Providers[3]
	if next.Days30.Tokens != 19600 || next.Days30.Requests != 6 {
		t.Fatalf("removed usage.json stayed in totals: %+v", next.Days30)
	}
	if _, ok := manager.files[sess2Path]; ok {
		t.Fatal("removed usage.json was not evicted from file cache")
	}

	// Verify GROK_HOME environment variable works when grokHome override is unset.
	managerEnv := usageFixtureManager(t)
	t.Setenv("GROK_HOME", grokRoot)
	pEnv := managerEnv.collect(now).Providers[3]
	if pEnv.Days30.Tokens != 19600 {
		t.Fatalf("expected 19600 tokens via GROK_HOME env, got %+v", pEnv.Days30)
	}
}

func TestApplyGrokQuotaPeriodsCreditsAndEdgeCases(t *testing.T) {
	t.Run("weekly live billing payload", func(t *testing.T) {
		p := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
		payload := map[string]any{
			"config": map[string]any{
				"creditUsagePercent": 31.25,
				"currentPeriod": map[string]any{
					"type":  "USAGE_PERIOD_TYPE_WEEKLY",
					"start": "2026-11-24T00:00:00Z",
					"end":   "2026-12-01T00:00:00Z",
				},
				"onDemandCap":          map[string]any{"val": 0.0},
				"onDemandUsed":         map[string]any{"val": 0.0},
				"prepaidBalance":       map[string]any{"val": 0.0},
				"isUnifiedBillingUser": true,
				"billingPeriodStart":   "2026-11-01T00:00:00Z",
				"billingPeriodEnd":     "2026-12-01T00:00:00Z",
			},
			"subscription_tier": "SuperGrok Plus",
		}
		if err := applyGrokQuota(&p, payload, ""); err != nil {
			t.Fatalf("applyGrokQuota failed: %v", err)
		}
		if p.Plan != "SuperGrok Plus" || p.QuotaSource != "Subscription" || p.Credits != nil {
			t.Fatalf("unexpected plan/source/credits: %+v", p)
		}
		if len(p.Windows) != 1 {
			t.Fatalf("expected 1 window, got %+v", p.Windows)
		}
		w := p.Windows[0]
		wantReset, _ := time.Parse(time.RFC3339, "2026-12-01T00:00:00Z")
		if w.Label != "Weekly" || w.Used != 31.25 || w.Remaining != 68.75 || w.Minutes != 10080 || w.ResetAt != wantReset.Unix() {
			t.Fatalf("unexpected weekly window: %+v", w)
		}
	})

	t.Run("monthly period with prepaid balance and billingPeriodEnd fallback", func(t *testing.T) {
		p := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
		payload := map[string]any{
			"config": map[string]any{
				"creditUsagePercent": 120.0, // clamped to 100
				"currentPeriod": map[string]any{
					"type": "USAGE_PERIOD_TYPE_MONTHLY",
				},
				"prepaidBalance":   map[string]any{"val": 42.5},
				"billingPeriodEnd": "2026-12-31T23:59:59Z",
			},
		}
		if err := applyGrokQuota(&p, payload, "SuperGrok"); err != nil {
			t.Fatalf("applyGrokQuota failed: %v", err)
		}
		if p.Plan != "SuperGrok" || p.Credits == nil || *p.Credits != 42.5 {
			t.Fatalf("unexpected fallback plan or credits: %+v", p)
		}
		w := p.Windows[0]
		if w.Label != "Monthly" || w.Minutes != 43200 || w.Used != 100 || w.Remaining != 0 || w.ResetAt == 0 {
			t.Fatalf("unexpected monthly window: %+v", w)
		}
	})

	t.Run("daily period and negative clamp", func(t *testing.T) {
		p := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
		payload := map[string]any{
			"config": map[string]any{
				"creditUsagePercent": -5.0,
				"currentPeriod": map[string]any{
					"type": "USAGE_PERIOD_TYPE_DAILY",
					"end":  "2026-11-28T00:00:00Z",
				},
			},
			"subscription_tier": "SuperGrok",
		}
		if err := applyGrokQuota(&p, payload, ""); err != nil {
			t.Fatalf("applyGrokQuota failed: %v", err)
		}
		w := p.Windows[0]
		if w.Label != "Daily" || w.Minutes != 1440 || w.Used != 0 || w.Remaining != 100 {
			t.Fatalf("unexpected daily window: %+v", w)
		}
	})

	t.Run("missing creditUsagePercent returns non-fatal error", func(t *testing.T) {
		p := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
		err := applyGrokQuota(&p, map[string]any{"config": map[string]any{}}, "")
		if err == nil || !strings.Contains(err.Error(), "Did Not Report") {
			t.Fatalf("expected missing windows error, got %v", err)
		}
	})
}

func TestFetchGrokQuotaPreChecksAndErrorHandling(t *testing.T) {
	ctx := context.Background()

	t.Run("missing binary", func(t *testing.T) {
		p := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
		err := fetchGrokQuotaWith(ctx, t.TempDir(), t.TempDir(), &p,
			func(string) string { return "" },
			func(context.Context, string, string) (map[string]any, string, error) {
				t.Fatal("RPC should not run when binary is missing")
				return nil, "", nil
			},
		)
		if err == nil || err.Error() != "Grok CLI Not Installed" {
			t.Fatalf("expected 'Grok CLI Not Installed', got %v", err)
		}
	})

	t.Run("missing auth.json skips spawn", func(t *testing.T) {
		p := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
		grokHome := t.TempDir()
		err := fetchGrokQuotaWith(ctx, t.TempDir(), grokHome, &p,
			func(string) string { return "/bin/grok" },
			func(context.Context, string, string) (map[string]any, string, error) {
				t.Fatal("RPC should not run when auth.json is missing")
				return nil, "", nil
			},
		)
		if err == nil || err.Error() != "Grok CLI Not Signed In" {
			t.Fatalf("expected 'Grok CLI Not Signed In', got %v", err)
		}
	})

	t.Run("empty auth.json skips spawn", func(t *testing.T) {
		p := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
		grokHome := t.TempDir()
		writeRawFile(t, filepath.Join(grokHome, "auth.json"), "   {}\n")
		err := fetchGrokQuotaWith(ctx, t.TempDir(), grokHome, &p,
			func(string) string { return "/bin/grok" },
			func(context.Context, string, string) (map[string]any, string, error) {
				t.Fatal("RPC should not run when auth.json is empty")
				return nil, "", nil
			},
		)
		if err == nil || err.Error() != "Grok CLI Not Signed In" {
			t.Fatalf("expected 'Grok CLI Not Signed In', got %v", err)
		}
	})

	t.Run("timeout error", func(t *testing.T) {
		p := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
		grokHome := t.TempDir()
		writeRawFile(t, filepath.Join(grokHome, "auth.json"), `{"https://auth.x.ai::u":{"token":"redacted"}}`)
		err := fetchGrokQuotaWith(ctx, t.TempDir(), grokHome, &p,
			func(string) string { return "/bin/grok" },
			func(context.Context, string, string) (map[string]any, string, error) {
				return nil, "", context.DeadlineExceeded
			},
		)
		if err == nil || err.Error() != "Timed Out Reading Grok Subscription Limits" {
			t.Fatalf("expected timeout Title Case error, got %v", err)
		}
	})

	t.Run("rpc auth failure", func(t *testing.T) {
		p := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
		grokHome := t.TempDir()
		writeRawFile(t, filepath.Join(grokHome, "auth.json"), `{"https://auth.x.ai::u":{"token":"redacted"}}`)
		err := fetchGrokQuotaWith(ctx, t.TempDir(), grokHome, &p,
			func(string) string { return "/bin/grok" },
			func(context.Context, string, string) (map[string]any, string, error) {
				return nil, "", errGrokNotSignedIn
			},
		)
		if err == nil || err.Error() != "Grok CLI Not Signed In" {
			t.Fatalf("expected 'Grok CLI Not Signed In', got %v", err)
		}
	})

	t.Run("rpc generic failure", func(t *testing.T) {
		p := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
		grokHome := t.TempDir()
		writeRawFile(t, filepath.Join(grokHome, "auth.json"), `{"https://auth.x.ai::u":{"token":"redacted"}}`)
		err := fetchGrokQuotaWith(ctx, t.TempDir(), grokHome, &p,
			func(string) string { return "/bin/grok" },
			func(context.Context, string, string) (map[string]any, string, error) {
				return nil, "", errors.New("upstream 503")
			},
		)
		if err == nil || err.Error() != "Could Not Read Grok Subscription Limits" {
			t.Fatalf("expected generic Title Case error, got %v", err)
		}
	})
}

// TestGrokBillingHelperProcess acts as a cross-platform mock `grok agent --no-leader stdio`
// subprocess when GO_WANT_GROK_BILLING_HELPER=1 is set.
func TestGrokBillingHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_GROK_BILLING_HELPER") != "1" {
		return
	}
	mode := os.Getenv("GROK_HELPER_MODE")
	if mode == "hang" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if mode == "stderr-auth-exit" {
		for i := 0; i < 8; i++ {
			fmt.Fprintln(os.Stderr, "Error: user is not signed in; run grok login")
		}
		os.Exit(1)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			// Emit non-JSON banner and a notification before replying to verify filtering.
			fmt.Fprintln(os.Stdout, "grok debug startup banner")
			fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","method":"_x.ai/mcp/init_progress","params":{"status":"ready"}}`)
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":1}}`+"\n", req.ID)
		case "authenticate":
			if mode == "auth-error" {
				fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"cached_token expired"}}`+"\n", req.ID)
				os.Exit(0)
			}
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%d,"result":{"_meta":{"auth_mode":"oauth","subscription_tier":"SuperGrok Plus"}}}`+"\n", req.ID)
		case "_x.ai/billing":
			if mode == "billing-error" {
				fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32001,"message":"billing unavailable"}}`+"\n", req.ID)
				os.Exit(0)
			}
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%d,"result":{"config":{"creditUsagePercent":31.0,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-11-24T00:00:00Z","end":"2026-12-01T00:00:00Z"},"prepaidBalance":{"val":12.5},"billingPeriodEnd":"2026-12-01T00:00:00Z"},"subscription_tier":"SuperGrok Plus"}}`+"\n", req.ID)
		}
	}
	os.Exit(0)
}

func helperGrokBillingCmd(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestGrokBillingHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GO_WANT_GROK_BILLING_HELPER=1",
		"GROK_HELPER_MODE="+mode,
	)
	return cmd
}

func TestRunGrokBillingStdioProcessLifecycle(t *testing.T) {
	grokHome := t.TempDir()

	t.Run("happy path with interleaved notifications", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		data, authTier, err := runGrokBillingCmd(ctx, helperGrokBillingCmd("ok"), grokHome)
		if err != nil {
			t.Fatalf("runGrokBillingCmd failed: %v", err)
		}
		if authTier != "SuperGrok Plus" {
			t.Fatalf("unexpected authTier: %q", authTier)
		}
		p := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
		if err := applyGrokQuota(&p, data, authTier); err != nil {
			t.Fatalf("applyGrokQuota failed: %v", err)
		}
		if p.Plan != "SuperGrok Plus" || len(p.Windows) != 1 || p.Windows[0].Label != "Weekly" || p.Windows[0].Used != 31 || p.Windows[0].Remaining != 69 || p.Credits == nil || *p.Credits != 12.5 {
			t.Fatalf("unexpected parsed quota: %+v", p)
		}
	})

	t.Run("authenticate rpc error maps to not signed in", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, err := runGrokBillingCmd(ctx, helperGrokBillingCmd("auth-error"), grokHome)
		if !errors.Is(err, errGrokNotSignedIn) {
			t.Fatalf("expected errGrokNotSignedIn, got %v", err)
		}
	})

	t.Run("billing rpc error propagates cleanly", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, err := runGrokBillingCmd(ctx, helperGrokBillingCmd("billing-error"), grokHome)
		if err == nil || errors.Is(err, errGrokNotSignedIn) {
			t.Fatalf("expected generic rpc error, got %v", err)
		}
	})

	t.Run("stderr auth error on early exit is race-free", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, err := runGrokBillingCmd(ctx, helperGrokBillingCmd("stderr-auth-exit"), grokHome)
		if !errors.Is(err, errGrokNotSignedIn) {
			t.Fatalf("expected errGrokNotSignedIn on stderr auth exit, got %v", err)
		}
	})

	t.Run("hanging process terminates on timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, _, err := runGrokBillingCmd(ctx, helperGrokBillingCmd("hang"), grokHome)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 4*time.Second {
			t.Fatalf("hanging process did not terminate promptly: %v", elapsed)
		}
	})
}

func TestFindGrokUsageBinaryAndUnixEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", "")
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}

	binName := "grok"
	if runtime.GOOS == "windows" {
		binName = "grok.exe"
	}

	grokBin := filepath.Join(home, ".grok", "bin", binName)
	if err := os.MkdirAll(filepath.Dir(grokBin), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grokBin, []byte("stub"), 0700); err != nil {
		t.Fatal(err)
	}
	if found := findGrokUsageBinary(home); found != grokBin {
		t.Fatalf("expected ~/.grok/bin binary %s, got %q", grokBin, found)
	}

	localBin := filepath.Join(home, ".local", "bin", binName)
	if err := os.MkdirAll(filepath.Dir(localBin), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localBin, []byte("stub"), 0700); err != nil {
		t.Fatal(err)
	}
	if found := findGrokUsageBinary(home); found != localBin {
		t.Fatalf("expected ~/.local/bin binary %s to take priority, got %q", localBin, found)
	}

	if runtime.GOOS == "windows" {
		return
	}

	// End-to-end POSIX mock binary verifying CLI args ("agent --no-leader stdio") and auth.json check.
	script := `#!/bin/sh
if [ "$1" != "agent" ] || [ "$2" != "--no-leader" ] || [ "$3" != "stdio" ]; then
  echo "unexpected args: $*" >&2
  exit 2
fi
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}\n'
      ;;
    *'"method":"authenticate"'*)
      printf '{"jsonrpc":"2.0","id":2,"result":{"_meta":{"subscription_tier":"SuperGrok Plus"}}}\n'
      ;;
    *'"method":"_x.ai/billing"'*)
      printf '{"jsonrpc":"2.0","id":3,"result":{"config":{"creditUsagePercent":25,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-12-01T00:00:00Z"}},"subscription_tier":"SuperGrok Plus"}}\n'
      ;;
  esac
done
`
	if err := os.WriteFile(localBin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	// Without auth.json -> non-fatal Grok CLI Not Signed In.
	pUnsigned := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
	if err := fetchGrokQuota(context.Background(), home, &pUnsigned); err == nil || err.Error() != "Grok CLI Not Signed In" {
		t.Fatalf("expected unsigned error before auth.json exists, got %v", err)
	}

	// With auth.json -> succeeds via real stdio RPC.
	writeRawFile(t, filepath.Join(home, ".grok", "auth.json"), `{"https://auth.x.ai::acct":{"token":"redacted"}}`)
	pSigned := ProviderUsage{Provider: "grok", Windows: []UsageWindow{}}
	if err := fetchGrokQuota(context.Background(), home, &pSigned); err != nil {
		t.Fatalf("fetchGrokQuota failed: %v", err)
	}
	if pSigned.Plan != "SuperGrok Plus" || pSigned.QuotaSource != "Subscription" || len(pSigned.Windows) != 1 || pSigned.Windows[0].Remaining != 75 {
		t.Fatalf("unexpected end-to-end Grok quota: %+v", pSigned)
	}
}

func TestGrokUsageServeFourProviderRefreshAndCacheInvalidation(t *testing.T) {
	m := usageFixtureManager(t)
	now := time.Now()
	stamp := now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	writeJSONFixture(t, filepath.Join(m.home, ".grok", "sessions", "%2Frepo", "sess-live", "usage.json"), map[string]any{
		"sessionId": "sess-live",
		"updatedAt": stamp,
		"turns": []map[string]any{
			{
				"turnNumber":       1,
				"endedAt":          stamp,
				"inputTokens":      1200,
				"cachedReadTokens": 400,
				"outputTokens":     300,
				"totalTokens":      1500,
				"costUsdTicks":     20000000, // $0.002
				"primaryModelId":   "grok-4.7",
			},
		},
	})

	var grokCalls atomic.Int32
	m.quota = func(_ string, p *ProviderUsage) {
		switch p.Provider {
		case "codex":
			p.Windows = []UsageWindow{{Label: "Weekly", Used: 20, Remaining: 80}}
			p.QuotaAt = now.Unix()
		case "claude":
			p.Windows = []UsageWindow{{Label: "Weekly", Used: 35, Remaining: 65}}
			p.QuotaAt = now.Unix()
		case "antigravity":
			p.Windows = []UsageWindow{{Label: "Gemini Weekly", Used: 15, Remaining: 85}}
			p.QuotaAt = now.Unix()
		case "grok":
			grokCalls.Add(1)
			p.QuotaError = "Grok CLI Not Signed In"
		}
	}

	// GET /usage?provider=grok returns HTTP 200 with non-fatal QuotaError.
	rec := httptest.NewRecorder()
	m.serve(rec, httptest.NewRequest(http.MethodGet, "/usage?provider=grok", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for grok provider, got %d: %s", rec.Code, rec.Body.String())
	}
	var ctxReport UsageReport
	if err := json.Unmarshal(rec.Body.Bytes(), &ctxReport); err != nil {
		t.Fatal(err)
	}
	if len(ctxReport.Providers) != 1 || ctxReport.Providers[0].Provider != "grok" || ctxReport.Providers[0].QuotaError != "Grok CLI Not Signed In" {
		t.Fatalf("unexpected grok context quota response: %+v", ctxReport)
	}

	// Concurrent 4-provider refresh under -race preserves all other providers and Grok local totals despite Grok QuotaError.
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
				t.Errorf("invalid 4-provider report: %+v", report.Providers)
			}
			if report.Refreshing {
				t.Errorf("completed report still marked refreshing")
			}
			for idx := 0; idx < 3; idx++ {
				if len(report.Providers[idx].Windows) != 1 {
					t.Errorf("provider %d quota missing: %+v", idx, report.Providers[idx])
				}
			}
			grok := report.Providers[3]
			if grok.QuotaError != "Grok CLI Not Signed In" || grok.Days30.Tokens != 1500 || grok.Today.Tokens != 1500 || grok.Days30.Cost == nil || math.Abs(*grok.Days30.Cost-0.002) > 1e-9 {
				t.Errorf("grok local totals or non-fatal QuotaError wrong: %+v", grok)
			}
		}()
	}
	wg.Wait()
	if grokCalls.Load() != 1 {
		t.Fatalf("expected coalesced single grok quota call, got %d", grokCalls.Load())
	}

	// Legacy 3-provider disk cache must be invalidated automatically on restart.
	legacy3 := UsageReport{
		Host: hostName(),
		Day:  time.Now().Format("2006-01-02"),
		Providers: []ProviderUsage{
			{Provider: "codex", Windows: []UsageWindow{}},
			{Provider: "claude", Windows: []UsageWindow{}},
			{Provider: "antigravity", Windows: []UsageWindow{}},
		},
	}
	if validUsageProviders(legacy3) {
		t.Fatal("3-provider report must not satisfy validUsageProviders")
	}
	if err := os.WriteFile(m.cachePath(), mustJSON(legacy3), 0600); err != nil {
		t.Fatal(err)
	}
	reloaded := newUsageManager(m.home)
	if len(reloaded.cached) != 0 {
		t.Fatal("stale 3-provider disk cache should have been invalidated")
	}
}
