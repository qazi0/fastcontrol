package main

// Antigravity ACP JSON-RPC 2.0 client over stdio.
// Communicates with `agy acp --app-data-dir <dir>` using Agent Client Protocol v1
// line-delimited JSON-RPC 2.0 framing, including streaming session updates,
// structured tool diffs, interactive permission requests, and MCP elicitation forms.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
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
	acpProtocolVersion = 1
	acpClientName      = "fastcontrol"
	acpClientVersion   = "0.2.17"

	acpDefaultAppDataDir = "antigravity"
	acpCLIAppDataDir     = "antigravity-cli"

	acpDefaultStderrBytes = 16 << 10
	acpMaxLineBytes       = 64 << 20
	acpSupportProbeLimit  = 5 * time.Second
	acpMaxListPages       = 50
	acpDiffContextLines   = 3
	acpDiffMaxDPMatrix    = 400000
)

type acpSupportCacheEntry struct {
	size      int64
	modTimeNs int64
	supported bool
}

var (
	agyACPCacheMu    sync.Mutex
	agyACPCache      = map[string]acpSupportCacheEntry{}
	agyACPInFlight   = map[string]chan struct{}{}
	agyProbeHelpFunc = probeAgyACPHelp
)

// agyBinary locates the Antigravity CLI binary (`agy`) via PATH and standard
// platform install paths such as `~/.local/bin/agy`.
func agyBinary() string {
	names := []string{"agy"}
	if runtime.GOOS == "windows" {
		names = []string{"agy.exe", "agy"}
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil && isAgyExecutable(p) {
			return p
		}
	}
	home, _ := os.UserHomeDir()
	for _, candidate := range agyBinaryCandidates(home, os.Getenv("LOCALAPPDATA"), runtime.GOOS) {
		if isAgyExecutable(candidate) {
			return candidate
		}
	}
	return ""
}

func agyBinaryCandidates(home, localAppData, goos string) []string {
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
		add(home, ".local", "bin", "agy.exe")
		add(localAppData, "Antigravity", "agy.exe")
		add(localAppData, "Programs", "Antigravity", "agy.exe")
		add(localAppData, "Programs", "Antigravity", "bin", "agy.exe")
		return out
	}
	add(home, ".local", "bin", "agy")
	if goos == "darwin" {
		out = append(out, "/opt/homebrew/bin/agy", "/usr/local/bin/agy")
	} else {
		out = append(out, "/usr/local/bin/agy", "/usr/bin/agy")
	}
	return out
}

func isAgyExecutable(path string) bool {
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

// agySupportsACP verifies that `bin` supports `acp` and `--app-data-dir`.
// Results are cached under a mutex keyed by path, size, and mtime so repeated
// checks do not spawn extra subprocesses while still noticing in-place upgrades.
func agySupportsACP(bin string) bool {
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
		agyACPCacheMu.Lock()
		if entry, ok := agyACPCache[bin]; ok && entry.size == size && entry.modTimeNs == modNs {
			agyACPCacheMu.Unlock()
			return entry.supported
		}
		if wait, busy := agyACPInFlight[key]; busy {
			agyACPCacheMu.Unlock()
			<-wait
			continue
		}
		done := make(chan struct{})
		agyACPInFlight[key] = done
		probe := agyProbeHelpFunc
		agyACPCacheMu.Unlock()

		supported := probe(bin)

		agyACPCacheMu.Lock()
		agyACPCache[bin] = acpSupportCacheEntry{
			size:      size,
			modTimeNs: modNs,
			supported: supported,
		}
		delete(agyACPInFlight, key)
		close(done)
		agyACPCacheMu.Unlock()
		return supported
	}
}

func probeAgyACPHelp(bin string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), acpSupportProbeLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "acp", "--help")
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
	text := string(out)
	return strings.Contains(text, "--app-data-dir")
}

// acpClientConfig configures an Antigravity ACP JSON-RPC 2.0 client.
type acpClientConfig struct {
	Binary                     string
	AppDataDir                 string
	GeminiDir                  string
	Cwd                        string
	DangerouslySkipPermissions bool
	DisableTelemetry           bool
	Env                        []string
	StderrBytes                int
	CallTimeout                time.Duration

	// Callbacks are dispatched in FIFO wire order on a dedicated goroutine
	// separate from the stdio reader, so callbacks may safely invoke client
	// methods such as RespondPermission, RespondElicitation, or Cancel.
	OnUpdate      func(acpSessionUpdate)
	OnPermission  func(acpPermissionRequest)
	OnElicitation func(acpElicitationRequest)
	OnClose       func(error)
}

// acpRPCError represents a JSON-RPC 2.0 error returned by the ACP server.
type acpRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *acpRPCError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("acp rpc error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("acp rpc error %d", e.Code)
}

type acpAgentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type acpPromptCapabilities struct {
	Image           bool `json:"image,omitempty"`
	Audio           bool `json:"audio,omitempty"`
	EmbeddedContext bool `json:"embeddedContext,omitempty"`
}

type acpSessionCapabilities struct {
	Resume                bool `json:"resume,omitempty"`
	Close                 bool `json:"close,omitempty"`
	Load                  bool `json:"load,omitempty"`
	List                  bool `json:"list,omitempty"`
	AdditionalDirectories bool `json:"additionalDirectories,omitempty"`
}

type acpAgentCapabilities struct {
	LoadSession          bool                   `json:"loadSession,omitempty"`
	PromptCapabilities   acpPromptCapabilities  `json:"promptCapabilities,omitempty"`
	SessionCapabilities  acpSessionCapabilities `json:"sessionCapabilities,omitempty"`
	SupportsTier         bool                   `json:"supportsTier,omitempty"`
	SupportsSystemAppend bool                   `json:"supportsSystemAppend,omitempty"`
}

type acpTierInfo struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	UpgradeURI string `json:"upgradeUri,omitempty"`
}

type acpAuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type acpInitResult struct {
	ProtocolVersion   int                  `json:"protocolVersion"`
	AgentInfo         acpAgentInfo         `json:"agentInfo"`
	AgentCapabilities acpAgentCapabilities `json:"agentCapabilities"`
	Tier              *acpTierInfo         `json:"tier,omitempty"`
	AuthMethods       []acpAuthMethod      `json:"authMethods,omitempty"`
}

