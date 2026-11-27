package main

// Grok CLI ACP JSON-RPC 2.0 client over stdio.
// Communicates with `grok agent --no-leader [--always-approve] stdio` using
// Agent Client Protocol v1 line-delimited JSON-RPC 2.0 framing, including
// `_x.ai/*` extensions, streaming updates, interactive approvals, questions,
// plan-exit cards, MCP elicitations, and a FIFO dispatch drain barrier.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	grokACPClientVersion = "0.2.18"

	// All Grok ACP extension RPCs and notifications require the leading underscore `_x.ai/*`.
	grokExtModelsList             = "_x.ai/models/list"
	grokExtBilling                = "_x.ai/billing"
	grokExtCommandsList           = "_x.ai/commands/list"
	grokExtAuthCheckSub           = "_x.ai/auth/check_subscription"
	grokExtSessionInfo            = "_x.ai/session/info"
	grokExtSessionUsage           = "_x.ai/session/usage"
	grokExtSessionRename          = "_x.ai/session/rename"
	grokExtSessionDelete          = "_x.ai/session/delete"
	grokExtSessionFork            = "_x.ai/session/fork"
	grokExtCompactConversation    = "_x.ai/compact_conversation"
	grokExtYoloModeChanged        = "_x.ai/yolo_mode_changed"
	grokExtTogglePlanMode         = "_x.ai/toggle_plan_mode"
	grokExtAskUserQuestion        = "_x.ai/ask_user_question"
	grokExtExitPlanMode           = "_x.ai/exit_plan_mode"
	grokExtMCPElicit              = "_x.ai/mcp/elicit"
	grokMethodSessionUpdate       = "_x.ai/session/update"
	grokMethodSessionNotification = "_x.ai/session_notification"

	grokDefaultAuthMethod = "cached_token"
)

var (
	grokACPCacheMu    sync.Mutex
	grokACPCache      = map[string]acpSupportCacheEntry{}
	grokACPInFlight   = map[string]chan struct{}{}
	grokProbeHelpFunc = probeGrokACPHelp
)

// grokBinary locates the Grok CLI binary (`grok`) via PATH and standard
// platform install locations such as `~/.local/bin/grok` and `~/.grok/bin/grok`.
func grokBinary() string {
	names := []string{"grok"}
	if runtime.GOOS == "windows" {
		names = []string{"grok.exe", "grok"}
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil && isGrokExecutable(p) {
			return p
		}
	}
	home, _ := os.UserHomeDir()
	for _, candidate := range grokBinaryCandidates(home, os.Getenv("LOCALAPPDATA"), runtime.GOOS) {
		if isGrokExecutable(candidate) {
			return candidate
		}
	}
	return ""
}

func grokBinaryCandidates(home, localAppData, goos string) []string {
	var out []string
	add := func(parts ...string) {
		for _, p := range parts {
			if p == "" {
				return
			}
		}
		out = append(out, filepath.Join(parts...))
	}
	if goos == "windows" {
		add(home, ".local", "bin", "grok.exe")
		add(home, ".grok", "bin", "grok.exe")
		add(localAppData, "Grok", "grok.exe")
		add(localAppData, "Grok", "bin", "grok.exe")
		add(localAppData, "Programs", "Grok", "grok.exe")
		add(localAppData, "Programs", "Grok", "bin", "grok.exe")
		return out
	}
	add(home, ".local", "bin", "grok")
	add(home, ".grok", "bin", "grok")
	if goos == "darwin" {
		out = append(out, "/opt/homebrew/bin/grok", "/usr/local/bin/grok")
	} else {
		out = append(out, "/usr/local/bin/grok", "/usr/bin/grok")
	}
	return out
}

func isGrokExecutable(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode()&0111 != 0
}

// grokSupportsACP verifies that `bin` supports `grok agent stdio`.
// Results are cached under a mutex keyed by path, size, and mtime with in-flight
// deduplication so concurrent callers share a single bounded probe while noticing
// in-place upgrades automatically.
func grokSupportsACP(bin string) bool {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return false
	}
	st, err := os.Stat(bin)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	size := st.Size()
	modNs := st.ModTime().UnixNano()
	key := fmt.Sprintf("%s|%d|%d", bin, size, modNs)

	for {
		grokACPCacheMu.Lock()
		if entry, ok := grokACPCache[bin]; ok && entry.size == size && entry.modTimeNs == modNs {
			grokACPCacheMu.Unlock()
			return entry.supported
		}
		if wait, busy := grokACPInFlight[key]; busy {
			grokACPCacheMu.Unlock()
			<-wait
			continue
		}
		done := make(chan struct{})
		grokACPInFlight[key] = done
		probe := grokProbeHelpFunc
		grokACPCacheMu.Unlock()

		supported := probe(bin)

		grokACPCacheMu.Lock()
		grokACPCache[bin] = acpSupportCacheEntry{
			size:      size,
			modTimeNs: modNs,
			supported: supported,
		}
		delete(grokACPInFlight, key)
		close(done)
		grokACPCacheMu.Unlock()
		return supported
	}
}

func probeGrokACPHelp(bin string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), acpSupportProbeLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "agent", "stdio", "--help")
	cmd.Env = codexEnv()
	prepareProcess(cmd)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			killProcess(cmd.Process)
		}
		return nil
	}
	cmd.WaitDelay = 500 * time.Millisecond
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "stdio")
}

// grokACPClientConfig configures a Grok CLI ACP JSON-RPC 2.0 client.
type grokACPClientConfig struct {
	Binary          string
	Cwd             string
	AlwaysApprove   bool
	Model           string
	ReasoningEffort string
	AgentProfile    string
	PluginDirs      []string
	Env             []string
	StderrBytes     int
	CallTimeout     time.Duration

	// Callbacks are dispatched in FIFO wire order on a dedicated goroutine
	// separate from the stdio readLoop, so callbacks may safely invoke client
	// methods such as RespondPermission, RespondAskUserQuestion,
	// RespondExitPlanMode, RespondElicitation, SetConfigOption, or Cancel.
	// Never call client.Close() or client.DrainDispatch() synchronously inside
	// a callback.
	OnUpdate              func(grokSessionUpdate)
	OnExtUpdate           func(grokExtSessionUpdate)
	OnSessionNotification func(grokSessionNotification)
	OnPermission          func(grokPermissionRequest)
	OnAskUserQuestion     func(grokAskUserQuestionRequest)
	OnExitPlanMode        func(grokExitPlanModeRequest)
	OnElicitation         func(grokElicitationRequest)
	OnClose               func(error)
}

type grokReasoningEffortOption struct {
	ID          string `json:"id,omitempty"`
	Value       string `json:"value"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

type grokModelInfo struct {
	ModelID                 string                      `json:"modelId"`
	Name                    string                      `json:"name"`
	Description             string                      `json:"description,omitempty"`
	TotalContextTokens      int64                       `json:"totalContextTokens,omitempty"`
	AgentType               string                      `json:"agentType,omitempty"`
	SupportsReasoningEffort bool                        `json:"supportsReasoningEffort,omitempty"`
	DefaultReasoningEffort  string                      `json:"reasoningEffort,omitempty"`
	ReasoningEfforts        []grokReasoningEffortOption `json:"reasoningEfforts,omitempty"`
	ContextWindows          json.RawMessage             `json:"contextWindows,omitempty"`
	Meta                    json.RawMessage             `json:"_meta,omitempty"`
}

type grokModelsState struct {
	CurrentModelID  string          `json:"currentModelId,omitempty"`
	AvailableModels []grokModelInfo `json:"availableModels,omitempty"`
}

type grokMCPCapabilities struct {
	HTTP bool `json:"http,omitempty"`
	SSE  bool `json:"sse,omitempty"`
}

type grokAgentCapabilities struct {
	LoadSession         bool                   `json:"loadSession,omitempty"`
	PromptCapabilities  acpPromptCapabilities  `json:"promptCapabilities,omitempty"`
	MCPCapabilities     grokMCPCapabilities    `json:"mcpCapabilities,omitempty"`
	SessionCapabilities acpSessionCapabilities `json:"sessionCapabilities,omitempty"`
	Auth                bool                   `json:"auth,omitempty"`
	FSNotify            bool                   `json:"fsNotify,omitempty"`
	Meta                json.RawMessage        `json:"_meta,omitempty"`
}

type grokInitResult struct {
	ProtocolVersion     int                   `json:"protocolVersion"`
	AgentInfo           acpAgentInfo          `json:"agentInfo"`
	AgentCapabilities   grokAgentCapabilities `json:"agentCapabilities"`
	AuthMethods         []acpAuthMethod       `json:"authMethods,omitempty"`
	AgentVersion        string                `json:"agentVersion,omitempty"`
	DefaultAuthMethodID string                `json:"defaultAuthMethodId,omitempty"`
	GrokShell           bool                  `json:"grokShell,omitempty"`
	CurrentWorkingDir   string                `json:"currentWorkingDirectory,omitempty"`
	ModelState          grokModelsState       `json:"modelState,omitempty"`
	Meta                json.RawMessage       `json:"_meta,omitempty"`
}

type grokAuthResult struct {
	AuthMode         string          `json:"authMode,omitempty"`
	SubscriptionTier string          `json:"subscriptionTier,omitempty"`
	IsZDR            bool            `json:"isZdr,omitempty"`
	CodingDataTeam   bool            `json:"codingDataTeam,omitempty"`
	BackendBilled    bool            `json:"backendBilled,omitempty"`
	Meta             json.RawMessage `json:"_meta,omitempty"`
}

type grokSubscriptionStatus struct {
	Authenticated    bool            `json:"authenticated"`
	AuthMode         string          `json:"authMode,omitempty"`
	SubscriptionTier string          `json:"subscriptionTier,omitempty"`
	Meta             json.RawMessage `json:"meta,omitempty"`
}

// grokSessionOptions carries per-session `_meta` parameters sent on
// `session/new`, `session/load`, and `session/resume`.
type grokSessionOptions struct {
	YoloMode             bool
	AutoMode             bool
	Rules                string
	SystemPromptOverride string
	AgentProfile         any
}

type grokSessionSummary struct {
	SessionID string          `json:"sessionId"`
	Cwd       string          `json:"cwd,omitempty"`
	Title     string          `json:"title,omitempty"`
	UpdatedAt string          `json:"updatedAt,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

type grokSessionDetail struct {
	SessionID      string `json:"sessionId,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	CurrentModelID string `json:"currentModelId,omitempty"`
	Title          string `json:"title,omitempty"`
}

type grokSessionState struct {
	SessionID           string             `json:"sessionId"`
	Models              grokModelsState    `json:"models,omitempty"`
	ConfigOptions       []acpConfigOption  `json:"configOptions,omitempty"`
	Detail              *grokSessionDetail `json:"detail,omitempty"`
	TurnCount           int                `json:"turnCount,omitempty"`
	MemoryNotesWritable bool               `json:"memoryNotesWritable,omitempty"`
	Meta                json.RawMessage    `json:"_meta,omitempty"`
}

func (s *grokSessionState) ConfigOption(id string) *acpConfigOption {
	if s == nil {
		return nil
	}
	for i := range s.ConfigOptions {
		if s.ConfigOptions[i].ID == id {
			return &s.ConfigOptions[i]
		}
	}
	return nil
}

func (s *grokSessionState) CurrentModel() string {
	if opt := s.ConfigOption("model"); opt != nil && opt.CurrentValue != "" {
		return opt.CurrentValue
	}
	if s != nil {
		if s.Models.CurrentModelID != "" {
			return s.Models.CurrentModelID
		}
		if s.Detail != nil && s.Detail.CurrentModelID != "" {
			return s.Detail.CurrentModelID
		}
	}
	return ""
}

func (s *grokSessionState) CurrentReasoningEffort() string {
	if opt := s.ConfigOption("reasoning_effort"); opt != nil && opt.CurrentValue != "" {
		return opt.CurrentValue
	}
	if opt := s.ConfigOption("thought_level"); opt != nil && opt.CurrentValue != "" {
		return opt.CurrentValue
	}
	return ""
}

type grokBillingAmount struct {
	Val float64 `json:"val"`
}

type grokBillingPeriod struct {
	Type  string `json:"type,omitempty"`
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
}

type grokBillingConfig struct {
	CreditUsagePercent   float64            `json:"creditUsagePercent"`
	CurrentPeriod        *grokBillingPeriod `json:"currentPeriod,omitempty"`
	OnDemandCap          *grokBillingAmount `json:"onDemandCap,omitempty"`
	OnDemandUsed         *grokBillingAmount `json:"onDemandUsed,omitempty"`
	PrepaidBalance       *grokBillingAmount `json:"prepaidBalance,omitempty"`
	IsUnifiedBillingUser bool               `json:"isUnifiedBillingUser,omitempty"`
	BillingPeriodStart   string             `json:"billingPeriodStart,omitempty"`
	BillingPeriodEnd     string             `json:"billingPeriodEnd,omitempty"`
}

type grokBillingResult struct {
	Config           *grokBillingConfig `json:"config,omitempty"`
	SubscriptionTier string             `json:"subscription_tier,omitempty"`
	Raw              json.RawMessage    `json:"-"`
}

type grokCommandsResult struct {
	Commands []acpAvailableCommand `json:"commands,omitempty"`
	Tools    []string              `json:"tools,omitempty"`
}

type grokHeldPromptsInfo struct {
	State    string `json:"state,omitempty"`
	Queued   int    `json:"queued,omitempty"`
	Received int    `json:"received,omitempty"`
}

type grokUsageCategory struct {
	Label  string `json:"label,omitempty"`
	Tokens int64  `json:"tokens,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type grokContextInfo struct {
	Used                    int64               `json:"used,omitempty"`
	Total                   int64               `json:"total,omitempty"`
	UsagePct                float64             `json:"usagePct,omitempty"`
	AutoCompactThresholdPct float64             `json:"autoCompactThresholdPercent,omitempty"`
	CompactionCount         int                 `json:"compactionCount,omitempty"`
	TurnCount               int                 `json:"turnCount,omitempty"`
	ToolCallCount           int                 `json:"toolCallCount,omitempty"`
	MessageCount            int                 `json:"messageCount,omitempty"`
	FreeTokens              int64               `json:"freeTokens,omitempty"`
	UsageCategories         []grokUsageCategory `json:"usageCategories,omitempty"`
}

type grokSessionInfo struct {
	SessionID        string               `json:"sessionId"`
	Cwd              string               `json:"cwd,omitempty"`
	AgentName        string               `json:"agentName,omitempty"`
	Model            string               `json:"model,omitempty"`
	ModelDisplayName string               `json:"modelDisplayName,omitempty"`
	ResolvedModelID  string               `json:"resolvedModelId,omitempty"`
	APIBackend       string               `json:"apiBackend,omitempty"`
	Turns            int                  `json:"turns,omitempty"`
	TurnIndex        int                  `json:"turnIndex,omitempty"`
	HeldPrompts      *grokHeldPromptsInfo `json:"heldPrompts,omitempty"`
	Context          *grokContextInfo     `json:"context,omitempty"`
	Raw              json.RawMessage      `json:"-"`
}

type grokSessionUsage struct {
	InputTokens         int64 `json:"inputTokens,omitempty"`
	OutputTokens        int64 `json:"outputTokens,omitempty"`
	TotalTokens         int64 `json:"totalTokens,omitempty"`
	CachedReadTokens    int64 `json:"cachedReadTokens,omitempty"`
	CacheCreationTokens int64 `json:"cacheCreationTokens,omitempty"`
	ReasoningTokens     int64 `json:"reasoningTokens,omitempty"`
	ModelCalls          int   `json:"modelCalls,omitempty"`
	APIDurationMs       int64 `json:"apiDurationMs,omitempty"`
	NumTurns            int   `json:"numTurns,omitempty"`
	CostUsdTicks        int64 `json:"costUsdTicks,omitempty"`
}

type grokPlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority,omitempty"`
	Status   string `json:"status,omitempty"`
}

