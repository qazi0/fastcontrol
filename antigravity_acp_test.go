package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockACPRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *acpRPCError    `json:"error"`
}

// mockACPServer drives an in-process JSON-RPC 2.0 ACP server over io.Pipe.
type mockACPServer struct {
	t *testing.T

	clientOut *io.PipeWriter // server writes -> client reads
	serverIn  *io.PipeReader // client writes -> server reads

	writeMu sync.Mutex
	mu      sync.Mutex

	handlers  map[string]func(msg mockACPRPCMessage)
	responses map[string]chan mockACPRPCMessage
	notifs    chan mockACPRPCMessage

	done chan struct{}
}

func newMockACPServer(t *testing.T, cfg acpClientConfig) (*mockACPServer, *acpClient) {
	t.Helper()
	srvRead, cliWrite := io.Pipe()
	cliRead, srvWrite := io.Pipe()

	s := &mockACPServer{
		t:         t,
		clientOut: srvWrite,
		serverIn:  srvRead,
		handlers:  map[string]func(mockACPRPCMessage){},
		responses: map[string]chan mockACPRPCMessage{},
		notifs:    make(chan mockACPRPCMessage, 64),
		done:      make(chan struct{}),
	}
	c := newACPClientFromStreams(cliRead, cliWrite, cfg)
	return s, c
}

