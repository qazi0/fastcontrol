package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type UsageWindow struct {
	Label     string  `json:"label"`
	Remaining float64 `json:"remaining"`
	Used      float64 `json:"used"`
	ResetAt   int64   `json:"resetAt,omitempty"`
	Minutes   int64   `json:"minutes,omitempty"`
}
type UsageTotals struct {
	Tokens   int64    `json:"tokens"`
	Input    int64    `json:"input"`
	Cached   int64    `json:"cached"`
	Writes   int64    `json:"writes"`
	Output   int64    `json:"output"`
	Requests int64    `json:"requests"`
	Cost     *float64 `json:"costUsd"`
	Unpriced int64    `json:"unpricedTokens,omitempty"`
	cost     float64
	priced   int64
}
type UsageModel struct {
	Model string `json:"model"`
	UsageTotals
}
type ProviderUsage struct {
	Provider     string        `json:"provider"`
	QuotaLoading bool          `json:"quotaLoading,omitempty"`
	LocalLoading bool          `json:"localLoading,omitempty"`
	LocalAt      int64         `json:"localAt,omitempty"`
	Plan         string        `json:"plan,omitempty"`
	Windows      []UsageWindow `json:"windows"`
	QuotaError   string        `json:"quotaError,omitempty"`
	QuotaSource  string        `json:"quotaSource,omitempty"`
	QuotaAt      int64         `json:"quotaAt,omitempty"`
	Credits      *float64      `json:"credits,omitempty"`
	ResetCredits *int64        `json:"resetCredits,omitempty"`
	Today        UsageTotals   `json:"today"`
	Days30       UsageTotals   `json:"days30"`
	Models       []UsageModel  `json:"models"`
	LocalError   string        `json:"localError,omitempty"`
	Skipped      int           `json:"skipped,omitempty"`
}
type UsageReport struct {
	Refreshing bool            `json:"refreshing,omitempty"`
	Host       string          `json:"host"`
	Day        string          `json:"day"`
	Zone       string          `json:"zone"`
	UpdatedAt  int64           `json:"updatedAt"`
	Providers  []ProviderUsage `json:"providers"`
}
type usageTokens struct{ Input, Cached, Write, Write1h, Output, Reasoning int64 }
type usageRow struct {
	Key, Model, Tier string
	At               time.Time
	Tokens           usageTokens
	Cost             float64
	HasCost          bool
}
type usageFile struct {
	Size     int64
	Mod      time.Time
	Rows     []usageRow
	Skipped  int
	Provider string
}

var usageProviders = []string{"codex", "claude", "antigravity", "grok"}

func isUsageProvider(provider string) bool {
	for _, p := range usageProviders {
		if p == provider {
			return true
		}
	}
	return false
}

type UsageManager struct {
	home             string
	antigravityRoots []string // optional override for Antigravity app-data roots (tests)
	grokHome         string   // optional override for Grok home directory (tests)
	mu               sync.Mutex
	cached           []byte
	cachedAt         time.Time
	job              *usageCollection
	quotas           map[string]*usageQuotaEntry // guarded by mu; independent of local scans
	// Only the single snapshot worker reads or writes this file index.
	files map[string]usageFile
	quota func(string, *ProviderUsage)
}

func (m *UsageManager) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Use GET", 405)
		return
	}
	var b []byte
	var err error
	provider := r.URL.Query().Get("provider")
	if provider != "" && !isUsageProvider(provider) {
		http.Error(w, "Unknown Provider", 400)
		return
	}
	if provider != "" {
		b, err = m.contextQuota(r.Context(), provider, r.URL.Query().Get("refresh") == "1")
	} else if r.URL.Query().Get("live") == "1" {
		b = m.live(r.URL.Query().Get("refresh") == "1")
	} else {
		b, err = m.get(r.Context(), r.URL.Query().Get("refresh") == "1")
	}
	if err != nil {
		http.Error(w, "Usage Refresh Timed Out", 504)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}