type grokSessionUpdate struct {
	SessionID         string                `json:"sessionId"`
	Kind              string                `json:"sessionUpdate"`
	Text              string                `json:"text,omitempty"`
	Content           *acpContentBlock      `json:"content,omitempty"`
	ToolCall          *acpToolCallInfo      `json:"toolCall,omitempty"`
	Plan              []grokPlanEntry       `json:"plan,omitempty"`
	Usage             *acpUsageUpdate       `json:"usage,omitempty"`
	AvailableCommands []acpAvailableCommand `json:"availableCommands,omitempty"`
	Title             string                `json:"title,omitempty"`
	UpdatedAt         string                `json:"updatedAt,omitempty"`
	CurrentModeID     string                `json:"currentModeId,omitempty"`
	ConfigOptions     []acpConfigOption     `json:"configOptions,omitempty"`
	EventID           string                `json:"eventId,omitempty"`
	AgentTimestampMs  int64                 `json:"agentTimestampMs,omitempty"`
	Meta              json.RawMessage       `json:"_meta,omitempty"`
	Raw               json.RawMessage       `json:"-"`
}

type grokExtSessionUpdate struct {
	SessionID        string            `json:"sessionId"`
	Kind             string            `json:"kind"`
	StopReason       string            `json:"stopReason,omitempty"`
	ElapsedMs        int64             `json:"elapsedMs,omitempty"`
	CostUsdTicks     int64             `json:"costUsdTicks,omitempty"`
	Usage            *grokSessionUsage `json:"usage,omitempty"`
	SubagentID       string            `json:"subagentId,omitempty"`
	ChildSessionID   string            `json:"childSessionId,omitempty"`
	Status           string            `json:"status,omitempty"`
	Summary          string            `json:"summary,omitempty"`
	EventID          string            `json:"eventId,omitempty"`
	AgentTimestampMs int64             `json:"agentTimestampMs,omitempty"`
	Meta             json.RawMessage   `json:"_meta,omitempty"`
	Raw              json.RawMessage   `json:"-"`
}

type grokSessionNotification struct {
	SessionID string          `json:"sessionId"`
	Kind      string          `json:"kind"`
	ModelID   string          `json:"modelId,omitempty"`
	Message   string          `json:"message,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

type grokPermissionRequest struct {
	RequestID string                `json:"requestId"`
	SessionID string                `json:"sessionId"`
	ToolCall  acpToolCallInfo       `json:"toolCall"`
	Options   []acpPermissionOption `json:"options,omitempty"`
	Raw       json.RawMessage       `json:"-"`
}

type grokQuestionOption struct {
	ID          string `json:"id,omitempty"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Preview     string `json:"preview,omitempty"`
}

type grokQuestion struct {
	ID          string               `json:"id,omitempty"`
	Question    string               `json:"question"`
	Options     []grokQuestionOption `json:"options,omitempty"`
	MultiSelect bool                 `json:"multiSelect,omitempty"`
}

