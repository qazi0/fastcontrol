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

// mockGrokACPServer drives an in-process JSON-RPC 2.0 Grok ACP server over io.Pipe.
type mockGrokACPServer struct {
	t *testing.T

	clientOut *io.PipeWriter // server writes -> client reads
	serverIn  *io.PipeReader // client writes -> server reads

	writeMu sync.Mutex
	mu      sync.Mutex

	handlers  map[string]func(msg mockACPRPCMessage)
	responses map[string]chan mockACPRPCMessage
	notifs    chan mockACPRPCMessage
	rawLines  []string

	done chan struct{}
}

func newMockGrokACPServer(t *testing.T, cfg grokACPClientConfig) (*mockGrokACPServer, *grokACPClient) {
	t.Helper()
	srvRead, cliWrite := io.Pipe()
	cliRead, srvWrite := io.Pipe()

	s := &mockGrokACPServer{
		t:         t,
		clientOut: srvWrite,
		serverIn:  srvRead,
		handlers:  map[string]func(mockACPRPCMessage){},
		responses: map[string]chan mockACPRPCMessage{},
		notifs:    make(chan mockACPRPCMessage, 64),
		done:      make(chan struct{}),
	}
	c := newGrokACPClientFromStreams(cliRead, cliWrite, cfg)
	return s, c
}

