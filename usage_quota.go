package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const antigravityQuotaTimeout = 10 * time.Second

type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.max > 0 {
		remaining := b.max - b.buf.Len()
		if remaining > 0 {
			if len(p) > remaining {
				_, _ = b.buf.Write(p[:remaining])
			} else {
				_, _ = b.buf.Write(p)
			}
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

func readUsageJSON(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	err = json.Unmarshal(b, &value)
	return value, err
}
func fetchUsageJSON(ctx context.Context, url, headers string) (map[string]any, error) {
	curl := "curl"
	if speechRuntimeName() == "whisper-cli.exe" {
		curl = "curl.exe"
	}
	cmd := exec.CommandContext(ctx, curl, "--silent", "--show-error", "--fail", "--max-time", "20", "--max-filesize", "1048576", "--header", "@-", url)
	cmd.Stdin = strings.NewReader(headers)
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, errors.New("Could Not Read Subscription Limits. Check the Provider Sign-In and Internet Connection")
	}
	var value map[string]any
	if json.Unmarshal(output.Bytes(), &value) != nil {
		return nil, errors.New("Provider Returned an Invalid Usage Response")
	}
	return value, nil
}
func fetchProviderQuota(home string, p *ProviderUsage) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var data map[string]any
	var err error
	switch p.Provider {
	case "codex":
		var auth map[string]any
		auth, err = readUsageJSON(filepath.Join(usageRoot(home, "codex"), "auth.json"))
		tokens := mapValue(auth["tokens"])
		token := str(tokens, "access_token")
		if token == "" {
			err = errors.New("Sign In to Codex on This Computer to Read Subscription Limits")
		} else {
			headers := "Authorization: Bearer " + token + "\nAccept: application/json\nUser-Agent: fastcontrol\n"
			if id := str(tokens, "account_id"); id != "" {
				headers += "ChatGPT-Account-Id: " + id + "\n"
			}
			data, err = fetchUsageJSON(ctx, "https://chatgpt.com/backend-api/wham/usage", headers)
			if err == nil {
				applyCodexQuota(p, data)
				applyUsagePlanLabel(home, p, str(tokens, "account_id"))
			}
		}
	case "claude":
		var auth map[string]any
		auth, err = readUsageJSON(filepath.Join(usageRoot(home, "claude"), ".credentials.json"))
		oauth := mapValue(auth["claudeAiOauth"])
		if str(oauth, "accessToken") == "" || tokenInt(oauth, "expiresAt") < time.Now().UnixMilli() {
			if stored, keyErr := readClaudeUsageCredential(ctx); keyErr == nil {
				oauth = mapValue(stored["claudeAiOauth"])
				err = nil
			}
		}
		p.Plan = str(oauth, "subscriptionType")
		tier := strings.ToLower(str(oauth, "rateLimitTier"))
		if p.Plan == "max" {
			if strings.Contains(tier, "20x") {
				p.Plan = "Max 20X"
			} else if strings.Contains(tier, "5x") {
				p.Plan = "Max 5X"
			}
		}
		if str(oauth, "accessToken") == "" {
			err = errors.New("Sign In to Claude Code on This Computer to Read Subscription Limits")
		} else if tokenInt(oauth, "expiresAt") > 0 && tokenInt(oauth, "expiresAt") < time.Now().UnixMilli() {
			err = errors.New("Claude Sign-In Has Expired. Open Claude Code on This Computer to Refresh It")
		} else {
			data, err = fetchUsageJSON(ctx, "https://api.anthropic.com/api/oauth/usage", "Authorization: Bearer "+str(oauth, "accessToken")+"\nAccept: application/json\nanthropic-beta: oauth-2025-04-20\nUser-Agent: claude-code/2.1.0\n")
			if err == nil {
				applyClaudeQuota(p, data)
			}
		}
	case "antigravity":
		err = fetchAntigravityQuota(ctx, home, p)
	default:
		err = errors.New("Unknown Provider")
	}
	if err != nil {
		p.QuotaError = err.Error()
	} else {
		p.QuotaAt = time.Now().Unix()
		p.QuotaSource = "Subscription"
	}
}
func quotaNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			return v, true
		}
	case string:
		n, err := strconv.ParseFloat(v, 64)
		if err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
			return n, true
		}
	}
	return 0, false
}
func quotaWindow(label string, value map[string]any, minutes int64) (UsageWindow, bool) {
	used, ok := quotaNumber(value["used_percent"])
	if !ok {
		used, ok = quotaNumber(value["utilization"])
	}
	if !ok {
		return UsageWindow{}, false
	}
	reset := tokenInt(value, "reset_at")
	if reset == 0 {
		if date, err := time.Parse(time.RFC3339Nano, str(value, "resets_at")); err == nil {
			reset = date.Unix()
		}
	}
	if n := tokenInt(value, "limit_window_seconds"); n > 0 {
		minutes = n / 60
	}
	used = math.Max(0, math.Min(100, used))
	return UsageWindow{Label: label, Used: used, Remaining: 100 - used, ResetAt: reset, Minutes: minutes}, true
}
func applyCodexQuota(p *ProviderUsage, data map[string]any) {
	p.Plan = str(data, "plan_type")
	add := func(label string, details map[string]any) {
		for _, key := range []string{"primary_window", "secondary_window"} {
			v := mapValue(details[key])
			minutes := tokenInt(v, "limit_window_seconds") / 60
			name := label
			if minutes >= 10080 {
				name += " Weekly"
			} else if minutes == 300 {
				name += " 5-Hour"
			} else if minutes > 0 {
				if minutes%60 == 0 {
					name += " " + strconv.FormatInt(minutes/60, 10) + "-Hour"
				} else {
					name += " " + strconv.FormatInt(minutes, 10) + "-Minute"
				}
			}
			if w, ok := quotaWindow(strings.TrimSpace(name), v, minutes); ok {
				p.Windows = append(p.Windows, w)
			}
		}
	}
	add("", mapValue(data["rate_limit"]))
	add("Code Review", mapValue(data["code_review_rate_limit"]))
	for _, entry := range arrayValue(data["additional_rate_limits"]) {
		value := mapValue(entry)
		name := str(value, "limit_name")
		if name == "" {
			name = str(value, "limit_id")
		}
		add(name, mapValue(value["rate_limit"]))
	}
	if credits := mapValue(data["credits"]); len(credits) > 0 {
		if n, ok := quotaNumber(credits["balance"]); ok && n >= 0 {
			p.Credits = &n
		}
	}
	if value := mapValue(data["rate_limit_reset_credits"]); len(value) > 0 {
		if n, ok := quotaNumber(value["available_count"]); ok && n >= 0 {
			count := int64(n)
			p.ResetCredits = &count
		}
	}
	if len(p.Windows) == 0 {
		p.QuotaError = "This Account Did Not Report Subscription Limit Windows"
	}
}
func applyClaudeQuota(p *ProviderUsage, data map[string]any) {
	labels := map[string]string{"five_hour": "5-Hour", "seven_day": "Weekly", "seven_day_opus": "Opus Weekly", "seven_day_sonnet": "Sonnet Weekly", "seven_day_oauth_apps": "OAuth Apps Weekly"}
	for _, key := range []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet", "seven_day_oauth_apps"} {
		minutes := int64(10080)
		if key == "five_hour" {
			minutes = 300
		}
		if w, ok := quotaWindow(labels[key], mapValue(data[key]), minutes); ok {
			p.Windows = append(p.Windows, w)
		}
	}
	if len(p.Windows) == 0 {
		p.QuotaError = "This Account Did Not Report Subscription Limit Windows"
	}
}