type acpSessionSummary struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd,omitempty"`
	Title     string `json:"title,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type acpModelInfo struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type acpModelsState struct {
	CurrentModelID  string         `json:"currentModelId,omitempty"`
	AvailableModels []acpModelInfo `json:"availableModels,omitempty"`
}

type acpModeInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type acpModesState struct {
	CurrentModeID  string        `json:"currentModeId,omitempty"`
	AvailableModes []acpModeInfo `json:"availableModes,omitempty"`
}

type acpConfigOptionChoice struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type acpConfigOption struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name,omitempty"`
	Category     string                  `json:"category,omitempty"`
	Type         string                  `json:"type,omitempty"`
	CurrentValue string                  `json:"currentValue,omitempty"`
	Options      []acpConfigOptionChoice `json:"options,omitempty"`
}

type acpSessionState struct {
	SessionID     string            `json:"sessionId"`
	Models        acpModelsState    `json:"models,omitempty"`
	Modes         acpModesState     `json:"modes,omitempty"`
	ConfigOptions []acpConfigOption `json:"configOptions,omitempty"`
	Tier          *acpTierInfo      `json:"tier,omitempty"`
}

func (s *acpSessionState) ConfigOption(id string) *acpConfigOption {
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

func (s *acpSessionState) CurrentModel() string {
	if opt := s.ConfigOption("model"); opt != nil && opt.CurrentValue != "" {
		return opt.CurrentValue
	}
	if s != nil {
		return s.Models.CurrentModelID
	}
	return ""
}

func (s *acpSessionState) CurrentThoughtLevel() string {
	if opt := s.ConfigOption("thought_level"); opt != nil {
		return opt.CurrentValue
	}
	return ""
}

func (s *acpSessionState) CurrentMode() string {
	if opt := s.ConfigOption("mode"); opt != nil && opt.CurrentValue != "" {
		return opt.CurrentValue
	}
	if s != nil {
		return s.Modes.CurrentModeID
	}
	return ""
}

type acpContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}

func acpTextBlock(text string) acpContentBlock {
	return acpContentBlock{Type: "text", Text: text}
}

func acpImageBlock(data []byte, mimeType string) acpContentBlock {
	return acpContentBlock{
		Type:     "image",
		Data:     base64.StdEncoding.EncodeToString(data),
		MimeType: mimeType,
	}
}

func acpImageBlockFromFile(path string) (acpContentBlock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return acpContentBlock{}, err
	}
	mime := imageMIME(b)
	if mime == "" {
		return acpContentBlock{}, errors.New("unsupported image format")
	}
	return acpImageBlock(b, mime), nil
}

