package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

func newUsageManager(home string) *UsageManager {
	m := &UsageManager{home: home, files: map[string]usageFile{}, quota: fetchProviderQuota}
	// Persist local totals only. Subscription readings must be bound to the
	// account currently signed in, so quotas are never restored from disk.
	if info, err := os.Stat(m.cachePath()); err == nil && info.Size() <= 2<<20 {
		if data, err := os.ReadFile(m.cachePath()); err == nil {
			var report UsageReport
			if json.Unmarshal(data, &report) == nil && report.Host == hostName() && report.Day == time.Now().Format("2006-01-02") && validUsageProviders(report) {
				for i := range report.Providers {
					p := report.Providers[i]
					report.Providers[i] = localUsageOnly(p)
				}
				m.cached = mustJSON(report)
			}
		}
	}
	return m
}

func localUsageOnly(p ProviderUsage) ProviderUsage {
	return ProviderUsage{Provider: p.Provider, LocalAt: p.LocalAt, Today: p.Today, Days30: p.Days30, Models: p.Models, LocalError: p.LocalError, Skipped: p.Skipped, Windows: []UsageWindow{}}
}

func mergeLocalUsage(p *ProviderUsage, local ProviderUsage) {
	p.Today, p.Days30, p.Models = local.Today, local.Days30, local.Models
	p.LocalError, p.Skipped, p.LocalAt = local.LocalError, local.Skipped, local.LocalAt
	p.LocalLoading = false
}

type usagePart struct {
	provider int
	quota    ProviderUsage
	locals   *UsageReport
}

// Each worker owns its result until sending it. Only the coordinator merges
// results and publishes immutable JSON. The local worker alone owns m.files.
func (m *UsageManager) collectParts(now time.Time, refresh bool) <-chan usagePart {
	parts := make(chan usagePart, 3)
	for i, provider := range []string{"codex", "claude"} {
		go func(i int, provider string) {
			p, done := m.readQuota(provider, refresh)
			if done != nil {
				<-done
				p, _ = m.readQuota(provider, false)
			}
			parts <- usagePart{provider: i, quota: p}
		}(i, provider)
	}
	go func() {
		locals := m.collectLocal(now)
		parts <- usagePart{locals: &locals}
	}()
	return parts
}

func mergeUsagePart(report *UsageReport, part usagePart) {
	if part.locals != nil {
		for i := range report.Providers {
			mergeLocalUsage(&report.Providers[i], part.locals.Providers[i])
		}
		return
	}
	old := report.Providers[part.provider]
	p := part.quota
	mergeLocalUsage(&p, old)
	p.LocalLoading = old.LocalLoading
	report.Providers[part.provider] = p
}

func validUsageProviders(report UsageReport) bool {
	return len(report.Providers) == 2 && report.Providers[0].Provider == "codex" && report.Providers[1].Provider == "claude"
}

func emptyUsageReport(now time.Time) UsageReport {
	zone, _ := now.Zone()
	return UsageReport{Host: hostName(), Day: now.Format("2006-01-02"), Zone: zone, Providers: []ProviderUsage{{Provider: "codex", Windows: []UsageWindow{}}, {Provider: "claude", Windows: []UsageWindow{}}}}
}

func (m *UsageManager) collect(now time.Time) UsageReport {
	report := emptyUsageReport(now)
	parts := m.collectParts(now, true)
	for i := 0; i < 3; i++ {
		mergeUsagePart(&report, <-parts)
	}
	report.UpdatedAt = now.Unix()
	return report
}

// startLocked coalesces HTTP readers and refresh actions into one collection.
type usageCollection struct {
	done chan struct{}
	data []byte
}