type grokAskUserQuestionRequest struct {
	RequestID  string          `json:"requestId"`
	SessionID  string          `json:"sessionId"`
	ToolCallID string          `json:"toolCallId"`
	Questions  []grokQuestion  `json:"questions,omitempty"`
	Mode       string          `json:"mode,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

// grokAnsweredQuestion matches Grok's 6-field `AnsweredQuestion` serde struct.
type grokAnsweredQuestion struct {
	Question     string   `json:"question"`
	Options      []string `json:"options"`
	Descriptions []string `json:"descriptions"`
	MultiSelect  bool     `json:"multiSelect"`
	Picked       []string `json:"picked"`
	Typed        string   `json:"typed,omitempty"`
}

// grokQuestionAnnotation matches Grok's `QuestionAnnotation` serde struct.
type grokQuestionAnnotation struct {
	Preview string `json:"preview,omitempty"`
	Notes   string `json:"notes,omitempty"`
}

type grokExitPlanModeRequest struct {
	RequestID   string          `json:"requestId"`
	SessionID   string          `json:"sessionId"`
	ToolCallID  string          `json:"toolCallId"`
	PlanContent string          `json:"planContent"`
	Raw         json.RawMessage `json:"-"`
}

type grokElicitationRequest struct {
	RequestID       string                `json:"requestId"`
	SessionID       string                `json:"sessionId,omitempty"`
	ToolCallID      string                `json:"toolCallId,omitempty"`
	ServerName      string                `json:"serverName,omitempty"`
	ElicitationID   string                `json:"elicitationId,omitempty"`
	Mode            string                `json:"mode,omitempty"`
	Message         string                `json:"message"`
	URL             string                `json:"url,omitempty"`
	Fields          []acpElicitationField `json:"fields,omitempty"`
	RequestedSchema json.RawMessage       `json:"requestedSchema,omitempty"`
	Raw             json.RawMessage       `json:"-"`
}

type grokDispatchEvent struct {
	update       *grokSessionUpdate
	extUpdate    *grokExtSessionUpdate
	sessionNotif *grokSessionNotification
	permission   *grokPermissionRequest
	askQuestion  *grokAskUserQuestionRequest
	exitPlan     *grokExitPlanModeRequest
	elicitation  *grokElicitationRequest
	unknownReq   *acpUnknownServerReq
	barrier      chan struct{}
	custom       func()
}

// grokACPClient manages a JSON-RPC 2.0 ACP connection to `grok agent --no-leader stdio`
// or in-process streams.
type grokACPClient struct {
	cfg    grokACPClientConfig
	cmd    *exec.Cmd
	stderr *acpStderrBuffer

	procCancel context.CancelFunc

	ctx    context.Context
	cancel context.CancelFunc

	writeMu      sync.Mutex
	stdin        io.WriteCloser
	stdinClosed  bool
	stdoutCloser io.Closer

	mu         sync.Mutex
	nextID     int64
	pending    map[string]chan acpRPCResponse
	serverReqs map[string]acpPendingServerReq
	initResult *grokInitResult
	closed     bool
	closeErr   error

	eventMu     sync.Mutex
	eventQueue  []grokDispatchEvent
	eventClosed bool
	eventSig    chan struct{}

	stdoutDone   chan struct{}
	readerDone   chan struct{}
	dispatchDone chan struct{}
	closeOnce    sync.Once
}

// buildGrokACPArgs constructs the CLI argument slice for `grok agent ... stdio`.
// Agent options MUST precede the `stdio` transport subcommand.
func buildGrokACPArgs(cfg grokACPClientConfig) ([]string, error) {
	args := []string{"agent", "--no-leader"}
	if cfg.AlwaysApprove {
		args = append(args, "--always-approve")
	}
	if model := strings.TrimSpace(cfg.Model); model != "" {
		if strings.HasPrefix(model, "-") {
			return nil, fmt.Errorf("invalid model %q", model)
		}
		args = append(args, "-m", model)
	}
	if effort := strings.TrimSpace(cfg.ReasoningEffort); effort != "" {
		if strings.HasPrefix(effort, "-") {
			return nil, fmt.Errorf("invalid reasoning effort %q", effort)
		}
		args = append(args, "--reasoning-effort", effort)
	}
	if profile := strings.TrimSpace(cfg.AgentProfile); profile != "" {
		if strings.HasPrefix(profile, "-") {
			return nil, fmt.Errorf("invalid agent profile %q", profile)
		}
		args = append(args, "--agent-profile", profile)
	}
	for _, dir := range cfg.PluginDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if strings.HasPrefix(dir, "-") {
			return nil, fmt.Errorf("invalid plugin dir %q", dir)
		}
		args = append(args, "--plugin-dir", dir)
	}
	args = append(args, "stdio")
	return args, nil
}

// newGrokACPClient starts a `grok agent --no-leader stdio` subprocess and attaches
// reader/dispatcher loops. Call Initialize(ctx) before issuing session RPCs.
func newGrokACPClient(cfg grokACPClientConfig) (*grokACPClient, error) {
	bin := strings.TrimSpace(cfg.Binary)
	if bin == "" {
		bin = grokBinary()
	}
	if bin == "" {
		return nil, errors.New("grok cli (grok) not found")
	}
	args, err := buildGrokACPArgs(cfg)
	if err != nil {
		return nil, err
	}

	procCtx, procCancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, bin, args...)
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}
	if len(cfg.Env) > 0 {
		cmd.Env = cfg.Env
	} else {
		cmd.Env = codexEnv()
	}
	prepareProcess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		procCancel()
		return nil, fmt.Errorf("grok acp stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		procCancel()
		return nil, fmt.Errorf("grok acp stdout pipe: %w", err)
	}
	stderr := newACPStderrBuffer(cfg.StderrBytes)
	cmd.Stderr = stderr

	stdoutDone := make(chan struct{})
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		terminateProcess(cmd.Process)
		select {
		case <-stdoutDone:
		case <-time.After(750 * time.Millisecond):
			killProcess(cmd.Process)
		}
		return nil
	}
	cmd.WaitDelay = 1500 * time.Millisecond

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		procCancel()
		return nil, fmt.Errorf("start grok agent stdio: %w", err)
	}

	return startGrokACPClientLoops(stdout, stdin, cfg, cmd, stderr, procCancel, stdoutDone), nil
}

// dialGrokACPClient starts a `grok agent --no-leader stdio` subprocess and completes
// the ACP v1 initialize handshake.
func dialGrokACPClient(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
	c, err := newGrokACPClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	res, err := c.Initialize(ctx)
	if err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	return c, res, nil
}

// newGrokACPClientFromStreams constructs a grokACPClient over arbitrary reader/writer
// streams for in-process pipe tests and custom transports.
func newGrokACPClientFromStreams(r io.Reader, w io.WriteCloser, cfg grokACPClientConfig) *grokACPClient {
	return startGrokACPClientLoops(r, w, cfg, nil, newACPStderrBuffer(cfg.StderrBytes), nil, make(chan struct{}))
}

func startGrokACPClientLoops(
	r io.Reader,
	w io.WriteCloser,
	cfg grokACPClientConfig,
	cmd *exec.Cmd,
	stderr *acpStderrBuffer,
	procCancel context.CancelFunc,
	stdoutDone chan struct{},
) *grokACPClient {
	ctx, cancel := context.WithCancel(context.Background())
	c := &grokACPClient{
		cfg:          cfg,
		cmd:          cmd,
		stderr:       stderr,
		procCancel:   procCancel,
		ctx:          ctx,
		cancel:       cancel,
		stdin:        w,
		pending:      map[string]chan acpRPCResponse{},
		serverReqs:   map[string]acpPendingServerReq{},
		eventSig:     make(chan struct{}, 1),
		stdoutDone:   stdoutDone,
		readerDone:   make(chan struct{}),
		dispatchDone: make(chan struct{}),
	}
	if cmd == nil {
		if closer, ok := r.(io.Closer); ok {
			c.stdoutCloser = closer
		}
	}
	go c.dispatchLoop()
	go c.readLoop(r)
	return c
}

// Initialize performs the ACP v1 initialize handshake with empty clientCapabilities
// so Grok executes all filesystem and terminal tools in-process.
func (c *grokACPClient) Initialize(ctx context.Context) (*grokInitResult, error) {
	params := map[string]any{
		"protocolVersion": acpProtocolVersion,
		"clientInfo": map[string]any{
			"name":    acpClientName,
			"version": grokACPClientVersion,
		},
		"clientCapabilities": map[string]any{},
	}
	raw, err := c.call(ctx, "initialize", params, true)
	if err != nil {
		return nil, err
	}
	res, err := parseGrokInitResult(raw)
	if err != nil {
		return nil, fmt.Errorf("parse grok initialize result: %w", err)
	}
	c.mu.Lock()
	c.initResult = res
	c.mu.Unlock()
	return res, nil
}

// InitResult returns a copy of the cached initialize result, or nil if not yet initialized.
func (c *grokACPClient) InitResult() *grokInitResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initResult == nil {
		return nil
	}
	cp := *c.initResult
	if len(c.initResult.AuthMethods) > 0 {
		cp.AuthMethods = append([]acpAuthMethod(nil), c.initResult.AuthMethods...)
	}
	if len(c.initResult.ModelState.AvailableModels) > 0 {
		cp.ModelState.AvailableModels = cloneGrokModels(c.initResult.ModelState.AvailableModels)
	}
	if len(c.initResult.Meta) > 0 {
		cp.Meta = append(json.RawMessage(nil), c.initResult.Meta...)
	}
	if len(c.initResult.AgentCapabilities.Meta) > 0 {
		cp.AgentCapabilities.Meta = append(json.RawMessage(nil), c.initResult.AgentCapabilities.Meta...)
	}
	return &cp
}

// Authenticate calls `authenticate` with methodID (defaults to `defaultAuthMethodId`
// from initialize or `"cached_token"`).
func (c *grokACPClient) Authenticate(ctx context.Context, methodID string) (*grokAuthResult, error) {
	methodID = strings.TrimSpace(methodID)
	if methodID == "" {
		if init := c.InitResult(); init != nil && strings.TrimSpace(init.DefaultAuthMethodID) != "" {
			methodID = strings.TrimSpace(init.DefaultAuthMethodID)
		} else {
			methodID = grokDefaultAuthMethod
		}
	}
	raw, err := c.call(ctx, "authenticate", map[string]any{"methodId": methodID}, true)
	if err != nil {
		return nil, err
	}
	return parseGrokAuthResult(raw)
}

// CheckSubscription calls `_x.ai/auth/check_subscription`.
func (c *grokACPClient) CheckSubscription(ctx context.Context) (*grokSubscriptionStatus, error) {
	raw, err := c.call(ctx, grokExtAuthCheckSub, map[string]any{}, true)
	if err != nil {
		return nil, err
	}
	return parseGrokSubscriptionStatus(raw)
}

// ListSessions calls `session/list` (optionally filtered by cwd) and follows
// pagination cursors up to acpMaxListPages.
func (c *grokACPClient) ListSessions(ctx context.Context, cwd string) ([]grokSessionSummary, error) {
	var all []grokSessionSummary
	cursor := ""
	seenCursor := map[string]bool{}
	cwd = strings.TrimSpace(cwd)

	for page := 0; page < acpMaxListPages; page++ {
		params := map[string]any{}
		if cwd != "" {
			params["cwd"] = cwd
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "session/list", params, true)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Sessions   []grokSessionSummary `json:"sessions"`
			NextCursor string               `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("parse session/list result: %w", err)
		}
		all = append(all, resp.Sessions...)
		if resp.NextCursor == "" || seenCursor[resp.NextCursor] {
			break
		}
		seenCursor[resp.NextCursor] = true
		cursor = resp.NextCursor
	}
	if all == nil {
		all = []grokSessionSummary{}
	}
	return all, nil
}

func buildGrokSessionMeta(opts grokSessionOptions) map[string]any {
	meta := map[string]any{
		"yoloMode": opts.YoloMode,
		"autoMode": opts.AutoMode,
	}
	if strings.TrimSpace(opts.Rules) != "" {
		meta["rules"] = opts.Rules
	}
	if strings.TrimSpace(opts.SystemPromptOverride) != "" {
		meta["systemPromptOverride"] = opts.SystemPromptOverride
	}
	if opts.AgentProfile != nil {
		meta["agentProfile"] = opts.AgentProfile
	}
	return meta
}

// NewSession calls `session/new` for cwd and session `_meta` options.
func (c *grokACPClient) NewSession(ctx context.Context, cwd string, opts grokSessionOptions) (*grokSessionState, error) {
	params := map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{},
		"_meta":      buildGrokSessionMeta(opts),
	}
	raw, err := c.call(ctx, "session/new", params, true)
	if err != nil {
		return nil, err
	}
	return parseGrokSessionState(raw, "")
}

// LoadSession calls `session/load` for an existing sessionID, cwd, and `_meta` options.
func (c *grokACPClient) LoadSession(ctx context.Context, sessionID, cwd string, opts grokSessionOptions) (*grokSessionState, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("sessionId is required")
	}
	params := map[string]any{
		"sessionId":  sessionID,
		"cwd":        cwd,
		"mcpServers": []any{},
		"_meta":      buildGrokSessionMeta(opts),
	}
	raw, err := c.call(ctx, "session/load", params, true)
	if err != nil {
		return nil, err
	}
	return parseGrokSessionState(raw, sessionID)
}

// ResumeSession calls `session/resume` for an existing sessionID, cwd, and `_meta` options.
func (c *grokACPClient) ResumeSession(ctx context.Context, sessionID, cwd string, opts grokSessionOptions) (*grokSessionState, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("sessionId is required")
	}
	params := map[string]any{
		"sessionId":  sessionID,
		"cwd":        cwd,
		"mcpServers": []any{},
		"_meta":      buildGrokSessionMeta(opts),
	}
	raw, err := c.call(ctx, "session/resume", params, true)
	if err != nil {
		return nil, err
	}
	return parseGrokSessionState(raw, sessionID)
}

// CloseSession cancels pending interactive requests for sessionID and calls `session/close`.
func (c *grokACPClient) CloseSession(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionId is required")
	}
	c.CancelAllPending(sessionID)
	_, err := c.call(ctx, "session/close", map[string]any{"sessionId": sessionID}, true)
	return err
}

// Prompt calls `session/prompt` with text and/or base64 image content blocks.
// Multiple Prompt calls (e.g. concurrent steering) and Cancel calls may run concurrently.
func (c *grokACPClient) Prompt(ctx context.Context, sessionID string, blocks []acpContentBlock) (*acpPromptResult, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("sessionId is required")
	}
	if len(blocks) == 0 {
		return nil, errors.New("prompt content blocks are required")
	}
	params := map[string]any{
		"sessionId": sessionID,
		"prompt":    blocks,
	}
	raw, err := c.call(ctx, "session/prompt", params, false)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		StopReason string          `json:"stopReason"`
		Usage      *acpUsageUpdate `json:"usage"`
	}
	if len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("parse session/prompt result: %w", err)
		}
	}
	return &acpPromptResult{
		StopReason: parsed.StopReason,
		Usage:      parsed.Usage,
		Raw:        append(json.RawMessage(nil), raw...),
	}, nil
}

// Cancel cancels all pending interactive requests for sessionID and sends a
// `session/cancel` JSON-RPC notification so the active turn aborts cleanly.
func (c *grokACPClient) Cancel(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionId is required")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	c.CancelAllPending(sessionID)
	return c.notify("session/cancel", map[string]any{"sessionId": sessionID})
}

// SetConfigOption calls `session/set_config_option` for configID (e.g. "model",
// "reasoning_effort") with a PLAIN STRING value (Grok ACP rejects object values
// with -32602). Returns any updated configOptions echoed by the server.
func (c *grokACPClient) SetConfigOption(ctx context.Context, sessionID, configID, value string) ([]acpConfigOption, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("sessionId is required")
	}
	if strings.TrimSpace(configID) == "" {
		return nil, errors.New("configId is required")
	}
	raw, err := c.call(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  configID,
		"value":     value,
	}, true)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	if len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if json.Unmarshal(raw, &resp) == nil && len(bytes.TrimSpace(resp.ConfigOptions)) > 0 {
			return parseACPConfigOptions(resp.ConfigOptions), nil
		}
	}
	return nil, nil
}