type acpUsageCost struct {
	Amount   float64 `json:"amount,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

type acpUsageUpdate struct {
	Used int64         `json:"used,omitempty"`
	Size int64         `json:"size,omitempty"`
	Cost *acpUsageCost `json:"cost,omitempty"`
}

type acpPromptResult struct {
	StopReason string          `json:"stopReason,omitempty"`
	Usage      *acpUsageUpdate `json:"usage,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

type acpToolLocation struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

type acpToolDiff struct {
	Path    string `json:"path,omitempty"`
	OldText string `json:"oldText,omitempty"`
	NewText string `json:"newText,omitempty"`
	HasOld  bool   `json:"hasOld,omitempty"`
}

type acpToolContent struct {
	Type       string           `json:"type"`
	Text       string           `json:"text,omitempty"`
	Content    *acpContentBlock `json:"content,omitempty"`
	Diff       *acpToolDiff     `json:"diff,omitempty"`
	TerminalID string           `json:"terminalId,omitempty"`
}

type acpToolCallInfo struct {
	ToolCallID string            `json:"toolCallId"`
	Name       string            `json:"name,omitempty"`
	Args       map[string]any    `json:"args,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Title      string            `json:"title,omitempty"`
	Status     string            `json:"status,omitempty"`
	Locations  []acpToolLocation `json:"locations,omitempty"`
	RawInput   json.RawMessage   `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage   `json:"rawOutput,omitempty"`
	Content    []acpToolContent  `json:"content,omitempty"`
}

// DiffText formats all structured diff blocks in this tool call into unified diff text.
func (t acpToolCallInfo) DiffText() string {
	var parts []string
	for _, block := range t.Content {
		if block.Diff == nil {
			continue
		}
		if d := formatACPDiff(block.Diff.OldText, block.Diff.NewText, block.Diff.Path); d != "" {
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, "\n")
}

// TextOutput returns concatenated text content blocks from this tool call.
func (t acpToolCallInfo) TextOutput() string {
	var parts []string
	for _, block := range t.Content {
		if block.Text != "" {
			parts = append(parts, block.Text)
		} else if block.Content != nil && block.Content.Text != "" {
			parts = append(parts, block.Content.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// InputMap returns Args if populated, otherwise unmarshals RawInput if it is a JSON object.
func (t acpToolCallInfo) InputMap() map[string]any {
	if len(t.Args) > 0 {
		return t.Args
	}
	if len(t.RawInput) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(t.RawInput, &m) == nil {
		return m
	}
	return nil
}

type acpAvailableCommand struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputHint   string `json:"inputHint,omitempty"`
}

type acpSessionUpdate struct {
	SessionID         string                `json:"sessionId"`
	Kind              string                `json:"sessionUpdate"`
	Text              string                `json:"text,omitempty"`
	Content           *acpContentBlock      `json:"content,omitempty"`
	ToolCall          *acpToolCallInfo      `json:"toolCall,omitempty"`
	Usage             *acpUsageUpdate       `json:"usage,omitempty"`
	AvailableCommands []acpAvailableCommand `json:"availableCommands,omitempty"`
	Title             string                `json:"title,omitempty"`
	UpdatedAt         string                `json:"updatedAt,omitempty"`
	CurrentModeID     string                `json:"currentModeId,omitempty"`
	ConfigOptions     []acpConfigOption     `json:"configOptions,omitempty"`
	Raw               json.RawMessage       `json:"-"`
}

type acpPermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind,omitempty"`
}

type acpPermissionRequest struct {
	RequestID string                `json:"requestId"`
	SessionID string                `json:"sessionId"`
	ToolCall  acpToolCallInfo       `json:"toolCall"`
	Options   []acpPermissionOption `json:"options,omitempty"`
}

type acpElicitationOption struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type acpElicitationField struct {
	Name        string                 `json:"name"`
	Type        string                 `json:"type,omitempty"`
	Title       string                 `json:"title,omitempty"`
	Description string                 `json:"description,omitempty"`
	Required    bool                   `json:"required,omitempty"`
	MultiSelect bool                   `json:"multiSelect,omitempty"`
	Secret      bool                   `json:"secret,omitempty"`
	Format      string                 `json:"format,omitempty"`
	Default     any                    `json:"default,omitempty"`
	Options     []acpElicitationOption `json:"options,omitempty"`
}

type acpElicitationRequest struct {
	RequestID       string                `json:"requestId"`
	SessionID       string                `json:"sessionId,omitempty"`
	Mode            string                `json:"mode,omitempty"`
	Message         string                `json:"message"`
	URL             string                `json:"url,omitempty"`
	Fields          []acpElicitationField `json:"fields,omitempty"`
	RequestedSchema json.RawMessage       `json:"requestedSchema,omitempty"`
}

// acpStderrBuffer retains the trailing bytes written to a child process stderr.
type acpStderrBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newACPStderrBuffer(limit int) *acpStderrBuffer {
	if limit <= 0 {
		limit = acpDefaultStderrBytes
	}
	return &acpStderrBuffer{max: limit}
}

func (b *acpStderrBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.max {
		b.buf = append(b.buf[:0], p[len(p)-b.max:]...)
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		drop := len(b.buf) - b.max
		copy(b.buf, b.buf[drop:])
		b.buf = b.buf[:b.max]
	}
	return len(p), nil
}

func (b *acpStderrBuffer) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

type acpRPCResponse struct {
	result json.RawMessage
	err    error
}

type acpPendingServerReq struct {
	rawID     json.RawMessage
	sessionID string
	method    string
}

type acpDispatchEvent struct {
	update      *acpSessionUpdate
	permission  *acpPermissionRequest
	elicitation *acpElicitationRequest
	unknownReq  *acpUnknownServerReq
}

type acpUnknownServerReq struct {
	rawID  json.RawMessage
	method string
}

// acpClient manages a JSON-RPC 2.0 ACP connection over stdio or in-process streams.
type acpClient struct {
	cfg    acpClientConfig
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
	initResult *acpInitResult
	closed     bool
	closeErr   error

	eventMu     sync.Mutex
	eventQueue  []acpDispatchEvent
	eventClosed bool
	eventSig    chan struct{}

	stdoutDone   chan struct{}
	readerDone   chan struct{}
	dispatchDone chan struct{}
	closeOnce    sync.Once
}

// buildACPArgs constructs the CLI argument slice for `agy acp`.
func buildACPArgs(cfg acpClientConfig) ([]string, error) {
	appDataDir := strings.TrimSpace(cfg.AppDataDir)
	if appDataDir == "" {
		appDataDir = acpDefaultAppDataDir
	}
	if strings.HasPrefix(appDataDir, "-") {
		return nil, fmt.Errorf("invalid app-data-dir %q", appDataDir)
	}
	args := []string{"acp", "--app-data-dir", appDataDir}
	if geminiDir := strings.TrimSpace(cfg.GeminiDir); geminiDir != "" {
		if strings.HasPrefix(geminiDir, "-") {
			return nil, fmt.Errorf("invalid gemini-dir %q", geminiDir)
		}
		args = append(args, "--gemini-dir", geminiDir)
	}
	if cfg.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if cfg.DisableTelemetry {
		args = append(args, "--disable-telemetry")
	}
	return args, nil
}

// newACPClient starts an `agy acp` subprocess and attaches reader/dispatcher loops.
// Call Initialize(ctx) before issuing session RPCs.
func newACPClient(cfg acpClientConfig) (*acpClient, error) {
	bin := strings.TrimSpace(cfg.Binary)
	if bin == "" {
		bin = agyBinary()
	}
	if bin == "" {
		return nil, errors.New("antigravity cli (agy) not found")
	}
	args, err := buildACPArgs(cfg)
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
		return nil, fmt.Errorf("acp stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		procCancel()
		return nil, fmt.Errorf("acp stdout pipe: %w", err)
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
		return nil, fmt.Errorf("start agy acp: %w", err)
	}

	return startACPClientLoops(stdout, stdin, cfg, cmd, stderr, procCancel, stdoutDone), nil
}

// dialACPClient starts an `agy acp` subprocess and completes the ACP initialize handshake.
func dialACPClient(ctx context.Context, cfg acpClientConfig) (*acpClient, *acpInitResult, error) {
	c, err := newACPClient(cfg)
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

// newACPClientFromStreams constructs an acpClient over arbitrary reader/writer streams
// (for in-process pipe tests and custom transports).
func newACPClientFromStreams(r io.Reader, w io.WriteCloser, cfg acpClientConfig) *acpClient {
	return startACPClientLoops(r, w, cfg, nil, newACPStderrBuffer(cfg.StderrBytes), nil, make(chan struct{}))
}

func startACPClientLoops(
	r io.Reader,
	w io.WriteCloser,
	cfg acpClientConfig,
	cmd *exec.Cmd,
	stderr *acpStderrBuffer,
	procCancel context.CancelFunc,
	stdoutDone chan struct{},
) *acpClient {
	ctx, cancel := context.WithCancel(context.Background())
	c := &acpClient{
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

// Initialize performs the ACP v1 initialize handshake.
func (c *acpClient) Initialize(ctx context.Context) (*acpInitResult, error) {
	params := map[string]any{
		"protocolVersion": acpProtocolVersion,
		"clientInfo": map[string]any{
			"name":    acpClientName,
			"version": acpClientVersion,
		},
		"clientCapabilities": map[string]any{
			"elicitation": map[string]any{
				"form": map[string]any{},
			},
		},
	}
	raw, err := c.call(ctx, "initialize", params, true)
	if err != nil {
		return nil, err
	}
	res, err := parseACPInitResult(raw)
	if err != nil {
		return nil, fmt.Errorf("parse initialize result: %w", err)
	}
	c.mu.Lock()
	c.initResult = res
	c.mu.Unlock()
	return res, nil
}

// InitResult returns a copy of the cached initialize result, or nil if not yet initialized.
func (c *acpClient) InitResult() *acpInitResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initResult == nil {
		return nil
	}
	cp := *c.initResult
	if c.initResult.Tier != nil {
		t := *c.initResult.Tier
		cp.Tier = &t
	}
	if len(c.initResult.AuthMethods) > 0 {
		cp.AuthMethods = append([]acpAuthMethod(nil), c.initResult.AuthMethods...)
	}
	return &cp
}

// ListSessions calls `session/list` (following pagination cursors if returned).
func (c *acpClient) ListSessions(ctx context.Context) ([]acpSessionSummary, error) {
	var all []acpSessionSummary
	cursor := ""
	seenCursor := map[string]bool{}
	for page := 0; page < acpMaxListPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "session/list", params, true)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Sessions   []acpSessionSummary `json:"sessions"`
			NextCursor string              `json:"nextCursor"`
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
		all = []acpSessionSummary{}
	}
	return all, nil
}

// NewSession calls `session/new` for cwd and optional additionalDirectories.
func (c *acpClient) NewSession(ctx context.Context, cwd string, addDirs []string) (*acpSessionState, error) {
	params := map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{},
	}
	if len(addDirs) > 0 {
		params["additionalDirectories"] = addDirs
	}
	raw, err := c.call(ctx, "session/new", params, true)
	if err != nil {
		return nil, err
	}
	return parseACPSessionState(raw, "")
}

// LoadSession calls `session/load` for an existing sessionID and cwd.
func (c *acpClient) LoadSession(ctx context.Context, sessionID, cwd string) (*acpSessionState, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("sessionId is required")
	}
	params := map[string]any{
		"sessionId":  sessionID,
		"cwd":        cwd,
		"mcpServers": []any{},
	}
	raw, err := c.call(ctx, "session/load", params, true)
	if err != nil {
		return nil, err
	}
	return parseACPSessionState(raw, sessionID)
}

// ResumeSession calls `session/resume` for an existing sessionID and cwd.
func (c *acpClient) ResumeSession(ctx context.Context, sessionID, cwd string) (*acpSessionState, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("sessionId is required")
	}
	params := map[string]any{
		"sessionId":  sessionID,
		"cwd":        cwd,
		"mcpServers": []any{},
	}
	raw, err := c.call(ctx, "session/resume", params, true)
	if err != nil {
		return nil, err
	}
	return parseACPSessionState(raw, sessionID)
}

// CloseSession calls `session/close` and cancels any pending interactive requests for sessionID.
func (c *acpClient) CloseSession(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionId is required")
	}
	c.CancelAllPending(sessionID)
	_, err := c.call(ctx, "session/close", map[string]any{"sessionId": sessionID}, true)
	return err
}