func (m *UsageManager) startLocked(refresh bool) *usageCollection {
	if m.job != nil {
		return m.job
	}
	if !refresh && len(m.cached) > 0 && time.Since(m.cachedAt) < time.Minute {
		return nil
	}
	now := time.Now()
	report := emptyUsageReport(now)
	if len(m.cached) > 0 {
		var previous UsageReport
		if json.Unmarshal(m.cached, &previous) == nil && previous.Day == report.Day && validUsageProviders(previous) {
			report = previous
		}
	}
	report.Refreshing = true
	for i := range report.Providers {
		report.Providers[i].QuotaLoading = true
		report.Providers[i].LocalLoading = true
	}
	m.cached = mustJSON(report)
	m.job = &usageCollection{done: make(chan struct{})}
	job := m.job
	go func() {
		parts := m.collectParts(now, refresh)
		for i := 0; i < 3; i++ {
			mergeUsagePart(&report, <-parts)
			report.Refreshing = i < 2
			report.UpdatedAt = time.Now().Unix()
			data := mustJSON(report)
			m.mu.Lock()
			m.cached = data
			m.mu.Unlock()
		}
		m.persistLocal(report)
		m.mu.Lock()
		job.data = m.cached
		m.cachedAt = time.Now()
		m.job = nil
		close(job.done)
		m.mu.Unlock()
	}()
	return job
}

type usageQuotaEntry struct {
	data []byte
	at   time.Time
	job  chan struct{}
}

// Context details never start a transcript scan. Full usage and context
// requests share these per-provider workers and their immutable cached bytes.
func (m *UsageManager) readQuota(provider string, refresh bool) (ProviderUsage, chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quotas == nil {
		m.quotas = map[string]*usageQuotaEntry{}
	}
	entry := m.quotas[provider]
	if entry == nil {
		entry = &usageQuotaEntry{}
		m.quotas[provider] = entry
	}
	if entry.job == nil && (refresh || len(entry.data) == 0 || time.Since(entry.at) >= time.Minute) {
		entry.job = make(chan struct{})
		done := entry.job
		go func() {
			p := ProviderUsage{Provider: provider, Windows: []UsageWindow{}}
			m.quota(m.home, &p)
			data := mustJSON(p)
			m.mu.Lock()
			entry.data = data
			entry.at = time.Now()
			entry.job = nil
			close(done)
			m.mu.Unlock()
		}()
	}
	p := ProviderUsage{Provider: provider, Windows: []UsageWindow{}}
	if len(entry.data) > 0 {
		_ = json.Unmarshal(entry.data, &p)
	}
	p.QuotaLoading = entry.job != nil
	return p, entry.job
}

func (m *UsageManager) quotaOnly(provider string, refresh bool) []byte {
	p, _ := m.readQuota(provider, refresh)
	report := emptyUsageReport(time.Now())
	report.Providers = []ProviderUsage{p}
	report.Refreshing = p.QuotaLoading
	report.UpdatedAt = p.QuotaAt
	return mustJSON(report)
}

func (m *UsageManager) contextQuota(ctx context.Context, provider string, refresh bool) ([]byte, error) {
	p, done := m.readQuota(provider, refresh)
	if done != nil && p.QuotaAt == 0 {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return m.quotaOnly(provider, false), nil
}

func (m *UsageManager) live(refresh bool) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startLocked(refresh)
	return m.cached
}

func (m *UsageManager) get(ctx context.Context, refresh bool) ([]byte, error) {
	m.mu.Lock()
	done := m.startLocked(refresh)
	if done == nil {
		data := m.cached
		m.mu.Unlock()
		return data, nil
	}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done.done:
		return done.data, nil
	}
}

func (m *UsageManager) cachePath() string {
	return filepath.Join(m.home, ".fastcontrol/usage-cache.json")
}

func (m *UsageManager) persistLocal(report UsageReport) {
	for i := range report.Providers {
		report.Providers[i] = localUsageOnly(report.Providers[i])
	}
	path := m.cachePath()
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".usage-cache-")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(report)
	closeErr := f.Close()
	if err == nil && closeErr == nil {
		_ = os.Rename(f.Name(), path)
	}
}