// SetModel calls `session/set_model` to switch the active model on sessionID.
func (c *grokACPClient) SetModel(ctx context.Context, sessionID, modelID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionId is required")
	}
	if strings.TrimSpace(modelID) == "" {
		return errors.New("modelId is required")
	}
	_, err := c.call(ctx, "session/set_model", map[string]any{
		"sessionId": sessionID,
		"modelId":   modelID,
	}, true)
	return err
}

// NotifyYoloModeChanged sends a JSON-RPC notification (no `id`) for
// `_x.ai/yolo_mode_changed`. Grok ACP hangs on standard `session/set_mode`,
// so live permission mode changes MUST use notifications or session/resume `_meta`.
func (c *grokACPClient) NotifyYoloModeChanged(sessionID string, yoloMode, autoMode bool, permissionMode string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionId is required")
	}
	params := map[string]any{
		"sessionId": sessionID,
		"yolo_mode": yoloMode,
		"auto_mode": autoMode,
	}
	if pm := strings.TrimSpace(permissionMode); pm != "" {
		params["permission_mode"] = pm
	}
	return c.notify(grokExtYoloModeChanged, params)
}

// NotifyTogglePlanMode sends a JSON-RPC notification (no `id`) for
// `_x.ai/toggle_plan_mode`. Calling it as a request with an `id` fails with -32601.
func (c *grokACPClient) NotifyTogglePlanMode(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionId is required")
	}
	return c.notify(grokExtTogglePlanMode, map[string]any{"sessionId": sessionID})
}

// ListModels calls `_x.ai/models/list`, unwrapping Grok's double-wrapped `{result: {...}}` payload.
func (c *grokACPClient) ListModels(ctx context.Context) (*grokModelsState, error) {
	raw, err := c.call(ctx, grokExtModelsList, map[string]any{}, true)
	if err != nil {
		return nil, err
	}
	inner := unwrapGrokResultObject(raw)
	state, err := parseGrokModelsState(inner)
	if err != nil {
		return nil, fmt.Errorf("parse %s result: %w", grokExtModelsList, err)
	}
	return &state, nil
}

// Billing calls `_x.ai/billing` to retrieve live Grok subscription and quota information.
func (c *grokACPClient) Billing(ctx context.Context) (*grokBillingResult, error) {
	raw, err := c.call(ctx, grokExtBilling, map[string]any{}, true)
	if err != nil {
		return nil, err
	}
	return parseGrokBillingResult(raw)
}

// ListCommands calls `_x.ai/commands/list` (global when sessionID is empty,
// session-scoped when sessionID is non-empty).
func (c *grokACPClient) ListCommands(ctx context.Context, sessionID string) (*grokCommandsResult, error) {
	params := map[string]any{}
	if sid := strings.TrimSpace(sessionID); sid != "" {
		params["sessionId"] = sid
	}
	raw, err := c.call(ctx, grokExtCommandsList, params, true)
	if err != nil {
		return nil, err
	}
	return parseGrokCommandsResult(raw)
}

// SessionInfo calls `_x.ai/session/info`, unwrapping Grok's double-wrapped `{result: {...}}` payload.
func (c *grokACPClient) SessionInfo(ctx context.Context, sessionID string) (*grokSessionInfo, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("sessionId is required")
	}
	raw, err := c.call(ctx, grokExtSessionInfo, map[string]any{"sessionId": sessionID}, true)
	if err != nil {
		return nil, err
	}
	return parseGrokSessionInfo(raw)
}

// SessionUsage calls `_x.ai/session/usage` for sessionID.
func (c *grokACPClient) SessionUsage(ctx context.Context, sessionID string) (*grokSessionUsage, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("sessionId is required")
	}
	raw, err := c.call(ctx, grokExtSessionUsage, map[string]any{"sessionId": sessionID}, true)
	if err != nil {
		return nil, err
	}
	return parseGrokSessionUsage(raw)
}

// RenameSession calls `_x.ai/session/rename` with camelCase `sessionId` and `title`.
func (c *grokACPClient) RenameSession(ctx context.Context, sessionID, title string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionId is required")
	}
	_, err := c.call(ctx, grokExtSessionRename, map[string]any{
		"sessionId": sessionID,
		"title":     title,
	}, true)
	return err
}

// DeleteSession cancels pending requests for sessionID and calls `_x.ai/session/delete`.
func (c *grokACPClient) DeleteSession(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionId is required")
	}
	c.CancelAllPending(sessionID)
	_, err := c.call(ctx, grokExtSessionDelete, map[string]any{"sessionId": sessionID}, true)
	return err
}

// ForkSession calls `_x.ai/session/fork` with `sourceSessionId` and returns the new session ID.
func (c *grokACPClient) ForkSession(ctx context.Context, sourceSessionID string) (string, error) {
	if strings.TrimSpace(sourceSessionID) == "" {
		return "", errors.New("sourceSessionId is required")
	}
	raw, err := c.call(ctx, grokExtSessionFork, map[string]any{"sourceSessionId": sourceSessionID}, true)
	if err != nil {
		return "", err
	}
	inner := unwrapGrokResultObject(raw)
	var resp struct {
		SessionID      string `json:"sessionId"`
		SessionIDSnake string `json:"session_id"`
	}
	if len(bytes.TrimSpace(inner)) > 0 && !bytes.Equal(bytes.TrimSpace(inner), []byte("null")) {
		if err := json.Unmarshal(inner, &resp); err != nil {
			return "", fmt.Errorf("parse %s result: %w", grokExtSessionFork, err)
		}
	}
	if resp.SessionID != "" {
		return resp.SessionID, nil
	}
	return resp.SessionIDSnake, nil
}

// CompactConversation calls `_x.ai/compact_conversation` with canonical `session_id`.
func (c *grokACPClient) CompactConversation(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionId is required")
	}
	_, err := c.call(ctx, grokExtCompactConversation, map[string]any{
		"session_id": sessionID,
	}, true)
	return err
}

// RespondPermission sends the JSON-RPC response to a pending `session/request_permission` request.
func (c *grokACPClient) RespondPermission(reqID, optionID string, cancelled bool) error {
	req, ok := c.popServerRequest(reqID, "session/request_permission")
	if !ok {
		return errors.New("permission request is no longer pending")
	}
	var outcome map[string]any
	if cancelled || strings.TrimSpace(optionID) == "" {
		outcome = map[string]any{"outcome": "cancelled"}
	} else {
		outcome = map[string]any{"outcome": "selected", "optionId": optionID}
	}
	return c.writeResponse(req.rawID, map[string]any{"outcome": outcome})
}

// RespondAskUserQuestion sends the JSON-RPC response to a pending `_x.ai/ask_user_question`
// request using Grok's internally tagged enum variants (`accepted`, `chat_about_this`,
// `skip_interview`, `cancelled`) and 6-field `AnsweredQuestion` entries.
func (c *grokACPClient) RespondAskUserQuestion(
	reqID, outcome string,
	answers []grokAnsweredQuestion,
	annotations map[string]grokQuestionAnnotation,
) error {
	outcome = strings.ToLower(strings.TrimSpace(outcome))
	if outcome == "" {
		outcome = "cancelled"
	}
	switch outcome {
	case "accepted", "chat_about_this", "skip_interview", "cancelled":
	default:
		return fmt.Errorf("invalid ask_user_question outcome %q", outcome)
	}
	req, ok := c.popServerRequestAny(reqID, grokExtAskUserQuestion, "x.ai/ask_user_question")
	if !ok {
		return errors.New("ask_user_question request is no longer pending")
	}
	result := map[string]any{"outcome": outcome}
	switch outcome {
	case "accepted":
		result["answers"] = normalizeGrokAnsweredQuestions(answers)
		if annotations == nil {
			annotations = map[string]grokQuestionAnnotation{}
		}
		result["annotations"] = annotations
	case "chat_about_this":
		result["partial_answers"] = normalizeGrokAnsweredQuestions(answers)
	}
	return c.writeResponse(req.rawID, result)
}

func normalizeGrokAnsweredQuestions(in []grokAnsweredQuestion) []grokAnsweredQuestion {
	out := make([]grokAnsweredQuestion, len(in))
	for i, q := range in {
		if q.Options == nil {
			q.Options = []string{}
		}
		if q.Descriptions == nil {
			q.Descriptions = []string{}
		}
		if q.Picked == nil {
			q.Picked = []string{}
		}
		out[i] = q
	}
	return out
}

// RespondExitPlanMode sends the JSON-RPC response to a pending `_x.ai/exit_plan_mode`
// request matching Grok's 2-field `ExitPlanModeExtResponse` (`outcome` + `feedback`).
func (c *grokACPClient) RespondExitPlanMode(reqID, outcome, feedback string) error {
	outcome = strings.TrimSpace(outcome)
	if outcome == "" {
		outcome = "cancelled"
	}
	req, ok := c.popServerRequestAny(reqID, grokExtExitPlanMode, "x.ai/exit_plan_mode")
	if !ok {
		return errors.New("exit_plan_mode request is no longer pending")
	}
	var fb *string
	if s := strings.TrimSpace(feedback); s != "" {
		fb = &s
	}
	result := struct {
		Outcome  string  `json:"outcome"`
		Feedback *string `json:"feedback"`
	}{
		Outcome:  outcome,
		Feedback: fb,
	}
	return c.writeResponse(req.rawID, result)
}

// RespondElicitation sends the JSON-RPC response to a pending `_x.ai/mcp/elicit`
// (or `elicitation/create`) request. Valid actions are "accept", "decline", and
// "cancel" (empty defaults to "cancel").
func (c *grokACPClient) RespondElicitation(reqID, action string, content map[string]any) error {
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		action = "cancel"
	}
	switch action {
	case "accept", "decline", "cancel":
	default:
		return fmt.Errorf("invalid elicitation action %q", action)
	}
	req, ok := c.popServerRequestAny(reqID, grokExtMCPElicit, "x.ai/mcp/elicit", "elicitation/create")
	if !ok {
		return errors.New("elicitation request is no longer pending")
	}
	result := map[string]any{"action": action}
	if action == "accept" {
		if content == nil {
			content = map[string]any{}
		}
		result["content"] = content
	}
	return c.writeResponse(req.rawID, result)
}

