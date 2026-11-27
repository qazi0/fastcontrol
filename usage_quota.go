package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	antigravityQuotaTimeout = 10 * time.Second
	grokQuotaTimeout        = 10 * time.Second
	grokQuotaClientName     = "fastcontrol"
	grokQuotaClientVersion  = "0.2.18"
	grokQuotaMaxLineBytes   = 4 << 20
)

var errGrokNotSignedIn = errors.New("Grok CLI Not Signed In")

type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	b.mu.Lock()
	defer b.mu.Unlock()
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
	case "grok":
		err = fetchGrokQuota(ctx, home, p)
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

func isGrokUsageExecutable(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode()&0111 != 0
}

func findGrokUsageBinary(home string) string {
	names := []string{"grok"}
	if runtime.GOOS == "windows" {
		names = []string{"grok.exe", "grok.cmd", "grok.bat", "grok"}
	}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil && isGrokUsageExecutable(path) {
			return path
		}
	}
	var candidates []string
	addDir := func(dir string) {
		if strings.TrimSpace(dir) == "" {
			return
		}
		if runtime.GOOS == "windows" {
			candidates = append(candidates,
				filepath.Join(dir, "grok.exe"),
				filepath.Join(dir, "grok.cmd"),
				filepath.Join(dir, "grok.bat"),
				filepath.Join(dir, "grok"),
			)
		} else {
			candidates = append(candidates, filepath.Join(dir, "grok"))
		}
	}
	if home != "" {
		addDir(filepath.Join(home, ".local", "bin"))
		addDir(filepath.Join(home, ".grok", "bin"))
	}
	if envHome := strings.TrimSpace(os.Getenv("GROK_HOME")); envHome != "" {
		addDir(filepath.Join(envHome, "bin"))
	}
	if runtime.GOOS != "windows" {
		candidates = append(candidates, "/opt/homebrew/bin/grok", "/usr/local/bin/grok")
	}
	for _, path := range candidates {
		if isGrokUsageExecutable(path) {
			return path
		}
	}
	return ""
}

func hasGrokAuthFile(grokHome string) bool {
	if strings.TrimSpace(grokHome) == "" {
		return false
	}
	path := filepath.Join(grokHome, "auth.json")
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return false
	}
	if st.Size() > 1<<20 {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	trimmed := strings.TrimSpace(string(data))
	return trimmed != "" && trimmed != "{}" && trimmed != "null"
}

func looksLikeGrokAuthError(text string) bool {
	s := strings.ToLower(text)
	for _, needle := range []string{
		"sign in",
		"signed in",
		"not logged in",
		"log in",
		"login",
		"unauthenticated",
		"unauthorized",
		"authentication",
		"cached_token",
		"no auth",
		"auth.json",
		"expired",
		"credentials",
		"oauth",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func withGrokEnvVar(base []string, key, value string) []string {
	out := make([]string, 0, len(base)+1)
	replaced := false
	for _, entry := range base {
		k, _, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(k, key) {
			if !replaced {
				out = append(out, key+"="+value)
				replaced = true
			}
			continue
		}
		out = append(out, entry)
	}
	if !replaced {
		out = append(out, key+"="+value)
	}
	return out
}

type grokQuotaRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type grokQuotaRPCEnvelope struct {
	JSONRPC string             `json:"jsonrpc"`
	ID      json.RawMessage    `json:"id"`
	Method  string             `json:"method"`
	Result  json.RawMessage    `json:"result"`
	Error   *grokQuotaRPCError `json:"error"`
}

func grokRPCIDMatches(raw json.RawMessage, want int) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var num int
	if json.Unmarshal(trimmed, &num) == nil {
		return num == want
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		if parsed, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return parsed == want
		}
	}
	return false
}

func readBoundedRPCLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	oversized := false
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > limit {
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
			return nil, err
		}
		return line, err
	}
}

func runGrokBillingRPC(ctx context.Context, bin, grokHome string) (map[string]any, string, error) {
	cmd := exec.Command(bin, "agent", "--no-leader", "stdio")
	return runGrokBillingCmd(ctx, cmd, grokHome)
}