func (s *mockGrokACPServer) start() {
	go func() {
		defer close(s.done)
		br := bufio.NewReader(s.serverIn)
		for {
			line, err := readACPLine(br, 16<<20)
			if len(line) > 0 {
				s.mu.Lock()
				s.rawLines = append(s.rawLines, string(line))
				s.mu.Unlock()
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

func (s *mockGrokACPServer) on(method string, fn func(mockACPRPCMessage)) {
	s.mu.Lock()
	s.handlers[method] = fn
	s.mu.Unlock()
}

func (s *mockGrokACPServer) sendJSON(v any) error {
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

func (s *mockGrokACPServer) sendResult(id json.RawMessage, result any) error {
	return s.sendJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (s *mockGrokACPServer) sendError(id json.RawMessage, code int, message string) error {
	return s.sendJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func (s *mockGrokACPServer) sendNotify(method string, params any) error {
	return s.sendJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (s *mockGrokACPServer) sendServerRequest(id any, method string, params any) (chan mockACPRPCMessage, error) {
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

func (s *mockGrokACPServer) close() {
	_ = s.clientOut.Close()
	_ = s.serverIn.Close()
	<-s.done
}

func TestBuildGrokACPArgs(t *testing.T) {
	args, err := buildGrokACPArgs(grokACPClientConfig{})
	if err != nil {
		t.Fatalf("default args error: %v", err)
	}
	if strings.Join(args, " ") != "agent --no-leader stdio" {
		t.Fatalf("unexpected default args: %v", args)
	}

	args, err = buildGrokACPArgs(grokACPClientConfig{
		AlwaysApprove:   true,
		Model:           "grok-4.7",
		ReasoningEffort: "xhigh",
		AgentProfile:    "/tmp/profile.toml",
		PluginDirs:      []string{"/tmp/p1", "/tmp/p2"},
	})
	if err != nil {
		t.Fatalf("custom args error: %v", err)
	}
	want := "agent --no-leader --always-approve -m grok-4.7 --reasoning-effort xhigh --agent-profile /tmp/profile.toml --plugin-dir /tmp/p1 --plugin-dir /tmp/p2 stdio"
	if strings.Join(args, " ") != want {
		t.Fatalf("got %q, want %q", strings.Join(args, " "), want)
	}
	if args[len(args)-1] != "stdio" {
		t.Fatalf("stdio MUST be final subcommand after agent flags, got %v", args)
	}

	if _, err := buildGrokACPArgs(grokACPClientConfig{Model: "--bad"}); err == nil {
		t.Fatalf("expected error for flag-like model")
	}
	if _, err := buildGrokACPArgs(grokACPClientConfig{ReasoningEffort: "-bad"}); err == nil {
		t.Fatalf("expected error for flag-like reasoning effort")
	}
	if _, err := buildGrokACPArgs(grokACPClientConfig{AgentProfile: "-bad"}); err == nil {
		t.Fatalf("expected error for flag-like agent profile")
	}
	if _, err := buildGrokACPArgs(grokACPClientConfig{PluginDirs: []string{"--bad"}}); err == nil {
		t.Fatalf("expected error for flag-like plugin dir")
	}
}

func TestGrokBinaryCandidates(t *testing.T) {
	mac := grokBinaryCandidates("/Users/alice", "", "darwin")
	if len(mac) < 4 {
		t.Fatalf("expected at least 4 darwin candidates, got %v", mac)
	}
	if !strings.HasSuffix(mac[0], filepath.Join(".local", "bin", "grok")) ||
		!strings.HasSuffix(mac[1], filepath.Join(".grok", "bin", "grok")) ||
		mac[2] != "/opt/homebrew/bin/grok" ||
		mac[3] != "/usr/local/bin/grok" {
		t.Fatalf("unexpected darwin candidates: %v", mac)
	}

	linux := grokBinaryCandidates("/home/alice", "", "linux")
	if len(linux) < 4 || !strings.HasSuffix(linux[1], filepath.Join(".grok", "bin", "grok")) {
		t.Fatalf("unexpected linux candidates: %v", linux)
	}

	win := grokBinaryCandidates(`C:\Users\alice`, `C:\Users\alice\AppData\Local`, "windows")
	if len(win) < 4 || !strings.HasSuffix(win[0], "grok.exe") || !strings.HasSuffix(win[1], filepath.Join(".grok", "bin", "grok.exe")) {
		t.Fatalf("unexpected windows candidates: %v", win)
	}
}

func TestGrokSupportsACPCachingAndInvalidation(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-grok")
	if err := os.WriteFile(bin, []byte("fake-grok-v1"), 0755); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	var support atomic.Bool
	support.Store(true)

	origProbe := grokProbeHelpFunc
	t.Cleanup(func() {
		grokACPCacheMu.Lock()
		grokProbeHelpFunc = origProbe
		delete(grokACPCache, bin)
		grokACPCacheMu.Unlock()
	})
	grokACPCacheMu.Lock()
	grokProbeHelpFunc = func(path string) bool {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return support.Load()
	}
	delete(grokACPCache, bin)
	grokACPCacheMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !grokSupportsACP(bin) {
				t.Errorf("expected grokSupportsACP=true")
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 deduplicated probe call, got %d", got)
	}
	if !grokSupportsACP(bin) || calls.Load() != 1 {
		t.Fatalf("expected cached true result, calls=%d", calls.Load())
	}

	// Updating binary content/size invalidates cache cleanly.
	support.Store(false)
	if err := os.WriteFile(bin, []byte("fake-grok-v2-longer-payload"), 0755); err != nil {
		t.Fatal(err)
	}
	if grokSupportsACP(bin) {
		t.Fatalf("expected invalidated cache to re-probe and return false")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 total probe calls after update, got %d", got)
	}
	if grokSupportsACP("") {
		t.Fatalf("expected empty binary path to return false")
	}
}

func TestGrokACPInitializeAuthSessionAndExtensions(t *testing.T) {
	srv, cli := newMockGrokACPServer(t, grokACPClientConfig{CallTimeout: 5 * time.Second})
	defer func() {
		_ = cli.Close()
		srv.close()
	}()

	var mu sync.Mutex
	var gotInitParams map[string]any
	var gotAuthParams map[string]any
	var gotNewParams map[string]any
	var gotLoadParams map[string]any
	var gotResumeParams map[string]any
	var gotSetCfgRaw json.RawMessage
	var gotSetModelParams map[string]any
	var gotRenameParams map[string]any
	var gotDeleteParams map[string]any
	var gotForkParams map[string]any
	var gotCompactParams map[string]any
	var gotListCwds []string
	var listCalls atomic.Int32
	var gotCommandsParams []map[string]any

	yoloNotifCh := make(chan mockACPRPCMessage, 2)
	planNotifCh := make(chan mockACPRPCMessage, 2)

	srv.on("initialize", func(msg mockACPRPCMessage) {
		mu.Lock()
		_ = json.Unmarshal(msg.Params, &gotInitParams)
		mu.Unlock()
		_ = srv.sendResult(msg.ID, map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"loadSession": true,
				"promptCapabilities": map[string]any{
					"image":           true,
					"audio":           false,
					"embeddedContext": true,
				},
				"mcpCapabilities": map[string]any{"http": true, "sse": true},
				"sessionCapabilities": map[string]any{
					"list":   map[string]any{},
					"resume": map[string]any{},
					"close":  map[string]any{},
				},
				"auth": map[string]any{},
				"_meta": map[string]any{
					"x.ai/fs_notify": true,
				},
			},
			"authMethods": []map[string]any{
				{"id": "cached_token", "name": "Cached Token"},
				{"id": "grok.com", "name": "Grok", "description": "Sign in with Grok"},
			},
			"_meta": map[string]any{
				"agentVersion":            "1.0.92",
				"grokShell":               true,
				"defaultAuthMethodId":     "cached_token",
				"currentWorkingDirectory": "/repo/app",
				"modelState": map[string]any{
					"currentModelId": "grok-4.7",
					"availableModels": []map[string]any{
						{
							"modelId":     "grok-4.7",
							"name":        "Grok 4.7",
							"description": "Frontier model",
							"_meta": map[string]any{
								"totalContextTokens":      500000,
								"agentType":               "grok-build-plan",
								"supportsReasoningEffort": true,
								"reasoningEffort":         "high",
								"reasoningEfforts": []map[string]any{
									{"id": "low", "value": "low", "label": "Low", "default": false},
									{"id": "high", "value": "high", "label": "High", "default": true},
									{"id": "xhigh", "value": "xhigh", "label": "Extra High", "default": false},
								},
							},
						},
					},
				},
			},
		})
	})

	srv.on("authenticate", func(msg mockACPRPCMessage) {
		mu.Lock()
		_ = json.Unmarshal(msg.Params, &gotAuthParams)
		mu.Unlock()
		_ = srv.sendResult(msg.ID, map[string]any{
			"_meta": map[string]any{
				"auth_mode":         "oauth",
				"subscription_tier": "SuperGrok Plus",
				"is_zdr":            false,
				"coding_data_team":  false,
				"backend_billed":    true,
			},
		})
	})

	srv.on("_x.ai/auth/check_subscription", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{
			"authenticated": true,
			"meta": map[string]any{
				"auth_mode":         "oauth",
				"subscription_tier": "SuperGrok Plus",
			},
		})
	})

	// Double-wrapped _x.ai/models/list response (live Grok wire format from #118).
	srv.on("_x.ai/models/list", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{
			"result": map[string]any{
				"currentModelId": "grok-4.7",
				"availableModels": []map[string]any{
					{
						"modelId": "grok-4.7",
						"name":    "Grok 4.7",
						"_meta": map[string]any{
							"totalContextTokens":      500000,
							"supportsReasoningEffort": true,
							"reasoningEfforts": []map[string]any{
								{"id": "high", "value": "high", "label": "High", "default": true},
								{"id": "xhigh", "value": "xhigh", "label": "Extra High", "default": false},
							},
						},
					},
					{
						"modelId": "grok-4.7-build-fast",
						"name":    "Grok 4.7 Fast",
						"_meta": map[string]any{
							"totalContextTokens": 256000,
						},
					},
				},
			},
		})
	})

	srv.on("_x.ai/billing", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{
			"config": map[string]any{
				"creditUsagePercent": 31.5,
				"currentPeriod": map[string]any{
					"type":  "USAGE_PERIOD_TYPE_WEEKLY",
					"start": "2026-11-22T00:00:00Z",
					"end":   "2026-11-29T00:00:00Z",
				},
				"onDemandCap":          map[string]any{"val": 100},
				"onDemandUsed":         map[string]any{"val": 12.5},
				"prepaidBalance":       map[string]any{"val": 42},
				"isUnifiedBillingUser": true,
				"billingPeriodStart":   "2026-11-01T00:00:00Z",
				"billingPeriodEnd":     "2026-12-01T00:00:00Z",
			},
			"subscription_tier": "SuperGrok Plus",
		})
	})

	srv.on("_x.ai/commands/list", func(msg mockACPRPCMessage) {
		var p map[string]any
		_ = json.Unmarshal(msg.Params, &p)
		mu.Lock()
		gotCommandsParams = append(gotCommandsParams, p)
		mu.Unlock()
		_ = srv.sendResult(msg.ID, map[string]any{
			"commands": []map[string]any{
				{"name": "compact", "description": "Compact history", "input": nil},
				{"name": "always-approve", "description": "Toggle always approve", "input": map[string]any{"hint": "on|off"}},
			},
			"tools": []string{"Read", "Edit", "Bash"},
		})
	})

	srv.on("session/list", func(msg mockACPRPCMessage) {
		var p struct {
			Cwd    string `json:"cwd"`
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		mu.Lock()
		gotListCwds = append(gotListCwds, p.Cwd)
		mu.Unlock()
		if listCalls.Add(1) == 1 {
			_ = srv.sendResult(msg.ID, map[string]any{
				"sessions": []map[string]any{
					{"sessionId": "g-1", "cwd": "/repo/app", "title": "First", "updatedAt": "2026-11-27T10:00:00Z"},
				},
				"nextCursor": "cur-2",
			})
			return
		}
		_ = srv.sendResult(msg.ID, map[string]any{
			"sessions": []map[string]any{
				{"sessionId": "g-2", "cwd": "/repo/app", "title": "Second", "updatedAt": "2026-11-27T11:00:00Z"},
			},
		})
	})

	sessionPayload := func(sid string) map[string]any {
		return map[string]any{
			"sessionId": sid,
			"models": map[string]any{
				"currentModelId": "grok-4.7",
				"availableModels": []map[string]any{
					{"modelId": "grok-4.7", "name": "Grok 4.7", "_meta": map[string]any{"totalContextTokens": 500000}},
				},
			},
			"configOptions": []map[string]any{
				{
					"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": "grok-4.7",
					"options": []map[string]any{{"value": "grok-4.7", "name": "Grok 4.7"}},
				},
				{
					"id": "reasoning_effort", "name": "Reasoning Effort", "category": "thought_level", "type": "select", "currentValue": "xhigh",
					"options": []map[string]any{
						{"value": "high", "name": "High"},
						{"value": "xhigh", "name": "Extra High"},
					},
				},
			},
			"_meta": map[string]any{
				"x.ai/sessionDetail": map[string]any{
					"sessionId":      sid,
					"kind":           "interactive",
					"cwd":            "/repo/app",
					"currentModelId": "grok-4.7",
					"title":          "Session Title",
				},
				"x.ai/session/turnCount":   4,
				"x.ai/memoryNotesWritable": true,
			},
		}
	}

	srv.on("session/new", func(msg mockACPRPCMessage) {
		mu.Lock()
		_ = json.Unmarshal(msg.Params, &gotNewParams)
		mu.Unlock()
		_ = srv.sendResult(msg.ID, sessionPayload("g-new"))
	})
	srv.on("session/load", func(msg mockACPRPCMessage) {
		mu.Lock()
		_ = json.Unmarshal(msg.Params, &gotLoadParams)
		mu.Unlock()
		_ = srv.sendResult(msg.ID, sessionPayload("g-load"))
	})
	srv.on("session/resume", func(msg mockACPRPCMessage) {
		mu.Lock()
		_ = json.Unmarshal(msg.Params, &gotResumeParams)
		mu.Unlock()
		// Live Grok session/resume omits sessionId at top level and puts it in _meta.
		res := sessionPayload("g-resume")
		delete(res, "sessionId")
		_ = srv.sendResult(msg.ID, res)
	})
	srv.on("session/close", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{"_meta": map[string]any{"x.ai/closeOutcome": "closed"}})
	})

	// Reject non-string value in session/set_config_option just like live Grok ACP!
	srv.on("session/set_config_option", func(msg mockACPRPCMessage) {
		mu.Lock()
		gotSetCfgRaw = append(json.RawMessage(nil), msg.Params...)
		mu.Unlock()
		var p struct {
			SessionID string          `json:"sessionId"`
			ConfigID  string          `json:"configId"`
			Value     json.RawMessage `json:"value"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		var strVal string
		if err := json.Unmarshal(p.Value, &strVal); err != nil {
			_ = srv.sendError(msg.ID, -32602, "data did not match any variant of untagged enum SessionConfigOptionValue")
			return
		}
		_ = srv.sendResult(msg.ID, map[string]any{
			"configOptions": []map[string]any{
				{"id": p.ConfigID, "currentValue": strVal},
			},
		})
	})

	srv.on("session/set_model", func(msg mockACPRPCMessage) {
		mu.Lock()
		_ = json.Unmarshal(msg.Params, &gotSetModelParams)
		mu.Unlock()
		_ = srv.sendResult(msg.ID, map[string]any{})
	})

	// Reject _x.ai/toggle_plan_mode or _x.ai/yolo_mode_changed if sent WITH an id!
	srv.on("_x.ai/yolo_mode_changed", func(msg mockACPRPCMessage) {
		if len(bytes.TrimSpace(msg.ID)) > 0 {
			_ = srv.sendError(msg.ID, -32601, "notification-only")
			return
		}
		yoloNotifCh <- msg
	})
	srv.on("_x.ai/toggle_plan_mode", func(msg mockACPRPCMessage) {
		if len(bytes.TrimSpace(msg.ID)) > 0 {
			_ = srv.sendError(msg.ID, -32601, "notification-only")
			return
		}
		planNotifCh <- msg
	})

	// Double-wrapped _x.ai/session/info response (live Grok wire format from #118).
	srv.on("_x.ai/session/info", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{
			"result": map[string]any{
				"sessionId":        "g-resume",
				"cwd":              "/repo/app",
				"agentName":        "grok-build-plan",
				"model":            "grok-4.7",
				"modelDisplayName": "Grok 4.7",
				"resolvedModelId":  "grok-4.7",
				"apiBackend":       "xai-proxy",
				"turns":            5,
				"turnIndex":        4,
				"heldPrompts":      map[string]any{"state": "idle", "queued": 0, "received": 5},
				"context": map[string]any{
					"used":                        42000,
					"total":                       500000,
					"usagePct":                    8.4,
					"autoCompactThresholdPercent": 85.0,
					"compactionCount":             1,
					"turnCount":                   5,
					"toolCallCount":               12,
					"messageCount":                18,
					"freeTokens":                  458000,
					"usageCategories": []map[string]any{
						{"label": "Messages", "tokens": 30000, "detail": "chat"},
					},
				},
			},
		})
	})

	srv.on("_x.ai/session/usage", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{
			"usage": map[string]any{
				"inputTokens":         12000,
				"outputTokens":        3400,
				"totalTokens":         15400,
				"cachedReadTokens":    4000,
				"cacheCreationTokens": 500,
				"reasoningTokens":     1100,
				"modelCalls":          3,
				"apiDurationMs":       2450,
				"numTurns":            2,
			},
		})
	})

	srv.on("_x.ai/session/rename", func(msg mockACPRPCMessage) {
		mu.Lock()
		_ = json.Unmarshal(msg.Params, &gotRenameParams)
		mu.Unlock()
		_ = srv.sendResult(msg.ID, map[string]any{"success": true})
	})
	srv.on("_x.ai/session/delete", func(msg mockACPRPCMessage) {
		mu.Lock()
		_ = json.Unmarshal(msg.Params, &gotDeleteParams)
		mu.Unlock()
		_ = srv.sendResult(msg.ID, map[string]any{"success": true})
	})
	srv.on("_x.ai/session/fork", func(msg mockACPRPCMessage) {
		mu.Lock()
		_ = json.Unmarshal(msg.Params, &gotForkParams)
		mu.Unlock()
		_ = srv.sendResult(msg.ID, map[string]any{"result": map[string]any{"sessionId": "g-forked"}})
	})
	srv.on("_x.ai/compact_conversation", func(msg mockACPRPCMessage) {
		mu.Lock()
		_ = json.Unmarshal(msg.Params, &gotCompactParams)
		mu.Unlock()
		_ = srv.sendResult(msg.ID, map[string]any{"compacted": true})
	})

	srv.start()
	ctx := context.Background()

	// 1. Initialize: empty clientCapabilities + fastcontrol 0.2.18 + _meta parsing.
	initRes, err := cli.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	if initRes.ProtocolVersion != 1 || initRes.AgentVersion != "1.0.92" || initRes.DefaultAuthMethodID != "cached_token" {
		t.Fatalf("unexpected initRes: %+v", initRes)
	}
	if !initRes.AgentCapabilities.LoadSession || !initRes.AgentCapabilities.PromptCapabilities.Image ||
		!initRes.AgentCapabilities.SessionCapabilities.Resume || !initRes.AgentCapabilities.SessionCapabilities.List ||
		!initRes.AgentCapabilities.SessionCapabilities.Close || !initRes.AgentCapabilities.FSNotify ||
		!initRes.AgentCapabilities.MCPCapabilities.HTTP || !initRes.AgentCapabilities.Auth {
		t.Fatalf("unexpected capabilities: %+v", initRes.AgentCapabilities)
	}
	if len(initRes.ModelState.AvailableModels) != 1 ||
		initRes.ModelState.AvailableModels[0].TotalContextTokens != 500000 ||
		len(initRes.ModelState.AvailableModels[0].ReasoningEfforts) != 3 ||
		initRes.ModelState.AvailableModels[0].DefaultReasoningEffort != "high" {
		t.Fatalf("unexpected init modelState: %+v", initRes.ModelState)
	}
	if cli.InitResult() == nil || cli.InitResult().AgentVersion != "1.0.92" {
		t.Fatalf("cached InitResult mismatch")
	}

	mu.Lock()
	clientInfo, _ := gotInitParams["clientInfo"].(map[string]any)
	clientCaps, _ := gotInitParams["clientCapabilities"].(map[string]any)
	mu.Unlock()
	if clientInfo["name"] != "fastcontrol" || clientInfo["version"] != "0.2.18" {
		t.Fatalf("unexpected clientInfo: %+v", clientInfo)
	}
	if len(clientCaps) != 0 {
		t.Fatalf("clientCapabilities MUST be empty {} so Grok runs tools in-process, got %+v", clientCaps)
	}

	// 2. Authenticate + CheckSubscription.
	authRes, err := cli.Authenticate(ctx, "")
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}
	if authRes.SubscriptionTier != "SuperGrok Plus" || authRes.AuthMode != "oauth" || !authRes.BackendBilled {
		t.Fatalf("unexpected authRes: %+v", authRes)
	}
	mu.Lock()
	if gotAuthParams["methodId"] != "cached_token" {
		t.Fatalf("expected default methodId cached_token, got %+v", gotAuthParams)
	}
	mu.Unlock()

	subRes, err := cli.CheckSubscription(ctx)
	if err != nil || !subRes.Authenticated || subRes.SubscriptionTier != "SuperGrok Plus" {
		t.Fatalf("CheckSubscription mismatch: err=%v res=%+v", err, subRes)
	}

	// 3. Double-wrapped _x.ai/models/list.
	models, err := cli.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if models.CurrentModelID != "grok-4.7" || len(models.AvailableModels) != 2 ||
		models.AvailableModels[0].TotalContextTokens != 500000 ||
		len(models.AvailableModels[0].ReasoningEfforts) != 2 ||
		models.AvailableModels[1].TotalContextTokens != 256000 {
		t.Fatalf("unexpected models: %+v", models)
	}

	// 4. _x.ai/billing.
	billing, err := cli.Billing(ctx)
	if err != nil {
		t.Fatalf("Billing failed: %v", err)
	}
	if billing.SubscriptionTier != "SuperGrok Plus" || billing.Config == nil ||
		billing.Config.CreditUsagePercent != 31.5 ||
		billing.Config.CurrentPeriod == nil || billing.Config.CurrentPeriod.Type != "USAGE_PERIOD_TYPE_WEEKLY" ||
		billing.Config.PrepaidBalance == nil || billing.Config.PrepaidBalance.Val != 42 {
		t.Fatalf("unexpected billing: %+v", billing)
	}

	// 5. _x.ai/commands/list (global & session-scoped).
	cmdsGlobal, err := cli.ListCommands(ctx, "")
	if err != nil || len(cmdsGlobal.Commands) != 2 || cmdsGlobal.Commands[1].InputHint != "on|off" || len(cmdsGlobal.Tools) != 3 {
		t.Fatalf("ListCommands global failed: err=%v res=%+v", err, cmdsGlobal)
	}
	if _, err := cli.ListCommands(ctx, "g-resume"); err != nil {
		t.Fatalf("ListCommands session failed: %v", err)
	}
	mu.Lock()
	if _, hasSID := gotCommandsParams[0]["sessionId"]; hasSID {
		t.Fatalf("global commands/list should omit sessionId: %+v", gotCommandsParams[0])
	}
	if gotCommandsParams[1]["sessionId"] != "g-resume" {
		t.Fatalf("session commands/list missing sessionId: %+v", gotCommandsParams[1])
	}
	mu.Unlock()

	// 6. Paginated session/list with cwd.
	sessions, err := cli.ListSessions(ctx, "/repo/app")
	if err != nil || len(sessions) != 2 || sessions[0].SessionID != "g-1" || sessions[1].SessionID != "g-2" {
		t.Fatalf("ListSessions failed: err=%v sessions=%+v", err, sessions)
	}
	mu.Lock()
	if len(gotListCwds) != 2 || gotListCwds[0] != "/repo/app" || gotListCwds[1] != "/repo/app" {
		t.Fatalf("unexpected session/list cwds: %+v", gotListCwds)
	}
	mu.Unlock()

	// 7. NewSession, LoadSession, ResumeSession with _meta {yoloMode, autoMode}.
	stNew, err := cli.NewSession(ctx, "/repo/app", grokSessionOptions{YoloMode: true, AutoMode: false})
	if err != nil || stNew.SessionID != "g-new" || stNew.CurrentModel() != "grok-4.7" || stNew.CurrentReasoningEffort() != "xhigh" {
		t.Fatalf("NewSession failed: err=%v st=%+v", err, stNew)
	}
	mu.Lock()
	newMeta, _ := gotNewParams["_meta"].(map[string]any)
	mu.Unlock()
	if newMeta["yoloMode"] != true || newMeta["autoMode"] != false {
		t.Fatalf("unexpected session/new _meta: %+v", newMeta)
	}

	stLoad, err := cli.LoadSession(ctx, "g-load", "/repo/app", grokSessionOptions{AutoMode: true})
	if err != nil || stLoad.SessionID != "g-load" {
		t.Fatalf("LoadSession failed: err=%v st=%+v", err, stLoad)
	}
	mu.Lock()
	loadMeta, _ := gotLoadParams["_meta"].(map[string]any)
	mu.Unlock()
	if gotLoadParams["sessionId"] != "g-load" || loadMeta["autoMode"] != true || loadMeta["yoloMode"] != false {
		t.Fatalf("unexpected session/load params: %+v", gotLoadParams)
	}

	stRes, err := cli.ResumeSession(ctx, "g-resume", "/repo/app", grokSessionOptions{})
	if err != nil || stRes.SessionID != "g-resume" || stRes.TurnCount != 4 || !stRes.MemoryNotesWritable || stRes.Detail == nil || stRes.Detail.Title != "Session Title" {
		t.Fatalf("ResumeSession failed: err=%v st=%+v", err, stRes)
	}
	mu.Lock()
	if gotResumeParams["sessionId"] != "g-resume" {
		t.Fatalf("unexpected session/resume params: %+v", gotResumeParams)
	}
	mu.Unlock()

	// 8. SetConfigOption sends PLAIN STRING value (NOT object) and SetModel sends modelId.
	opts, err := cli.SetConfigOption(ctx, "g-resume", "reasoning_effort", "high")
	if err != nil {
		t.Fatalf("SetConfigOption failed: %v", err)
	}
	if len(opts) != 1 || opts[0].ID != "reasoning_effort" || opts[0].CurrentValue != "high" {
		t.Fatalf("unexpected SetConfigOption returned options: %+v", opts)
	}
	mu.Lock()
	var cfgWire map[string]any
	_ = json.Unmarshal(gotSetCfgRaw, &cfgWire)
	mu.Unlock()
	if v, ok := cfgWire["value"].(string); !ok || v != "high" {
		t.Fatalf("session/set_config_option value MUST be plain string, got %s", string(gotSetCfgRaw))
	}

	if err := cli.SetModel(ctx, "g-resume", "grok-4.7-build-fast"); err != nil {
		t.Fatalf("SetModel failed: %v", err)
	}
	mu.Lock()
	if gotSetModelParams["modelId"] != "grok-4.7-build-fast" {
		t.Fatalf("unexpected SetModel params: %+v", gotSetModelParams)
	}
	mu.Unlock()

	// 9. Mode notifications (NO id!): _x.ai/yolo_mode_changed and _x.ai/toggle_plan_mode.
	if err := cli.NotifyYoloModeChanged("g-resume", true, false, "bypassPermissions"); err != nil {
		t.Fatalf("NotifyYoloModeChanged failed: %v", err)
	}
	select {
	case m := <-yoloNotifCh:
		if len(bytes.TrimSpace(m.ID)) != 0 {
			t.Fatalf("_x.ai/yolo_mode_changed MUST NOT include id, got %s", string(m.ID))
		}
		var p map[string]any
		_ = json.Unmarshal(m.Params, &p)
		if p["sessionId"] != "g-resume" || p["yolo_mode"] != true || p["auto_mode"] != false || p["permission_mode"] != "bypassPermissions" {
			t.Fatalf("unexpected yolo_mode_changed params: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for _x.ai/yolo_mode_changed notification")
	}

	if err := cli.NotifyTogglePlanMode("g-resume"); err != nil {
		t.Fatalf("NotifyTogglePlanMode failed: %v", err)
	}
	select {
	case m := <-planNotifCh:
		if len(bytes.TrimSpace(m.ID)) != 0 {
			t.Fatalf("_x.ai/toggle_plan_mode MUST NOT include id, got %s", string(m.ID))
		}
		var p map[string]any
		_ = json.Unmarshal(m.Params, &p)
		if p["sessionId"] != "g-resume" {
			t.Fatalf("unexpected toggle_plan_mode params: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for _x.ai/toggle_plan_mode notification")
	}

	// 10. Double-wrapped _x.ai/session/info, _x.ai/session/usage, rename, fork, compact, delete, close.
	info, err := cli.SessionInfo(ctx, "g-resume")
	if err != nil {
		t.Fatalf("SessionInfo failed: %v", err)
	}
	if info.SessionID != "g-resume" || info.Model != "grok-4.7" || info.Turns != 5 ||
		info.Context == nil || info.Context.Used != 42000 || info.Context.Total != 500000 ||
		len(info.Context.UsageCategories) != 1 || info.HeldPrompts == nil || info.HeldPrompts.State != "idle" {
		t.Fatalf("unexpected SessionInfo: %+v", info)
	}

	usage, err := cli.SessionUsage(ctx, "g-resume")
	if err != nil {
		t.Fatalf("SessionUsage failed: %v", err)
	}
	if usage.TotalTokens != 15400 || usage.ReasoningTokens != 1100 || usage.NumTurns != 2 {
		t.Fatalf("unexpected SessionUsage: %+v", usage)
	}

	if err := cli.RenameSession(ctx, "g-resume", "New Title"); err != nil {
		t.Fatalf("RenameSession failed: %v", err)
	}
	mu.Lock()
	if gotRenameParams["sessionId"] != "g-resume" || gotRenameParams["title"] != "New Title" {
		t.Fatalf("unexpected rename params: %+v", gotRenameParams)
	}
	mu.Unlock()

	forkedID, err := cli.ForkSession(ctx, "g-resume")
	if err != nil || forkedID != "g-forked" {
		t.Fatalf("ForkSession failed: err=%v id=%q", err, forkedID)
	}
	mu.Lock()
	if gotForkParams["sourceSessionId"] != "g-resume" {
		t.Fatalf("unexpected fork params: %+v", gotForkParams)
	}
	mu.Unlock()

	if err := cli.CompactConversation(ctx, "g-resume"); err != nil {
		t.Fatalf("CompactConversation failed: %v", err)
	}
	mu.Lock()
	if gotCompactParams["session_id"] != "g-resume" {
		t.Fatalf("compact_conversation MUST send session_id, got %+v", gotCompactParams)
	}
	mu.Unlock()

	if err := cli.DeleteSession(ctx, "g-forked"); err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
	mu.Lock()
	if gotDeleteParams["sessionId"] != "g-forked" {
		t.Fatalf("unexpected delete params: %+v", gotDeleteParams)
	}
	mu.Unlock()

	if err := cli.CloseSession(ctx, "g-resume"); err != nil {
		t.Fatalf("CloseSession failed: %v", err)
	}

	// Verify session/set_mode was NEVER sent anywhere.
	srv.mu.Lock()
	for _, line := range srv.rawLines {
		if strings.Contains(line, `"session/set_mode"`) {
			t.Fatalf("client MUST NEVER send session/set_mode, saw: %s", line)
		}
	}
	srv.mu.Unlock()
}

func TestGrokACPNotificationsAndFIFODrainDispatch(t *testing.T) {
	var mu sync.Mutex
	var order []string
	var updates []grokSessionUpdate
	var extUpdates []grokExtSessionUpdate
	var notifs []grokSessionNotification

	srv, cli := newMockGrokACPServer(t, grokACPClientConfig{
		OnUpdate: func(u grokSessionUpdate) {
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			updates = append(updates, u)
			order = append(order, "upd:"+u.Kind)
			mu.Unlock()
		},
		OnExtUpdate: func(u grokExtSessionUpdate) {
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			extUpdates = append(extUpdates, u)
			order = append(order, "ext:"+u.Kind)
			mu.Unlock()
		},
		OnSessionNotification: func(n grokSessionNotification) {
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			notifs = append(notifs, n)
			order = append(order, "notif:"+n.Kind)
			mu.Unlock()
		},
	})
	defer func() {
		_ = cli.Close()
		srv.close()
	}()

	// Simulate session/load emitting 12 notifications right before returning the JSON-RPC response.
	srv.on("session/load", func(msg mockACPRPCMessage) {
		_ = srv.sendNotify("session/update", map[string]any{
			"sessionId": "g-s",
			"_meta":     map[string]any{"eventId": "ev-1", "agentTimestampMs": 1700000001000},
			"update":    map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "Plan refactor"}},
		})
		_ = srv.sendNotify("session/update", map[string]any{
			"sessionId": "g-s",
			"update":    map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "Checking files"}},
		})
		_ = srv.sendNotify("session/update", map[string]any{
			"sessionId": "g-s",
			"update": map[string]any{
				"sessionUpdate": "plan",
				"entries": []map[string]any{
					{"content": "Step 1: Parse", "priority": "high", "status": "in_progress"},
					{"content": "Step 2: Test", "priority": "medium", "status": "pending"},
				},
			},
		})
		_ = srv.sendNotify("session/update", map[string]any{
			"sessionId": "g-s",
			"update": map[string]any{
				"sessionUpdate": "tool_call",
				"toolCallId":    "tc-1",
				"title":         "Edit grok_acp.go",
				"kind":          "edit",
				"status":        "in_progress",
				"_meta":         map[string]any{"x.ai/tool": "Edit"},
			},
		})
		_ = srv.sendNotify("session/update", map[string]any{
			"sessionId": "g-s",
			"update": map[string]any{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    "tc-1",
				"status":        "completed",
				"content": []map[string]any{
					{"type": "diff", "path": "grok_acp.go", "oldText": "a\nb\n", "newText": "a\nB\n"},
				},
			},
		})
		_ = srv.sendNotify("session/update", map[string]any{
			"sessionId": "g-s",
			"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Updated!"}},
		})
		_ = srv.sendNotify("session/update", map[string]any{
			"sessionId": "g-s",
			"update":    map[string]any{"sessionUpdate": "usage_update", "used": 25000, "size": 500000},
		})
		_ = srv.sendNotify("session/update", map[string]any{
			"sessionId": "g-s",
			"update": map[string]any{
				"sessionUpdate": "available_commands_update",
				"availableCommands": []map[string]any{
					{"name": "compact", "description": "Compact context"},
				},
			},
		})
		_ = srv.sendNotify("session/update", map[string]any{
			"sessionId": "g-s",
			"update":    map[string]any{"sessionUpdate": "session_info_update", "title": "Refactor Grok"},
		})
		_ = srv.sendNotify("session/update", map[string]any{
			"sessionId": "g-s",
			"update": map[string]any{
				"sessionUpdate": "config_option_update",
				"configOptions": []map[string]any{{"id": "reasoning_effort", "currentValue": "high"}},
			},
		})
		_ = srv.sendNotify("_x.ai/session/update", map[string]any{
			"sessionId": "g-s",
			"_meta":     map[string]any{"eventId": "ev-done", "agentTimestampMs": 1700000009000},
			"update": map[string]any{
				"sessionUpdate": "turn_completed",
				"stop_reason":   "end_turn",
				"elapsed_ms":    1234,
				"costUsdTicks":  50000000,
				"usage": map[string]any{
					"inputTokens":  1000,
					"outputTokens": 250,
					"totalTokens":  1250,
				},
			},
		})
		_ = srv.sendNotify("_x.ai/session_notification", map[string]any{
			"sessionId": "g-s",
			"type":      "model_changed",
			"modelId":   "grok-4.7",
			"message":   "Switched model",
		})
		_ = srv.sendResult(msg.ID, map[string]any{"sessionId": "g-s"})
	})
	srv.start()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := cli.LoadSession(ctx, "g-s", "/repo", grokSessionOptions{}); err != nil {
		t.Fatalf("LoadSession failed: %v", err)
	}

	// Enqueue a custom FIFO callback and drain: EVERY prior notification must have finished first!
	if !cli.EnqueueEvent(func() {
		mu.Lock()
		order = append(order, "custom:after_load")
		mu.Unlock()
	}) {
		t.Fatalf("EnqueueEvent returned false on live client")
	}
	if err := cli.DrainDispatch(ctx); err != nil {
		t.Fatalf("DrainDispatch failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(updates) != 10 {
		t.Fatalf("expected all 10 session/update events drained, got %d", len(updates))
	}
	if len(extUpdates) != 1 {
		t.Fatalf("expected 1 _x.ai/session/update event drained, got %d", len(extUpdates))
	}
	if len(notifs) != 1 {
		t.Fatalf("expected 1 _x.ai/session_notification event drained, got %d", len(notifs))
	}
	if order[len(order)-1] != "custom:after_load" {
		t.Fatalf("expected custom callback strictly after all replay notifications, order=%v", order)
	}

	// Verify parsed fields across variants.
	if updates[0].EventID != "ev-1" || updates[0].AgentTimestampMs != 1700000001000 || updates[0].Text != "Plan refactor" {
		t.Fatalf("unexpected update[0]: %+v", updates[0])
	}
	if len(updates[2].Plan) != 2 || updates[2].Plan[0].Content != "Step 1: Parse" || updates[2].Plan[0].Status != "in_progress" {
		t.Fatalf("unexpected plan update: %+v", updates[2])
	}
	if updates[3].ToolCall == nil || updates[3].ToolCall.Name != "Edit" || updates[3].ToolCall.Kind != "edit" {
		t.Fatalf("unexpected tool_call update: %+v", updates[3])
	}
	if updates[4].ToolCall == nil || !strings.Contains(updates[4].ToolCall.DiffText(), "-b\n+B\n") {
		t.Fatalf("unexpected tool_call_update diff: %+v", updates[4])
	}
	if updates[6].Usage == nil || updates[6].Usage.Used != 25000 || updates[6].Usage.Size != 500000 {
		t.Fatalf("unexpected usage update: %+v", updates[6])
	}
	if len(updates[7].AvailableCommands) != 1 || updates[7].AvailableCommands[0].Name != "compact" {
		t.Fatalf("unexpected commands update: %+v", updates[7])
	}
	if updates[8].Title != "Refactor Grok" {
		t.Fatalf("unexpected session_info_update: %+v", updates[8])
	}
	if len(updates[9].ConfigOptions) != 1 || updates[9].ConfigOptions[0].CurrentValue != "high" {
		t.Fatalf("unexpected config_option_update: %+v", updates[9])
	}
	if extUpdates[0].Kind != "turn_completed" || extUpdates[0].StopReason != "end_turn" ||
		extUpdates[0].ElapsedMs != 1234 || extUpdates[0].CostUsdTicks != 50000000 ||
		extUpdates[0].Usage == nil || extUpdates[0].Usage.TotalTokens != 1250 {
		t.Fatalf("unexpected ext update: %+v", extUpdates[0])
	}
	if notifs[0].Kind != "model_changed" || notifs[0].ModelID != "grok-4.7" || notifs[0].Message != "Switched model" {
		t.Fatalf("unexpected session notification: %+v", notifs[0])
	}
}

func TestGrokACPInteractiveRequestsAndCancelAllPending(t *testing.T) {
	permCh := make(chan grokPermissionRequest, 8)
	askCh := make(chan grokAskUserQuestionRequest, 8)
	planCh := make(chan grokExitPlanModeRequest, 8)
	elicitCh := make(chan grokElicitationRequest, 8)

	srv, cli := newMockGrokACPServer(t, grokACPClientConfig{
		OnPermission:      func(req grokPermissionRequest) { permCh <- req },
		OnAskUserQuestion: func(req grokAskUserQuestionRequest) { askCh <- req },
		OnExitPlanMode:    func(req grokExitPlanModeRequest) { planCh <- req },
		OnElicitation:     func(req grokElicitationRequest) { elicitCh <- req },
	})
	defer func() {
		_ = cli.Close()
		srv.close()
	}()
	srv.start()

	// 1. session/request_permission (allow_once + pop-once guard).
	permResp, err := srv.sendServerRequest(101, "session/request_permission", map[string]any{
		"sessionId": "s-int",
		"toolCall": map[string]any{
			"toolCallId": "tc-bash",
			"title":      "Run go test",
			"kind":       "execute",
		},
		"options": []map[string]any{
			{"optionId": "allow_once", "name": "Allow Once", "kind": "allow_once"},
			{"optionId": "enable_always_approve", "name": "Always Approve", "kind": "allow_always"},
			{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	permReq := <-permCh
	if permReq.RequestID != "101" || permReq.SessionID != "s-int" || len(permReq.Options) != 3 {
		t.Fatalf("unexpected permission request: %+v", permReq)
	}
	if err := cli.RespondPermission(permReq.RequestID, "allow_once", false); err != nil {
		t.Fatalf("RespondPermission failed: %v", err)
	}
	if err := cli.RespondPermission(permReq.RequestID, "allow_once", false); err == nil {
		t.Fatalf("expected pop-once error on duplicate permission reply")
	}
	msgPerm := <-permResp
	if string(bytes.TrimSpace(msgPerm.ID)) != "101" || !strings.Contains(string(msgPerm.Result), `"optionId":"allow_once"`) {
		t.Fatalf("unexpected permission reply: id=%s result=%s", string(msgPerm.ID), string(msgPerm.Result))
	}

	// 2. _x.ai/ask_user_question (accepted with 6-field AnsweredQuestion + annotations).
	askResp1, err := srv.sendServerRequest("ask-1", "_x.ai/ask_user_question", map[string]any{
		"sessionId":  "s-int",
		"toolCallId": "tc-ask-1",
		"mode":       "plan",
		"questions": []map[string]any{
			{
				"id":          "q1",
				"question":    "Choose deployment target",
				"multiSelect": true,
				"options": []map[string]any{
					{"id": "opt1", "label": "Mac", "description": "macOS arm64"},
					{"id": "opt2", "label": "Linux", "description": "Linux x64", "preview": "linux/amd64"},
				},
			},
			{
				"id":           "q2",
				"question":     "Run tests first?",
				"multi_select": false,
				"options": []map[string]any{
					{"id": "yes", "label": "Yes", "description": "Run race tests"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	askReq1 := <-askCh
	if askReq1.RequestID != "ask-1" || askReq1.ToolCallID != "tc-ask-1" || askReq1.Mode != "plan" || len(askReq1.Questions) != 2 || !askReq1.Questions[0].MultiSelect {
		t.Fatalf("unexpected ask_user_question request: %+v", askReq1)
	}
	err = cli.RespondAskUserQuestion(
		askReq1.RequestID,
		"accepted",
		[]grokAnsweredQuestion{
			{
				Question:     "Choose deployment target",
				Options:      []string{"Mac", "Linux"},
				Descriptions: []string{"macOS arm64", "Linux x64"},
				MultiSelect:  true,
				Picked:       []string{"Mac"},
				Typed:        "also verify windows",
			},
		},
		map[string]grokQuestionAnnotation{
			"q1": {Preview: "darwin/arm64", Notes: "primary"},
		},
	)
	if err != nil {
		t.Fatalf("RespondAskUserQuestion accepted failed: %v", err)
	}
	msgAsk1 := <-askResp1
	if string(bytes.TrimSpace(msgAsk1.ID)) != `"ask-1"` {
		t.Fatalf("expected raw string ID \"ask-1\", got %s", string(msgAsk1.ID))
	}
	var askOut1 struct {
		Outcome     string                            `json:"outcome"`
		Answers     []grokAnsweredQuestion            `json:"answers"`
		Annotations map[string]grokQuestionAnnotation `json:"annotations"`
	}
	if err := json.Unmarshal(msgAsk1.Result, &askOut1); err != nil {
		t.Fatalf("unmarshal ask accepted reply failed: %v (%s)", err, string(msgAsk1.Result))
	}
	if askOut1.Outcome != "accepted" || len(askOut1.Answers) != 1 ||
		askOut1.Answers[0].Question != "Choose deployment target" ||
		len(askOut1.Answers[0].Picked) != 1 || askOut1.Answers[0].Picked[0] != "Mac" ||
		askOut1.Answers[0].Typed != "also verify windows" ||
		askOut1.Annotations["q1"].Notes != "primary" {
		t.Fatalf("unexpected ask accepted payload: %+v", askOut1)
	}

	// 2b. _x.ai/ask_user_question chat_about_this and skip_interview variants.
	askResp2, err := srv.sendServerRequest(202, "_x.ai/ask_user_question", map[string]any{
		"sessionId": "s-int", "toolCallId": "tc-ask-2", "mode": "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	askReq2 := <-askCh
	if err := cli.RespondAskUserQuestion(askReq2.RequestID, "chat_about_this", []grokAnsweredQuestion{{Question: "Q1"}}, nil); err != nil {
		t.Fatalf("RespondAskUserQuestion chat_about_this failed: %v", err)
	}
	msgAsk2 := <-askResp2
	if !strings.Contains(string(msgAsk2.Result), `"outcome":"chat_about_this"`) || !strings.Contains(string(msgAsk2.Result), `"partial_answers":[`) {
		t.Fatalf("unexpected chat_about_this reply: %s", string(msgAsk2.Result))
	}

	askResp3, err := srv.sendServerRequest(203, "_x.ai/ask_user_question", map[string]any{
		"sessionId": "s-int", "toolCallId": "tc-ask-3", "mode": "plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	askReq3 := <-askCh
	if err := cli.RespondAskUserQuestion(askReq3.RequestID, "skip_interview", nil, nil); err != nil {
		t.Fatalf("RespondAskUserQuestion skip_interview failed: %v", err)
	}
	msgAsk3 := <-askResp3
	if string(bytes.TrimSpace(msgAsk3.Result)) != `{"outcome":"skip_interview"}` {
		t.Fatalf("unexpected skip_interview reply: %s", string(msgAsk3.Result))
	}

	// 3. _x.ai/exit_plan_mode ({sessionId, toolCallId, planContent} -> {outcome, feedback}).
	planResp1, err := srv.sendServerRequest(301, "_x.ai/exit_plan_mode", map[string]any{
		"sessionId":   "s-int",
		"toolCallId":  "tc-plan-1",
		"planContent": "## Plan\n1. Build client\n2. Run tests",
	})
	if err != nil {
		t.Fatal(err)
	}
	planReq1 := <-planCh
	if planReq1.RequestID != "301" || planReq1.SessionID != "s-int" || planReq1.ToolCallID != "tc-plan-1" || !strings.Contains(planReq1.PlanContent, "Build client") {
		t.Fatalf("unexpected exit_plan_mode req: %+v", planReq1)
	}
	if err := cli.RespondExitPlanMode(planReq1.RequestID, "approved", "Proceed with step 1"); err != nil {
		t.Fatalf("RespondExitPlanMode failed: %v", err)
	}
	msgPlan1 := <-planResp1
	var planOut1 struct {
		Outcome  string  `json:"outcome"`
		Feedback *string `json:"feedback"`
	}
	if err := json.Unmarshal(msgPlan1.Result, &planOut1); err != nil || planOut1.Outcome != "approved" || planOut1.Feedback == nil || *planOut1.Feedback != "Proceed with step 1" {
		t.Fatalf("unexpected exit_plan_mode reply: %s", string(msgPlan1.Result))
	}

	// 4. _x.ai/mcp/elicit fallback.
	elicitResp1, err := srv.sendServerRequest(401, "_x.ai/mcp/elicit", map[string]any{
		"sessionId":     "s-int",
		"toolCallId":    "tc-elicit-1",
		"serverName":    "github",
		"elicitationId": "el-1",
		"message":       "Provide API key",
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"token": map[string]any{"type": "string", "title": "Token", "format": "password"},
			},
			"required": []string{"token"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	elicitReq1 := <-elicitCh
	if elicitReq1.ServerName != "github" || elicitReq1.ElicitationID != "el-1" || len(elicitReq1.Fields) != 1 || !elicitReq1.Fields[0].Secret {
		t.Fatalf("unexpected mcp elicit request: %+v", elicitReq1)
	}
	if err := cli.RespondElicitation(elicitReq1.RequestID, "accept", map[string]any{"token": "abc"}); err != nil {
		t.Fatalf("RespondElicitation failed: %v", err)
	}
	msgElicit1 := <-elicitResp1
	if !strings.Contains(string(msgElicit1.Result), `"action":"accept"`) || !strings.Contains(string(msgElicit1.Result), `"token":"abc"`) {
		t.Fatalf("unexpected mcp elicit reply: %s", string(msgElicit1.Result))
	}

	// 5. CancelAllPending(sessionID) auto-cancels all 4 pending types for s-int while leaving s-other intact!
	pCancel, _ := srv.sendServerRequest(501, "session/request_permission", map[string]any{"sessionId": "s-int", "toolCall": map[string]any{"toolCallId": "p1"}})
	aCancel, _ := srv.sendServerRequest(502, "_x.ai/ask_user_question", map[string]any{"sessionId": "s-int", "toolCallId": "a1"})
	eCancel, _ := srv.sendServerRequest(503, "_x.ai/exit_plan_mode", map[string]any{"sessionId": "s-int", "toolCallId": "e1", "planContent": "p"})
	mCancel, _ := srv.sendServerRequest(504, "_x.ai/mcp/elicit", map[string]any{"sessionId": "s-int", "message": "m"})
	otherResp, _ := srv.sendServerRequest(505, "_x.ai/exit_plan_mode", map[string]any{"sessionId": "s-other", "toolCallId": "o1", "planContent": "other"})

	<-permCh
	<-askCh
	<-planCh
	<-elicitCh
	<-planCh

	if cli.PendingRequestCount("s-int") != 4 || cli.PendingRequestCount("s-other") != 1 || cli.PendingRequestCount("") != 5 {
		t.Fatalf("unexpected pending counts: s-int=%d s-other=%d total=%d", cli.PendingRequestCount("s-int"), cli.PendingRequestCount("s-other"), cli.PendingRequestCount(""))
	}

	if err := cli.Cancel(context.Background(), "s-int"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}
	if !strings.Contains(string((<-pCancel).Result), `"outcome":"cancelled"`) {
		t.Fatalf("expected cancelled permission")
	}
	if string(bytes.TrimSpace((<-aCancel).Result)) != `{"outcome":"cancelled"}` {
		t.Fatalf("expected cancelled ask_user_question")
	}
	if string(bytes.TrimSpace((<-eCancel).Result)) != `{"outcome":"cancelled","feedback":null}` {
		t.Fatalf("expected cancelled exit_plan_mode with null feedback")
	}
	if string(bytes.TrimSpace((<-mCancel).Result)) != `{"action":"cancel"}` {
		t.Fatalf("expected cancelled mcp elicit")
	}
	if cli.PendingRequestCount("s-int") != 0 || cli.PendingRequestCount("s-other") != 1 {
		t.Fatalf("expected only s-other pending after s-int Cancel")
	}

	// Unknown server request receives JSON-RPC -32601 without wedging.
	unkResp, err := srv.sendServerRequest(506, "_x.ai/unknown_ext", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-unkResp:
		if msg.Error == nil || msg.Error.Code != -32601 {
			t.Fatalf("expected -32601 for unknown server request, got %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for -32601")
	}

	// Close() auto-cancels remaining pending request on s-other.
	closeDone := make(chan struct{})
	go func() {
		_ = cli.Close()
		close(closeDone)
	}()
	select {
	case msg := <-otherResp:
		if string(bytes.TrimSpace(msg.Result)) != `{"outcome":"cancelled","feedback":null}` {
			t.Fatalf("expected auto-cancel on Close, got %s", string(msg.Result))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for auto-cancel on Close")
	}
	<-closeDone
}

func TestGrokACPConcurrentSteeringCancelAndCleanShutdown(t *testing.T) {
	beforeGoroutines := runtime.NumGoroutine()

	closedCh := make(chan error, 1)
	srv, cli := newMockGrokACPServer(t, grokACPClientConfig{
		CallTimeout: 3 * time.Second,
		OnClose: func(err error) {
			closedCh <- err
		},
	})

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
		switch promptCount.Add(1) {
		case 1:
			gotBlocks = p.Prompt
			close(firstStarted)
			<-steerReceived
			<-cancelReceived
			_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "cancelled"})
		case 2:
			close(steerReceived)
			_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "end_turn"})
		default:
			// Hold third prompt until server EOF to test unblocking on abrupt transport close.
		}
	})
	srv.on("session/cancel", func(msg mockACPRPCMessage) {
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		cancelReceived <- p.SessionID
	})
	srv.on("session/set_config_option", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{})
	})
	srv.start()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	blocks := []acpContentBlock{
		acpTextBlock("Check image"),
		acpImageBlock(imgBytes, "image/png"),
	}

	turn1Done := make(chan *acpPromptResult, 1)
	turn1Err := make(chan error, 1)
	go func() {
		res, err := cli.Prompt(ctx, "g-race", blocks)
		if err != nil {
			turn1Err <- err
			return
		}
		turn1Done <- res
	}()

	<-firstStarted

	// Concurrent config updates + steering prompt under -race.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cli.SetConfigOption(ctx, "g-race", "reasoning_effort", "medium"); err != nil {
				t.Errorf("concurrent SetConfigOption failed: %v", err)
			}
		}()
	}

	steerRes, err := cli.Prompt(ctx, "g-race", []acpContentBlock{acpTextBlock("Steer now")})
	if err != nil || steerRes.StopReason != "end_turn" {
		t.Fatalf("steering Prompt failed: err=%v res=%+v", err, steerRes)
	}
	wg.Wait()

	if err := cli.Cancel(ctx, "g-race"); err != nil {
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
		t.Fatalf("timed out waiting for turn 1")
	}

	if len(gotBlocks) != 2 || gotBlocks[0].Text != "Check image" || gotBlocks[1].Data != base64.StdEncoding.EncodeToString(imgBytes) {
		t.Fatalf("unexpected prompt blocks: %+v", gotBlocks)
	}

	// Start an in-flight Prompt and simulate abrupt server EOF.
	eofErr := make(chan error, 1)
	go func() {
		_, err := cli.Prompt(ctx, "g-race", []acpContentBlock{acpTextBlock("will-eof")})
		eofErr <- err
	}()
	time.Sleep(25 * time.Millisecond)

	srv.close()

	select {
	case err := <-eofErr:
		if err == nil {
			t.Fatalf("expected Prompt to fail on abrupt EOF")
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
	// DrainDispatch on already-closed client returns cleanly without blocking.
	if err := cli.DrainDispatch(ctx); err != nil {
		t.Fatalf("DrainDispatch on closed client failed: %v", err)
	}

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

func TestGrokACPCallbackNonBlockingAndRPCError(t *testing.T) {
	var cli *grokACPClient
	setDone := make(chan error, 1)
	srv, cli := newMockGrokACPServer(t, grokACPClientConfig{
		CallTimeout: 3 * time.Second,
		OnUpdate: func(u grokSessionUpdate) {
			if u.Kind == "session_info_update" {
				_, err := cli.SetConfigOption(context.Background(), u.SessionID, "reasoning_effort", "low")
				setDone <- err
			}
		},
	})
	defer func() {
		_ = cli.Close()
		srv.close()
	}()
	srv.on("session/set_config_option", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{})
	})
	srv.on("_x.ai/session/info", func(msg mockACPRPCMessage) {
		_ = srv.sendError(msg.ID, -32002, "session not found")
	})
	srv.start()

	if err := srv.sendNotify("session/update", map[string]any{
		"sessionId": "g-cb",
		"update":    map[string]any{"sessionUpdate": "session_info_update", "title": "Trigger"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-setDone:
		if err != nil {
			t.Fatalf("SetConfigOption inside OnUpdate failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out calling SetConfigOption inside OnUpdate (reader deadlock)")
	}

	_, err := cli.SessionInfo(context.Background(), "missing")
	var rpcErr *acpRPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32002 || rpcErr.Message != "session not found" {
		t.Fatalf("expected *acpRPCError(-32002), got %T %v", err, err)
	}
}

func TestGrokACPLiveReadOnlyInitializeAndExtensions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live grok agent stdio integration test in short mode")
	}
	bin := grokBinary()
	if bin == "" {
		t.Skip("grok binary not installed")
	}
	if !grokSupportsACP(bin) {
		t.Skipf("grok binary %s does not support agent stdio", bin)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, initRes, err := dialGrokACPClient(ctx, grokACPClientConfig{
		Binary:      bin,
		CallTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("live dialGrokACPClient failed: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("live client.Close failed: %v", err)
		}
	}()

	if initRes.ProtocolVersion != 1 {
		t.Fatalf("unexpected live protocolVersion: %d", initRes.ProtocolVersion)
	}
	if !initRes.AgentCapabilities.LoadSession || !initRes.AgentCapabilities.SessionCapabilities.List {
		t.Fatalf("unexpected live capabilities: %+v", initRes.AgentCapabilities)
	}

	models, err := client.ListModels(ctx)
	if err != nil {
		t.Fatalf("live _x.ai/models/list failed: %v (stderr: %s)", err, client.Stderr())
	}
	if len(models.AvailableModels) == 0 {
		t.Fatalf("live _x.ai/models/list returned no models: %+v", models)
	}

	cmds, err := client.ListCommands(ctx, "")
	if err != nil {
		t.Fatalf("live _x.ai/commands/list failed: %v (stderr: %s)", err, client.Stderr())
	}
	if len(cmds.Commands) == 0 {
		t.Fatalf("live _x.ai/commands/list returned no commands")
	}

	sessions, err := client.ListSessions(ctx, "")
	if err != nil {
		t.Fatalf("live session/list failed: %v (stderr: %s)", err, client.Stderr())
	}
	for _, s := range sessions {
		if s.SessionID == "" {
			t.Fatalf("live session/list returned empty sessionId: %+v", s)
		}
	}
}