// CancelAllPending cancels all pending interactive requests for sessionID
// (or for all sessions when sessionID is empty). Safe to call concurrently or repeatedly.
func (c *grokACPClient) CancelAllPending(sessionID string) {
	c.mu.Lock()
	var keys []string
	for k, req := range c.serverReqs {
		if sessionID == "" || req.sessionID == sessionID {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	reqs := make([]acpPendingServerReq, 0, len(keys))
	for _, k := range keys {
		reqs = append(reqs, c.serverReqs[k])
		delete(c.serverReqs, k)
	}
	c.mu.Unlock()

	for _, req := range reqs {
		switch req.method {
		case "session/request_permission":
			_ = c.writeResponse(req.rawID, map[string]any{
				"outcome": map[string]any{"outcome": "cancelled"},
			})
		case grokExtAskUserQuestion, "x.ai/ask_user_question":
			_ = c.writeResponse(req.rawID, map[string]any{
				"outcome": "cancelled",
			})
		case grokExtExitPlanMode, "x.ai/exit_plan_mode":
			_ = c.writeResponse(req.rawID, struct {
				Outcome  string  `json:"outcome"`
				Feedback *string `json:"feedback"`
			}{
				Outcome:  "cancelled",
				Feedback: nil,
			})
		case grokExtMCPElicit, "x.ai/mcp/elicit", "elicitation/create":
			_ = c.writeResponse(req.rawID, map[string]any{
				"action": "cancel",
			})
		}
	}
}

// PendingRequestCount returns the number of pending interactive server requests
// for sessionID (or all sessions when sessionID is empty).
func (c *grokACPClient) PendingRequestCount(sessionID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sessionID == "" {
		return len(c.serverReqs)
	}
	n := 0
	for _, req := range c.serverReqs {
		if req.sessionID == sessionID {
			n++
		}
	}
	return n
}

// EnqueueEvent schedules fn onto the FIFO dispatchLoop behind every notification
// already parsed by readLoop. Returns false if the dispatch queue is already closed.
func (c *grokACPClient) EnqueueEvent(fn func()) bool {
	if fn == nil {
		return false
	}
	return c.enqueueEvent(grokDispatchEvent{custom: fn})
}

// DrainDispatch inserts a FIFO barrier into the dispatch queue and waits until
// dispatchLoop has finished processing every earlier notification and request
// already read by readLoop. Never call DrainDispatch synchronously from inside
// an OnUpdate / OnExtUpdate / OnSessionNotification / interactive callback.
func (c *grokACPClient) DrainDispatch(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	barrier := make(chan struct{})
	if !c.enqueueEvent(grokDispatchEvent{barrier: barrier}) {
		select {
		case <-c.dispatchDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-barrier:
		return nil
	case <-c.dispatchDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stderr returns the bounded diagnostic stderr output captured from the subprocess.
func (c *grokACPClient) Stderr() string {
	return c.stderr.String()
}

// Closed reports whether the client transport has closed.
func (c *grokACPClient) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Done returns a channel closed once the reader, subprocess, and event dispatcher have exited.
func (c *grokACPClient) Done() <-chan struct{} {
	return c.dispatchDone
}

// Err returns the close or process exit error after shutdown.
func (c *grokACPClient) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// Close cancels pending requests, closes stdin, terminates the subprocess cleanly
// if needed, and waits for all background goroutines to finish.
func (c *grokACPClient) Close() error {
	c.closeOnce.Do(func() {
		c.CancelAllPending("")
		c.cancel()
		c.closeStdin()

		if c.cmd != nil {
			select {
			case <-c.readerDone:
			case <-time.After(300 * time.Millisecond):
				if c.procCancel != nil {
					c.procCancel()
				}
			}
		} else if c.stdoutCloser != nil {
			_ = c.stdoutCloser.Close()
		}
	})
	<-c.readerDone
	<-c.dispatchDone
	return nil
}

func (c *grokACPClient) closeStdin() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.stdinClosed && c.stdin != nil {
		c.stdinClosed = true
		_ = c.stdin.Close()
	}
}

func (c *grokACPClient) call(ctx context.Context, method string, params any, applyTimeout bool) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if applyTimeout && c.cfg.CallTimeout > 0 {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.cfg.CallTimeout)
			defer cancel()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.closed {
		err := c.closedErrorLocked()
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	idNum := c.nextID
	idKey := strconv.FormatInt(idNum, 10)
	ch := make(chan acpRPCResponse, 1)
	c.pending[idKey] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, idKey)
		c.mu.Unlock()
	}()

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      idNum,
		"method":  method,
	}
	if params != nil {
		payload["params"] = params
	}
	if err := c.writeJSON(payload); err != nil {
		return nil, c.wrapTransportError(err)
	}

	select {
	case resp := <-ch:
		return resp.result, resp.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		c.mu.Lock()
		err := c.closedErrorLocked()
		c.mu.Unlock()
		return nil, err
	}
}

func (c *grokACPClient) notify(method string, params any) error {
	c.mu.Lock()
	if c.closed {
		err := c.closedErrorLocked()
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		payload["params"] = params
	}
	return c.wrapTransportError(c.writeJSON(payload))
}

func (c *grokACPClient) writeResponse(rawID json.RawMessage, result any) error {
	payload := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      rawID,
		Result:  result,
	}
	return c.writeJSON(payload)
}

func (c *grokACPClient) writeRPCError(rawID json.RawMessage, code int, message string) error {
	payload := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   acpRPCError     `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      rawID,
		Error: acpRPCError{
			Code:    code,
			Message: message,
		},
	}
	return c.writeJSON(payload)
}

func (c *grokACPClient) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdinClosed || c.stdin == nil {
		return errors.New("grok acp transport closed")
	}
	_, err = c.stdin.Write(b)
	return err
}

func (c *grokACPClient) popServerRequest(reqID, expectedMethod string) (acpPendingServerReq, bool) {
	return c.popServerRequestAny(reqID, expectedMethod)
}

func (c *grokACPClient) popServerRequestAny(reqID string, expectedMethods ...string) (acpPendingServerReq, bool) {
	reqID = strings.TrimSpace(reqID)
	if reqID == "" {
		return acpPendingServerReq{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	req, ok := c.serverReqs[reqID]
	if !ok {
		return acpPendingServerReq{}, false
	}
	if len(expectedMethods) > 0 {
		matched := false
		for _, m := range expectedMethods {
			if m == "" || req.method == m {
				matched = true
				break
			}
		}
		if !matched {
			return acpPendingServerReq{}, false
		}
	}
	delete(c.serverReqs, reqID)
	return req, true
}

func (c *grokACPClient) closedErrorLocked() error {
	if c.closeErr != nil {
		return c.closeErr
	}
	if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
		return fmt.Errorf("grok acp client closed: %s", clipPlain(msg, 400))
	}
	return errors.New("grok acp client closed")
}

func (c *grokACPClient) wrapTransportError(err error) error {
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
		return fmt.Errorf("%w (stderr: %s)", err, clipPlain(msg, 400))
	}
	return err
}

func (c *grokACPClient) readLoop(r io.Reader) {
	defer close(c.readerDone)

	br := bufio.NewReaderSize(r, 64<<10)
	var readErr error
	for {
		line, err := readACPLine(br, acpMaxLineBytes)
		if len(line) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}

	close(c.stdoutDone)
	c.closeStdin()

	var waitErr error
	if c.cmd != nil {
		waitErr = c.cmd.Wait()
		if c.procCancel != nil {
			c.procCancel()
		}
	}

	finalErr := readErr
	if finalErr == nil && waitErr != nil && c.ctx.Err() == nil {
		finalErr = waitErr
	}
	if finalErr != nil {
		finalErr = c.wrapTransportError(finalErr)
	}

	c.mu.Lock()
	c.closed = true
	if c.closeErr == nil {
		c.closeErr = finalErr
	}
	pendingErr := c.closedErrorLocked()
	for id, ch := range c.pending {
		select {
		case ch <- acpRPCResponse{err: pendingErr}:
		default:
		}
		delete(c.pending, id)
	}
	c.serverReqs = map[string]acpPendingServerReq{}
	c.mu.Unlock()

	c.cancel()
	c.closeEventQueue()
}

func (c *grokACPClient) handleLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   *acpRPCError    `json:"error"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}

	hasID := len(bytes.TrimSpace(msg.ID)) > 0 && !bytes.Equal(bytes.TrimSpace(msg.ID), []byte("null"))

	// Case 1: JSON-RPC response to a client request.
	if hasID && msg.Method == "" {
		idKey := acpRawIDKey(msg.ID)
		c.mu.Lock()
		ch := c.pending[idKey]
		c.mu.Unlock()
		if ch != nil {
			resp := acpRPCResponse{result: msg.Result}
			if msg.Error != nil {
				resp.err = msg.Error
			}
			select {
			case ch <- resp:
			default:
			}
		}
		return
	}

	// Case 2: JSON-RPC server notification.
	if !hasID && msg.Method != "" {
		switch msg.Method {
		case "session/update":
			if upd, ok := parseGrokSessionUpdate(msg.Params); ok {
				c.enqueueEvent(grokDispatchEvent{update: &upd})
			}
		case grokMethodSessionUpdate, "x.ai/session/update":
			if upd, ok := parseGrokExtSessionUpdate(msg.Params); ok {
				c.enqueueEvent(grokDispatchEvent{extUpdate: &upd})
			}
		case grokMethodSessionNotification, "x.ai/session_notification":
			if notif, ok := parseGrokSessionNotification(msg.Params); ok {
				c.enqueueEvent(grokDispatchEvent{sessionNotif: &notif})
			}
		}
		return
	}

	// Case 3: JSON-RPC interactive server request (must correlate raw ID and reply).
	if hasID && msg.Method != "" {
		rawID := append(json.RawMessage(nil), bytes.TrimSpace(msg.ID)...)
		reqKey := acpRawIDKey(rawID)
		switch msg.Method {
		case "session/request_permission":
			req := parseGrokPermissionRequest(reqKey, msg.Params)
			c.registerServerReq(reqKey, rawID, req.SessionID, msg.Method)
			c.enqueueEvent(grokDispatchEvent{permission: &req})
		case grokExtAskUserQuestion, "x.ai/ask_user_question":
			req := parseGrokAskUserQuestionRequest(reqKey, msg.Params)
			c.registerServerReq(reqKey, rawID, req.SessionID, msg.Method)
			c.enqueueEvent(grokDispatchEvent{askQuestion: &req})
		case grokExtExitPlanMode, "x.ai/exit_plan_mode":
			req := parseGrokExitPlanModeRequest(reqKey, msg.Params)
			c.registerServerReq(reqKey, rawID, req.SessionID, msg.Method)
			c.enqueueEvent(grokDispatchEvent{exitPlan: &req})
		case grokExtMCPElicit, "x.ai/mcp/elicit", "elicitation/create":
			req := parseGrokElicitationRequest(reqKey, msg.Params)
			c.registerServerReq(reqKey, rawID, req.SessionID, msg.Method)
			c.enqueueEvent(grokDispatchEvent{elicitation: &req})
		default:
			c.enqueueEvent(grokDispatchEvent{
				unknownReq: &acpUnknownServerReq{rawID: rawID, method: msg.Method},
			})
		}
	}
}

func (c *grokACPClient) registerServerReq(reqKey string, rawID json.RawMessage, sessionID, method string) {
	c.mu.Lock()
	c.serverReqs[reqKey] = acpPendingServerReq{
		rawID:     rawID,
		sessionID: sessionID,
		method:    method,
	}
	c.mu.Unlock()
}

func (c *grokACPClient) enqueueEvent(ev grokDispatchEvent) bool {
	c.eventMu.Lock()
	if c.eventClosed {
		c.eventMu.Unlock()
		return false
	}
	c.eventQueue = append(c.eventQueue, ev)
	c.eventMu.Unlock()
	select {
	case c.eventSig <- struct{}{}:
	default:
	}
	return true
}

func (c *grokACPClient) closeEventQueue() {
	c.eventMu.Lock()
	c.eventClosed = true
	c.eventMu.Unlock()
	select {
	case c.eventSig <- struct{}{}:
	default:
	}
}

