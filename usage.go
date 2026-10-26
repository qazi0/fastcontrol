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
}
type usageFile struct {
	Size     int64
	Mod      time.Time
	Rows     []usageRow
	Skipped  int
	Provider string
}
type UsageManager struct {
	home     string
	mu       sync.Mutex
	cached   []byte
	cachedAt time.Time
	job      *usageCollection
	quotas   map[string]*usageQuotaEntry // guarded by mu; independent of local scans
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
	if provider != "" && provider != "codex" && provider != "claude" {
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
	if provider == "claude" {
		variable = "CLAUDE_CONFIG_DIR"
		fallback = ".claude"
	}
	if value := strings.TrimSpace(os.Getenv(variable)); value != "" {
		return value
	}
	return filepath.Join(home, fallback)
}
func (m *UsageManager) collectLocal(now time.Time) UsageReport {
	zone, _ := now.Zone()
	report := UsageReport{Host: hostName(), Day: now.Format("2006-01-02"), Zone: zone, UpdatedAt: now.Unix(), Providers: []ProviderUsage{{Provider: "codex"}, {Provider: "claude"}}}
	locals := map[string]ProviderUsage{"codex": {Provider: "codex"}, "claude": {Provider: "claude"}}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	since := midnight.AddDate(0, 0, -29)
	seenFiles := map[string]bool{}
	rows := map[string]usageRow{}
	rowProvider := map[string]string{}
	for _, provider := range []string{"codex", "claude"} {
		root := usageRoot(m.home, provider)
		folders := []string{"sessions", "archived_sessions"}
		if provider == "claude" {
			folders = []string{"projects"}
		}
		local := locals[provider]
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
				if st.ModTime().Before(since) {
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
					if !exists || row.Tokens.Input+row.Tokens.Output > prior.Tokens.Input+prior.Tokens.Output || (row.Tokens == prior.Tokens && row.At.After(prior.At)) {
						rows[key] = row
						rowProvider[key] = provider
					}
				}
				return nil
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
	models := map[string]map[string]*UsageModel{"codex": {}, "claude": {}}
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
	if cost, ok := estimateUsageCost(provider, row.Model, row.Tier, t); ok {
		u.cost += cost
		u.priced++
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

func tokenInt(m map[string]any, key string) int64 {
	v, _ := m[key].(float64)
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 9e15 {
		return 0
	}
	return int64(v)
}
func parseUsageTokens(m map[string]any, provider string) usageTokens {
	t := usageTokens{Input: tokenInt(m, "input_tokens"), Output: tokenInt(m, "output_tokens"), Reasoning: tokenInt(m, "reasoning_output_tokens")}
	if provider == "claude" {
		t.Cached = tokenInt(m, "cache_read_input_tokens")
		t.Write = tokenInt(m, "cache_creation_input_tokens")
		t.Write1h = tokenInt(mapValue(m["cache_creation"]), "ephemeral_1h_input_tokens")
		t.Input += t.Cached + t.Write
	} else {
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
func parseUsageFile(path, provider string) ([]usageRow, int, error) {
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
			}
			if bytes.Contains(line, kind) || bytes.Contains(line, spaced) {
				skipped++
			}
			line = nil
		}
		if len(line) > 0 && (bytes.Contains(line, []byte("token_usage_record")) || bytes.Contains(line, []byte("token_count")) || bytes.Contains(line, []byte("turn_context")) || bytes.Contains(line, []byte("session_meta")) || provider == "claude" && bytes.Contains(line, []byte("\"usage\""))) {
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