func runGrokBillingCmd(ctx context.Context, cmd *exec.Cmd, grokHome string) (map[string]any, string, error) {
	prepareProcess(cmd)
	if strings.TrimSpace(grokHome) != "" {
		baseEnv := cmd.Env
		if len(baseEnv) == 0 {
			baseEnv = os.Environ()
		}
		cmd.Env = withGrokEnvVar(baseEnv, "GROK_HOME", grokHome)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, "", err
	}
	stderr := &limitedBuffer{max: 64 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, "", err
	}

	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				killProcess(cmd.Process)
			}
		case <-stopWatch:
		}
	}()
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			close(stopWatch)
			<-watchDone
			_ = stdin.Close()
			_ = stdout.Close()
			waitCh := make(chan error, 1)
			go func() { waitCh <- cmd.Wait() }()
			select {
			case <-waitCh:
				return
			case <-time.After(250 * time.Millisecond):
			}
			if cmd.Process != nil {
				terminateProcess(cmd.Process)
			}
			select {
			case <-waitCh:
				return
			case <-time.After(250 * time.Millisecond):
			}
			if cmd.Process != nil {
				killProcess(cmd.Process)
			}
			<-waitCh
		})
	}
	defer cleanup()

	reader := bufio.NewReaderSize(stdout, 64<<10)
	sendAndRead := func(id int, method string, params any) (json.RawMessage, *grokQuotaRPCError, error) {
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  method,
			"params":  params,
		})
		if err != nil {
			return nil, nil, err
		}
		payload = append(payload, '\n')
		if _, err := stdin.Write(payload); err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			cleanup()
			if looksLikeGrokAuthError(stderr.String()) {
				return nil, nil, errGrokNotSignedIn
			}
			return nil, nil, err
		}
		for {
			line, readErr := readBoundedRPCLine(reader, grokQuotaMaxLineBytes)
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var env grokQuotaRPCEnvelope
				if json.Unmarshal(trimmed, &env) == nil && env.Method == "" && grokRPCIDMatches(env.ID, id) {
					return env.Result, env.Error, nil
				}
			}
			if readErr != nil {
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				cleanup()
				if looksLikeGrokAuthError(stderr.String()) {
					return nil, nil, errGrokNotSignedIn
				}
				if errors.Is(readErr, io.EOF) {
					return nil, nil, errors.New("Grok Billing Response Ended Early")
				}
				return nil, nil, readErr
			}
		}
	}

	_, rpcErr, err := sendAndRead(1, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    grokQuotaClientName,
			"version": grokQuotaClientVersion,
		},
		"clientCapabilities": map[string]any{},
	})
	if err != nil {
		return nil, "", err
	}
	if rpcErr != nil {
		if looksLikeGrokAuthError(rpcErr.Message) {
			return nil, "", errGrokNotSignedIn
		}
		return nil, "", errors.New(rpcErr.Message)
	}

	authRaw, rpcErr, err := sendAndRead(2, "authenticate", map[string]any{
		"methodId": "cached_token",
	})
	if err != nil {
		return nil, "", err
	}
	if rpcErr != nil {
		return nil, "", errGrokNotSignedIn
	}
	authTier := ""
	if len(authRaw) > 0 {
		var authResult map[string]any
		if json.Unmarshal(authRaw, &authResult) == nil {
			meta := mapValue(authResult["_meta"])
			authTier = firstNonEmptyUsageString(str(meta, "subscription_tier"), str(meta, "subscriptionTier"))
		}
	}

	billingRaw, rpcErr, err := sendAndRead(3, "_x.ai/billing", map[string]any{})
	if err != nil {
		return nil, "", err
	}
	if rpcErr != nil {
		if looksLikeGrokAuthError(rpcErr.Message) {
			return nil, "", errGrokNotSignedIn
		}
		return nil, "", errors.New(rpcErr.Message)
	}
	var billing map[string]any
	if len(billingRaw) == 0 || json.Unmarshal(billingRaw, &billing) != nil {
		return nil, "", errors.New("Provider Returned an Invalid Usage Response")
	}
	return billing, authTier, nil
}

func fetchGrokQuota(ctx context.Context, home string, p *ProviderUsage) error {
	return fetchGrokQuotaWith(ctx, home, usageRoot(home, "grok"), p, findGrokUsageBinary, runGrokBillingRPC)
}