func (c *grokACPClient) dispatchLoop() {
	defer close(c.dispatchDone)
	for {
		c.eventMu.Lock()
		if len(c.eventQueue) == 0 {
			done := c.eventClosed
			c.eventMu.Unlock()
			if done {
				break
			}
			<-c.eventSig
			continue
		}
		batch := c.eventQueue
		c.eventQueue = nil
		c.eventMu.Unlock()

		for _, ev := range batch {
			switch {
			case ev.barrier != nil:
				close(ev.barrier)
			case ev.custom != nil:
				ev.custom()
			case ev.update != nil:
				if c.cfg.OnUpdate != nil {
					c.cfg.OnUpdate(*ev.update)
				}
			case ev.extUpdate != nil:
				if c.cfg.OnExtUpdate != nil {
					c.cfg.OnExtUpdate(*ev.extUpdate)
				}
			case ev.sessionNotif != nil:
				if c.cfg.OnSessionNotification != nil {
					c.cfg.OnSessionNotification(*ev.sessionNotif)
				}
			case ev.permission != nil:
				if c.cfg.OnPermission != nil {
					c.cfg.OnPermission(*ev.permission)
				}
			case ev.askQuestion != nil:
				if c.cfg.OnAskUserQuestion != nil {
					c.cfg.OnAskUserQuestion(*ev.askQuestion)
				}
			case ev.exitPlan != nil:
				if c.cfg.OnExitPlanMode != nil {
					c.cfg.OnExitPlanMode(*ev.exitPlan)
				}
			case ev.elicitation != nil:
				if c.cfg.OnElicitation != nil {
					c.cfg.OnElicitation(*ev.elicitation)
				}
			case ev.unknownReq != nil:
				_ = c.writeRPCError(ev.unknownReq.rawID, -32601, "method not supported: "+ev.unknownReq.method)
			}
		}
	}
	if c.cfg.OnClose != nil {
		c.cfg.OnClose(c.Err())
	}
}

// --- Parsing helpers ---

func unwrapGrokResultObject(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return trimmed
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(trimmed, &env) == nil {
		inner := bytes.TrimSpace(env.Result)
		if len(inner) > 0 && !bytes.Equal(inner, []byte("null")) {
			return inner
		}
	}
	return trimmed
}

func cloneGrokModels(models []grokModelInfo) []grokModelInfo {
	out := make([]grokModelInfo, len(models))
	for i, m := range models {
		cp := m
		if len(m.ReasoningEfforts) > 0 {
			cp.ReasoningEfforts = append([]grokReasoningEffortOption(nil), m.ReasoningEfforts...)
		}
		if len(m.ContextWindows) > 0 {
			cp.ContextWindows = append(json.RawMessage(nil), m.ContextWindows...)
		}
		if len(m.Meta) > 0 {
			cp.Meta = append(json.RawMessage(nil), m.Meta...)
		}
		out[i] = cp
	}
	return out
}

func parseGrokModelInfo(raw json.RawMessage) (grokModelInfo, bool) {
	var wire struct {
		ModelID                 string                      `json:"modelId"`
		ID                      string                      `json:"id"`
		Name                    string                      `json:"name"`
		Description             string                      `json:"description"`
		TotalContextTokens      int64                       `json:"totalContextTokens"`
		AgentType               string                      `json:"agentType"`
		SupportsReasoningEffort bool                        `json:"supportsReasoningEffort"`
		ReasoningEffort         string                      `json:"reasoningEffort"`
		ReasoningEfforts        []grokReasoningEffortOption `json:"reasoningEfforts"`
		ContextWindows          json.RawMessage             `json:"contextWindows"`
		Meta                    json.RawMessage             `json:"_meta"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return grokModelInfo{}, false
	}
	id := wire.ModelID
	if id == "" {
		id = wire.ID
	}
	if id == "" && wire.Name == "" {
		return grokModelInfo{}, false
	}
	m := grokModelInfo{
		ModelID:                 id,
		Name:                    wire.Name,
		Description:             wire.Description,
		TotalContextTokens:      wire.TotalContextTokens,
		AgentType:               wire.AgentType,
		SupportsReasoningEffort: wire.SupportsReasoningEffort,
		DefaultReasoningEffort:  wire.ReasoningEffort,
		ReasoningEfforts:        wire.ReasoningEfforts,
		ContextWindows:          append(json.RawMessage(nil), wire.ContextWindows...),
		Meta:                    append(json.RawMessage(nil), wire.Meta...),
	}
	if len(bytes.TrimSpace(wire.Meta)) > 0 && !bytes.Equal(bytes.TrimSpace(wire.Meta), []byte("null")) {
		var meta struct {
			TotalContextTokens      int64                       `json:"totalContextTokens"`
			AgentType               string                      `json:"agentType"`
			SupportsReasoningEffort bool                        `json:"supportsReasoningEffort"`
			ReasoningEffort         string                      `json:"reasoningEffort"`
			ReasoningEfforts        []grokReasoningEffortOption `json:"reasoningEfforts"`
			ContextWindows          json.RawMessage             `json:"contextWindows"`
		}
		if json.Unmarshal(wire.Meta, &meta) == nil {
			if m.TotalContextTokens == 0 && meta.TotalContextTokens > 0 {
				m.TotalContextTokens = meta.TotalContextTokens
			}
			if m.AgentType == "" && meta.AgentType != "" {
				m.AgentType = meta.AgentType
			}
			if meta.SupportsReasoningEffort {
				m.SupportsReasoningEffort = true
			}
			if m.DefaultReasoningEffort == "" && meta.ReasoningEffort != "" {
				m.DefaultReasoningEffort = meta.ReasoningEffort
			}
			if len(m.ReasoningEfforts) == 0 && len(meta.ReasoningEfforts) > 0 {
				m.ReasoningEfforts = meta.ReasoningEfforts
			}
			if len(m.ContextWindows) == 0 && len(meta.ContextWindows) > 0 {
				m.ContextWindows = append(json.RawMessage(nil), meta.ContextWindows...)
			}
		}
	}
	if m.DefaultReasoningEffort == "" {
		for _, opt := range m.ReasoningEfforts {
			if opt.Default && opt.Value != "" {
				m.DefaultReasoningEffort = opt.Value
				break
			}
		}
	}
	if len(m.ReasoningEfforts) > 0 {
		m.SupportsReasoningEffort = true
	}
	return m, true
}

func parseGrokModelsState(raw json.RawMessage) (grokModelsState, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return grokModelsState{}, nil
	}
	var wire struct {
		CurrentModelID  string            `json:"currentModelId"`
		AvailableModels []json.RawMessage `json:"availableModels"`
	}
	if err := json.Unmarshal(trimmed, &wire); err != nil {
		return grokModelsState{}, err
	}
	state := grokModelsState{
		CurrentModelID: wire.CurrentModelID,
	}
	if len(wire.AvailableModels) > 0 {
		state.AvailableModels = make([]grokModelInfo, 0, len(wire.AvailableModels))
		for _, item := range wire.AvailableModels {
			if m, ok := parseGrokModelInfo(item); ok {
				state.AvailableModels = append(state.AvailableModels, m)
			}
		}
	}
	return state, nil
}

func parseGrokInitResult(raw json.RawMessage) (*grokInitResult, error) {
	var wire struct {
		ProtocolVersion int             `json:"protocolVersion"`
		AgentInfo       acpAgentInfo    `json:"agentInfo"`
		AuthMethods     []acpAuthMethod `json:"authMethods"`
		Meta            json.RawMessage `json:"_meta"`
		Caps            struct {
			LoadSession        bool                  `json:"loadSession"`
			LoadSessionSnake   bool                  `json:"load_session"`
			PromptCapabilities acpPromptCapabilities `json:"promptCapabilities"`
			MCPCapabilities    grokMCPCapabilities   `json:"mcpCapabilities"`
			Auth               json.RawMessage       `json:"auth"`
			SessionCaps        struct {
				Resume                json.RawMessage `json:"resume"`
				Close                 json.RawMessage `json:"close"`
				Load                  json.RawMessage `json:"load"`
				List                  json.RawMessage `json:"list"`
				AdditionalDirectories json.RawMessage `json:"additionalDirectories"`
			} `json:"sessionCapabilities"`
			SessionCapsSnake struct {
				Resume                json.RawMessage `json:"resume"`
				Close                 json.RawMessage `json:"close"`
				Load                  json.RawMessage `json:"load"`
				List                  json.RawMessage `json:"list"`
				AdditionalDirectories json.RawMessage `json:"additionalDirectories"`
			} `json:"session_capabilities"`
			Meta json.RawMessage `json:"_meta"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	res := &grokInitResult{
		ProtocolVersion: wire.ProtocolVersion,
		AgentInfo:       wire.AgentInfo,
		AuthMethods:     wire.AuthMethods,
		Meta:            append(json.RawMessage(nil), wire.Meta...),
		AgentCapabilities: grokAgentCapabilities{
			LoadSession:        wire.Caps.LoadSession || wire.Caps.LoadSessionSnake,
			PromptCapabilities: wire.Caps.PromptCapabilities,
			MCPCapabilities:    wire.Caps.MCPCapabilities,
			Auth:               acpCapPresent(wire.Caps.Auth),
			Meta:               append(json.RawMessage(nil), wire.Caps.Meta...),
			SessionCapabilities: acpSessionCapabilities{
				Resume:                acpCapPresent(wire.Caps.SessionCaps.Resume) || acpCapPresent(wire.Caps.SessionCapsSnake.Resume),
				Close:                 acpCapPresent(wire.Caps.SessionCaps.Close) || acpCapPresent(wire.Caps.SessionCapsSnake.Close),
				Load:                  acpCapPresent(wire.Caps.SessionCaps.Load) || acpCapPresent(wire.Caps.SessionCapsSnake.Load),
				List:                  acpCapPresent(wire.Caps.SessionCaps.List) || acpCapPresent(wire.Caps.SessionCapsSnake.List),
				AdditionalDirectories: acpCapPresent(wire.Caps.SessionCaps.AdditionalDirectories) || acpCapPresent(wire.Caps.SessionCapsSnake.AdditionalDirectories),
			},
		},
	}

	if len(bytes.TrimSpace(wire.Caps.Meta)) > 0 {
		var capMeta struct {
			FSNotify bool `json:"x.ai/fs_notify"`
		}
		if json.Unmarshal(wire.Caps.Meta, &capMeta) == nil {
			res.AgentCapabilities.FSNotify = capMeta.FSNotify
		}
	}

	if len(bytes.TrimSpace(wire.Meta)) > 0 && !bytes.Equal(bytes.TrimSpace(wire.Meta), []byte("null")) {
		var meta struct {
			AgentVersion        string          `json:"agentVersion"`
			DefaultAuthMethodID string          `json:"defaultAuthMethodId"`
			GrokShell           bool            `json:"grokShell"`
			CurrentWorkingDir   string          `json:"currentWorkingDirectory"`
			ModelState          json.RawMessage `json:"modelState"`
		}
		if json.Unmarshal(wire.Meta, &meta) == nil {
			res.AgentVersion = meta.AgentVersion
			res.DefaultAuthMethodID = meta.DefaultAuthMethodID
			res.GrokShell = meta.GrokShell
			res.CurrentWorkingDir = meta.CurrentWorkingDir
			if ms, err := parseGrokModelsState(meta.ModelState); err == nil {
				res.ModelState = ms
			}
		}
	}
	if res.AgentInfo.Version == "" && res.AgentVersion != "" {
		res.AgentInfo.Version = res.AgentVersion
	}
	if res.AgentInfo.Name == "" && res.GrokShell {
		res.AgentInfo.Name = "grok"
	}
	return res, nil
}

func parseGrokAuthResult(raw json.RawMessage) (*grokAuthResult, error) {
	res := &grokAuthResult{}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return res, nil
	}
	var env struct {
		Meta json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, fmt.Errorf("parse authenticate result: %w", err)
	}
	metaRaw := bytes.TrimSpace(env.Meta)
	if len(metaRaw) == 0 || bytes.Equal(metaRaw, []byte("null")) {
		metaRaw = trimmed
	}
	res.Meta = append(json.RawMessage(nil), metaRaw...)
	var m struct {
		AuthMode              string `json:"auth_mode"`
		AuthModeCamel         string `json:"authMode"`
		SubscriptionTier      string `json:"subscription_tier"`
		SubscriptionTierCamel string `json:"subscriptionTier"`
		IsZDR                 bool   `json:"is_zdr"`
		IsZDRCamel            bool   `json:"isZdr"`
		CodingDataTeam        bool   `json:"coding_data_team"`
		BackendBilled         bool   `json:"backend_billed"`
	}
	if json.Unmarshal(metaRaw, &m) == nil {
		res.AuthMode = grokFirstNonEmpty(m.AuthMode, m.AuthModeCamel)
		res.SubscriptionTier = grokFirstNonEmpty(m.SubscriptionTier, m.SubscriptionTierCamel)
		res.IsZDR = m.IsZDR || m.IsZDRCamel
		res.CodingDataTeam = m.CodingDataTeam
		res.BackendBilled = m.BackendBilled
	}
	return res, nil
}