func applyUsagePlanLabel(home string, p *ProviderUsage, account string) {
	config, err := readUsageJSON(filepath.Join(home, ".fastcontrol", "usage-plans.json"))
	if err != nil || account == "" {
		return
	}
	label := mapValue(config[p.Provider])
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(account)))
	if str(label, "account") != fingerprint || str(label, "basePlan") != p.Plan {
		return
	}
	value := str(label, "label")
	if value == "Pro 20X" || value == "Pro 5X" {
		p.Plan = value
	}
}

func findAntigravityUsageBinary(home string) string {
	if path, err := exec.LookPath("agy"); err == nil {
		return path
	}
	var candidates []string
	if home != "" {
		if runtime.GOOS == "windows" {
			candidates = append(candidates,
				filepath.Join(home, ".local", "bin", "agy.exe"),
				filepath.Join(home, ".local", "bin", "agy.cmd"),
				filepath.Join(home, ".local", "bin", "agy.bat"),
				filepath.Join(home, ".local", "bin", "agy"),
			)
		} else {
			candidates = append(candidates, filepath.Join(home, ".local", "bin", "agy"))
		}
	}
	if runtime.GOOS != "windows" {
		candidates = append(candidates, "/opt/homebrew/bin/agy", "/usr/local/bin/agy")
	}
	for _, path := range candidates {
		if st, err := os.Stat(path); err == nil && !st.IsDir() && (runtime.GOOS == "windows" || st.Mode()&0111 != 0) {
			return path
		}
	}
	return ""
}