// Prompt calls `session/prompt` with text and/or base64 image content blocks.
// Multiple Prompt calls (e.g. steering) and Cancel calls may run concurrently.
func (c *acpClient) Prompt(ctx context.Context, sessionID string, blocks []acpContentBlock) (*acpPromptResult, error) {
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

// Cancel cancels any pending interactive requests for sessionID and sends a
// `session/cancel` JSON-RPC notification so the active turn aborts cleanly.
func (c *acpClient) Cancel(ctx context.Context, sessionID string) error {
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

// SetConfigOption calls `session/set_config_option` for configID (e.g. "model", "thought_level", "mode").
func (c *acpClient) SetConfigOption(ctx context.Context, sessionID, configID, value string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionId is required")
	}
	if strings.TrimSpace(configID) == "" {
		return errors.New("configId is required")
	}
	_, err := c.call(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  configID,
		"value":     value,
	}, true)
	return err
}

// SetMode calls `session/set_mode` to switch execution/permission mode.
func (c *acpClient) SetMode(ctx context.Context, sessionID, modeID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sessionId is required")
	}
	if strings.TrimSpace(modeID) == "" {
		return errors.New("modeId is required")
	}
	_, err := c.call(ctx, "session/set_mode", map[string]any{
		"sessionId": sessionID,
		"modeId":    modeID,
	}, true)
	return err
}

// RespondPermission sends the JSON-RPC response to a pending `session/request_permission` request.
func (c *acpClient) RespondPermission(reqID, optionID string, cancelled bool) error {
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

// RespondElicitation sends the JSON-RPC response to a pending `elicitation/create` request.
// Valid actions are "accept", "decline", and "cancel" (empty defaults to "cancel").
func (c *acpClient) RespondElicitation(reqID, action string, content map[string]any) error {
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		action = "cancel"
	}
	switch action {
	case "accept", "decline", "cancel":
	default:
		return fmt.Errorf("invalid elicitation action %q", action)
	}
	req, ok := c.popServerRequest(reqID, "elicitation/create")
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

// CancelAllPending cancels all pending permission and elicitation requests for sessionID
// (or for all sessions when sessionID is empty). Safe to call multiple times or concurrently.
func (c *acpClient) CancelAllPending(sessionID string) {
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
		case "elicitation/create":
			_ = c.writeResponse(req.rawID, map[string]any{
				"action": "cancel",
			})
		}
	}
}