func parseGrokSubscriptionStatus(raw json.RawMessage) (*grokSubscriptionStatus, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return &grokSubscriptionStatus{}, nil
	}
	var wire struct {
		Authenticated bool            `json:"authenticated"`
		Meta          json.RawMessage `json:"meta"`
		Underscore    json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(trimmed, &wire); err != nil {
		return nil, fmt.Errorf("parse %s result: %w", grokExtAuthCheckSub, err)
	}
	metaRaw := bytes.TrimSpace(wire.Meta)
	if len(metaRaw) == 0 || bytes.Equal(metaRaw, []byte("null")) {
		metaRaw = bytes.TrimSpace(wire.Underscore)
	}
	res := &grokSubscriptionStatus{
		Authenticated: wire.Authenticated,
		Meta:          append(json.RawMessage(nil), metaRaw...),
	}
	if len(metaRaw) > 0 && !bytes.Equal(metaRaw, []byte("null")) {
		var m struct {
			AuthMode              string `json:"auth_mode"`
			AuthModeCamel         string `json:"authMode"`
			SubscriptionTier      string `json:"subscription_tier"`
			SubscriptionTierCamel string `json:"subscriptionTier"`
		}
		if json.Unmarshal(metaRaw, &m) == nil {
			res.AuthMode = grokFirstNonEmpty(m.AuthMode, m.AuthModeCamel)
			res.SubscriptionTier = grokFirstNonEmpty(m.SubscriptionTier, m.SubscriptionTierCamel)
		}
	}
	return res, nil
}

func parseGrokSessionState(raw json.RawMessage, fallbackSessionID string) (*grokSessionState, error) {
	var wire struct {
		SessionID     string          `json:"sessionId"`
		Models        json.RawMessage `json:"models"`
		ConfigOptions json.RawMessage `json:"configOptions"`
		Meta          json.RawMessage `json:"_meta"`
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(trimmed, &wire); err != nil {
			return nil, fmt.Errorf("parse grok session state: %w", err)
		}
	}
	models, err := parseGrokModelsState(wire.Models)
	if err != nil {
		return nil, fmt.Errorf("parse grok session models: %w", err)
	}
	st := &grokSessionState{
		SessionID:     grokFirstNonEmpty(wire.SessionID, fallbackSessionID),
		Models:        models,
		ConfigOptions: parseACPConfigOptions(wire.ConfigOptions),
		Meta:          append(json.RawMessage(nil), wire.Meta...),
	}
	if len(bytes.TrimSpace(wire.Meta)) > 0 && !bytes.Equal(bytes.TrimSpace(wire.Meta), []byte("null")) {
		var meta struct {
			SessionDetail       *grokSessionDetail `json:"x.ai/sessionDetail"`
			TurnCount           int                `json:"x.ai/session/turnCount"`
			MemoryNotesWritable bool               `json:"x.ai/memoryNotesWritable"`
		}
		if json.Unmarshal(wire.Meta, &meta) == nil {
			st.Detail = meta.SessionDetail
			st.TurnCount = meta.TurnCount
			st.MemoryNotesWritable = meta.MemoryNotesWritable
			if st.SessionID == "" && meta.SessionDetail != nil {
				st.SessionID = meta.SessionDetail.SessionID
			}
		}
	}
	return st, nil
}

func parseGrokBillingResult(raw json.RawMessage) (*grokBillingResult, error) {
	inner := unwrapGrokResultObject(raw)
	res := &grokBillingResult{
		Raw: append(json.RawMessage(nil), raw...),
	}
	if len(inner) == 0 || bytes.Equal(inner, []byte("null")) {
		return res, nil
	}
	var wire struct {
		Config                *grokBillingConfig `json:"config"`
		SubscriptionTier      string             `json:"subscription_tier"`
		SubscriptionTierCamel string             `json:"subscriptionTier"`
	}
	if err := json.Unmarshal(inner, &wire); err != nil {
		return nil, fmt.Errorf("parse %s result: %w", grokExtBilling, err)
	}
	res.Config = wire.Config
	res.SubscriptionTier = grokFirstNonEmpty(wire.SubscriptionTier, wire.SubscriptionTierCamel)
	return res, nil
}

func parseGrokCommandsResult(raw json.RawMessage) (*grokCommandsResult, error) {
	inner := unwrapGrokResultObject(raw)
	res := &grokCommandsResult{}
	if len(inner) == 0 || bytes.Equal(inner, []byte("null")) {
		return res, nil
	}
	var wire struct {
		Commands []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Input       json.RawMessage `json:"input"`
		} `json:"commands"`
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal(inner, &wire); err != nil {
		return nil, fmt.Errorf("parse %s result: %w", grokExtCommandsList, err)
	}
	res.Tools = wire.Tools
	if len(wire.Commands) > 0 {
		res.Commands = make([]acpAvailableCommand, 0, len(wire.Commands))
		for _, item := range wire.Commands {
			cmd := acpAvailableCommand{
				Name:        item.Name,
				Description: item.Description,
				InputHint:   parseGrokCommandInputHint(item.Input),
			}
			res.Commands = append(res.Commands, cmd)
		}
	}
	return res, nil
}

func parseGrokCommandInputHint(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var obj struct {
		Hint string `json:"hint"`
	}
	if json.Unmarshal(trimmed, &obj) == nil && obj.Hint != "" {
		return obj.Hint
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return s
	}
	return ""
}

func parseGrokSessionInfo(raw json.RawMessage) (*grokSessionInfo, error) {
	inner := unwrapGrokResultObject(raw)
	if len(inner) == 0 || bytes.Equal(inner, []byte("null")) {
		return &grokSessionInfo{Raw: append(json.RawMessage(nil), raw...)}, nil
	}
	var info grokSessionInfo
	if err := json.Unmarshal(inner, &info); err != nil {
		return nil, fmt.Errorf("parse %s result: %w", grokExtSessionInfo, err)
	}
	info.Raw = append(json.RawMessage(nil), raw...)
	return &info, nil
}

func parseGrokSessionUsage(raw json.RawMessage) (*grokSessionUsage, error) {
	inner := unwrapGrokResultObject(raw)
	if len(inner) == 0 || bytes.Equal(inner, []byte("null")) {
		return &grokSessionUsage{}, nil
	}
	var env struct {
		Usage *grokSessionUsage `json:"usage"`
	}
	if err := json.Unmarshal(inner, &env); err == nil && env.Usage != nil {
		return env.Usage, nil
	}
	var flat grokSessionUsage
	if err := json.Unmarshal(inner, &flat); err != nil {
		return nil, fmt.Errorf("parse %s result: %w", grokExtSessionUsage, err)
	}
	return &flat, nil
}

func parseGrokSessionUpdate(paramsRaw json.RawMessage) (grokSessionUpdate, bool) {
	var envelope struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
		Meta      json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(paramsRaw, &envelope) != nil || len(bytes.TrimSpace(envelope.Update)) == 0 {
		return grokSessionUpdate{}, false
	}
	var common struct {
		SessionUpdate     string          `json:"sessionUpdate"`
		Content           json.RawMessage `json:"content"`
		Text              string          `json:"text"`
		Entries           []grokPlanEntry `json:"entries"`
		Steps             []grokPlanEntry `json:"steps"`
		Items             []grokPlanEntry `json:"items"`
		Used              int64           `json:"used"`
		Size              int64           `json:"size"`
		Cost              *acpUsageCost   `json:"cost"`
		Title             string          `json:"title"`
		UpdatedAt         string          `json:"updatedAt"`
		CurrentModeID     string          `json:"currentModeId"`
		ModeID            string          `json:"modeId"`
		ConfigOptions     json.RawMessage `json:"configOptions"`
		ConfigID          string          `json:"configId"`
		Value             string          `json:"value"`
		Meta              json.RawMessage `json:"_meta"`
		AvailableCommands []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Input       json.RawMessage `json:"input"`
		} `json:"availableCommands"`
	}
	if json.Unmarshal(envelope.Update, &common) != nil || common.SessionUpdate == "" {
		return grokSessionUpdate{}, false
	}

	u := grokSessionUpdate{
		SessionID: envelope.SessionID,
		Kind:      common.SessionUpdate,
		Meta:      append(json.RawMessage(nil), envelope.Meta...),
		Raw:       append(json.RawMessage(nil), envelope.Update...),
	}
	if len(bytes.TrimSpace(envelope.Meta)) > 0 {
		var em struct {
			EventID          string `json:"eventId"`
			AgentTimestampMs int64  `json:"agentTimestampMs"`
		}
		if json.Unmarshal(envelope.Meta, &em) == nil {
			u.EventID = em.EventID
			u.AgentTimestampMs = em.AgentTimestampMs
		}
	}

	switch common.SessionUpdate {
	case "agent_message_chunk", "agent_thought_chunk", "user_message_chunk":
		var block acpContentBlock
		if len(bytes.TrimSpace(common.Content)) > 0 {
			if json.Unmarshal(common.Content, &block) == nil && (block.Type != "" || block.Text != "" || block.Data != "") {
				u.Content = &block
				u.Text = block.Text
			} else {
				var s string
				if json.Unmarshal(common.Content, &s) == nil {
					u.Text = s
					u.Content = &acpContentBlock{Type: "text", Text: s}
				}
			}
		}
		if u.Text == "" && common.Text != "" {
			u.Text = common.Text
		}
	case "tool_call", "tool_call_update":
		tc := parseACPToolCallInfo(envelope.Update)
		if tc.Name == "" && len(bytes.TrimSpace(common.Meta)) > 0 {
			tc.Name = parseGrokToolNameMeta(common.Meta)
		}
		u.ToolCall = &tc
	case "plan":
		switch {
		case len(common.Entries) > 0:
			u.Plan = common.Entries
		case len(common.Steps) > 0:
			u.Plan = common.Steps
		case len(common.Items) > 0:
			u.Plan = common.Items
		}
		if common.Text != "" {
			u.Text = common.Text
		}
	case "usage_update":
		u.Usage = &acpUsageUpdate{
			Used: common.Used,
			Size: common.Size,
			Cost: common.Cost,
		}
	case "available_commands_update":
		cmds := make([]acpAvailableCommand, 0, len(common.AvailableCommands))
		for _, item := range common.AvailableCommands {
			cmds = append(cmds, acpAvailableCommand{
				Name:        item.Name,
				Description: item.Description,
				InputHint:   parseGrokCommandInputHint(item.Input),
			})
		}
		u.AvailableCommands = cmds
	case "session_info_update":
		u.Title = common.Title
		u.UpdatedAt = common.UpdatedAt
	case "current_mode_update":
		u.CurrentModeID = grokFirstNonEmpty(common.CurrentModeID, common.ModeID)
	case "config_option_update", "config_options_update":
		u.ConfigOptions = parseACPConfigOptions(common.ConfigOptions)
		if len(u.ConfigOptions) == 0 && common.ConfigID != "" {
			u.ConfigOptions = []acpConfigOption{{
				ID:           common.ConfigID,
				CurrentValue: common.Value,
			}}
		}
	}
	return u, true
}

