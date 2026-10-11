package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeUsageFixture(t *testing.T, path string, records ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, record := range records {
		if err := json.NewEncoder(file).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
}
func usageFixtureManager(t *testing.T) *UsageManager {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	manager := newUsageManager(home)
	manager.quota = func(_ string, p *ProviderUsage) {}
	return manager
}
func TestUsageRequestMirrorsAndForks(t *testing.T) {
	manager := usageFixtureManager(t)
	now := time.Now()
	stamp := now.Add(-time.Minute).Format(time.RFC3339Nano)
	tokens := map[string]any{"input_tokens": 1000, "cached_input_tokens": 800, "output_tokens": 100}
	modern := map[string]any{"type": "token_usage_record", "timestamp": stamp, "payload": map[string]any{"response_id": "response-a", "usage": tokens, "thread_token_usage": tokens}}
	mirror := map[string]any{"type": "event_msg", "timestamp": stamp, "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": tokens, "last_token_usage": tokens}}}
	context := map[string]any{"type": "turn_context", "payload": map[string]any{"model": "gpt-6.1-sol"}}
	writeUsageFixture(t, filepath.Join(manager.home, ".codex/sessions/a.jsonl"), context, modern, mirror)
	writeUsageFixture(t, filepath.Join(manager.home, ".codex/sessions/fork.jsonl"), context, modern, mirror)
	report := manager.collect(now)
	p := report.Providers[0]
	if p.Days30.Tokens != 1100 || p.Days30.Requests != 1 {
		t.Fatalf("request counted twice: %+v", p.Days30)
	}
	if p.Days30.Cost == nil || *p.Days30.Cost < .00147 || *p.Days30.Cost > .00149 {
		t.Fatalf("cache price: %+v", p.Days30)
	}
	if err := os.Remove(filepath.Join(manager.home, ".codex/sessions/fork.jsonl")); err != nil {
		t.Fatal(err)
	}
	writeUsageFixture(t, filepath.Join(manager.home, ".codex/sessions/a.jsonl"), context)
	if value := manager.collect(now).Providers[0].Days30.Tokens; value != 0 {
		t.Fatalf("removed rows remained: %d", value)
	}
}
func TestClaudeUsageFinalChunksAndCacheWrites(t *testing.T) {
	manager := usageFixtureManager(t)
	now := time.Now()
	row := func(output int) map[string]any {
		return map[string]any{"type": "assistant", "sessionId": "session-a", "requestId": "request-a", "timestamp": now.Add(-time.Minute).Format(time.RFC3339Nano), "message": map[string]any{"id": "message-a", "model": "claude-opus-5-5", "usage": map[string]any{"input_tokens": 20, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 80, "cache_creation": map[string]any{"ephemeral_1h_input_tokens": 50}, "output_tokens": output}}}
	}
	writeUsageFixture(t, filepath.Join(manager.home, ".claude/projects/a/session.jsonl"), row(10), row(30))
	writeUsageFixture(t, filepath.Join(manager.home, ".claude/projects/a/subagents/mirror.jsonl"), row(30))
	p := manager.collect(now).Providers[1]
	if p.Days30.Tokens != 230 || p.Days30.Requests != 1 {
		t.Fatalf("streaming mirror: %+v", p.Days30)
	}
	if p.Days30.Cost == nil || *p.Days30.Cost < .00124 || *p.Days30.Cost > .00126 {
		t.Fatalf("one-hour writes: %+v", p.Days30)
	}
}
func TestUsageDatesUnknownRatesAndLongContext(t *testing.T) {
	manager := usageFixtureManager(t)
	location := time.FixedZone("PKT", 5*3600)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, location)
	context := map[string]any{"type": "turn_context", "payload": map[string]any{"model": "unpriced-model"}}
	row := func(id string, day int) map[string]any {
		return map[string]any{"type": "token_usage_record", "timestamp": now.AddDate(0, 0, day).Format(time.RFC3339Nano), "payload": map[string]any{"response_id": id, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}}
	}
	writeUsageFixture(t, filepath.Join(manager.home, ".codex/sessions/a.jsonl"), context, row("today", 0), row("old", -29), row("outside", -30))
	report := manager.collect(now)
	p := report.Providers[0]
	if p.Today.Tokens != 12 || p.Days30.Tokens != 24 || p.Days30.Unpriced != 24 || p.Days30.Cost != nil {
		t.Fatalf("date or unknown-rate contract: %+v", p)
	}
	if cost, ok := estimateUsageCost("codex", "gpt-6.1-sol", "", usageTokens{Input: 300000, Cached: 200000, Output: 1000}); !ok || cost < .454 || cost > .456 {
		t.Fatalf("long context: %v %v", cost, ok)
	}
}
func TestQuotaUnknownWindowsAndResetCredits(t *testing.T) {
	p := ProviderUsage{Provider: "codex"}
	applyCodexQuota(&p, map[string]any{"plan_type": "pro", "rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": float64(12.5), "limit_window_seconds": float64(604800), "reset_at": float64(1800000000)}, "secondary_window": nil}, "rate_limit_reset_credits": map[string]any{"available_count": float64(2)}})
	if len(p.Windows) != 1 || p.Windows[0].Remaining != 87.5 || p.Windows[0].Label != "Weekly" || p.ResetCredits == nil || *p.ResetCredits != 2 {
		t.Fatalf("quota parse: %+v", p)
	}
	p = ProviderUsage{Provider: "claude"}
	applyClaudeQuota(&p, map[string]any{"seven_day": map[string]any{"utilization": float64(40), "resets_at": "2026-10-13T14:00:00Z"}, "five_hour": nil})
	if len(p.Windows) != 1 || p.Windows[0].Remaining != 60 || p.Windows[0].ResetAt == 0 {
		t.Fatalf("Claude quota parse: %+v", p)
	}
}