// PendingRequestCount returns the number of pending interactive server requests
// for sessionID (or all sessions when sessionID is empty).
func (c *acpClient) PendingRequestCount(sessionID string) int {
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

// Stderr returns the bounded diagnostic stderr output captured from the subprocess.
func (c *acpClient) Stderr() string {
	return c.stderr.String()
}

// Closed reports whether the client transport has closed.
func (c *acpClient) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Done returns a channel closed once the reader, subprocess, and event dispatcher have exited.
func (c *acpClient) Done() <-chan struct{} {
	return c.dispatchDone
}

// Err returns the close or process exit error after shutdown.
func (c *acpClient) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// Close cancels pending requests, closes stdin, terminates the subprocess cleanly
// if needed, and waits for all background goroutines to finish.
func (c *acpClient) Close() error {
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

func (c *acpClient) closeStdin() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.stdinClosed && c.stdin != nil {
		c.stdinClosed = true
		_ = c.stdin.Close()
	}
}

func (c *acpClient) call(ctx context.Context, method string, params any, applyTimeout bool) (json.RawMessage, error) {
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

func (c *acpClient) notify(method string, params any) error {
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

func (c *acpClient) writeResponse(rawID json.RawMessage, result any) error {
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

func (c *acpClient) writeRPCError(rawID json.RawMessage, code int, message string) error {
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

func (c *acpClient) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdinClosed || c.stdin == nil {
		return errors.New("acp transport closed")
	}
	_, err = c.stdin.Write(b)
	return err
}

func (c *acpClient) popServerRequest(reqID, expectedMethod string) (acpPendingServerReq, bool) {
	reqID = strings.TrimSpace(reqID)
	if reqID == "" {
		return acpPendingServerReq{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	req, ok := c.serverReqs[reqID]
	if !ok || (expectedMethod != "" && req.method != expectedMethod) {
		return acpPendingServerReq{}, false
	}
	delete(c.serverReqs, reqID)
	return req, true
}

func (c *acpClient) closedErrorLocked() error {
	if c.closeErr != nil {
		return c.closeErr
	}
	if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
		return fmt.Errorf("acp client closed: %s", clipPlain(msg, 400))
	}
	return errors.New("acp client closed")
}

func (c *acpClient) wrapTransportError(err error) error {
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
		return fmt.Errorf("%w (stderr: %s)", err, clipPlain(msg, 400))
	}
	return err
}

func (c *acpClient) readLoop(r io.Reader) {
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

func (c *acpClient) handleLine(line []byte) {
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
		if msg.Method == "session/update" {
			if upd, ok := parseACPSessionUpdate(msg.Params); ok {
				c.enqueueEvent(acpDispatchEvent{update: &upd})
			}
		}
		return
	}

	// Case 3: JSON-RPC interactive server request (must correlate ID and reply).
	if hasID && msg.Method != "" {
		rawID := append(json.RawMessage(nil), bytes.TrimSpace(msg.ID)...)
		reqKey := acpRawIDKey(rawID)
		switch msg.Method {
		case "session/request_permission":
			req := parseACPPermissionRequest(reqKey, msg.Params)
			c.mu.Lock()
			c.serverReqs[reqKey] = acpPendingServerReq{
				rawID:     rawID,
				sessionID: req.SessionID,
				method:    msg.Method,
			}
			c.mu.Unlock()
			c.enqueueEvent(acpDispatchEvent{permission: &req})
		case "elicitation/create":
			req := parseACPElicitationRequest(reqKey, msg.Params)
			c.mu.Lock()
			c.serverReqs[reqKey] = acpPendingServerReq{
				rawID:     rawID,
				sessionID: req.SessionID,
				method:    msg.Method,
			}
			c.mu.Unlock()
			c.enqueueEvent(acpDispatchEvent{elicitation: &req})
		default:
			c.enqueueEvent(acpDispatchEvent{
				unknownReq: &acpUnknownServerReq{rawID: rawID, method: msg.Method},
			})
		}
	}
}

func (c *acpClient) enqueueEvent(ev acpDispatchEvent) {
	c.eventMu.Lock()
	if c.eventClosed {
		c.eventMu.Unlock()
		return
	}
	c.eventQueue = append(c.eventQueue, ev)
	c.eventMu.Unlock()
	select {
	case c.eventSig <- struct{}{}:
	default:
	}
}

func (c *acpClient) closeEventQueue() {
	c.eventMu.Lock()
	c.eventClosed = true
	c.eventMu.Unlock()
	select {
	case c.eventSig <- struct{}{}:
	default:
	}
}

func (c *acpClient) dispatchLoop() {
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
			case ev.update != nil:
				if c.cfg.OnUpdate != nil {
					c.cfg.OnUpdate(*ev.update)
				}
			case ev.permission != nil:
				if c.cfg.OnPermission != nil {
					c.cfg.OnPermission(*ev.permission)
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

func readACPLine(br *bufio.Reader, maxBytes int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			if len(buf)+len(chunk) > maxBytes {
				return nil, fmt.Errorf("acp line exceeds %d bytes", maxBytes)
			}
			buf = append(buf, chunk...)
		}
		if err == nil {
			return bytes.TrimRight(buf, "\r\n"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(buf) > 0 {
				return bytes.TrimRight(buf, "\r\n"), io.EOF
			}
			return nil, io.EOF
		}
		return nil, err
	}
}

func acpRawIDKey(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return s
	}
	var n int64
	if json.Unmarshal(trimmed, &n) == nil {
		return strconv.FormatInt(n, 10)
	}
	return string(trimmed)
}

func acpCapPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("false")) {
		return false
	}
	return true
}

func parseACPTierMeta(metaRaw json.RawMessage) (supportsTier bool, supportsSystemAppend bool, tier *acpTierInfo) {
	if len(bytes.TrimSpace(metaRaw)) == 0 {
		return false, false, nil
	}
	var meta struct {
		Antigravity struct {
			Tier         json.RawMessage `json:"tier"`
			SystemPrompt struct {
				Append bool `json:"append"`
			} `json:"systemPrompt"`
		} `json:"antigravity"`
	}
	if json.Unmarshal(metaRaw, &meta) != nil {
		return false, false, nil
	}
	supportsSystemAppend = meta.Antigravity.SystemPrompt.Append
	tierRaw := bytes.TrimSpace(meta.Antigravity.Tier)
	if len(tierRaw) == 0 || bytes.Equal(tierRaw, []byte("null")) {
		return false, supportsSystemAppend, nil
	}
	var flag bool
	if json.Unmarshal(tierRaw, &flag) == nil {
		return flag, supportsSystemAppend, nil
	}
	var info acpTierInfo
	if json.Unmarshal(tierRaw, &info) == nil && (info.ID != "" || info.Name != "" || info.UpgradeURI != "") {
		return true, supportsSystemAppend, &info
	}
	return false, supportsSystemAppend, nil
}

func parseACPInitResult(raw json.RawMessage) (*acpInitResult, error) {
	var wire struct {
		ProtocolVersion int             `json:"protocolVersion"`
		AgentInfo       acpAgentInfo    `json:"agentInfo"`
		AuthMethods     []acpAuthMethod `json:"authMethods"`
		Meta            json.RawMessage `json:"_meta"`
		Caps            struct {
			LoadSession        bool                  `json:"loadSession"`
			LoadSessionSnake   bool                  `json:"load_session"`
			PromptCapabilities acpPromptCapabilities `json:"promptCapabilities"`
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
	res := &acpInitResult{
		ProtocolVersion: wire.ProtocolVersion,
		AgentInfo:       wire.AgentInfo,
		AuthMethods:     wire.AuthMethods,
		AgentCapabilities: acpAgentCapabilities{
			LoadSession:        wire.Caps.LoadSession || wire.Caps.LoadSessionSnake,
			PromptCapabilities: wire.Caps.PromptCapabilities,
			SessionCapabilities: acpSessionCapabilities{
				Resume:                acpCapPresent(wire.Caps.SessionCaps.Resume) || acpCapPresent(wire.Caps.SessionCapsSnake.Resume),
				Close:                 acpCapPresent(wire.Caps.SessionCaps.Close) || acpCapPresent(wire.Caps.SessionCapsSnake.Close),
				Load:                  acpCapPresent(wire.Caps.SessionCaps.Load) || acpCapPresent(wire.Caps.SessionCapsSnake.Load),
				List:                  acpCapPresent(wire.Caps.SessionCaps.List) || acpCapPresent(wire.Caps.SessionCapsSnake.List),
				AdditionalDirectories: acpCapPresent(wire.Caps.SessionCaps.AdditionalDirectories) || acpCapPresent(wire.Caps.SessionCapsSnake.AdditionalDirectories),
			},
		},
	}
	if sTier, sAppend, tier := parseACPTierMeta(wire.Caps.Meta); sTier || sAppend || tier != nil {
		res.AgentCapabilities.SupportsTier = sTier
		res.AgentCapabilities.SupportsSystemAppend = sAppend
		if tier != nil {
			res.Tier = tier
		}
	}
	if sTier, sAppend, tier := parseACPTierMeta(wire.Meta); sTier || sAppend || tier != nil {
		if sTier {
			res.AgentCapabilities.SupportsTier = true
		}
		if sAppend {
			res.AgentCapabilities.SupportsSystemAppend = true
		}
		if tier != nil {
			res.Tier = tier
		}
	}
	return res, nil
}

func parseACPConfigOptions(raw json.RawMessage) []acpConfigOption {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var wire []struct {
		ID           string            `json:"id"`
		Name         string            `json:"name"`
		Category     string            `json:"category"`
		Type         string            `json:"type"`
		CurrentValue string            `json:"currentValue"`
		Options      []json.RawMessage `json:"options"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return nil
	}
	out := make([]acpConfigOption, 0, len(wire))
	for _, w := range wire {
		opt := acpConfigOption{
			ID:           w.ID,
			Name:         w.Name,
			Category:     w.Category,
			Type:         w.Type,
			CurrentValue: w.CurrentValue,
		}
		for _, itemRaw := range w.Options {
			var grouped struct {
				Options []acpConfigOptionChoice `json:"options"`
			}
			if json.Unmarshal(itemRaw, &grouped) == nil && len(grouped.Options) > 0 {
				opt.Options = append(opt.Options, grouped.Options...)
				continue
			}
			var choice acpConfigOptionChoice
			if json.Unmarshal(itemRaw, &choice) == nil && (choice.Value != "" || choice.Name != "") {
				opt.Options = append(opt.Options, choice)
			}
		}
		out = append(out, opt)
	}
	return out
}

func parseACPSessionState(raw json.RawMessage, fallbackSessionID string) (*acpSessionState, error) {
	var wire struct {
		SessionID     string          `json:"sessionId"`
		Models        acpModelsState  `json:"models"`
		Modes         acpModesState   `json:"modes"`
		ConfigOptions json.RawMessage `json:"configOptions"`
		Meta          json.RawMessage `json:"_meta"`
	}
	if len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &wire); err != nil {
			return nil, fmt.Errorf("parse session state: %w", err)
		}
	}
	sid := wire.SessionID
	if sid == "" {
		sid = fallbackSessionID
	}
	_, _, tier := parseACPTierMeta(wire.Meta)
	return &acpSessionState{
		SessionID:     sid,
		Models:        wire.Models,
		Modes:         wire.Modes,
		ConfigOptions: parseACPConfigOptions(wire.ConfigOptions),
		Tier:          tier,
	}, nil
}

func parseACPToolContents(raw json.RawMessage) []acpToolContent {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]acpToolContent, 0, len(items))
	for _, itemRaw := range items {
		var w struct {
			Type       string           `json:"type"`
			Text       string           `json:"text"`
			Content    *acpContentBlock `json:"content"`
			Path       string           `json:"path"`
			OldText    *string          `json:"oldText"`
			NewText    string           `json:"newText"`
			TerminalID string           `json:"terminalId"`
		}
		if json.Unmarshal(itemRaw, &w) != nil {
			continue
		}
		tc := acpToolContent{
			Type:       w.Type,
			Text:       w.Text,
			Content:    w.Content,
			TerminalID: w.TerminalID,
		}
		switch w.Type {
		case "diff":
			diff := &acpToolDiff{
				Path:    w.Path,
				NewText: w.NewText,
			}
			if w.OldText != nil {
				diff.OldText = *w.OldText
				diff.HasOld = true
			}
			tc.Diff = diff
		case "content":
			if w.Content != nil && w.Content.Text != "" && tc.Text == "" {
				tc.Text = w.Content.Text
			}
		case "text":
			if tc.Content == nil {
				tc.Content = &acpContentBlock{Type: "text", Text: w.Text}
			}
		}
		out = append(out, tc)
	}
	return out
}

func parseACPToolCallInfo(raw json.RawMessage) acpToolCallInfo {
	var w struct {
		ToolCallID string            `json:"toolCallId"`
		Name       string            `json:"name"`
		Args       map[string]any    `json:"args"`
		Kind       string            `json:"kind"`
		Title      string            `json:"title"`
		Status     string            `json:"status"`
		Locations  []acpToolLocation `json:"locations"`
		RawInput   json.RawMessage   `json:"rawInput"`
		RawOutput  json.RawMessage   `json:"rawOutput"`
		Content    json.RawMessage   `json:"content"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return acpToolCallInfo{}
	}
	return acpToolCallInfo{
		ToolCallID: w.ToolCallID,
		Name:       w.Name,
		Args:       w.Args,
		Kind:       w.Kind,
		Title:      w.Title,
		Status:     w.Status,
		Locations:  w.Locations,
		RawInput:   w.RawInput,
		RawOutput:  w.RawOutput,
		Content:    parseACPToolContents(w.Content),
	}
}

func parseACPSessionUpdate(paramsRaw json.RawMessage) (acpSessionUpdate, bool) {
	var envelope struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(paramsRaw, &envelope) != nil || len(bytes.TrimSpace(envelope.Update)) == 0 {
		return acpSessionUpdate{}, false
	}
	var common struct {
		SessionUpdate     string          `json:"sessionUpdate"`
		Content           json.RawMessage `json:"content"`
		Text              string          `json:"text"`
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
		AvailableCommands []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Input       json.RawMessage `json:"input"`
		} `json:"availableCommands"`
	}
	if json.Unmarshal(envelope.Update, &common) != nil || common.SessionUpdate == "" {
		return acpSessionUpdate{}, false
	}

	u := acpSessionUpdate{
		SessionID: envelope.SessionID,
		Kind:      common.SessionUpdate,
		Raw:       append(json.RawMessage(nil), envelope.Update...),
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
		u.ToolCall = &tc
	case "usage_update":
		u.Usage = &acpUsageUpdate{
			Used: common.Used,
			Size: common.Size,
			Cost: common.Cost,
		}
	case "available_commands_update":
		cmds := make([]acpAvailableCommand, 0, len(common.AvailableCommands))
		for _, item := range common.AvailableCommands {
			cmd := acpAvailableCommand{
				Name:        item.Name,
				Description: item.Description,
			}
			if len(bytes.TrimSpace(item.Input)) > 0 {
				var inputObj struct {
					Hint string `json:"hint"`
				}
				if json.Unmarshal(item.Input, &inputObj) == nil && inputObj.Hint != "" {
					cmd.InputHint = inputObj.Hint
				} else {
					var inputStr string
					if json.Unmarshal(item.Input, &inputStr) == nil {
						cmd.InputHint = inputStr
					}
				}
			}
			cmds = append(cmds, cmd)
		}
		u.AvailableCommands = cmds
	case "session_info_update":
		u.Title = common.Title
		u.UpdatedAt = common.UpdatedAt
	case "current_mode_update":
		u.CurrentModeID = common.CurrentModeID
		if u.CurrentModeID == "" {
			u.CurrentModeID = common.ModeID
		}
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

func parseACPPermissionRequest(reqID string, paramsRaw json.RawMessage) acpPermissionRequest {
	var w struct {
		SessionID string                `json:"sessionId"`
		ToolCall  json.RawMessage       `json:"toolCall"`
		Options   []acpPermissionOption `json:"options"`
	}
	_ = json.Unmarshal(paramsRaw, &w)
	return acpPermissionRequest{
		RequestID: reqID,
		SessionID: w.SessionID,
		ToolCall:  parseACPToolCallInfo(w.ToolCall),
		Options:   w.Options,
	}
}

func parseACPElicitationRequest(reqID string, paramsRaw json.RawMessage) acpElicitationRequest {
	var w struct {
		SessionID       string          `json:"sessionId"`
		Mode            string          `json:"mode"`
		Message         string          `json:"message"`
		URL             string          `json:"url"`
		RequestedSchema json.RawMessage `json:"requestedSchema"`
		Meta            struct {
			SessionID   string `json:"sessionId"`
			Antigravity struct {
				SessionID string `json:"sessionId"`
			} `json:"antigravity"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(paramsRaw, &w)
	sid := w.SessionID
	if sid == "" {
		sid = w.Meta.SessionID
	}
	if sid == "" {
		sid = w.Meta.Antigravity.SessionID
	}
	mode := w.Mode
	if mode == "" {
		if w.URL != "" {
			mode = "url"
		} else {
			mode = "form"
		}
	}
	return acpElicitationRequest{
		RequestID:       reqID,
		SessionID:       sid,
		Mode:            mode,
		Message:         w.Message,
		URL:             w.URL,
		Fields:          parseACPElicitationSchema(w.RequestedSchema),
		RequestedSchema: append(json.RawMessage(nil), w.RequestedSchema...),
	}
}

func parseACPElicitationSchema(schemaRaw json.RawMessage) []acpElicitationField {
	if len(bytes.TrimSpace(schemaRaw)) == 0 || bytes.Equal(bytes.TrimSpace(schemaRaw), []byte("null")) {
		return nil
	}
	var schema struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if json.Unmarshal(schemaRaw, &schema) != nil || len(bytes.TrimSpace(schema.Properties)) == 0 {
		return nil
	}
	reqSet := map[string]bool{}
	for _, r := range schema.Required {
		reqSet[r] = true
	}
	orderedProps := orderedJSONObjectEntries(schema.Properties)
	fields := make([]acpElicitationField, 0, len(orderedProps))
	for _, entry := range orderedProps {
		field := parseACPElicitationProperty(entry.key, entry.value, reqSet[entry.key])
		fields = append(fields, field)
	}
	return fields
}

type jsonObjectEntry struct {
	key   string
	value json.RawMessage
}

// orderedJSONObjectEntries decodes a JSON object while preserving property order.
func orderedJSONObjectEntries(raw json.RawMessage) []jsonObjectEntry {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil
	}
	var entries []jsonObjectEntry
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}
		key, ok := keyTok.(string)
		if !ok {
			break
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			break
		}
		entries = append(entries, jsonObjectEntry{key: key, value: val})
	}
	return entries
}

func parseACPElicitationProperty(name string, raw json.RawMessage, required bool) acpElicitationField {
	var prop struct {
		Type        string            `json:"type"`
		Title       string            `json:"title"`
		Description string            `json:"description"`
		Format      string            `json:"format"`
		Default     any               `json:"default"`
		WriteOnly   bool              `json:"writeOnly"`
		Secret      bool              `json:"secret"`
		XSecret     bool              `json:"x-secret"`
		Sensitive   bool              `json:"sensitive"`
		Enum        []any             `json:"enum"`
		EnumNames   []string          `json:"enumNames"`
		OneOf       []json.RawMessage `json:"oneOf"`
		AnyOf       []json.RawMessage `json:"anyOf"`
		Items       json.RawMessage   `json:"items"`
	}
	_ = json.Unmarshal(raw, &prop)

	field := acpElicitationField{
		Name:        name,
		Type:        prop.Type,
		Title:       prop.Title,
		Description: prop.Description,
		Required:    required,
		MultiSelect: prop.Type == "array",
		Format:      prop.Format,
		Default:     prop.Default,
	}

	// Secret/password hints from schema format, flags, or password field naming.
	lowerFormat := strings.ToLower(strings.TrimSpace(prop.Format))
	lowerName := strings.ToLower(strings.TrimSpace(name))
	if prop.WriteOnly || prop.Secret || prop.XSecret || prop.Sensitive ||
		lowerFormat == "password" || lowerFormat == "secret" ||
		strings.Contains(lowerName, "password") || strings.Contains(lowerName, "passphrase") {
		field.Secret = true
	}

	field.Options = extractSchemaOptions(prop.Enum, prop.EnumNames, prop.OneOf, prop.AnyOf)

	if prop.Type == "array" && len(bytes.TrimSpace(prop.Items)) > 0 {
		var items struct {
			Enum      []any             `json:"enum"`
			EnumNames []string          `json:"enumNames"`
			OneOf     []json.RawMessage `json:"oneOf"`
			AnyOf     []json.RawMessage `json:"anyOf"`
		}
		if json.Unmarshal(prop.Items, &items) == nil {
			itemOpts := extractSchemaOptions(items.Enum, items.EnumNames, items.OneOf, items.AnyOf)
			if len(itemOpts) > 0 {
				field.Options = itemOpts
			}
		}
	}

	return field
}

func extractSchemaOptions(enumVals []any, enumNames []string, oneOf, anyOf []json.RawMessage) []acpElicitationOption {
	if len(enumVals) > 0 {
		opts := make([]acpElicitationOption, 0, len(enumVals))
		for i, v := range enumVals {
			val := schemaScalarString(v)
			label := val
			if i < len(enumNames) && enumNames[i] != "" {
				label = enumNames[i]
			}
			opts = append(opts, acpElicitationOption{Value: val, Label: label})
		}
		return opts
	}

	variants := oneOf
	if len(variants) == 0 {
		variants = anyOf
	}
	if len(variants) == 0 {
		return nil
	}
	var opts []acpElicitationOption
	for _, vRaw := range variants {
		var v struct {
			Const       any    `json:"const"`
			Enum        []any  `json:"enum"`
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		if json.Unmarshal(vRaw, &v) != nil {
			continue
		}
		var val string
		switch {
		case v.Const != nil:
			val = schemaScalarString(v.Const)
		case len(v.Enum) == 1:
			val = schemaScalarString(v.Enum[0])
		default:
			continue
		}
		label := v.Title
		if label == "" {
			label = val
		}
		opts = append(opts, acpElicitationOption{
			Value:       val,
			Label:       label,
			Description: v.Description,
		})
	}
	return opts
}

func schemaScalarString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprint(x)
	}
}

// formatACPDiff builds a clean unified diff from structured ACP diff text blocks
// without any external dependencies.
func formatACPDiff(oldText, newText, path string) string {
	if oldText == newText {
		return ""
	}
	displayPath := strings.TrimSpace(path)
	if displayPath == "" {
		displayPath = "file"
	}

	oldLines := splitDiffLines(oldText)
	newLines := splitDiffLines(newText)

	oldHeader := displayPath
	newHeader := displayPath
	if len(oldLines) == 0 {
		oldHeader = "/dev/null"
	}
	if len(newLines) == 0 {
		newHeader = "/dev/null"
	}

	ops := computeLineDiffOps(oldLines, newLines)
	if len(ops) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("--- ")
	b.WriteString(oldHeader)
	b.WriteByte('\n')
	b.WriteString("+++ ")
	b.WriteString(newHeader)
	b.WriteByte('\n')

	writeUnifiedHunks(&b, ops, acpDiffContextLines)
	return b.String()
}

type diffOpKind byte

const (
	diffOpEqual diffOpKind = iota
	diffOpDelete
	diffOpInsert
)

type diffOp struct {
	kind    diffOpKind
	text    string
	oldLine int // 1-based line number in old file (0 for inserts)
	newLine int // 1-based line number in new file (0 for deletes)
}

func splitDiffLines(text string) []string {
	if text == "" {
		return nil
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return []string{""}
	}
	return strings.Split(text, "\n")
}

func computeLineDiffOps(oldLines, newLines []string) []diffOp {
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	oldEnd := len(oldLines)
	newEnd := len(newLines)
	for oldEnd > prefix && newEnd > prefix && oldLines[oldEnd-1] == newLines[newEnd-1] {
		oldEnd--
		newEnd--
	}

	ops := make([]diffOp, 0, len(oldLines)+len(newLines))
	for i := 0; i < prefix; i++ {
		ops = append(ops, diffOp{kind: diffOpEqual, text: oldLines[i], oldLine: i + 1, newLine: i + 1})
	}

	oldMid := oldLines[prefix:oldEnd]
	newMid := newLines[prefix:newEnd]

	if len(oldMid) > 0 && len(newMid) > 0 && len(oldMid)*len(newMid) <= acpDiffMaxDPMatrix {
		ops = append(ops, lcsDiffOps(oldMid, newMid, prefix, prefix)...)
	} else {
		for i, line := range oldMid {
			ops = append(ops, diffOp{kind: diffOpDelete, text: line, oldLine: prefix + i + 1})
		}
		for j, line := range newMid {
			ops = append(ops, diffOp{kind: diffOpInsert, text: line, newLine: prefix + j + 1})
		}
	}

	for i := 0; i < len(oldLines)-oldEnd; i++ {
		ops = append(ops, diffOp{
			kind:    diffOpEqual,
			text:    oldLines[oldEnd+i],
			oldLine: oldEnd + i + 1,
			newLine: newEnd + i + 1,
		})
	}
	return ops
}

func lcsDiffOps(a, b []string, oldBase, newBase int) []diffOp {
	m, n := len(a), len(b)
	cols := n + 1
	dp := make([]int, (m+1)*cols)
	for i := m - 1; i >= 0; i-- {
		row := i * cols
		nextRow := (i + 1) * cols
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[row+j] = dp[nextRow+j+1] + 1
			} else if dp[nextRow+j] >= dp[row+j+1] {
				dp[row+j] = dp[nextRow+j]
			} else {
				dp[row+j] = dp[row+j+1]
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < m && j < n {
		if a[i] == b[j] {
			ops = append(ops, diffOp{kind: diffOpEqual, text: a[i], oldLine: oldBase + i + 1, newLine: newBase + j + 1})
			i++
			j++
		} else if dp[(i+1)*cols+j] >= dp[i*cols+j+1] {
			ops = append(ops, diffOp{kind: diffOpDelete, text: a[i], oldLine: oldBase + i + 1})
			i++
		} else {
			ops = append(ops, diffOp{kind: diffOpInsert, text: b[j], newLine: newBase + j + 1})
			j++
		}
	}
	for ; i < m; i++ {
		ops = append(ops, diffOp{kind: diffOpDelete, text: a[i], oldLine: oldBase + i + 1})
	}
	for ; j < n; j++ {
		ops = append(ops, diffOp{kind: diffOpInsert, text: b[j], newLine: newBase + j + 1})
	}
	return ops
}

type diffRange struct {
	start int
	end   int
}

func writeUnifiedHunks(b *strings.Builder, ops []diffOp, context int) {
	var ranges []diffRange
	for i, op := range ops {
		if op.kind == diffOpEqual {
			continue
		}
		start := i - context
		if start < 0 {
			start = 0
		}
		end := i + context + 1
		if end > len(ops) {
			end = len(ops)
		}
		if len(ranges) > 0 && start <= ranges[len(ranges)-1].end {
			if end > ranges[len(ranges)-1].end {
				ranges[len(ranges)-1].end = end
			}
		} else {
			ranges = append(ranges, diffRange{start: start, end: end})
		}
	}

	for _, r := range ranges {
		oldStart, oldCount := 0, 0
		newStart, newCount := 0, 0
		for _, op := range ops[r.start:r.end] {
			switch op.kind {
			case diffOpEqual:
				if oldCount == 0 {
					oldStart = op.oldLine
				}
				if newCount == 0 {
					newStart = op.newLine
				}
				oldCount++
				newCount++
			case diffOpDelete:
				if oldCount == 0 {
					oldStart = op.oldLine
				}
				oldCount++
			case diffOpInsert:
				if newCount == 0 {
					newStart = op.newLine
				}
				newCount++
			}
		}
		if oldCount == 0 {
			oldStart = precedingLineNumber(ops, r.start, true)
		}
		if newCount == 0 {
			newStart = precedingLineNumber(ops, r.start, false)
		}

		fmt.Fprintf(b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, op := range ops[r.start:r.end] {
			switch op.kind {
			case diffOpEqual:
				b.WriteByte(' ')
			case diffOpDelete:
				b.WriteByte('-')
			case diffOpInsert:
				b.WriteByte('+')
			}
			b.WriteString(op.text)
			b.WriteByte('\n')
		}
	}
}

func precedingLineNumber(ops []diffOp, idx int, oldSide bool) int {
	for i := idx - 1; i >= 0; i-- {
		if oldSide && ops[i].oldLine > 0 {
			return ops[i].oldLine
		}
		if !oldSide && ops[i].newLine > 0 {
			return ops[i].newLine
		}
	}
	return 0
}