func fetchGrokQuotaWith(
	ctx context.Context,
	home, grokHome string,
	p *ProviderUsage,
	findBin func(string) string,
	runRPC func(context.Context, string, string) (map[string]any, string, error),
) error {
	bin := findBin(home)
	if bin == "" {
		return errors.New("Grok CLI Not Installed")
	}
	if !hasGrokAuthFile(grokHome) {
		return errGrokNotSignedIn
	}
	qctx, cancel := context.WithTimeout(ctx, grokQuotaTimeout)
	defer cancel()
	data, authTier, err := runRPC(qctx, bin, grokHome)
	if err != nil {
		if errors.Is(qctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return errors.New("Timed Out Reading Grok Subscription Limits")
		}
		if errors.Is(err, errGrokNotSignedIn) {
			return errGrokNotSignedIn
		}
		return errors.New("Could Not Read Grok Subscription Limits")
	}
	return applyGrokQuota(p, data, authTier)
}

func parseRFC3339Unix(text string) int64 {
	s := strings.TrimSpace(text)
	if s == "" {
		return 0
	}
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil && ts.Unix() > 0 {
		return ts.Unix()
	}
	return 0
}

func grokQuotaPeriodLabel(periodType string, startUnix, endUnix int64) (string, int64) {
	upper := strings.ToUpper(strings.TrimSpace(periodType))
	switch {
	case strings.Contains(upper, "WEEKLY") || strings.Contains(upper, "WEEK"):
		return "Weekly", 10080
	case strings.Contains(upper, "MONTHLY") || strings.Contains(upper, "MONTH"):
		return "Monthly", 43200
	case strings.Contains(upper, "DAILY") || strings.Contains(upper, "DAY"):
		return "Daily", 1440
	case strings.Contains(upper, "HOURLY") || strings.Contains(upper, "HOUR"):
		return "Hourly", 60
	}
	if startUnix > 0 && endUnix > startUnix {
		minutes := (endUnix - startUnix) / 60
		switch {
		case minutes >= 38880:
			return "Monthly", minutes
		case minutes >= 8640 && minutes <= 11520:
			return "Weekly", minutes
		case minutes >= 1200 && minutes <= 1680:
			return "Daily", minutes
		case minutes > 0:
			if minutes%60 == 0 {
				return strconv.FormatInt(minutes/60, 10) + "-Hour", minutes
			}
			return strconv.FormatInt(minutes, 10) + "-Minute", minutes
		}
	}
	return "Weekly", 10080
}

func applyGrokQuota(p *ProviderUsage, data map[string]any, fallbackPlan string) error {
	if len(data) == 0 {
		return errors.New("This Account Did Not Report Subscription Limit Windows")
	}
	if _, hasConfig := data["config"]; !hasConfig {
		if inner := mapValue(data["result"]); len(inner) > 0 {
			data = inner
		}
	}
	p.Plan = firstNonEmptyUsageString(
		str(data, "subscription_tier"),
		str(data, "subscriptionTier"),
		fallbackPlan,
	)
	config := mapValue(data["config"])
	used, ok := quotaNumber(config["creditUsagePercent"])
	if !ok {
		return errors.New("This Account Did Not Report Subscription Limit Windows")
	}
	used = math.Max(0, math.Min(100, used))
	remaining := math.Max(0, math.Min(100, math.Round((100-used)*10000)/10000))
	period := mapValue(config["currentPeriod"])
	startUnix := parseRFC3339Unix(str(period, "start"))
	endUnix := parseRFC3339Unix(str(period, "end"))
	resetAt := endUnix
	if resetAt == 0 {
		resetAt = parseRFC3339Unix(str(config, "billingPeriodEnd"))
	}
	label, minutes := grokQuotaPeriodLabel(str(period, "type"), startUnix, endUnix)
	p.Windows = append(p.Windows, UsageWindow{
		Label:     label,
		Used:      used,
		Remaining: remaining,
		ResetAt:   resetAt,
		Minutes:   minutes,
	})
	if prepaid := mapValue(config["prepaidBalance"]); len(prepaid) > 0 {
		if val, ok := quotaNumber(prepaid["val"]); ok && val > 0 {
			p.Credits = &val
		}
	}
	p.QuotaSource = "Subscription"
	return nil
}