func usageRoot(home, provider string) string {
	variable, fallback := "CODEX_HOME", ".codex"
	switch provider {
	case "claude":
		variable = "CLAUDE_CONFIG_DIR"
		fallback = ".claude"
	case "grok":
		variable = "GROK_HOME"
		fallback = ".grok"
	}
	if value := strings.TrimSpace(os.Getenv(variable)); value != "" {
		return filepath.Clean(value)
	}
	if strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, fallback)
}
func grokUsageHome(home, override string) string {
	if value := strings.TrimSpace(override); value != "" {
		return filepath.Clean(value)
	}
	return usageRoot(home, "grok")
}
func antigravityUsageRoots(home string, override []string) []string {
	var raw []string
	if len(override) > 0 {
		raw = override
	} else if value := strings.TrimSpace(os.Getenv("ANTIGRAVITY_HOME")); value != "" {
		raw = filepath.SplitList(value)
	} else {
		raw = []string{
			filepath.Join(home, ".gemini", "antigravity"),
			filepath.Join(home, ".gemini", "antigravity-cli"),
		}
	}
	seen := map[string]bool{}
	roots := make([]string, 0, len(raw))
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		clean := filepath.Clean(r)
		if !seen[clean] {
			seen[clean] = true
			roots = append(roots, clean)
		}
	}
	return roots
}
func (m *UsageManager) ingestUsageFile(path, provider string, st os.FileInfo, since, now time.Time, seenFiles map[string]bool, rows map[string]usageRow, rowProvider map[string]string, local *ProviderUsage) error {
	if st.IsDir() || st.ModTime().Before(since) {
		return nil
	}
	seenFiles[path] = true
	cached, ok := m.files[path]
	if !ok || cached.Size != st.Size() || !cached.Mod.Equal(st.ModTime()) {
		data, skipped, err := parseUsageFile(path, provider)
		if err != nil {
			return err
		}
		cached = usageFile{Size: st.Size(), Mod: st.ModTime(), Rows: data, Skipped: skipped, Provider: provider}
		m.files[path] = cached
	}
	local.Skipped += cached.Skipped
	for _, row := range cached.Rows {
		if row.At.Before(since) || row.At.After(now.Add(time.Minute)) {
			continue
		}
		key := provider + ":" + row.Key
		prior, exists := rows[key]
		if !exists || row.Tokens.Input+row.Tokens.Output > prior.Tokens.Input+prior.Tokens.Output || (row.Tokens == prior.Tokens && (row.At.After(prior.At) || (row.At.Equal(prior.At) && row.Cost > prior.Cost))) {
			rows[key] = row
			rowProvider[key] = provider
		}
	}
	return nil
}
func (m *UsageManager) collectLocal(now time.Time) UsageReport {
	zone, _ := now.Zone()
	providers := make([]ProviderUsage, len(usageProviders))
	locals := make(map[string]ProviderUsage, len(usageProviders))
	models := make(map[string]map[string]*UsageModel, len(usageProviders))
	for i, provider := range usageProviders {
		providers[i] = ProviderUsage{Provider: provider}
		locals[provider] = ProviderUsage{Provider: provider}
		models[provider] = map[string]*UsageModel{}
	}
	report := UsageReport{Host: hostName(), Day: now.Format("2006-01-02"), Zone: zone, UpdatedAt: now.Unix(), Providers: providers}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	since := midnight.AddDate(0, 0, -29)
	seenFiles := map[string]bool{}
	rows := map[string]usageRow{}
	rowProvider := map[string]string{}
	for _, provider := range usageProviders {
		local := locals[provider]
		if provider == "antigravity" {
			for _, root := range antigravityUsageRoots(m.home, m.antigravityRoots) {
				brainDir := filepath.Join(root, "brain")
				entries, err := os.ReadDir(brainDir)
				if err != nil {
					if !os.IsNotExist(err) {
						local.LocalError = "Some Local Logs Could Not Be Read"
					}
					continue
				}
				for _, entry := range entries {
					if !entry.IsDir() {
						continue
					}
					path := filepath.Join(brainDir, entry.Name(), ".system_generated", "logs", "transcript.jsonl")
					st, err := os.Stat(path)
					if err != nil {
						if !os.IsNotExist(err) {
							local.LocalError = "Some Local Logs Could Not Be Read"
						}
						continue
					}
					if err := m.ingestUsageFile(path, provider, st, since, now, seenFiles, rows, rowProvider, &local); err != nil {
						local.LocalError = "Some Local Logs Could Not Be Read"
					}
				}
			}
			locals[provider] = local
			continue
		}
		if provider == "grok" {
			if root := grokUsageHome(m.home, m.grokHome); root != "" {
				sessionsDir := filepath.Join(root, "sessions")
				cwdEntries, err := os.ReadDir(sessionsDir)
				if err != nil {
					if !os.IsNotExist(err) {
						local.LocalError = "Some Local Logs Could Not Be Read"
					}
				} else {
					for _, cwdEntry := range cwdEntries {
						if !cwdEntry.IsDir() {
							continue
						}
						cwdDir := filepath.Join(sessionsDir, cwdEntry.Name())
						sessionEntries, err := os.ReadDir(cwdDir)
						if err != nil {
							if !os.IsNotExist(err) {
								local.LocalError = "Some Local Logs Could Not Be Read"
							}
							continue
						}
						for _, sessionEntry := range sessionEntries {
							if !sessionEntry.IsDir() {
								continue
							}
							path := filepath.Join(cwdDir, sessionEntry.Name(), "usage.json")
							st, err := os.Stat(path)
							if err != nil {
								if !os.IsNotExist(err) {
									local.LocalError = "Some Local Logs Could Not Be Read"
								}
								continue
							}
							if err := m.ingestUsageFile(path, provider, st, since, now, seenFiles, rows, rowProvider, &local); err != nil {
								local.LocalError = "Some Local Logs Could Not Be Read"
							}
						}
					}
				}
			}
			locals[provider] = local
			continue
		}
		root := usageRoot(m.home, provider)
		if root == "" {
			locals[provider] = local
			continue
		}
		folders := []string{"sessions", "archived_sessions"}
		if provider == "claude" {
			folders = []string{"projects"}
		}
		for _, folder := range folders {
			err := filepath.WalkDir(filepath.Join(root, folder), func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					if os.IsNotExist(err) {
						return nil
					}
					return err
				}
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
					return nil
				}
				st, err := entry.Info()
				if err != nil {
					return err
				}
				return m.ingestUsageFile(path, provider, st, since, now, seenFiles, rows, rowProvider, &local)
			})
			if err != nil {
				local.LocalError = "Some Local Logs Could Not Be Read"
			}
		}
		locals[provider] = local
	}
	for path := range m.files {
		if !seenFiles[path] {
			delete(m.files, path)
		}
	}
	for key, row := range rows {
		provider := rowProvider[key]
		local := locals[provider]
		local.Days30.add(row, provider)
		if !row.At.Before(midnight) {
			local.Today.add(row, provider)
		}
		model := models[provider][row.Model]
		if model == nil {
			model = &UsageModel{Model: row.Model}
			models[provider][row.Model] = model
		}
		model.UsageTotals.add(row, provider)
		locals[provider] = local
	}
	for i := range report.Providers {
		p := &report.Providers[i]
		local := locals[p.Provider]
		p.LocalAt = now.Unix()
		p.Today = local.Today
		p.Days30 = local.Days30
		p.LocalError = local.LocalError
		p.Skipped = local.Skipped
		p.Today.finish()
		p.Days30.finish()
		p.Models = []UsageModel{}
		for _, model := range models[p.Provider] {
			model.finish()
			p.Models = append(p.Models, *model)
		}
		sort.Slice(p.Models, func(i, j int) bool {
			if p.Models[i].Tokens == p.Models[j].Tokens {
				return p.Models[i].Model < p.Models[j].Model
			}
			return p.Models[i].Tokens > p.Models[j].Tokens
		})
		if p.Windows == nil {
			p.Windows = []UsageWindow{}
		}
	}
	return report
}
func (u *UsageTotals) add(row usageRow, provider string) {
	t := row.Tokens
	u.Input += t.Input
	u.Cached += t.Cached
	u.Writes += t.Write
	u.Output += t.Output
	u.Tokens += t.Input + t.Output
	u.Requests++
	if row.HasCost {
		u.cost += row.Cost
		u.priced++
	} else if provider != "grok" {
		if cost, ok := estimateUsageCost(provider, row.Model, row.Tier, t); ok {
			u.cost += cost
			u.priced++
		} else {
			u.Unpriced += t.Input + t.Output
		}
	} else {
		u.Unpriced += t.Input + t.Output
	}
}
func (u *UsageTotals) finish() {
	if u.Requests == 0 || u.priced > 0 {
		value := u.cost
		u.Cost = &value
	}
}