func runAntigravityUsageCommand(ctx context.Context, bin string) (string, string, error) {
	cmd := exec.CommandContext(ctx, bin, "-p", "/usage")
	cmd.Stdin = nil
	cmd.WaitDelay = 500 * time.Millisecond
	stdout := &limitedBuffer{max: 1 << 20}
	stderr := &limitedBuffer{max: 1 << 20}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func looksLikeAntigravityAuthError(text string) bool {
	s := strings.ToLower(text)
	for _, needle := range []string{
		"sign in",
		"signed in",
		"not logged in",
		"log in",
		"login",
		"authenticate",
		"authentication",
		"unauthenticated",
		"unauthorized",
		"credentials",
		"oauth",
		"antigravity-cli",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func fetchAntigravityQuota(ctx context.Context, home string, p *ProviderUsage) error {
	return fetchAntigravityQuotaWith(ctx, home, p, findAntigravityUsageBinary, runAntigravityUsageCommand)
}

func fetchAntigravityQuotaWith(
	ctx context.Context,
	home string,
	p *ProviderUsage,
	findBin func(string) string,
	runCmd func(context.Context, string) (string, string, error),
) error {
	bin := findBin(home)
	if bin == "" {
		return errors.New("Install Antigravity CLI (agy) on This Computer to Read Subscription Limits")
	}
	qctx, cancel := context.WithTimeout(ctx, antigravityQuotaTimeout)
	defer cancel()
	stdout, stderr, err := runCmd(qctx, bin)
	if err != nil {
		if errors.Is(qctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return errors.New("Timed Out Reading Antigravity Limits. Run agy on This Computer to Verify Sign-In")
		}
		if looksLikeAntigravityAuthError(stderr) || looksLikeAntigravityAuthError(stdout) {
			return errors.New("Sign In to Antigravity CLI on This Computer to Read Subscription Limits")
		}
		return errors.New("Could Not Read Antigravity Limits. Check Antigravity CLI Sign-In and Internet Connection")
	}
	windows := parseAntigravityQuotaTSV(stdout)
	if len(windows) == 0 {
		if looksLikeAntigravityAuthError(stdout) || looksLikeAntigravityAuthError(stderr) {
			return errors.New("Sign In to Antigravity CLI on This Computer to Read Subscription Limits")
		}
		return errors.New("This Account Did Not Report Subscription Limit Windows")
	}
	p.Windows = windows
	return nil
}

func titleCaseWord(word string) string {
	upper := strings.ToUpper(word)
	switch upper {
	case "GPT", "AI", "API", "CLI":
		return upper
	case "OAUTH":
		return "OAuth"
	}
	runes := []rune(strings.ToLower(word))
	if len(runes) == 0 {
		return ""
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func titleCaseWords(text string) string {
	fields := strings.Fields(text)
	for i, field := range fields {
		fields[i] = titleCaseWord(field)
	}
	return strings.Join(fields, " ")
}

func formatAntigravityQuotaGroup(group string) string {
	fields := strings.Fields(group)
	if len(fields) > 1 && strings.EqualFold(fields[len(fields)-1], "models") {
		fields = fields[:len(fields)-1]
	}
	for i, field := range fields {
		fields[i] = titleCaseWord(field)
	}
	return strings.Join(fields, " ")
}

func formatAntigravityQuotaMetric(metric string) (string, int64) {
	lower := strings.ToLower(strings.TrimSpace(metric))
	switch {
	case strings.Contains(lower, "weekly") || strings.Contains(lower, "seven day") || strings.Contains(lower, "7 day") || strings.Contains(lower, "7-day"):
		return "Weekly", 10080
	case strings.Contains(lower, "five hour") || strings.Contains(lower, "5 hour") || strings.Contains(lower, "5-hour") || strings.Contains(lower, "5h"):
		return "5-Hour", 300
	}
	cleaned := lower
	cleaned = strings.ReplaceAll(cleaned, "limit remaining", "")
	cleaned = strings.ReplaceAll(cleaned, "remaining", "")
	cleaned = strings.ReplaceAll(cleaned, "limit", "")
	return titleCaseWords(cleaned), 0
}

func parseAntigravityQuotaTSV(output string) []UsageWindow {
	var windows []UsageWindow
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 4 {
			continue
		}
		group := strings.TrimSpace(cols[0])
		metric := strings.TrimSpace(cols[1])
		pctStr := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(cols[2]), "%"))
		resetStr := strings.TrimSpace(cols[3])
		remaining, err := strconv.ParseFloat(pctStr, 64)
		if err != nil || math.IsNaN(remaining) || math.IsInf(remaining, 0) {
			continue
		}
		remaining = math.Max(0, math.Min(100, remaining))
		used := math.Max(0, math.Min(100, math.Round((100-remaining)*10000)/10000))
		var resetAt int64
		if resetStr != "" {
			if ts, err := time.Parse(time.RFC3339Nano, resetStr); err == nil && ts.Unix() > 0 {
				resetAt = ts.Unix()
			}
		}
		groupLabel := formatAntigravityQuotaGroup(group)
		metricLabel, minutes := formatAntigravityQuotaMetric(metric)
		label := strings.TrimSpace(groupLabel + " " + metricLabel)
		if label == "" {
			label = "Usage Window"
		}
		windows = append(windows, UsageWindow{
			Label:     label,
			Remaining: remaining,
			Used:      used,
			ResetAt:   resetAt,
			Minutes:   minutes,
		})
	}
	return windows
}