func (s *mockACPServer) start() {
	go func() {
		defer close(s.done)
		br := bufio.NewReader(s.serverIn)
		for {
			line, err := readACPLine(br, 16<<20)
			if len(line) > 0 {
				var msg mockACPRPCMessage
				if json.Unmarshal(line, &msg) == nil {
					hasID := len(bytes.TrimSpace(msg.ID)) > 0 && !bytes.Equal(bytes.TrimSpace(msg.ID), []byte("null"))
					switch {
					case hasID && msg.Method != "":
						s.mu.Lock()
						h := s.handlers[msg.Method]
						s.mu.Unlock()
						if h != nil {
							go h(msg)
						} else {
							_ = s.sendError(msg.ID, -32601, "unhandled method: "+msg.Method)
						}
					case !hasID && msg.Method != "":
						select {
						case s.notifs <- msg:
						default:
						}
						s.mu.Lock()
						h := s.handlers[msg.Method]
						s.mu.Unlock()
						if h != nil {
							go h(msg)
						}
					case hasID && msg.Method == "":
						key := acpRawIDKey(msg.ID)
						s.mu.Lock()
						ch := s.responses[key]
						s.mu.Unlock()
						if ch != nil {
							select {
							case ch <- msg:
							default:
							}
						}
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

func (s *mockACPServer) on(method string, fn func(mockACPRPCMessage)) {
	s.mu.Lock()
	s.handlers[method] = fn
	s.mu.Unlock()
}

func (s *mockACPServer) sendJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.clientOut.Write(b)
	return err
}

func (s *mockACPServer) sendResult(id json.RawMessage, result any) error {
	return s.sendJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (s *mockACPServer) sendError(id json.RawMessage, code int, message string) error {
	return s.sendJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func (s *mockACPServer) sendNotify(method string, params any) error {
	return s.sendJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

// sendServerRequest sends a JSON-RPC request from server to client and returns
// a channel that receives the client's correlated response.
func (s *mockACPServer) sendServerRequest(id any, method string, params any) (chan mockACPRPCMessage, error) {
	rawID, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	key := acpRawIDKey(rawID)
	ch := make(chan mockACPRPCMessage, 1)
	s.mu.Lock()
	s.responses[key] = ch
	s.mu.Unlock()
	err = s.sendJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(rawID),
		"method":  method,
		"params":  params,
	})
	return ch, err
}

func (s *mockACPServer) close() {
	_ = s.clientOut.Close()
	_ = s.serverIn.Close()
	<-s.done
}

func TestBuildACPArgs(t *testing.T) {
	args, err := buildACPArgs(acpClientConfig{})
	if err != nil {
		t.Fatalf("default args error: %v", err)
	}
	if strings.Join(args, " ") != "acp --app-data-dir antigravity" {
		t.Fatalf("unexpected default args: %v", args)
	}

	args, err = buildACPArgs(acpClientConfig{
		AppDataDir:                 "antigravity-cli",
		GeminiDir:                  ".gemini-alt",
		DangerouslySkipPermissions: true,
		DisableTelemetry:           true,
	})
	if err != nil {
		t.Fatalf("custom args error: %v", err)
	}
	want := "acp --app-data-dir antigravity-cli --gemini-dir .gemini-alt --dangerously-skip-permissions --disable-telemetry"
	if strings.Join(args, " ") != want {
		t.Fatalf("got %q, want %q", strings.Join(args, " "), want)
	}

	if _, err := buildACPArgs(acpClientConfig{AppDataDir: "--injected"}); err == nil {
		t.Fatalf("expected error for flag-like app-data-dir")
	}
	if _, err := buildACPArgs(acpClientConfig{GeminiDir: "-bad"}); err == nil {
		t.Fatalf("expected error for flag-like gemini-dir")
	}
}

func TestAgyBinaryCandidates(t *testing.T) {
	mac := agyBinaryCandidates("/Users/alice", "", "darwin")
	if len(mac) < 3 || !strings.HasSuffix(mac[0], filepath.Join(".local", "bin", "agy")) {
		t.Fatalf("unexpected darwin candidates: %v", mac)
	}
	win := agyBinaryCandidates(`C:\Users\alice`, `C:\Users\alice\AppData\Local`, "windows")
	if len(win) < 2 || !strings.HasSuffix(win[0], "agy.exe") {
		t.Fatalf("unexpected windows candidates: %v", win)
	}
}

func TestAgySupportsACPCachingAndInvalidation(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-agy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho ok\n"), 0755); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	var support atomic.Bool
	support.Store(true)

	origProbe := agyProbeHelpFunc
	t.Cleanup(func() {
		agyACPCacheMu.Lock()
		agyProbeHelpFunc = origProbe
		delete(agyACPCache, bin)
		agyACPCacheMu.Unlock()
	})
	agyACPCacheMu.Lock()
	agyProbeHelpFunc = func(path string) bool {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return support.Load()
	}
	delete(agyACPCache, bin)
	agyACPCacheMu.Unlock()

	// Concurrent callers should deduplicate into a single probe.
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !agySupportsACP(bin) {
				t.Errorf("expected agySupportsACP=true")
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 probe call with caching/dedup, got %d", got)
	}

	// Subsequent call hits cache without spawning.
	if !agySupportsACP(bin) || calls.Load() != 1 {
		t.Fatalf("expected cached true result, calls=%d", calls.Load())
	}

	// Updating binary content/size invalidates cache cleanly.
	support.Store(false)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho updated-longer-binary\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if agySupportsACP(bin) {
		t.Fatalf("expected invalidated cache to re-probe and return false")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 total probe calls after binary update, got %d", got)
	}
	if agySupportsACP("") {
		t.Fatalf("expected empty binary path to return false")
	}
}

func TestACPInitializeAndSessionLifecycle(t *testing.T) {
	srv, cli := newMockACPServer(t, acpClientConfig{CallTimeout: 5 * time.Second})
	defer func() {
		_ = cli.Close()
		srv.close()
	}()

	var gotInitParams map[string]any
	var gotNewParams map[string]any
	var gotLoadParams map[string]any
	var gotResumeParams map[string]any
	var gotCloseParams map[string]any
	var gotSetCfgParams map[string]any
	var gotSetModeParams map[string]any
	var listCalls atomic.Int32

	srv.on("initialize", func(msg mockACPRPCMessage) {
		_ = json.Unmarshal(msg.Params, &gotInitParams)
		_ = srv.sendResult(msg.ID, map[string]any{
			"protocolVersion": 1,
			"agentInfo":       map[string]any{"name": "Antigravity ACP Server", "version": "1.5.18"},
			"agentCapabilities": map[string]any{
				"loadSession": true,
				"promptCapabilities": map[string]any{
					"image":           true,
					"audio":           false,
					"embeddedContext": true,
				},
				"sessionCapabilities": map[string]any{
					"resume":                map[string]any{},
					"close":                 map[string]any{},
					"load":                  map[string]any{},
					"list":                  map[string]any{},
					"additionalDirectories": map[string]any{},
				},
				"_meta": map[string]any{
					"antigravity": map[string]any{
						"tier":         true,
						"systemPrompt": map[string]any{"append": true},
					},
				},
			},
		})
	})

	srv.on("session/list", func(msg mockACPRPCMessage) {
		n := listCalls.Add(1)
		if n == 1 {
			_ = srv.sendResult(msg.ID, map[string]any{
				"sessions": []map[string]any{
					{"sessionId": "s-1", "cwd": "/repo/a", "title": "First", "updatedAt": "2026-11-27T10:00:00Z"},
				},
				"nextCursor": "page-2",
			})
			return
		}
		_ = srv.sendResult(msg.ID, map[string]any{
			"sessions": []map[string]any{
				{"sessionId": "s-2", "cwd": "/repo/b", "title": "Second", "updatedAt": "2026-11-27T11:00:00Z"},
			},
		})
	})

	sessionStatePayload := func(sid string) map[string]any {
		return map[string]any{
			"sessionId": sid,
			"models": map[string]any{
				"currentModelId": "gemini-4-neon-high",
				"availableModels": []map[string]any{
					{"modelId": "gemini-4-neon-high", "name": "Gemini 4 Neon (High)"},
					{"modelId": "claude-sonnet-5-5", "name": "Claude Sonnet 5.5"},
				},
			},
			"modes": map[string]any{
				"currentModeId": "default",
				"availableModes": []map[string]any{
					{"id": "default", "name": "Default", "description": "Ask before risky actions"},
					{"id": "accept-edits", "name": "Accept Edits", "description": "Auto-approve edits"},
					{"id": "auto", "name": "Auto"},
					{"id": "plan", "name": "Plan"},
				},
			},
			"configOptions": []map[string]any{
				{
					"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": "gemini-4-neon",
					"options": []map[string]any{
						{"value": "gemini-4-neon", "name": "Gemini 4 Neon"},
						{"value": "claude-sonnet-5-5", "name": "Claude Sonnet 5.5"},
					},
				},
				{
					"id": "thought_level", "name": "Thinking Effort", "category": "thought_level", "type": "select", "currentValue": "high",
					"options": []map[string]any{
						{"value": "low", "name": "Low"},
						{"value": "medium", "name": "Medium"},
						{"value": "high", "name": "High"},
						{"value": "xhigh", "name": "Max"},
					},
				},
				{
					"id": "mode", "name": "Mode", "category": "mode", "type": "select", "currentValue": "default",
					"options": []map[string]any{
						{"value": "default", "name": "Default"},
						{"value": "plan", "name": "Plan"},
					},
				},
			},
			"_meta": map[string]any{
				"antigravity": map[string]any{
					"tier": map[string]any{"id": "g1-ultra-tier", "name": "Google AI Ultra", "upgradeUri": ""},
				},
			},
		}
	}

	srv.on("session/new", func(msg mockACPRPCMessage) {
		_ = json.Unmarshal(msg.Params, &gotNewParams)
		_ = srv.sendResult(msg.ID, sessionStatePayload("sess-new-1"))
	})
	srv.on("session/load", func(msg mockACPRPCMessage) {
		_ = json.Unmarshal(msg.Params, &gotLoadParams)
		_ = srv.sendResult(msg.ID, sessionStatePayload("sess-load-1"))
	})
	srv.on("session/resume", func(msg mockACPRPCMessage) {
		_ = json.Unmarshal(msg.Params, &gotResumeParams)
		_ = srv.sendResult(msg.ID, sessionStatePayload("sess-res-1"))
	})
	srv.on("session/close", func(msg mockACPRPCMessage) {
		_ = json.Unmarshal(msg.Params, &gotCloseParams)
		_ = srv.sendResult(msg.ID, map[string]any{})
	})
	srv.on("session/set_config_option", func(msg mockACPRPCMessage) {
		_ = json.Unmarshal(msg.Params, &gotSetCfgParams)
		_ = srv.sendResult(msg.ID, map[string]any{})
	})
	srv.on("session/set_mode", func(msg mockACPRPCMessage) {
		_ = json.Unmarshal(msg.Params, &gotSetModeParams)
		_ = srv.sendResult(msg.ID, map[string]any{})
	})
	srv.start()

	ctx := context.Background()
	initRes, err := cli.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	if initRes.ProtocolVersion != 1 || initRes.AgentInfo.Name != "Antigravity ACP Server" || initRes.AgentInfo.Version != "1.5.18" {
		t.Fatalf("unexpected initRes: %+v", initRes)
	}
	if !initRes.AgentCapabilities.LoadSession || !initRes.AgentCapabilities.PromptCapabilities.Image || !initRes.AgentCapabilities.SessionCapabilities.List || !initRes.AgentCapabilities.SupportsTier || !initRes.AgentCapabilities.SupportsSystemAppend {
		t.Fatalf("unexpected capabilities: %+v", initRes.AgentCapabilities)
	}
	if cli.InitResult() == nil || cli.InitResult().AgentInfo.Version != "1.5.18" {
		t.Fatalf("cached InitResult mismatch")
	}
	clientInfo, _ := gotInitParams["clientInfo"].(map[string]any)
	if clientInfo["name"] != "fastcontrol" || clientInfo["version"] != "0.2.17" {
		t.Fatalf("unexpected clientInfo sent: %+v", clientInfo)
	}

	sessions, err := cli.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 2 || sessions[0].SessionID != "s-1" || sessions[1].SessionID != "s-2" {
		t.Fatalf("unexpected paginated sessions: %+v", sessions)
	}

	st, err := cli.NewSession(ctx, "/repo/work", []string{"/repo/extra"})
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if st.SessionID != "sess-new-1" || st.CurrentModel() != "gemini-4-neon" || st.CurrentThoughtLevel() != "high" || st.CurrentMode() != "default" {
		t.Fatalf("unexpected session state: %+v", st)
	}
	if st.Tier == nil || st.Tier.Name != "Google AI Ultra" || len(st.Models.AvailableModels) != 2 || len(st.Modes.AvailableModes) != 4 {
		t.Fatalf("unexpected tier/models/modes: %+v", st)
	}
	if dirs, _ := gotNewParams["additionalDirectories"].([]any); len(dirs) != 1 || dirs[0] != "/repo/extra" {
		t.Fatalf("additionalDirectories not sent: %+v", gotNewParams)
	}

	loaded, err := cli.LoadSession(ctx, "sess-load-1", "/repo/work")
	if err != nil || loaded.SessionID != "sess-load-1" {
		t.Fatalf("LoadSession failed: %v %+v", err, loaded)
	}
	if gotLoadParams["sessionId"] != "sess-load-1" || gotLoadParams["cwd"] != "/repo/work" {
		t.Fatalf("unexpected load params: %+v", gotLoadParams)
	}

	resumed, err := cli.ResumeSession(ctx, "sess-res-1", "/repo/work")
	if err != nil || resumed.SessionID != "sess-res-1" {
		t.Fatalf("ResumeSession failed: %v %+v", err, resumed)
	}
	if gotResumeParams["sessionId"] != "sess-res-1" {
		t.Fatalf("unexpected resume params: %+v", gotResumeParams)
	}

	if err := cli.SetConfigOption(ctx, "sess-new-1", "thought_level", "xhigh"); err != nil {
		t.Fatalf("SetConfigOption failed: %v", err)
	}
	if gotSetCfgParams["configId"] != "thought_level" || gotSetCfgParams["value"] != "xhigh" {
		t.Fatalf("unexpected set_config_option params: %+v", gotSetCfgParams)
	}

	if err := cli.SetMode(ctx, "sess-new-1", "plan"); err != nil {
		t.Fatalf("SetMode failed: %v", err)
	}
	if gotSetModeParams["modeId"] != "plan" {
		t.Fatalf("unexpected set_mode params: %+v", gotSetModeParams)
	}

	if err := cli.CloseSession(ctx, "sess-new-1"); err != nil {
		t.Fatalf("CloseSession failed: %v", err)
	}
	if gotCloseParams["sessionId"] != "sess-new-1" {
		t.Fatalf("unexpected close params: %+v", gotCloseParams)
	}
}

func TestACPPromptTextImageSteeringAndCancel(t *testing.T) {
	srv, cli := newMockACPServer(t, acpClientConfig{})
	defer func() {
		_ = cli.Close()
		srv.close()
	}()

	firstStarted := make(chan struct{})
	steerReceived := make(chan struct{})
	cancelReceived := make(chan string, 1)
	var gotBlocks []acpContentBlock
	var promptCount atomic.Int32

	srv.on("session/prompt", func(msg mockACPRPCMessage) {
		var p struct {
			SessionID string            `json:"sessionId"`
			Prompt    []acpContentBlock `json:"prompt"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		n := promptCount.Add(1)
		if n == 1 {
			gotBlocks = p.Prompt
			close(firstStarted)
			// Hold the first turn open until steering + cancel arrive.
			<-steerReceived
			<-cancelReceived
			_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "cancelled"})
			return
		}
		// Steering prompt arrives concurrently while turn 1 is still active!
		close(steerReceived)
		_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "end_turn"})
	})
	srv.on("session/cancel", func(msg mockACPRPCMessage) {
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		cancelReceived <- p.SessionID
	})
	srv.start()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	blocks := []acpContentBlock{
		acpTextBlock("Inspect this screenshot"),
		acpImageBlock(imgBytes, "image/png"),
	}

	turn1Done := make(chan *acpPromptResult, 1)
	turn1Err := make(chan error, 1)
	go func() {
		res, err := cli.Prompt(ctx, "sess-1", blocks)
		if err != nil {
			turn1Err <- err
			return
		}
		turn1Done <- res
	}()

	<-firstStarted

	// Concurrent steering prompt MUST not block behind turn 1.
	steerRes, err := cli.Prompt(ctx, "sess-1", []acpContentBlock{acpTextBlock("Also check line 12")})
	if err != nil {
		t.Fatalf("steering Prompt failed: %v", err)
	}
	if steerRes.StopReason != "end_turn" {
		t.Fatalf("unexpected steering stopReason: %q", steerRes.StopReason)
	}

	// Cancel turn 1 while running.
	if err := cli.Cancel(ctx, "sess-1"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	select {
	case err := <-turn1Err:
		t.Fatalf("turn 1 error: %v", err)
	case res := <-turn1Done:
		if res.StopReason != "cancelled" {
			t.Fatalf("expected cancelled stopReason, got %q", res.StopReason)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for turn 1 completion")
	}

	if len(gotBlocks) != 2 || gotBlocks[0].Type != "text" || gotBlocks[0].Text != "Inspect this screenshot" {
		t.Fatalf("unexpected text block: %+v", gotBlocks)
	}
	if gotBlocks[1].Type != "image" || gotBlocks[1].MimeType != "image/png" || gotBlocks[1].Data != base64.StdEncoding.EncodeToString(imgBytes) {
		t.Fatalf("unexpected image block: %+v", gotBlocks[1])
	}
}

func TestACPSessionUpdateVariantsAndDiffs(t *testing.T) {
	var mu sync.Mutex
	var updates []acpSessionUpdate
	allArrived := make(chan struct{})
	const expectedCount = 10

	srv, cli := newMockACPServer(t, acpClientConfig{
		OnUpdate: func(u acpSessionUpdate) {
			mu.Lock()
			updates = append(updates, u)
			done := len(updates) == expectedCount
			mu.Unlock()
			if done {
				close(allArrived)
			}
		},
	})
	defer func() {
		_ = cli.Close()
		srv.close()
	}()
	srv.start()

	sendUpd := func(update map[string]any) {
		t.Helper()
		if err := srv.sendNotify("session/update", map[string]any{
			"sessionId": "sess-u",
			"update":    update,
		}); err != nil {
			t.Fatalf("sendNotify: %v", err)
		}
	}

	sendUpd(map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "Fix bug"}})
	sendUpd(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "Thinking..."}})
	sendUpd(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Done!"}})
	sendUpd(map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    "tc-1",
		"name":          "edit_file",
		"args":          map[string]any{"path": "main.go"},
		"kind":          "edit",
		"title":         "Edit main.go",
		"status":        "in_progress",
		"locations":     []map[string]any{{"path": "main.go", "line": 42}},
		"rawInput":      map[string]any{"path": "main.go"},
	})
	sendUpd(map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "tc-1",
		"status":        "completed",
		"rawOutput":     map[string]any{"ok": true},
		"content": []map[string]any{
			{"type": "diff", "path": "main.go", "oldText": "a\nb\nc\n", "newText": "a\nB\nc\n"},
			{"type": "content", "content": map[string]any{"type": "text", "text": "Applied edit"}},
		},
	})
	sendUpd(map[string]any{"sessionUpdate": "usage_update", "used": 45000, "size": 1048576})
	sendUpd(map[string]any{
		"sessionUpdate": "available_commands_update",
		"availableCommands": []map[string]any{
			{"name": "goal", "description": "Run a goal", "input": map[string]any{"hint": "goal description"}},
			{"name": "compact", "description": "Compact context"},
		},
	})
	sendUpd(map[string]any{"sessionUpdate": "session_info_update", "title": "Fix Bug In Main"})
	sendUpd(map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": "accept-edits"})
	sendUpd(map[string]any{
		"sessionUpdate": "config_option_update",
		"configOptions": []map[string]any{
			{"id": "thought_level", "currentValue": "xhigh", "options": []map[string]any{{"value": "xhigh", "name": "Max"}}},
		},
	})

	select {
	case <-allArrived:
	case <-time.After(3 * time.Second):
		mu.Lock()
		got := len(updates)
		mu.Unlock()
		t.Fatalf("timed out waiting for updates: got %d of %d", got, expectedCount)
	}

	mu.Lock()
	defer mu.Unlock()
	if updates[0].Kind != "user_message_chunk" || updates[0].Text != "Fix bug" {
		t.Fatalf("unexpected update[0]: %+v", updates[0])
	}
	if updates[1].Kind != "agent_thought_chunk" || updates[1].Text != "Thinking..." {
		t.Fatalf("unexpected update[1]: %+v", updates[1])
	}
	if updates[2].Kind != "agent_message_chunk" || updates[2].Text != "Done!" {
		t.Fatalf("unexpected update[2]: %+v", updates[2])
	}
	if updates[3].ToolCall == nil || updates[3].ToolCall.ToolCallID != "tc-1" || updates[3].ToolCall.Name != "edit_file" || len(updates[3].ToolCall.Locations) != 1 || updates[3].ToolCall.Locations[0].Line != 42 {
		t.Fatalf("unexpected update[3]: %+v", updates[3])
	}
	tcUpd := updates[4].ToolCall
	if tcUpd == nil || tcUpd.Status != "completed" || len(tcUpd.Content) != 2 || tcUpd.Content[0].Diff == nil {
		t.Fatalf("unexpected update[4]: %+v", updates[4])
	}
	diffText := tcUpd.DiffText()
	if !strings.Contains(diffText, "--- main.go") || !strings.Contains(diffText, "-b\n+B\n") {
		t.Fatalf("unexpected formatted tool diff:\n%s", diffText)
	}
	if tcUpd.TextOutput() != "Applied edit" {
		t.Fatalf("unexpected text output: %q", tcUpd.TextOutput())
	}
	if updates[5].Usage == nil || updates[5].Usage.Used != 45000 || updates[5].Usage.Size != 1048576 {
		t.Fatalf("unexpected usage update: %+v", updates[5])
	}
	if len(updates[6].AvailableCommands) != 2 || updates[6].AvailableCommands[0].Name != "goal" || updates[6].AvailableCommands[0].InputHint != "goal description" {
		t.Fatalf("unexpected commands update: %+v", updates[6])
	}
	if updates[7].Title != "Fix Bug In Main" {
		t.Fatalf("unexpected session_info_update: %+v", updates[7])
	}
	if updates[8].CurrentModeID != "accept-edits" {
		t.Fatalf("unexpected current_mode_update: %+v", updates[8])
	}
	if len(updates[9].ConfigOptions) != 1 || updates[9].ConfigOptions[0].ID != "thought_level" || updates[9].ConfigOptions[0].CurrentValue != "xhigh" {
		t.Fatalf("unexpected config_option_update: %+v", updates[9])
	}
}

func TestACPPermissionRequestRoundTripsAndAutoCancel(t *testing.T) {
	permCh := make(chan acpPermissionRequest, 8)
	srv, cli := newMockACPServer(t, acpClientConfig{
		OnPermission: func(req acpPermissionRequest) {
			permCh <- req
		},
	})
	defer func() {
		_ = cli.Close()
		srv.close()
	}()
	srv.start()

	// 1. Allow once (numeric server JSON-RPC ID).
	resp1, err := srv.sendServerRequest(101, "session/request_permission", map[string]any{
		"sessionId": "sess-p",
		"toolCall": map[string]any{
			"toolCallId": "tc-run",
			"title":      "Run go test",
			"kind":       "execute",
			"rawInput":   map[string]any{"command": "go test ./..."},
		},
		"options": []map[string]any{
			{"optionId": "allow", "name": "Allow once", "kind": "allow_once"},
			{"optionId": "deny", "name": "Reject once", "kind": "reject_once"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req1 := <-permCh
	if req1.RequestID != "101" || req1.SessionID != "sess-p" || req1.ToolCall.Title != "Run go test" || len(req1.Options) != 2 {
		t.Fatalf("unexpected permission req1: %+v", req1)
	}
	if err := cli.RespondPermission(req1.RequestID, "allow", false); err != nil {
		t.Fatalf("RespondPermission allow failed: %v", err)
	}
	msg1 := <-resp1
	if string(bytes.TrimSpace(msg1.ID)) != "101" {
		t.Fatalf("expected raw numeric ID 101, got %s", string(msg1.ID))
	}
	var out1 struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(msg1.Result, &out1); err != nil || out1.Outcome.Outcome != "selected" || out1.Outcome.OptionID != "allow" {
		t.Fatalf("unexpected allow response: %s", string(msg1.Result))
	}
	// Duplicate reply must fail cleanly.
	if err := cli.RespondPermission(req1.RequestID, "allow", false); err == nil {
		t.Fatalf("expected error on duplicate permission reply")
	}

	// 2. Deny (string server JSON-RPC ID).
	resp2, err := srv.sendServerRequest("perm-2", "session/request_permission", map[string]any{
		"sessionId": "sess-p",
		"toolCall":  map[string]any{"toolCallId": "tc-rm", "title": "Delete file", "kind": "delete"},
		"options": []map[string]any{
			{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
			{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req2 := <-permCh
	if err := cli.RespondPermission(req2.RequestID, "deny", false); err != nil {
		t.Fatalf("RespondPermission deny failed: %v", err)
	}
	msg2 := <-resp2
	if string(bytes.TrimSpace(msg2.ID)) != `"perm-2"` {
		t.Fatalf("expected string ID \"perm-2\", got %s", string(msg2.ID))
	}
	var out2 struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(msg2.Result, &out2); err != nil || out2.Outcome.Outcome != "selected" || out2.Outcome.OptionID != "deny" {
		t.Fatalf("unexpected deny response: %s", string(msg2.Result))
	}

	// 3. Explicit cancel.
	resp3, err := srv.sendServerRequest(103, "session/request_permission", map[string]any{
		"sessionId": "sess-p",
		"toolCall":  map[string]any{"toolCallId": "tc-3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req3 := <-permCh
	if err := cli.RespondPermission(req3.RequestID, "", true); err != nil {
		t.Fatalf("RespondPermission cancel failed: %v", err)
	}
	msg3 := <-resp3
	if !strings.Contains(string(msg3.Result), `"outcome":"cancelled"`) {
		t.Fatalf("expected cancelled outcome, got %s", string(msg3.Result))
	}

	// 4. Auto-cancel on session/cancel (while leaving other session intact).
	resp4, err := srv.sendServerRequest(104, "session/request_permission", map[string]any{
		"sessionId": "sess-p",
		"toolCall":  map[string]any{"toolCallId": "tc-4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	respOther, err := srv.sendServerRequest(105, "session/request_permission", map[string]any{
		"sessionId": "sess-other",
		"toolCall":  map[string]any{"toolCallId": "tc-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-permCh
	<-permCh
	if cli.PendingRequestCount("sess-p") != 1 || cli.PendingRequestCount("") != 2 {
		t.Fatalf("unexpected pending counts: sess-p=%d total=%d", cli.PendingRequestCount("sess-p"), cli.PendingRequestCount(""))
	}

	if err := cli.Cancel(context.Background(), "sess-p"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}
	select {
	case msg4 := <-resp4:
		if !strings.Contains(string(msg4.Result), `"outcome":"cancelled"`) {
			t.Fatalf("expected auto-cancelled outcome on session/cancel, got %s", string(msg4.Result))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for auto-cancelled permission")
	}
	if cli.PendingRequestCount("sess-p") != 0 || cli.PendingRequestCount("sess-other") != 1 {
		t.Fatalf("expected sess-other to remain pending")
	}

	// 5. Client Close automatically cancels remaining pending requests before closing stdin.
	closeDone := make(chan struct{})
	go func() {
		_ = cli.Close()
		close(closeDone)
	}()
	select {
	case msgOther := <-respOther:
		if !strings.Contains(string(msgOther.Result), `"outcome":"cancelled"`) {
			t.Fatalf("expected auto-cancel on Close, got %s", string(msgOther.Result))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for auto-cancel on Close")
	}
	<-closeDone
}

func TestACPElicitationFormParsingAndRoundTrips(t *testing.T) {
	elicCh := make(chan acpElicitationRequest, 4)
	srv, cli := newMockACPServer(t, acpClientConfig{
		OnElicitation: func(req acpElicitationRequest) {
			elicCh <- req
		},
	})
	defer func() {
		_ = cli.Close()
		srv.close()
	}()
	srv.start()

	// Send a rich JSON Schema covering field ordering, enum/enumNames, oneOf, array multi-select, required, and secret hints.
	rawSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"env": {
				"type": "string",
				"title": "Environment",
				"description": "Target deployment environment",
				"enum": ["staging", "prod"],
				"enumNames": ["Staging", "Production"]
			},
			"targets": {
				"type": "array",
				"title": "Targets",
				"items": {
					"type": "string",
					"oneOf": [
						{"const": "web", "title": "Web App", "description": "Browser UI"},
						{"const": "android", "title": "Android App", "description": "Mobile APK"}
					]
				}
			},
			"mode": {
				"type": "string",
				"title": "Rollout Mode",
				"anyOf": [
					{"const": "canary", "title": "Canary"},
					{"const": "full", "title": "Full"}
				]
			},
			"apiToken": {
				"type": "string",
				"title": "API Token",
				"format": "password"
			}
		},
		"required": ["env", "apiToken"]
	}`)

	resp1, err := srv.sendServerRequest(201, "elicitation/create", map[string]any{
		"sessionId":       "sess-e",
		"message":         "Configure release options",
		"requestedSchema": rawSchema,
	})
	if err != nil {
		t.Fatal(err)
	}
	req1 := <-elicCh
	if req1.RequestID != "201" || req1.SessionID != "sess-e" || req1.Message != "Configure release options" || req1.Mode != "form" {
		t.Fatalf("unexpected elicitation req: %+v", req1)
	}
	if len(req1.Fields) != 4 {
		t.Fatalf("expected 4 fields, got %d: %+v", len(req1.Fields), req1.Fields)
	}
	// Verify deterministic JSON property order is preserved.
	wantOrder := []string{"env", "targets", "mode", "apiToken"}
	for i, name := range wantOrder {
		if req1.Fields[i].Name != name {
			t.Fatalf("field[%d] name = %q, want %q", i, req1.Fields[i].Name, name)
		}
	}
	if !req1.Fields[0].Required || len(req1.Fields[0].Options) != 2 || req1.Fields[0].Options[1].Label != "Production" {
		t.Fatalf("unexpected env field: %+v", req1.Fields[0])
	}
	if !req1.Fields[1].MultiSelect || len(req1.Fields[1].Options) != 2 || req1.Fields[1].Options[0].Value != "web" || req1.Fields[1].Options[0].Description != "Browser UI" {
		t.Fatalf("unexpected targets multi-select field: %+v", req1.Fields[1])
	}
	if len(req1.Fields[2].Options) != 2 || req1.Fields[2].Options[0].Value != "canary" {
		t.Fatalf("unexpected anyOf field: %+v", req1.Fields[2])
	}
	if !req1.Fields[3].Required || !req1.Fields[3].Secret || req1.Fields[3].Format != "password" {
		t.Fatalf("unexpected secret field: %+v", req1.Fields[3])
	}

	if err := cli.RespondElicitation(req1.RequestID, "accept", map[string]any{
		"env":      "prod",
		"targets":  []string{"web", "android"},
		"mode":     "canary",
		"apiToken": "secret-val",
	}); err != nil {
		t.Fatalf("RespondElicitation accept failed: %v", err)
	}
	msg1 := <-resp1
	var out1 struct {
		Action  string         `json:"action"`
		Content map[string]any `json:"content"`
	}
	if err := json.Unmarshal(msg1.Result, &out1); err != nil || out1.Action != "accept" || out1.Content["env"] != "prod" {
		t.Fatalf("unexpected elicitation accept reply: %s", string(msg1.Result))
	}

	// Cancel round-trip.
	resp2, err := srv.sendServerRequest(202, "elicitation/create", map[string]any{
		"sessionId": "sess-e",
		"message":   "Optional note",
	})
	if err != nil {
		t.Fatal(err)
	}
	req2 := <-elicCh
	if err := cli.RespondElicitation(req2.RequestID, "cancel", nil); err != nil {
		t.Fatalf("RespondElicitation cancel failed: %v", err)
	}
	msg2 := <-resp2
	if string(bytes.TrimSpace(msg2.Result)) != `{"action":"cancel"}` {
		t.Fatalf("unexpected elicitation cancel reply: %s", string(msg2.Result))
	}
}

func TestACPConcurrentCallsAndCleanShutdown(t *testing.T) {
	beforeGoroutines := runtime.NumGoroutine()

	closedCh := make(chan error, 1)
	srv, cli := newMockACPServer(t, acpClientConfig{
		OnClose: func(err error) {
			closedCh <- err
		},
	})
	srv.on("session/set_mode", func(msg mockACPRPCMessage) {
		time.Sleep(5 * time.Millisecond)
		_ = srv.sendResult(msg.ID, map[string]any{})
	})
	srv.on("session/set_config_option", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{})
	})
	srv.on("session/prompt", func(msg mockACPRPCMessage) {
		// Never replies before EOF, verifying pending RPCs unblock immediately on EOF.
	})
	srv.start()

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				if err := cli.SetMode(ctx, "s-race", "auto"); err != nil {
					t.Errorf("SetMode %d failed: %v", n, err)
				}
			} else {
				if err := cli.SetConfigOption(ctx, "s-race", "thought_level", "medium"); err != nil {
					t.Errorf("SetConfigOption %d failed: %v", n, err)
				}
			}
		}(i)
	}
	wg.Wait()

	// Start an in-flight Prompt, then simulate abrupt server EOF.
	promptErr := make(chan error, 1)
	go func() {
		_, err := cli.Prompt(ctx, "s-race", []acpContentBlock{acpTextBlock("hello")})
		promptErr <- err
	}()
	time.Sleep(25 * time.Millisecond)

	// Server EOF closes stdout without waiting for Close().
	srv.close()

	select {
	case err := <-promptErr:
		if err == nil {
			t.Fatalf("expected Prompt to fail on EOF")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for Prompt to unblock on EOF")
	}

	select {
	case <-closedCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for OnClose")
	}

	if err := cli.Close(); err != nil {
		t.Fatalf("Close after EOF failed: %v", err)
	}
	if !cli.Closed() {
		t.Fatalf("expected Closed() == true")
	}

	// Verify goroutines returned cleanly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= beforeGoroutines+2 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > beforeGoroutines+2 {
		t.Fatalf("possible goroutine leak: before=%d after=%d", beforeGoroutines, after)
	}
}

func TestACPRPCErrorCallbacksNonBlockingAndStderrBuffer(t *testing.T) {
	// Verify bounded stderr ring buffer keeps trailing bytes only.
	buf := newACPStderrBuffer(8)
	_, _ = buf.Write([]byte("0123456789abcdef"))
	if got := buf.String(); got != "89abcdef" {
		t.Fatalf("stderr ring buffer got %q, want %q", got, "89abcdef")
	}
	_, _ = buf.Write([]byte("XY"))
	if got := buf.String(); got != "abcdefXY" {
		t.Fatalf("stderr ring buffer append got %q, want %q", got, "abcdefXY")
	}

	// Verify callbacks run off the stdio reader loop so calling client RPCs
	// directly inside OnUpdate / OnPermission never deadlocks, and verify *acpRPCError.
	var cli *acpClient
	modeDone := make(chan error, 1)
	var srv *mockACPServer
	srv, cli = newMockACPServer(t, acpClientConfig{
		CallTimeout: 3 * time.Second,
		OnUpdate: func(u acpSessionUpdate) {
			if u.Kind == "session_info_update" {
				modeDone <- cli.SetMode(context.Background(), u.SessionID, "plan")
			}
		},
		OnPermission: func(req acpPermissionRequest) {
			_ = cli.RespondPermission(req.RequestID, "allow", false)
		},
	})
	defer func() {
		_ = cli.Close()
		srv.close()
	}()
	srv.on("session/set_mode", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{})
	})
	srv.on("session/close", func(msg mockACPRPCMessage) {
		_ = srv.sendError(msg.ID, -32001, "session not found")
	})
	srv.start()

	if err := srv.sendNotify("session/update", map[string]any{
		"sessionId": "sess-cb",
		"update":    map[string]any{"sessionUpdate": "session_info_update", "title": "Trigger"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-modeDone:
		if err != nil {
			t.Fatalf("SetMode inside OnUpdate failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out calling SetMode inside OnUpdate (reader deadlock)")
	}

	permResp, err := srv.sendServerRequest(301, "session/request_permission", map[string]any{
		"sessionId": "sess-cb",
		"toolCall":  map[string]any{"toolCallId": "tc-cb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-permResp:
		if !strings.Contains(string(msg.Result), `"optionId":"allow"`) {
			t.Fatalf("unexpected inline permission reply: %s", string(msg.Result))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for inline RespondPermission")
	}

	// Verify typed *acpRPCError propagation.
	err = cli.CloseSession(context.Background(), "missing")
	var rpcErr *acpRPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32001 || rpcErr.Message != "session not found" {
		t.Fatalf("expected *acpRPCError(-32001), got %T %v", err, err)
	}

	// Verify unknown server request gets JSON-RPC -32601 method-not-found reply.
	unkResp, err := srv.sendServerRequest(302, "unknown/server_method", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-unkResp:
		if msg.Error == nil || msg.Error.Code != -32601 {
			t.Fatalf("expected -32601 error for unknown server request, got %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for -32601 response")
	}
}

func TestFormatACPDiff(t *testing.T) {
	if d := formatACPDiff("same\n", "same\n", "a.go"); d != "" {
		t.Fatalf("expected empty diff for identical text, got:\n%s", d)
	}

	added := formatACPDiff("", "line1\nline2\n", "new.go")
	if !strings.Contains(added, "--- /dev/null\n+++ new.go\n@@ -0,0 +1,2 @@\n+line1\n+line2\n") {
		t.Fatalf("unexpected add diff:\n%s", added)
	}

	deleted := formatACPDiff("old1\n", "", "gone.go")
	if !strings.Contains(deleted, "--- gone.go\n+++ /dev/null\n@@ -1,1 +0,0 @@\n-old1\n") {
		t.Fatalf("unexpected delete diff:\n%s", deleted)
	}

	// Two distant hunks in a 15-line file should emit two @@ headers.
	oldLines := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15"}
	newLines := []string{"1", "TWO", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "FOURTEEN", "15"}
	multi := formatACPDiff(strings.Join(oldLines, "\n")+"\n", strings.Join(newLines, "\n")+"\n", "nums.txt")
	if count := strings.Count(multi, "@@ "); count != 2 {
		t.Fatalf("expected 2 hunks, got %d:\n%s", count, multi)
	}
	if !strings.Contains(multi, "-2\n+TWO\n") || !strings.Contains(multi, "-14\n+FOURTEEN\n") {
		t.Fatalf("unexpected multi-hunk diff:\n%s", multi)
	}
}

func TestACPLiveReadOnlyInitializeAndList(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live agy acp integration test in short mode")
	}
	bin := agyBinary()
	if bin == "" {
		t.Skip("agy binary not installed")
	}
	if !agySupportsACP(bin) {
		t.Skipf("agy binary %s does not support acp --app-data-dir", bin)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, initRes, err := dialACPClient(ctx, acpClientConfig{
		Binary:      bin,
		AppDataDir:  acpDefaultAppDataDir,
		CallTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("live dialACPClient failed: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("live client.Close failed: %v", err)
		}
	}()

	if initRes.ProtocolVersion != 1 {
		t.Fatalf("unexpected live protocolVersion: %d", initRes.ProtocolVersion)
	}
	if initRes.AgentInfo.Name == "" || !initRes.AgentCapabilities.LoadSession {
		t.Fatalf("unexpected live initialize result: %+v", initRes)
	}

	sessions, err := client.ListSessions(ctx)
	if err != nil {
		t.Fatalf("live session/list failed: %v (stderr: %s)", err, client.Stderr())
	}
	// Read-only assertion: slice is valid (may be 0+ sessions depending on machine state).
	for _, s := range sessions {
		if s.SessionID == "" {
			t.Fatalf("live session/list returned entry with empty sessionId: %+v", s)
		}
	}
}

func TestWindowsPathExpansionBatchQuotingAndAgyDiscovery(t *testing.T) {
	// 1. %VAR% expansion and deduplication in semicolon-delimited Windows Registry PATH.
	envMap := map[string]string{
		"USERPROFILE":  `C:\Users\Alice Smith`,
		"LOCALAPPDATA": `C:\Users\Alice Smith\AppData\Local`,
		"APPDATA":      `C:\Users\Alice Smith\AppData\Roaming`,
	}
	lookup := func(k string) string { return envMap[k] }
	rawPath := `%USERPROFILE%\.local\bin; "%LOCALAPPDATA%\Programs\Antigravity\bin" ; %APPDATA%\npm ; c:\users\alice smith\.local\bin ; %UNKNOWN_TOKEN%\bin`
	dirs := splitWindowsPathList(rawPath, lookup)
	wantDirs := []string{
		`C:\Users\Alice Smith\.local\bin`,
		`C:\Users\Alice Smith\AppData\Local\Programs\Antigravity\bin`,
		`C:\Users\Alice Smith\AppData\Roaming\npm`,
		`%UNKNOWN_TOKEN%\bin`,
	}
	if len(dirs) != len(wantDirs) {
		t.Fatalf("unexpected expanded PATH dirs: got %v, want %v", dirs, wantDirs)
	}
	for i := range wantDirs {
		if dirs[i] != wantDirs[i] {
			t.Fatalf("dir[%d]=%q, want %q", i, dirs[i], wantDirs[i])
		}
	}

	// 2. mergeWindowsPathEnv merges missing live Registry PATH dirs case-insensitively.
	baseEnv := []string{`SystemRoot=C:\Windows`, `Path=C:\Windows\System32;C:\Users\Alice Smith\.local\bin`}
	mergedEnv := mergeWindowsPathEnv(baseEnv, []string{
		`c:\users\alice smith\.local\bin`,
		`C:\Users\Alice Smith\AppData\Local\Programs\Antigravity\bin`,
	})
	var gotMergedPath string
	for _, e := range mergedEnv {
		if strings.HasPrefix(e, "Path=") {
			gotMergedPath = strings.TrimPrefix(e, "Path=")
		}
	}
	if !strings.Contains(gotMergedPath, `C:\Users\Alice Smith\AppData\Local\Programs\Antigravity\bin`) ||
		strings.Count(strings.ToLower(gotMergedPath), strings.ToLower(`C:\Users\Alice Smith\.local\bin`)) != 1 {
		t.Fatalf("unexpected merged PATH env: %q", gotMergedPath)
	}

	// 3. Full Windows candidate path enumeration (including WinGet Packages scan and live Registry PATH).
	tmpLocal := t.TempDir()
	wingetBinDir := filepath.Join(tmpLocal, "Microsoft", "WinGet", "Packages", "Google.Antigravity_1.5.18", "bin")
	if err := os.MkdirAll(wingetBinDir, 0755); err != nil {
		t.Fatal(err)
	}
	winCands := agyBinaryCandidatesForEnv(cliDiscoveryEnv{
		goos:            "windows",
		home:            `C:\Users\Alice`,
		localAppData:    tmpLocal,
		appData:         `C:\Users\Alice\AppData\Roaming`,
		programFiles:    `C:\Program Files`,
		programFilesX86: `C:\Program Files (x86)`,
		livePathDirs:    []string{`D:\CustomTools\agy`},
		envOverrides:    []string{`E:\Override\agy.exe`},
	})
	mustContainSuffix := []string{
		`E:\Override\agy.exe`,
		filepath.Join(".local", "bin", "agy.exe"),
		filepath.Join(".antigravity", "bin", "agy.exe"),
		filepath.Join(".gemini", "antigravity", "bin", "agy.exe"),
		filepath.Join(".gemini", "antigravity-cli", "bin", "agy.exe"),
		filepath.Join(`D:\CustomTools\agy`, "agy.cmd"),
		filepath.Join("Programs", "Antigravity", "bin", "agy.cmd"),
		filepath.Join("Programs", "Antigravity", "agy.exe"),
		filepath.Join("Programs", "Antigravity IDE", "bin", "agy.cmd"),
		filepath.Join("Antigravity", "bin", "agy.exe"),
		filepath.Join("antigravity-cli", "bin", "agy.exe"),
		filepath.Join("Microsoft", "WinGet", "Links", "agy.exe"),
		filepath.Join("Google.Antigravity_1.5.18", "bin", "agy.exe"),
		filepath.Join("npm", "agy.cmd"),
		filepath.Join("scoop", "shims", "agy.exe"),
		filepath.Join("Antigravity", "bin", "agy.cmd"),
	}
	joinedWin := strings.Join(winCands, "\n")
	for _, want := range mustContainSuffix {
		if !strings.Contains(joinedWin, want) {
			t.Fatalf("expected Windows candidates to include %q; got:\n%s", want, joinedWin)
		}
	}

	// 4. Windows executable check accepts .exe/.cmd/.bat and rejects extensionless POSIX shim.
	tmpBin := t.TempDir()
	for _, name := range []string{"agy", "agy.exe", "agy.cmd", "agy.bat"} {
		if err := os.WriteFile(filepath.Join(tmpBin, name), []byte("stub"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if isCLIExecutableForOS(filepath.Join(tmpBin, "agy"), "windows") {
		t.Fatalf("extensionless POSIX script must not be treated as Windows executable")
	}
	for _, ext := range []string{"agy.exe", "agy.cmd", "agy.bat"} {
		if !isCLIExecutableForOS(filepath.Join(tmpBin, ext), "windows") {
			t.Fatalf("expected %s to be accepted on windows", ext)
		}
	}

	// 5. Windows .cmd/.bat command-line quoting handles spaces, trailing slashes, %, and rejects injection.
	cmdLine, err := buildWindowsBatchCmdLine(
		`C:\Windows\System32\cmd.exe`,
		`C:\Users\Alice Smith\AppData\Local\Programs\Antigravity\bin\agy.cmd`,
		[]string{"acp", "--app-data-dir", "antigravity", "--gemini-dir", `C:\Users\Alice Smith\.gemini\`, "100%safe"},
	)
	if err != nil {
		t.Fatalf("buildWindowsBatchCmdLine failed: %v", err)
	}
	wantPrefix := `C:\Windows\System32\cmd.exe /d /s /c ""C:\Users\Alice Smith\AppData\Local\Programs\Antigravity\bin\agy.cmd" acp --app-data-dir antigravity --gemini-dir "C:\Users\Alice Smith\.gemini\\" "100"^%"safe""`
	if cmdLine != wantPrefix {
		t.Fatalf("unexpected batch cmdLine:\ngot:  %s\nwant: %s", cmdLine, wantPrefix)
	}
	for _, bad := range []string{"bad\narg", "bad\rarg", `bad"arg`, "bad\x00arg"} {
		if _, err := buildWindowsBatchCmdLine("cmd.exe", `C:\agy.cmd`, []string{bad}); err == nil {
			t.Fatalf("expected injection arg %q to be rejected", bad)
		}
	}

	// 6. Explicit AGY_BIN env override takes priority in agyBinary().
	overrideName := "custom-agy"
	if runtime.GOOS == "windows" {
		overrideName = "custom-agy.exe"
	}
	overridePath := filepath.Join(t.TempDir(), overrideName)
	if err := os.WriteFile(overridePath, []byte("#!/bin/sh\necho ok\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGY_BIN", overridePath)
	t.Setenv("ANTIGRAVITY_CLI", "")
	if got := agyBinary(); got != filepath.Clean(overridePath) {
		t.Fatalf("expected AGY_BIN override %q, got %q", overridePath, got)
	}
}

func TestAgySupportsACPTransientFailureRetryAndStderrCRLF(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-agy-retry")
	if err := os.WriteFile(bin, []byte("stub-v1"), 0755); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 28, 2, 0, 0, 0, time.UTC)
	var nowMu sync.Mutex
	var calls atomic.Int32
	var nextRes atomic.Value
	nextRes.Store(acpProbeResult{status: acpSupportTimeout, detail: "timed out during Defender scan"})

	agyACPCacheMu.Lock()
	origHelp := agyProbeHelpFunc
	origDetail := agyProbeDetailFunc
	origNow := acpCacheNow
	origTTL := acpNegativeCacheTTL
	agyProbeHelpFunc = nil
	agyProbeDetailFunc = func(string) acpProbeResult {
		calls.Add(1)
		return nextRes.Load().(acpProbeResult)
	}
	acpCacheNow = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	acpNegativeCacheTTL = 30 * time.Second
	delete(agyACPCache, bin)
	agyACPCacheMu.Unlock()

	t.Cleanup(func() {
		agyACPCacheMu.Lock()
		agyProbeHelpFunc = origHelp
		agyProbeDetailFunc = origDetail
		acpCacheNow = origNow
		acpNegativeCacheTTL = origTTL
		delete(agyACPCache, bin)
		agyACPCacheMu.Unlock()
	})

	// 1. Initial transient timeout fails, stays cached briefly for passive checks.
	res1 := probeAgyACPSupport(bin, false)
	if res1.status != acpSupportTimeout || calls.Load() != 1 {
		t.Fatalf("expected initial timeout result, got %+v calls=%d", res1, calls.Load())
	}
	if agySupportsACP(bin) || calls.Load() != 1 {
		t.Fatalf("expected short-lived negative cache hit within TTL, calls=%d", calls.Load())
	}

	// 2. Explicit forceRetry bypasses negative cache immediately and caches positive permanently!
	nextRes.Store(acpProbeResult{status: acpSupportOK})
	res2 := probeAgyACPSupport(bin, true)
	if res2.status != acpSupportOK || calls.Load() != 2 {
		t.Fatalf("expected forceRetry to re-probe and succeed, got %+v calls=%d", res2, calls.Load())
	}
	if !agySupportsACP(bin) || calls.Load() != 2 {
		t.Fatalf("expected positive result to stay cached, calls=%d", calls.Load())
	}

	// 3. Verify negative TTL expiration retries automatically even without forceRetry.
	bin2 := filepath.Join(dir, "fake-agy-ttl")
	if err := os.WriteFile(bin2, []byte("stub-v2"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		agyACPCacheMu.Lock()
		delete(agyACPCache, bin2)
		agyACPCacheMu.Unlock()
	})
	nextRes.Store(acpProbeResult{status: acpSupportExecError, detail: "transient launch error"})
	if agySupportsACP(bin2) || calls.Load() != 3 {
		t.Fatalf("expected initial exec error on bin2, calls=%d", calls.Load())
	}
	nextRes.Store(acpProbeResult{status: acpSupportOK})
	nowMu.Lock()
	now = now.Add(31 * time.Second)
	nowMu.Unlock()
	if !agySupportsACP(bin2) || calls.Load() != 4 {
		t.Fatalf("expected expired negative cache to re-probe and succeed, calls=%d", calls.Load())
	}

	// 4. Real subprocess probe verifies CRLF + stderr help text acceptance and old-CLI rejection.
	if runtime.GOOS != "windows" {
		stderrBin := filepath.Join(dir, "agy-stderr-crlf")
		if err := os.WriteFile(stderrBin, []byte("#!/bin/sh\nprintf 'Usage: agy acp\\r\\n  --app-data-dir DIR\\r\\n' >&2\n"), 0755); err != nil {
			t.Fatal(err)
		}
		if got := probeAgyACPDetail(stderrBin); got.status != acpSupportOK {
			t.Fatalf("expected stderr CRLF help output to pass probe, got %+v", got)
		}

		oldBin := filepath.Join(dir, "agy-old")
		if err := os.WriteFile(oldBin, []byte("#!/bin/sh\nprintf 'Error: unknown command \"acp\"\\r\\nUsage: agy [flags]\\r\\n' >&2\nexit 2\n"), 0755); err != nil {
			t.Fatal(err)
		}
		if got := probeAgyACPDetail(oldBin); got.status != acpSupportUnsupported {
			t.Fatalf("expected old CLI unknown command to classify as unsupported, got %+v", got)
		}
	}
}