func TestUsageLegacyCounterOffsetDoesNotBecomeNewSpend(t *testing.T) {
	manager := usageFixtureManager(t)
	now := time.Now()
	stamp := now.Add(-time.Minute).Format(time.RFC3339Nano)
	context := map[string]any{"type": "turn_context", "payload": map[string]any{"model": "gpt-6.1-sol"}}
	legacy := func(at string, total, last int) map[string]any {
		return map[string]any{"type": "event_msg", "timestamp": at, "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": map[string]any{"input_tokens": total}, "last_token_usage": map[string]any{"input_tokens": last}}}}
	}
	modern := map[string]any{"type": "token_usage_record", "timestamp": stamp, "payload": map[string]any{"response_id": "response-new", "usage": map[string]any{"input_tokens": 200}, "thread_token_usage": map[string]any{"input_tokens": 200}}}
	writeUsageFixture(t, filepath.Join(manager.home, ".codex/sessions/a.jsonl"), context, legacy(now.Add(-time.Hour).Format(time.RFC3339Nano), 1000000, 100), modern, legacy(stamp, 1000200, 200), legacy(now.Add(-30*time.Second).Format(time.RFC3339Nano), 1000200, 200))
	if actual := manager.collect(now).Providers[0].Days30.Tokens; actual != 300 {
		t.Fatalf("inherited counter billed as new activity: %d", actual)
	}
}

func TestUsagePlanLabelStaysBoundToItsAccount(t *testing.T) {
	home := t.TempDir()
	account := "account-a"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(account)))
	writeUsageFixture(t, filepath.Join(home, ".fastcontrol/usage-plans.json"), map[string]any{"codex": map[string]any{"account": hash, "basePlan": "pro", "label": "Pro 20X"}})
	p := ProviderUsage{Provider: "codex", Plan: "pro"}
	applyUsagePlanLabel(home, &p, account)
	if p.Plan != "Pro 20X" {
		t.Fatal("known tier label missing")
	}
	p = ProviderUsage{Provider: "codex", Plan: "pro"}
	applyUsagePlanLabel(home, &p, "different-account")
	if p.Plan != "pro" {
		t.Fatal("tier label leaked to another account")
	}
}
