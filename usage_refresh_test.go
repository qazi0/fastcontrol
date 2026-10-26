package main

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func readLiveUsage(t *testing.T, m *UsageManager) UsageReport {
	t.Helper()
	var report UsageReport
	if err := json.Unmarshal(m.live(false), &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestUsageProgressDoesNotWaitForTheOtherProvider(t *testing.T) {
	m := usageFixtureManager(t)
	var calls atomic.Int32
	blocked, started := make(chan struct{}), make(chan struct{})
	m.quota = func(_ string, p *ProviderUsage) {
		calls.Add(1)
		if p.Provider == "claude" {
			close(started)
			<-blocked
		}
		p.Windows = []UsageWindow{{Label: "Weekly", Used: 23, Remaining: 77}}
		p.QuotaAt = time.Now().Unix()
	}
	initial := readLiveUsage(t, m)
	if !initial.Refreshing || !initial.Providers[0].LocalLoading || !initial.Providers[0].QuotaLoading {
		t.Fatal("an unfinished reading was presented as complete")
	}
	<-started
	var readers sync.WaitGroup
	for i := 0; i < 16; i++ {
		readers.Add(1)
		go func() { defer readers.Done(); _ = m.live(true) }()
	}
	readers.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for {
		report := readLiveUsage(t, m)
		if !report.Providers[0].QuotaLoading && !report.Providers[0].LocalLoading {
			if len(report.Providers[0].Windows) != 1 || !report.Providers[1].QuotaLoading || !report.Refreshing {
				t.Fatal("independent provider progress was lost")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ready provider blocked behind another subscription")
		}
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatal("overlapping readers started duplicate collection")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.get(ctx, false); err == nil {
		t.Fatal("cancelled waiter did not return")
	}
	close(blocked)
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	data, err := m.get(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var final UsageReport
	if json.Unmarshal(data, &final) != nil || final.Refreshing || final.Providers[1].QuotaLoading {
		t.Fatal("final reading remained pending")
	}
	// A restart can show local totals, but cannot resurrect another account's quota.
	restored := newUsageManager(m.home)
	var persisted UsageReport
	if json.Unmarshal(restored.cached, &persisted) != nil || persisted.Providers[0].LocalAt == 0 || len(persisted.Providers[0].Windows) != 0 || persisted.Providers[0].QuotaAt != 0 {
		t.Fatal("invalid persisted reading")
	}
	if info, err := os.Stat(m.cachePath()); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		t.Fatal("usage cache was not private")
	}
}

func TestContextUsageDoesNotStartLocalOrOtherProviderWork(t *testing.T) {
	m := usageFixtureManager(t)
	var codex, claude atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	m.quota = func(_ string, p *ProviderUsage) {
		if p.Provider == "codex" {
			codex.Add(1)
			close(started)
			<-release
		} else {
			claude.Add(1)
		}
		p.QuotaAt = time.Now().Unix()
		p.Windows = []UsageWindow{{Label: "Weekly", Used: 12, Remaining: 88}}
	}
	var first UsageReport
	if json.Unmarshal(m.quotaOnly("codex", false), &first) != nil || !first.Refreshing || len(first.Providers) != 1 {
		t.Fatal("invalid context response")
	}
	<-started
	m.mu.Lock()
	localStarted := m.job != nil || len(m.cached) > 0
	m.mu.Unlock()
	if localStarted || claude.Load() != 0 {
		t.Fatal("context requested unrelated work")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		p, done := m.readQuota("codex", false)
		if done == nil {
			if len(p.Windows) != 1 {
				t.Fatal("missing quota")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("quota did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := m.get(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if codex.Load() != 1 || claude.Load() != 1 {
		t.Fatal("full usage did not reuse the context quota")
	}
}