func parseGrokToolNameMeta(metaRaw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(metaRaw, &m) != nil {
		return ""
	}
	toolRaw, ok := m["x.ai/tool"]
	if !ok || len(bytes.TrimSpace(toolRaw)) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(toolRaw, &s) == nil && s != "" {
		return s
	}
	var obj struct {
		Name     string `json:"name"`
		ToolName string `json:"toolName"`
	}
	if json.Unmarshal(toolRaw, &obj) == nil {
		return grokFirstNonEmpty(obj.Name, obj.ToolName)
	}
	return ""
}

func parseGrokExtSessionUpdate(paramsRaw json.RawMessage) (grokExtSessionUpdate, bool) {
	trimmed := bytes.TrimSpace(paramsRaw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return grokExtSessionUpdate{}, false
	}
	var env struct {
		SessionID      string          `json:"sessionId"`
		SessionIDSnake string          `json:"session_id"`
		Update         json.RawMessage `json:"update"`
		Meta           json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(trimmed, &env) != nil {
		return grokExtSessionUpdate{}, false
	}
	inner := bytes.TrimSpace(env.Update)
	if len(inner) == 0 || bytes.Equal(inner, []byte("null")) {
		inner = trimmed
	}
	var body struct {
		SessionID           string            `json:"sessionId"`
		SessionIDSnake      string            `json:"session_id"`
		SessionUpdate       string            `json:"sessionUpdate"`
		Type                string            `json:"type"`
		Kind                string            `json:"kind"`
		Event               string            `json:"event"`
		StopReason          string            `json:"stop_reason"`
		StopReasonCamel     string            `json:"stopReason"`
		ElapsedMs           int64             `json:"elapsed_ms"`
		ElapsedMsCamel      int64             `json:"elapsedMs"`
		CostUsdTicks        int64             `json:"costUsdTicks"`
		CostUsdTicksSnake   int64             `json:"cost_usd_ticks"`
		Usage               *grokSessionUsage `json:"usage"`
		SubagentID          string            `json:"subagent_id"`
		SubagentIDCamel     string            `json:"subagentId"`
		ChildSessionID      string            `json:"child_session_id"`
		ChildSessionIDCamel string            `json:"childSessionId"`
		Status              string            `json:"status"`
		Summary             string            `json:"summary"`
		Recap               string            `json:"recap"`
		Text                string            `json:"text"`
		Message             string            `json:"message"`
	}
	if json.Unmarshal(inner, &body) != nil {
		return grokExtSessionUpdate{}, false
	}
	kind := grokFirstNonEmpty(body.SessionUpdate, body.Type, body.Kind, body.Event)
	if kind == "" {
		if extKind, extPayload, ok := grokUnwrapSingleTaggedObject(inner); ok {
			kind = extKind
			if len(extPayload) > 0 && extPayload[0] == '{' {
				_ = json.Unmarshal(extPayload, &body)
			}
		}
	}
	sid := grokFirstNonEmpty(env.SessionID, env.SessionIDSnake, body.SessionID, body.SessionIDSnake)
	if kind == "" && sid == "" {
		return grokExtSessionUpdate{}, false
	}
	elapsed := body.ElapsedMs
	if elapsed == 0 {
		elapsed = body.ElapsedMsCamel
	}
	ticks := body.CostUsdTicks
	if ticks == 0 {
		ticks = body.CostUsdTicksSnake
	}
	if ticks == 0 && body.Usage != nil {
		ticks = body.Usage.CostUsdTicks
	}
	u := grokExtSessionUpdate{
		SessionID:      sid,
		Kind:           kind,
		StopReason:     grokFirstNonEmpty(body.StopReason, body.StopReasonCamel),
		ElapsedMs:      elapsed,
		CostUsdTicks:   ticks,
		Usage:          body.Usage,
		SubagentID:     grokFirstNonEmpty(body.SubagentID, body.SubagentIDCamel),
		ChildSessionID: grokFirstNonEmpty(body.ChildSessionID, body.ChildSessionIDCamel),
		Status:         body.Status,
		Summary:        grokFirstNonEmpty(body.Summary, body.Recap, body.Text, body.Message),
		Meta:           append(json.RawMessage(nil), env.Meta...),
		Raw:            append(json.RawMessage(nil), inner...),
	}
	if len(bytes.TrimSpace(env.Meta)) > 0 {
		var em struct {
			EventID          string `json:"eventId"`
			AgentTimestampMs int64  `json:"agentTimestampMs"`
		}
		if json.Unmarshal(env.Meta, &em) == nil {
			u.EventID = em.EventID
			u.AgentTimestampMs = em.AgentTimestampMs
		}
	}
	return u, true
}

func parseGrokSessionNotification(paramsRaw json.RawMessage) (grokSessionNotification, bool) {
	trimmed := bytes.TrimSpace(paramsRaw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return grokSessionNotification{}, false
	}
	var wire struct {
		SessionID        string          `json:"sessionId"`
		SessionIDSnake   string          `json:"session_id"`
		Type             string          `json:"type"`
		Kind             string          `json:"kind"`
		NotificationType string          `json:"notificationType"`
		Event            string          `json:"event"`
		ModelID          string          `json:"modelId"`
		ModelIDSnake     string          `json:"model_id"`
		Model            string          `json:"model"`
		Message          string          `json:"message"`
		Text             string          `json:"text"`
		Summary          string          `json:"summary"`
		Notification     json.RawMessage `json:"notification"`
		Payload          json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(trimmed, &wire) != nil {
		return grokSessionNotification{}, false
	}
	n := grokSessionNotification{
		SessionID: grokFirstNonEmpty(wire.SessionID, wire.SessionIDSnake),
		Kind:      grokFirstNonEmpty(wire.Type, wire.Kind, wire.NotificationType, wire.Event),
		ModelID:   grokFirstNonEmpty(wire.ModelID, wire.ModelIDSnake, wire.Model),
		Message:   grokFirstNonEmpty(wire.Message, wire.Text, wire.Summary),
		Raw:       append(json.RawMessage(nil), trimmed...),
	}
	nested := bytes.TrimSpace(wire.Notification)
	if len(nested) == 0 || bytes.Equal(nested, []byte("null")) {
		nested = bytes.TrimSpace(wire.Payload)
	}
	if len(nested) > 0 && !bytes.Equal(nested, []byte("null")) {
		var sub struct {
			Type         string `json:"type"`
			Kind         string `json:"kind"`
			Event        string `json:"event"`
			ModelID      string `json:"modelId"`
			ModelIDSnake string `json:"model_id"`
			Model        string `json:"model"`
			Message      string `json:"message"`
			Text         string `json:"text"`
			Summary      string `json:"summary"`
		}
		if json.Unmarshal(nested, &sub) == nil {
			n.Kind = grokFirstNonEmpty(n.Kind, sub.Type, sub.Kind, sub.Event)
			n.ModelID = grokFirstNonEmpty(n.ModelID, sub.ModelID, sub.ModelIDSnake, sub.Model)
			n.Message = grokFirstNonEmpty(n.Message, sub.Message, sub.Text, sub.Summary)
		}
		if n.Kind == "" {
			if extKind, extPayload, ok := grokUnwrapSingleTaggedObject(nested); ok {
				n.Kind = extKind
				if len(extPayload) > 0 && extPayload[0] == '{' {
					var extSub struct {
						ModelID      string `json:"modelId"`
						ModelIDSnake string `json:"model_id"`
						Model        string `json:"model"`
						Message      string `json:"message"`
						Text         string `json:"text"`
						Summary      string `json:"summary"`
					}
					if json.Unmarshal(extPayload, &extSub) == nil {
						n.ModelID = grokFirstNonEmpty(n.ModelID, extSub.ModelID, extSub.ModelIDSnake, extSub.Model)
						n.Message = grokFirstNonEmpty(n.Message, extSub.Message, extSub.Text, extSub.Summary)
					}
				}
			}
		}
	}
	if n.SessionID == "" && n.Kind == "" && n.Message == "" {
		return grokSessionNotification{}, false
	}
	return n, true
}

func grokUnwrapSingleTaggedObject(raw json.RawMessage) (string, json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(trimmed, &m) != nil || len(m) != 1 {
		return "", nil, false
	}
	for k, v := range m {
		if k == "sessionId" || k == "session_id" || k == "_meta" {
			return "", nil, false
		}
		return k, bytes.TrimSpace(v), true
	}
	return "", nil, false
}

func parseGrokPermissionRequest(reqID string, paramsRaw json.RawMessage) grokPermissionRequest {
	base := parseACPPermissionRequest(reqID, paramsRaw)
	return grokPermissionRequest{
		RequestID: base.RequestID,
		SessionID: base.SessionID,
		ToolCall:  base.ToolCall,
		Options:   base.Options,
		Raw:       append(json.RawMessage(nil), paramsRaw...),
	}
}

func parseGrokAskUserQuestionRequest(reqID string, paramsRaw json.RawMessage) grokAskUserQuestionRequest {
	var wire struct {
		SessionID  string `json:"sessionId"`
		ToolCallID string `json:"toolCallId"`
		Mode       string `json:"mode"`
		Questions  []struct {
			ID               string               `json:"id"`
			Question         string               `json:"question"`
			Options          []grokQuestionOption `json:"options"`
			MultiSelect      bool                 `json:"multiSelect"`
			MultiSelectSnake bool                 `json:"multi_select"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(paramsRaw, &wire)
	qs := make([]grokQuestion, 0, len(wire.Questions))
	for _, q := range wire.Questions {
		qs = append(qs, grokQuestion{
			ID:          q.ID,
			Question:    q.Question,
			Options:     q.Options,
			MultiSelect: q.MultiSelect || q.MultiSelectSnake,
		})
	}
	return grokAskUserQuestionRequest{
		RequestID:  reqID,
		SessionID:  wire.SessionID,
		ToolCallID: wire.ToolCallID,
		Questions:  qs,
		Mode:       wire.Mode,
		Raw:        append(json.RawMessage(nil), paramsRaw...),
	}
}

func parseGrokExitPlanModeRequest(reqID string, paramsRaw json.RawMessage) grokExitPlanModeRequest {
	var wire struct {
		SessionID   string `json:"sessionId"`
		ToolCallID  string `json:"toolCallId"`
		PlanContent string `json:"planContent"`
	}
	_ = json.Unmarshal(paramsRaw, &wire)
	return grokExitPlanModeRequest{
		RequestID:   reqID,
		SessionID:   wire.SessionID,
		ToolCallID:  wire.ToolCallID,
		PlanContent: wire.PlanContent,
		Raw:         append(json.RawMessage(nil), paramsRaw...),
	}
}

func parseGrokElicitationRequest(reqID string, paramsRaw json.RawMessage) grokElicitationRequest {
	base := parseACPElicitationRequest(reqID, paramsRaw)
	var extra struct {
		ToolCallID    string `json:"toolCallId"`
		ServerName    string `json:"serverName"`
		ElicitationID string `json:"elicitationId"`
	}
	_ = json.Unmarshal(paramsRaw, &extra)
	return grokElicitationRequest{
		RequestID:       base.RequestID,
		SessionID:       base.SessionID,
		ToolCallID:      extra.ToolCallID,
		ServerName:      extra.ServerName,
		ElicitationID:   extra.ElicitationID,
		Mode:            base.Mode,
		Message:         base.Message,
		URL:             base.URL,
		Fields:          base.Fields,
		RequestedSchema: base.RequestedSchema,
		Raw:             append(json.RawMessage(nil), paramsRaw...),
	}
}

func grokFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