func clampTokenFloat(v float64) int64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 9e15 {
		return 0
	}
	return int64(v)
}
func tokenInt(m map[string]any, key string) int64 {
	v, _ := m[key].(float64)
	return clampTokenFloat(v)
}
func parseAntigravityUsageTokens(input, cacheRead, output float64) usageTokens {
	uncached := clampTokenFloat(input)
	cached := clampTokenFloat(cacheRead)
	out := clampTokenFloat(output)
	return usageTokens{
		Input:  uncached + cached,
		Cached: cached,
		Output: out,
	}
}
func parseUsageTokens(m map[string]any, provider string) usageTokens {
	t := usageTokens{Input: tokenInt(m, "input_tokens"), Output: tokenInt(m, "output_tokens"), Reasoning: tokenInt(m, "reasoning_output_tokens")}
	switch provider {
	case "claude":
		t.Cached = tokenInt(m, "cache_read_input_tokens")
		t.Write = tokenInt(m, "cache_creation_input_tokens")
		t.Write1h = tokenInt(mapValue(m["cache_creation"]), "ephemeral_1h_input_tokens")
		t.Input += t.Cached + t.Write
	case "antigravity":
		t.Cached = tokenInt(m, "cache_read_tokens")
		t.Input += t.Cached
	default:
		t.Cached = tokenInt(m, "cached_input_tokens")
		if t.Cached == 0 {
			t.Cached = tokenInt(m, "cache_read_input_tokens")
		}
		t.Write = tokenInt(m, "cache_write_input_tokens")
	}
	t.Cached = min(t.Cached, t.Input)
	t.Write = min(t.Write, t.Input-t.Cached)
	t.Write1h = min(t.Write1h, t.Write)
	return t
}
func antigravityUsageSession(path string) string {
	logsDir := filepath.Dir(path)
	sysDir := filepath.Dir(logsDir)
	if filepath.Base(logsDir) == "logs" && filepath.Base(sysDir) == ".system_generated" {
		if id := filepath.Base(filepath.Dir(sysDir)); id != "" && id != "." && id != string(filepath.Separator) {
			return id
		}
	}
	return filepath.Base(path)
}
func usageDelta(current, previous usageTokens) usageTokens {
	return usageTokens{Input: max(0, current.Input-previous.Input), Cached: max(0, current.Cached-previous.Cached), Write: max(0, current.Write-previous.Write), Output: max(0, current.Output-previous.Output)}
}
func boundedUsageLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	var prefix []byte
	oversized := false
	for {
		part, err := reader.ReadSlice('\n')
		if len(prefix) < 4096 {
			prefix = append(prefix, part[:min(len(part), 4096-len(prefix))]...)
		}
		if len(line)+len(part) > 8<<20 {
			oversized = true
			line = nil
		}
		if !oversized {
			line = append(line, part...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if oversized {
			return prefix, true, err
		}
		return line, oversized, err
	}
}

const (
	grokCostTicksPerUSD  = 1e10
	grokUsageMaxFileSize = 16 << 20
)

type grokModelUsageEntry struct {
	InputTokens              float64 `json:"inputTokens"`
	CachedReadTokens         float64 `json:"cachedReadTokens"`
	CacheReadInputTokens     float64 `json:"cacheReadInputTokens"`
	CacheCreationTokens      float64 `json:"cacheCreationTokens"`
	CacheCreationInputTokens float64 `json:"cacheCreationInputTokens"`
	OutputTokens             float64 `json:"outputTokens"`
	ReasoningTokens          float64 `json:"reasoningTokens"`
	TotalTokens              float64 `json:"totalTokens"`
	CostUsdTicks             float64 `json:"costUsdTicks"`
	CostUSD                  float64 `json:"costUSD"`
	CostUsd                  float64 `json:"costUsd"`
	ModelCalls               float64 `json:"modelCalls"`
}

type grokUsageSummary struct {
	InputTokens              float64                        `json:"inputTokens"`
	CachedReadTokens         float64                        `json:"cachedReadTokens"`
	CacheReadInputTokens     float64                        `json:"cacheReadInputTokens"`
	CacheCreationTokens      float64                        `json:"cacheCreationTokens"`
	CacheCreationInputTokens float64                        `json:"cacheCreationInputTokens"`
	OutputTokens             float64                        `json:"outputTokens"`
	ReasoningTokens          float64                        `json:"reasoningTokens"`
	TotalTokens              float64                        `json:"totalTokens"`
	CostUsdTicks             float64                        `json:"costUsdTicks"`
	CostUSD                  float64                        `json:"costUSD"`
	CostUsd                  float64                        `json:"costUsd"`
	ModelCalls               float64                        `json:"modelCalls"`
	TurnCount                float64                        `json:"turnCount"`
	ModelID                  string                         `json:"modelId"`
	PrimaryModelID           string                         `json:"primaryModelId"`
	ModelUsage               map[string]grokModelUsageEntry `json:"modelUsage"`
}

type grokUsageTurn struct {
	TurnNumber               *int64                         `json:"turnNumber"`
	TurnID                   string                         `json:"turnId"`
	EndedAt                  json.RawMessage                `json:"endedAt"`
	InputTokens              float64                        `json:"inputTokens"`
	CachedReadTokens         float64                        `json:"cachedReadTokens"`
	CacheReadInputTokens     float64                        `json:"cacheReadInputTokens"`
	CacheCreationTokens      float64                        `json:"cacheCreationTokens"`
	CacheCreationInputTokens float64                        `json:"cacheCreationInputTokens"`
	OutputTokens             float64                        `json:"outputTokens"`
	ReasoningTokens          float64                        `json:"reasoningTokens"`
	TotalTokens              float64                        `json:"totalTokens"`
	CostUsdTicks             float64                        `json:"costUsdTicks"`
	CostUSD                  float64                        `json:"costUSD"`
	CostUsd                  float64                        `json:"costUsd"`
	ModelCalls               float64                        `json:"modelCalls"`
	ModelID                  string                         `json:"modelId"`
	PrimaryModelID           string                         `json:"primaryModelId"`
	ModelUsage               map[string]grokModelUsageEntry `json:"modelUsage"`
}

type grokUsageDocument struct {
	SessionID string            `json:"sessionId"`
	UpdatedAt json.RawMessage   `json:"updatedAt"`
	Session   *grokUsageSummary `json:"session"`
	Turns     []grokUsageTurn   `json:"turns"`
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func firstNonEmptyUsageString(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func parseGrokTimestamp(raw json.RawMessage) time.Time {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return time.Time{}
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return time.Time{}
		}
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil && !ts.IsZero() && ts.Unix() > 0 {
			return ts
		}
		return time.Time{}
	}
	var n float64
	if json.Unmarshal(trimmed, &n) == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 {
		if n >= 1e12 {
			return time.UnixMilli(int64(n))
		}
		return time.Unix(int64(n), 0)
	}
	return time.Time{}
}

func parseGrokCost(ticks, usd, usdAlt float64) (float64, bool) {
	if !math.IsNaN(ticks) && !math.IsInf(ticks, 0) && ticks > 0 && ticks <= 9e15 {
		return ticks / grokCostTicksPerUSD, true
	}
	if !math.IsNaN(usd) && !math.IsInf(usd, 0) && usd > 0 && usd <= 1e6 {
		return usd, true
	}
	if !math.IsNaN(usdAlt) && !math.IsInf(usdAlt, 0) && usdAlt > 0 && usdAlt <= 1e6 {
		return usdAlt, true
	}
	return 0, false
}

func parseGrokUsageTokens(input, cachedRead, cacheReadAlt, cacheCreate, cacheCreateAlt, output, reasoning, total float64) usageTokens {
	rawIn := clampTokenFloat(input)
	usedAltCache := false
	cached := clampTokenFloat(cachedRead)
	if cached == 0 && cacheReadAlt > 0 {
		cached = clampTokenFloat(cacheReadAlt)
		usedAltCache = cached > 0
	}
	write := clampTokenFloat(cacheCreate)
	if write == 0 && cacheCreateAlt > 0 {
		write = clampTokenFloat(cacheCreateAlt)
		usedAltCache = usedAltCache || write > 0
	}
	out := clampTokenFloat(output)
	reason := clampTokenFloat(reasoning)
	tot := clampTokenFloat(total)

	fullIn := rawIn
	if cached+write > 0 {
		if tot > 0 {
			diffUncached := absInt64((rawIn + cached + write + out) - tot)
			diffFull := absInt64((rawIn + out) - tot)
			if diffUncached < diffFull {
				fullIn = rawIn + cached + write
			}
		} else if usedAltCache || rawIn < cached+write {
			fullIn = rawIn + cached + write
		}
	}
	if fullIn == 0 && out == 0 && tot > 0 {
		fullIn = tot
	}
	cached = min(cached, fullIn)
	write = min(write, fullIn-cached)
	return usageTokens{
		Input:     fullIn,
		Cached:    cached,
		Write:     write,
		Output:    out,
		Reasoning: reason,
	}
}

func appendGrokUsageRows(
	dst *[]usageRow,
	keyPrefix string,
	at time.Time,
	explicitModel, fallbackModel string,
	t usageTokens,
	cost float64,
	hasCost bool,
	modelUsage map[string]grokModelUsageEntry,
) {
	if at.IsZero() {
		return
	}
	if len(modelUsage) == 1 {
		for rawModel, entry := range modelUsage {
			model := firstNonEmptyUsageString(explicitModel, rawModel, fallbackModel)
			if t.Input+t.Output == 0 {
				t = parseGrokUsageTokens(
					entry.InputTokens,
					entry.CachedReadTokens, entry.CacheReadInputTokens,
					entry.CacheCreationTokens, entry.CacheCreationInputTokens,
					entry.OutputTokens, entry.ReasoningTokens, entry.TotalTokens,
				)
			}
			if !hasCost {
				cost, hasCost = parseGrokCost(entry.CostUsdTicks, entry.CostUSD, entry.CostUsd)
			}
			if t.Input+t.Output > 0 {
				*dst = append(*dst, usageRow{Key: keyPrefix, At: at, Model: model, Tokens: t, Cost: cost, HasCost: hasCost})
			}
			return
		}
	}
	if len(modelUsage) > 1 {
		keys := make([]string, 0, len(modelUsage))
		for k := range modelUsage {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		cands := make([]usageRow, 0, len(keys))
		anyModelCost := false
		for _, rawModel := range keys {
			entry := modelUsage[rawModel]
			mt := parseGrokUsageTokens(
				entry.InputTokens,
				entry.CachedReadTokens, entry.CacheReadInputTokens,
				entry.CacheCreationTokens, entry.CacheCreationInputTokens,
				entry.OutputTokens, entry.ReasoningTokens, entry.TotalTokens,
			)
			if mt.Input+mt.Output == 0 {
				continue
			}
			mCost, mHasCost := parseGrokCost(entry.CostUsdTicks, entry.CostUSD, entry.CostUsd)
			if mHasCost {
				anyModelCost = true
			}
			model := strings.TrimSpace(rawModel)
			cands = append(cands, usageRow{
				Key:     keyPrefix + ":" + model,
				At:      at,
				Model:   model,
				Tokens:  mt,
				Cost:    mCost,
				HasCost: mHasCost,
			})
		}
		if len(cands) > 0 && (anyModelCost || !hasCost) {
			*dst = append(*dst, cands...)
			return
		}
	}
	if t.Input+t.Output > 0 {
		*dst = append(*dst, usageRow{
			Key:     keyPrefix,
			At:      at,
			Model:   firstNonEmptyUsageString(explicitModel, fallbackModel),
			Tokens:  t,
			Cost:    cost,
			HasCost: hasCost,
		})
	}
}

func parseGrokUsageFile(path string) ([]usageRow, int, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	if st.Size() > grokUsageMaxFileSize {
		return nil, 1, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var doc grokUsageDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, 1, nil
	}
	sessionID := strings.TrimSpace(doc.SessionID)
	if sessionID == "" {
		sessionID = filepath.Base(filepath.Dir(path))
	}
	updatedAt := parseGrokTimestamp(doc.UpdatedAt)
	sessionModel := ""
	if doc.Session != nil {
		sessionModel = firstNonEmptyUsageString(doc.Session.ModelID, doc.Session.PrimaryModelID)
	}
	result := []usageRow{}
	if len(doc.Turns) > 0 {
		for idx, turn := range doc.Turns {
			at := parseGrokTimestamp(turn.EndedAt)
			if at.IsZero() {
				at = updatedAt
			}
			var key string
			switch {
			case turn.TurnNumber != nil:
				key = fmt.Sprintf("turn:%s:%d", sessionID, *turn.TurnNumber)
			case strings.TrimSpace(turn.TurnID) != "":
				key = fmt.Sprintf("turn:%s:%s", sessionID, strings.TrimSpace(turn.TurnID))
			default:
				key = fmt.Sprintf("turn:%s:idx:%d:%d", sessionID, idx, at.UnixNano())
			}
			turnModel := firstNonEmptyUsageString(turn.ModelID, turn.PrimaryModelID)
			tokens := parseGrokUsageTokens(
				turn.InputTokens,
				turn.CachedReadTokens, turn.CacheReadInputTokens,
				turn.CacheCreationTokens, turn.CacheCreationInputTokens,
				turn.OutputTokens, turn.ReasoningTokens, turn.TotalTokens,
			)
			cost, hasCost := parseGrokCost(turn.CostUsdTicks, turn.CostUSD, turn.CostUsd)
			appendGrokUsageRows(&result, key, at, turnModel, sessionModel, tokens, cost, hasCost, turn.ModelUsage)
		}
		return result, 0, nil
	}
	if doc.Session != nil {
		tokens := parseGrokUsageTokens(
			doc.Session.InputTokens,
			doc.Session.CachedReadTokens, doc.Session.CacheReadInputTokens,
			doc.Session.CacheCreationTokens, doc.Session.CacheCreationInputTokens,
			doc.Session.OutputTokens, doc.Session.ReasoningTokens, doc.Session.TotalTokens,
		)
		cost, hasCost := parseGrokCost(doc.Session.CostUsdTicks, doc.Session.CostUSD, doc.Session.CostUsd)
		appendGrokUsageRows(&result, "session:"+sessionID, updatedAt, sessionModel, "", tokens, cost, hasCost, doc.Session.ModelUsage)
	}
	return result, 0, nil
}

func parseUsageFile(path, provider string) ([]usageRow, int, error) {
	if provider == "grok" {
		return parseGrokUsageFile(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 64<<10)
	result := []usageRow{}
	skipped := 0
	model := ""
	tier := ""
	session := filepath.Base(path)
	if provider == "antigravity" {
		session = antigravityUsageSession(path)
	}
	previous := usageTokens{}
	havePrevious := false
	var latestModern usageTokens
	var latestModernAt time.Time
	modernPending := false
	forkAt := time.Time{}
	for {
		line, oversized, readErr := boundedUsageLine(reader)
		if oversized {
			kind := []byte("\"type\":\"token_usage_record\"")
			spaced := []byte("\"type\": \"token_usage_record\"")
			if provider == "claude" {
				kind = []byte("\"type\":\"assistant\"")
				spaced = []byte("\"type\": \"assistant\"")
			} else if provider == "antigravity" {
				kind = []byte("\"type\":\"PLANNER_RESPONSE\"")
				spaced = []byte("\"type\": \"PLANNER_RESPONSE\"")
			}
			if bytes.Contains(line, kind) || bytes.Contains(line, spaced) {
				skipped++
			}
			line = nil
		}
		if provider == "antigravity" {
			if len(line) > 0 && bytes.Contains(line, []byte("PLANNER_RESPONSE")) {
				var entry struct {
					Type            string  `json:"type"`
					CreatedAt       string  `json:"created_at"`
					StepIndex       *int64  `json:"step_index"`
					Model           string  `json:"model"`
					InputTokens     float64 `json:"input_tokens"`
					CacheReadTokens float64 `json:"cache_read_tokens"`
					OutputTokens    float64 `json:"output_tokens"`
				}
				if json.Unmarshal(line, &entry) == nil && entry.Type == "PLANNER_RESPONSE" {
					if at, err := time.Parse(time.RFC3339Nano, entry.CreatedAt); err == nil && !at.IsZero() {
						t := parseAntigravityUsageTokens(entry.InputTokens, entry.CacheReadTokens, entry.OutputTokens)
						if t.Input+t.Output > 0 {
							key := ""
							if entry.StepIndex != nil {
								key = fmt.Sprintf("step:%s:%d", session, *entry.StepIndex)
							} else {
								key = fmt.Sprintf("row:%s:%s:%d:%d:%d", session, entry.CreatedAt, t.Input, t.Cached, t.Output)
							}
							result = append(result, usageRow{Key: key, At: at, Model: strings.TrimSpace(entry.Model), Tokens: t})
						}
					}
				}
			}
		} else if len(line) > 0 && (bytes.Contains(line, []byte("token_usage_record")) || bytes.Contains(line, []byte("token_count")) || bytes.Contains(line, []byte("turn_context")) || bytes.Contains(line, []byte("session_meta")) || provider == "claude" && bytes.Contains(line, []byte("\"usage\""))) {
			var r struct {
				Type      string         `json:"type"`
				Timestamp string         `json:"timestamp"`
				Payload   map[string]any `json:"payload"`
				Message   *struct {
					ID    string         `json:"id"`
					Model string         `json:"model"`
					Usage map[string]any `json:"usage"`
				} `json:"message"`
				Session string `json:"sessionId"`
				Request string `json:"requestId"`
				UUID    string `json:"uuid"`
			}
			if json.Unmarshal(line, &r) == nil {
				at, _ := time.Parse(time.RFC3339Nano, r.Timestamp)
				p := r.Payload
				if provider == "claude" && r.Type == "assistant" && r.Message != nil && len(r.Message.Usage) > 0 && !at.IsZero() {
					key := r.Message.ID
					if key != "" {
						key = "message:" + r.Session + ":" + key + ":" + r.Request
					} else {
						key = "row:" + session + ":" + r.UUID + ":" + r.Timestamp
					}
					t := parseUsageTokens(r.Message.Usage, provider)
					if t.Input+t.Output > 0 {
						result = append(result, usageRow{Key: key, At: at, Model: r.Message.Model, Tier: str(r.Message.Usage, "service_tier"), Tokens: t})
					}
				}
				if provider == "codex" {
					switch r.Type {
					case "session_meta":
						if id := str(p, "id"); id != "" && session == filepath.Base(path) {
							session = id
							if parent := str(p, "forked_from_id"); parent != "" {
								forkAt, _ = time.Parse(time.RFC3339Nano, str(p, "timestamp"))
							}
						}
					case "turn_context":
						if value := str(p, "model"); value != "" {
							model = value
						}
						tier = str(p, "service_tier")
					case "token_usage_record":
						t := parseUsageTokens(mapValue(p["usage"]), provider)
						key := str(p, "response_id")
						if key == "" {
							key = fmt.Sprintf("%s:%s:%d", session, r.Timestamp, t.Input+t.Output)
						}
						name := str(p, "model")
						if name == "" {
							name = model
						}
						if !at.IsZero() && t.Input+t.Output > 0 {
							result = append(result, usageRow{Key: "response:" + key, At: at, Model: name, Tier: tier, Tokens: t})
						}
						latestModern = t
						latestModernAt = at
						modernPending = true
					case "event_msg":
						if str(p, "type") != "token_count" {
							break
						}
						info := mapValue(p["info"])
						total := mapValue(info["total_token_usage"])
						last := mapValue(info["last_token_usage"])
						t := parseUsageTokens(last, provider)
						if len(total) > 0 {
							current := parseUsageTokens(total, provider)
							if havePrevious && current.Input == previous.Input && current.Output == previous.Output && current.Cached == previous.Cached && current.Write == previous.Write {
								t = usageTokens{}
							} else if len(last) == 0 {
								if havePrevious && current.Input >= previous.Input && current.Output >= previous.Output {
									t = usageDelta(current, previous)
								} else {
									t = current
								}
							}
							previous = current
							havePrevious = true
						}
						if modernPending && parseUsageTokens(last, provider) == latestModern && !at.Before(latestModernAt) && at.Sub(latestModernAt) <= 2*time.Minute {
							t = usageTokens{}
							modernPending = false
						}
						if !at.IsZero() && (forkAt.IsZero() || !at.Before(forkAt)) && t.Input+t.Output > 0 {
							key := fmt.Sprintf("legacy:%s:%d:%d:%d:%d", r.Timestamp, t.Input, t.Cached, t.Write, t.Output)
							result = append(result, usageRow{Key: key, At: at, Model: model, Tier: tier, Tokens: t})
						}
					}
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, skipped, readErr
		}
	}
	return result, skipped, nil
}
