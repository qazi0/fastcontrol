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
	"strconv"
	"strings"
	"time"
)

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
	if p.Provider == "codex" {
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
	} else {
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
