package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type grokTestHarness struct {
	t         *testing.T
	home      string
	grokHome  string
	statePath string
	hub       *Hub
	mgr       *GrokManager
	client    *Client
	evClient  *Client

	mu    sync.Mutex
	clock time.Time
}

func newGrokTestHarness(t *testing.T, signedIn bool) *grokTestHarness {
	t.Helper()
	home := t.TempDir()
	grokHome := filepath.Join(home, ".grok")
	if err := os.MkdirAll(grokHome, 0o700); err != nil {
		t.Fatalf("mkdir grokHome: %v", err)
	}
	if signedIn {
		if err := os.WriteFile(filepath.Join(grokHome, "auth.json"), []byte(`{"auth":"present"}`), 0o600); err != nil {
			t.Fatalf("write auth.json: %v", err)
		}
	}
	statePath := filepath.Join(home, ".fastcontrol", "grok-state.json")
	state, err := newGrokState(statePath)
	if err != nil {
		t.Fatalf("newGrokState: %v", err)
	}

	ipc := NewIPCClient(filepath.Join(home, "unused.sock"))
	hub := NewHub(ipc, "")

	now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	h := &grokTestHarness{
		t:         t,
		home:      home,
		grokHome:  grokHome,
		statePath: statePath,
		hub:       hub,
		clock:     now,
	}

	stopCtx, stopCancel := context.WithCancel(context.Background())
	mgr := &GrokManager{
		hub:                      hub,
		bin:                      filepath.Join(home, "grok"),
		grokHome:                 grokHome,
		customGrokHome:           true,
		state:                    state,
		allowFullAccess:          true,
		prewarmOnStart:           false,
		supportsACP:              func(string) bool { return true },
		checkAuth:                grokHasAuth,
		now:                      func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.clock },
		recencyBroadcastInterval: 15 * time.Second,
		idleSessionTimeout:       12 * time.Minute,
		daemonIdleTimeout:        30 * time.Minute,
		keepaliveInterval:        45 * time.Second,
		keepaliveTimeout:         10 * time.Second,
		openFastWait:             150 * time.Millisecond,
		reconnectBackoff:         []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 75 * time.Millisecond},
		modelsCacheTTL:           5 * time.Minute,
		pollInterval:             20 * time.Millisecond,
		workPollInterval:         20 * time.Millisecond,
		records:                  map[string]grokRecord{},
		runs:                     map[string]*grokRun{},
		daemons:                  map[grokDaemonKey]*grokDaemon{},
		daemonDial:               map[grokDaemonKey]*grokDaemonDial{},
		watch:                    map[*Client]map[string]int{},
		watchGen:                 map[*Client]map[string]uint64{},
		workWatch:                map[*Client]map[string]grokWorkWatch{},
		watchedStamp:             map[string]grokWatchStamp{},
		gates:                    map[string]*grokGate{},
		reconnecting:             map[string]bool{},
		orgState:                 emptyOrganization(),
		stopCtx:                  stopCtx,
		stopCancel:               stopCancel,
	}
	hub.grok = mgr
	h.mgr = mgr

	c := &Client{hub: hub, out: make(chan []byte, 256), done: make(chan struct{})}
	ev := &Client{hub: hub, out: make(chan []byte, 256), done: make(chan struct{}), eventsOnly: true, events: true}
	hub.addClient(c)
	hub.addClient(ev)
	h.client = c
	h.evClient = ev

	t.Cleanup(func() {
		mgr.shutdown()
	})
	return h
}

func (h *grokTestHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.clock = h.clock.Add(d)
	h.mu.Unlock()
}

func (h *grokTestHarness) send(c *Client, payload map[string]any) {
	h.t.Helper()
	if _, ok := payload["provider"]; !ok {
		payload["provider"] = "grok"
	}
	h.hub.handle(c, mustJSON(payload))
}

func (h *grokTestHarness) writeSession(
	id, cwd, title, model, effort string,
	updatedAt time.Time,
	updates []map[string]any,
	extraSummary map[string]any,
) string {
	h.t.Helper()
	sum := sha256.Sum256([]byte(cwd))
	encoded := fmt.Sprintf("cwd-%x", sum[:8])
	sessDir := filepath.Join(h.grokHome, "sessions", encoded, id)
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		h.t.Fatalf("mkdir session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.grokHome, "sessions", encoded, ".cwd"), []byte(cwd+"\n"), 0o600); err != nil {
		h.t.Fatalf("write .cwd: %v", err)
	}
	summary := map[string]any{
		"info":              map[string]any{"id": id, "cwd": cwd},
		"generated_title":   title,
		"created_at":        updatedAt.Add(-2 * time.Minute).UTC().Format(time.RFC3339),
		"updated_at":        updatedAt.UTC().Format(time.RFC3339),
		"last_active_at":    updatedAt.UTC().Format(time.RFC3339),
		"current_model_id":  model,
		"reasoning_effort":  effort,
		"num_messages":      len(updates),
		"num_chat_messages": len(updates),
	}
	for k, v := range extraSummary {
		summary[k] = v
	}
	if err := os.WriteFile(filepath.Join(sessDir, "summary.json"), mustJSON(summary), 0o600); err != nil {
		h.t.Fatalf("write summary.json: %v", err)
	}
	if updates != nil {
		var buf bytes.Buffer
		for _, line := range updates {
			buf.Write(mustJSON(line))
			buf.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(sessDir, "updates.jsonl"), buf.Bytes(), 0o600); err != nil {
			h.t.Fatalf("write updates.jsonl: %v", err)
		}
	}
	return sessDir
}

func mockGrokInitResultPayload() map[string]any {
	return map[string]any{
		"protocolVersion": 1,
		"agentInfo":       map[string]any{"name": "grok", "version": "1.0.92"},
		"agentCapabilities": map[string]any{
			"loadSession": true,
			"auth":        map[string]any{},
			"promptCapabilities": map[string]any{
				"image":           true,
				"embeddedContext": true,
			},
			"sessionCapabilities": map[string]any{
				"list":   map[string]any{},
				"resume": map[string]any{},
				"close":  map[string]any{},
			},
		},
		"authMethods": []map[string]any{
			{"id": "cached_token", "name": "Cached Token"},
		},
		"_meta": map[string]any{
			"agentVersion":        "1.0.92",
			"defaultAuthMethodId": "cached_token",
			"grokShell":           true,
			"modelState": map[string]any{
				"currentModelId": "grok-4.7",
				"availableModels": []map[string]any{
					{
						"modelId":     "grok-4.7",
						"name":        "Grok 4.7",
						"description": "Flagship Grok model",
						"_meta": map[string]any{
							"totalContextTokens":      256000,
							"supportsReasoningEffort": true,
							"reasoningEffort":         "high",
							"reasoningEfforts": []map[string]any{
								{"value": "minimal", "label": "Minimal"},
								{"value": "low", "label": "Low"},
								{"value": "medium", "label": "Medium"},
								{"value": "high", "label": "High", "default": true},
								{"value": "xhigh", "label": "Extra High"},
							},
						},
					},
					{
						"modelId":     "grok-4.7-build-fast",
						"name":        "Grok 4.7 Fast",
						"description": "Fast build model",
						"_meta": map[string]any{
							"totalContextTokens":      256000,
							"supportsReasoningEffort": true,
							"reasoningEffort":         "medium",
							"reasoningEfforts": []map[string]any{
								{"value": "low", "label": "Low"},
								{"value": "medium", "label": "Medium", "default": true},
								{"value": "high", "label": "High"},
								{"value": "xhigh", "label": "Extra High"},
							},
						},
					},
				},
			},
		},
	}
}

func mockGrokSessionStatePayload(sessionID, model, effort string) map[string]any {
	return map[string]any{
		"sessionId": sessionID,
		"models": map[string]any{
			"currentModelId": model,
		},
		"configOptions": []map[string]any{
			{
				"id":           "model",
				"name":         "Model",
				"category":     "model",
				"type":         "select",
				"currentValue": model,
				"options": []map[string]any{
					{"value": "grok-4.7", "name": "Grok 4.7"},
					{"value": "grok-4.7-build-fast", "name": "Grok 4.7 Fast"},
				},
			},
			{
				"id":           "reasoning_effort",
				"name":         "Reasoning Effort",
				"category":     "thought_level",
				"type":         "select",
				"currentValue": effort,
				"options": []map[string]any{
					{"value": "minimal", "name": "Minimal"},
					{"value": "low", "name": "Low"},
					{"value": "medium", "name": "Medium"},
					{"value": "high", "name": "High"},
					{"value": "xhigh", "name": "Extra High"},
				},
			},
		},
	}
}

func setupStandardMockGrokServer(t *testing.T, srv *mockGrokACPServer) {
	t.Helper()
	srv.on("initialize", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, mockGrokInitResultPayload())
	})
	srv.on("authenticate", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{
			"_meta": map[string]any{
				"auth_mode":         "oauth",
				"subscription_tier": "SuperGrok Plus",
			},
		})
	})
	srv.on("session/list", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{"sessions": []any{}})
	})
	srv.on("session/close", func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{"_meta": map[string]any{"x.ai/closeOutcome": "closed"}})
	})
	srv.on(grokExtModelsList, func(msg mockACPRPCMessage) {
		_ = srv.sendResult(msg.ID, map[string]any{
			"result": mockGrokInitResultPayload()["_meta"].(map[string]any)["modelState"],
		})
	})
}

func TestGrokManagerAvailabilityRoutingAndUnsignedDegradation(t *testing.T) {
	h := newGrokTestHarness(t, false)
	cwd := t.TempDir()

	// 1. Availability checks across binary / ACP / local sessions.
	if !h.mgr.available() {
		t.Fatal("expected available() == true when ACP binary is present")
	}
	h.mgr.bin = ""
	if h.mgr.available() {
		t.Fatal("expected available() == false when no binary and no sessions exist")
	}
	h.writeSession("sess-local-1", cwd, "Unsigned Local Chat", "grok-4.7", "high", h.clock, []map[string]any{
		{
			"timestamp": h.clock.Format(time.RFC3339),
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "sess-local-1",
				"update": map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content":       map[string]any{"type": "text", "text": "Inspect local logs"},
				},
			},
		},
		{
			"timestamp": h.clock.Add(time.Second).Format(time.RFC3339),
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "sess-local-1",
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": "Logs look clean."},
				},
			},
		},
	}, nil)
	if !h.mgr.available() {
		t.Fatal("expected available() == true when local sessions exist even without binary")
	}
	h.mgr.bin = filepath.Join(h.home, "grok")

	// 2. Unsigned host: prewarm must NEVER spawn a daemon.
	var dialCount atomic.Int32
	h.mgr.dialACP = func(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
		dialCount.Add(1)
		return nil, nil, errors.New("should not dial when unsigned")
	}
	h.mgr.prewarmOnStart = true
	h.mgr.maybePrewarm()
	time.Sleep(30 * time.Millisecond)
	if dialCount.Load() != 0 {
		t.Fatalf("unsigned maybePrewarm spawned %d daemons, want 0", dialCount.Load())
	}

	// 3. Unsigned host can still browse local sessions read-only via op: "open".
	h.mgr.refresh()
	drainFrames(h.client)
	h.send(h.client, map[string]any{"op": "open", "id": "sess-local-1", "nonce": "open-unsigned"})
	openFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "provider") == "grok" && str(m, "id") == "sess-local-1"
	})
	meta := mapValue(openFrame["meta"])
	if str(meta, "mode") != "tail" || meta["connecting"] == true {
		t.Fatalf("unsigned open should emit tail mode without connecting, got %+v", meta)
	}
	ackOpen := waitAck(t, h.client, "open-unsigned")
	if ackOpen["ok"] != true {
		t.Fatalf("unsigned open ack failed: %+v", ackOpen)
	}
	if dialCount.Load() != 0 {
		t.Fatalf("unsigned open should not dial ACP, got %d dials", dialCount.Load())
	}

	// 4. Unsigned host rejects live control ops with clear Title Case "Sign In With grok login".
	h.send(h.client, map[string]any{"op": "new", "cwd": cwd, "text": "hello", "nonce": "new-unsigned"})
	ackNew := waitAck(t, h.client, "new-unsigned")
	if ackNew["ok"] != false || str(ackNew, "msg") != "Sign In With grok login" {
		t.Fatalf("unexpected unsigned new ack: %+v", ackNew)
	}
	h.send(h.client, map[string]any{"op": "wake", "id": "sess-local-1", "nonce": "wake-unsigned"})
	ackWake := waitAck(t, h.client, "wake-unsigned")
	if ackWake["ok"] != false || str(ackWake, "msg") != "Sign In With grok login" {
		t.Fatalf("unexpected unsigned wake ack: %+v", ackWake)
	}

	// 5. Unsigned models without cache emits error; with models_cache.json returns cache-only gracefully.
	h.send(h.client, map[string]any{"op": "models", "nonce": "models-empty"})
	modelsErrFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "models" && str(m, "provider") == "grok"
	})
	if str(modelsErrFrame, "error") != "Sign In With grok login" {
		t.Fatalf("expected Sign In With grok login models error, got %+v", modelsErrFrame)
	}

	cacheJSON := `{"models":[{"id":"grok-4.7","display_name":"Grok 4.7","supported_reasoning_efforts":["low","high"],"default_reasoning_effort":"high"}]}`
	if err := os.WriteFile(filepath.Join(h.grokHome, "models_cache.json"), []byte(cacheJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	h.send(h.client, map[string]any{"op": "models", "nonce": "models-cached"})
	modelsCachedFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "models" && str(m, "provider") == "grok"
	})
	if str(modelsCachedFrame, "error") != "" {
		t.Fatalf("expected graceful cache-only models without error, got %+v", modelsCachedFrame)
	}
	if len(arrayValue(modelsCachedFrame["models"])) != 1 {
		t.Fatalf("expected 1 cached model, got %+v", modelsCachedFrame["models"])
	}

	// 6. Hub routing: op:"events" with provider:"grok" toggles Client.events; hub.grok == nil fails cleanly.
	h.send(h.client, map[string]any{"op": "events", "provider": "grok", "on": true, "nonce": "ev-on"})
	ackEv := waitAck(t, h.client, "ev-on")
	if ackEv["ok"] != true {
		t.Fatalf("events op with provider grok failed: %+v", ackEv)
	}
	h.hub.mu.Lock()
	evEnabled := h.client.events
	h.hub.mu.Unlock()
	if !evEnabled {
		t.Fatal("expected client.events == true after events op")
	}

	h.hub.removeClient(h.client)
	h.mgr.mu.Lock()
	_, stillWatched := h.mgr.watch[h.client]
	h.mgr.mu.Unlock()
	if stillWatched {
		t.Fatal("removeClient should remove client from GrokManager.watch")
	}

	c2 := &Client{hub: h.hub, out: make(chan []byte, 32), done: make(chan struct{})}
	h.hub.addClient(c2)
	saved := h.hub.grok
	h.hub.grok = nil
	h.hub.handle(c2, mustJSON(map[string]any{"op": "threads", "provider": "grok", "nonce": "nil-grok"}))
	ackNil := waitAck(t, c2, "nil-grok")
	h.hub.grok = saved
	if ackNil["ok"] != false || str(ackNil, "msg") != "Grok Is Unavailable" {
		t.Fatalf("unexpected nil hub.grok ack: %+v", ackNil)
	}

	// 7. Signed auth.json present but ACP authenticate fails -> normalized to "Sign In With grok login".
	if err := os.WriteFile(filepath.Join(h.grokHome, "auth.json"), []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mgr.dialACP = func(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
		srv, cli := newMockGrokACPServer(t, cfg)
		srv.on("initialize", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockGrokInitResultPayload())
		})
		srv.on("authenticate", func(msg mockACPRPCMessage) {
			_ = srv.sendError(msg.ID, -32000, "cached token expired, run grok login")
		})
		srv.start()
		t.Cleanup(srv.close)
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}
	h.send(c2, map[string]any{"op": "wake", "id": "sess-local-1", "nonce": "wake-bad-auth"})
	ackBadAuth := waitAck(t, c2, "wake-bad-auth")
	if ackBadAuth["ok"] != false || str(ackBadAuth, "msg") != "Sign In With grok login" {
		t.Fatalf("expected normalized auth error, got %+v", ackBadAuth)
	}
}

func TestGrokManagerListProjectsGroupsOrganizationPinArchiveRenameAndThrottle(t *testing.T) {
	h := newGrokTestHarness(t, true)
	projA := filepath.Join(h.home, "proj-alpha")
	projB := filepath.Join(h.home, "proj-beta")
	if err := os.MkdirAll(projA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projB, 0o700); err != nil {
		t.Fatal(err)
	}

	h.writeSession("sess-a", projA, "Alpha Task", "grok-4.7", "high", h.clock.Add(-10*time.Second), nil, nil)
	h.writeSession("sess-b", projB, "Beta Task", "grok-4.7-build-fast", "medium", h.clock.Add(-20*time.Second), nil, nil)

	var renameCalled atomic.Int32
	var renamedTitle atomic.Value
	h.mgr.dialACP = func(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
		srv, cli := newMockGrokACPServer(t, cfg)
		setupStandardMockGrokServer(t, srv)
		srv.on("session/resume", func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			_ = srv.sendResult(msg.ID, mockGrokSessionStatePayload(str(p, "sessionId"), "grok-4.7", "high"))
		})
		srv.on(grokExtSessionRename, func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			renameCalled.Add(1)
			renamedTitle.Store(str(p, "title"))
			_ = srv.sendResult(msg.ID, map[string]any{"ok": true})
		})
		srv.start()
		t.Cleanup(srv.close)
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	// Initial refresh broadcasts thread list with project groups.
	h.mgr.refresh()
	listFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "threads" && str(m, "provider") == "grok"
	})
	groups := arrayValue(listFrame["groups"])
	if len(groups) != 2 {
		t.Fatalf("expected 2 workspace groups, got %d", len(groups))
	}
	groupID := str(mapValue(groups[0]), "id")

	// Projects & single project ops.
	h.send(h.client, map[string]any{"op": "projects"})
	projFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "projects" && str(m, "provider") == "grok"
	})
	if len(arrayValue(projFrame["projects"])) != 2 {
		t.Fatalf("expected 2 projects, got %+v", projFrame["projects"])
	}

	h.send(h.client, map[string]any{"op": "project", "cwd": projA})
	oneProjFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "project" && str(m, "provider") == "grok" && str(m, "cwd") == projA
	})
	if len(arrayValue(oneProjFrame["threads"])) != 1 {
		t.Fatalf("expected 1 thread in projA, got %+v", oneProjFrame["threads"])
	}

	// Group op.
	h.send(h.client, map[string]any{"op": "group", "id": groupID, "nonce": "grp-1"})
	ackGrp := waitAck(t, h.client, "grp-1")
	if ackGrp["ok"] != true || len(arrayValue(ackGrp["threads"])) != 1 {
		t.Fatalf("unexpected group ack: %+v", ackGrp)
	}

	// Pure-recency 15s broadcast throttle: bump mtime/updated_at only within 5s -> suppressed.
	drainFrames(h.client)
	h.advance(5 * time.Second)
	h.writeSession("sess-a", projA, "Alpha Task", "grok-4.7", "high", h.clock, nil, nil)
	h.mgr.refresh()
	if got := drainFrames(h.client); len(got) != 0 {
		t.Fatalf("expected pure-recency broadcast to be throttled within 15s, got %d frames", len(got))
	}
	h.advance(11 * time.Second)
	h.mgr.refresh()
	waitFrame(t, h.client, 2*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "threads" && str(m, "provider") == "grok"
	})

	// Pin / unpin broadcasts immediately.
	h.send(h.client, map[string]any{"op": "pin", "id": "sess-b", "nonce": "pin-b"})
	if ack := waitAck(t, h.client, "pin-b"); ack["ok"] != true {
		t.Fatalf("pin failed: %+v", ack)
	}
	if !h.mgr.state.IsPinned("sess-b") {
		t.Fatal("expected sess-b pinned")
	}
	h.send(h.client, map[string]any{"op": "unpin", "id": "sess-b", "nonce": "unpin-b"})
	if ack := waitAck(t, h.client, "unpin-b"); ack["ok"] != true {
		t.Fatalf("unpin failed: %+v", ack)
	}

	// Attach sess-a, then rename -> updates grokState AND calls _x.ai/session/rename.
	if _, err := h.mgr.ensureRun(context.Background(), "sess-a"); err != nil {
		t.Fatalf("ensureRun sess-a: %v", err)
	}
	h.send(h.client, map[string]any{"op": "rename", "id": "sess-a", "text": "   ", "nonce": "ren-bad"})
	if ack := waitAck(t, h.client, "ren-bad"); ack["ok"] != false {
		t.Fatalf("expected empty title to fail validation, got %+v", ack)
	}
	h.send(h.client, map[string]any{"op": "rename", "id": "sess-a", "text": "Renamed Alpha", "nonce": "ren-ok"})
	ackRen := waitAck(t, h.client, "ren-ok")
	if ackRen["ok"] != true || str(ackRen, "title") != "Renamed Alpha" {
		t.Fatalf("rename failed: %+v", ackRen)
	}
	if h.mgr.state.Title("sess-a") != "Renamed Alpha" {
		t.Fatalf("expected grokState title 'Renamed Alpha', got %q", h.mgr.state.Title("sess-a"))
	}
	if renameCalled.Load() != 1 || renamedTitle.Load() != "Renamed Alpha" {
		t.Fatalf("expected _x.ai/session/rename call with Renamed Alpha, calls=%d title=%v", renameCalled.Load(), renamedTitle.Load())
	}

	// Archive rejects nonexistent IDs and blocks while running.
	h.send(h.client, map[string]any{"op": "archive", "id": "no-such-session", "nonce": "arch-missing"})
	if ack := waitAck(t, h.client, "arch-missing"); ack["ok"] != false {
		t.Fatalf("expected archive on missing session to fail, got %+v", ack)
	}
	runA := h.mgr.runs["sess-a"]
	runA.mu.Lock()
	runA.running = true
	runA.mu.Unlock()
	h.send(h.client, map[string]any{"op": "archive", "id": "sess-a", "nonce": "arch-running"})
	if ack := waitAck(t, h.client, "arch-running"); ack["ok"] != false || !strings.Contains(str(ack, "msg"), "Stop This Session") {
		t.Fatalf("expected archive while running to fail, got %+v", ack)
	}
	runA.mu.Lock()
	runA.running = false
	runA.mu.Unlock()

	h.send(h.client, map[string]any{"op": "archive", "id": "sess-a", "nonce": "arch-ok"})
	if ack := waitAck(t, h.client, "arch-ok"); ack["ok"] != true {
		t.Fatalf("archive failed: %+v", ack)
	}
	if !h.mgr.state.IsArchived("sess-a") {
		t.Fatal("expected sess-a archived")
	}
	h.send(h.client, map[string]any{"op": "unarchive", "id": "sess-a", "nonce": "unarch-ok"})
	if ack := waitAck(t, h.client, "unarch-ok"); ack["ok"] != true {
		t.Fatalf("unarchive failed: %+v", ack)
	}
	if h.mgr.state.IsArchived("sess-a") {
		t.Fatal("expected sess-a unarchived")
	}
}

func TestGrokManagerModelsCacheAndLiveMerge(t *testing.T) {
	h := newGrokTestHarness(t, true)
	cacheJSON := `{"models":[{"id":"grok-4.7","display_name":"Grok 4.7 Cache","description":"Cached desc","supported_reasoning_efforts":["low","medium","high"],"default_reasoning_effort":"high"}]}`
	if err := os.WriteFile(filepath.Join(h.grokHome, "models_cache.json"), []byte(cacheJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	tomlCfg := "[models]\ndefault = \"grok-4.7-build-fast\"\ndefault_reasoning_effort = \"xhigh\"\n\n[ui]\npermission_mode = \"acceptEdits\"\n"
	if err := os.WriteFile(filepath.Join(h.grokHome, "config.toml"), []byte(tomlCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	var listCalls atomic.Int32
	h.mgr.dialACP = func(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
		srv, cli := newMockGrokACPServer(t, cfg)
		setupStandardMockGrokServer(t, srv)
		srv.on(grokExtModelsList, func(msg mockACPRPCMessage) {
			listCalls.Add(1)
			_ = srv.sendResult(msg.ID, map[string]any{
				"result": mockGrokInitResultPayload()["_meta"].(map[string]any)["modelState"],
			})
		})
		srv.start()
		t.Cleanup(srv.close)
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	h.send(h.client, map[string]any{"op": "models"})
	frame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "models" && str(m, "provider") == "grok"
	})
	models := arrayValue(frame["models"])
	if len(models) != 2 {
		t.Fatalf("expected 2 merged live models, got %d: %+v", len(models), models)
	}
	def := mapValue(frame["default"])
	if str(def, "model") != "grok-4.7-build-fast" || str(def, "effort") != "xhigh" || str(def, "mode") != "acceptEdits" {
		t.Fatalf("unexpected default settings: %+v", def)
	}
	modes := stringArray(frame["modes"])
	wantModes := []string{"default", "acceptEdits", "auto", "plan", "bypassPermissions"}
	if strings.Join(modes, ",") != strings.Join(wantModes, ",") {
		t.Fatalf("modes got %v, want %v", modes, wantModes)
	}
	if frame["allowFullAccess"] != true {
		t.Fatalf("expected allowFullAccess == true, got %+v", frame["allowFullAccess"])
	}
	if listCalls.Load() != 1 {
		t.Fatalf("expected 1 _x.ai/models/list call, got %d", listCalls.Load())
	}

	// Second call within 5m TTL uses cached catalog.
	h.advance(2 * time.Minute)
	h.send(h.client, map[string]any{"op": "models"})
	waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "models" && str(m, "provider") == "grok"
	})
	if listCalls.Load() != 1 {
		t.Fatalf("expected TTL cache hit without re-querying _x.ai/models/list, calls=%d", listCalls.Load())
	}

	// After 5m TTL expires, refreshes from live daemon again.
	h.advance(4 * time.Minute)
	h.send(h.client, map[string]any{"op": "models"})
	waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "models" && str(m, "provider") == "grok"
	})
	if listCalls.Load() != 2 {
		t.Fatalf("expected refresh after 5m TTL, calls=%d", listCalls.Load())
	}
}

func TestGrokManagerOpenWarmColdExternalAndGenerationSwitching(t *testing.T) {
	h := newGrokTestHarness(t, true)
	cwd := t.TempDir()

	makeTurns := func(sid string, count int) []map[string]any {
		var out []map[string]any
		baseT := h.clock.Add(-time.Duration(count) * time.Minute)
		for i := 0; i < count; i++ {
			ts := baseT.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
			out = append(out,
				map[string]any{
					"timestamp": ts,
					"method":    "session/update",
					"params": map[string]any{
						"sessionId": sid,
						"update": map[string]any{
							"sessionUpdate": "user_message_chunk",
							"content":       map[string]any{"type": "text", "text": fmt.Sprintf("Turn %d prompt", i+1)},
						},
					},
				},
				map[string]any{
					"timestamp": ts,
					"method":    "session/update",
					"params": map[string]any{
						"sessionId": sid,
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": fmt.Sprintf("Turn %d answer", i+1)},
						},
					},
				},
			)
		}
		return out
	}

	h.writeSession("sess-warm", cwd, "Warm Chat", "grok-4.7", "high", h.clock, makeTurns("sess-warm", 20), nil)
	h.writeSession("sess-cold", cwd, "Cold Chat", "grok-4.7", "high", h.clock.Add(-time.Second), makeTurns("sess-cold", 1), nil)
	h.writeSession("sess-ext", cwd, "External TUI Chat", "grok-4.7", "high", h.clock.Add(-2*time.Second), makeTurns("sess-ext", 1), nil)

	// Mark sess-ext as open in an external TUI process (current PID is guaranteed alive on all OSes).
	activeJSON := mustJSON([]map[string]any{
		{
			"session_id": "sess-ext",
			"pid":        os.Getpid(),
			"cwd":        cwd,
			"opened_at":  h.clock.Format(time.RFC3339),
		},
	})
	if err := os.WriteFile(filepath.Join(h.grokHome, "active_sessions.json"), activeJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	coldGate := make(chan struct{})
	var resumeCount atomic.Int32
	var loadCount atomic.Int32
	var extResumeCount atomic.Int32

	h.mgr.openFastWait = 80 * time.Millisecond
	h.mgr.dialACP = func(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
		srv, cli := newMockGrokACPServer(t, cfg)
		setupStandardMockGrokServer(t, srv)
		srv.on("session/resume", func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			sid := str(p, "sessionId")
			if sid == "sess-cold" {
				<-coldGate
				// Return error on resume to also exercise LoadSession fallback + replay drain.
				_ = srv.sendError(msg.ID, -32601, "resume unavailable")
				return
			}
			if sid == "sess-ext" {
				extResumeCount.Add(1)
			}
			resumeCount.Add(1)
			_ = srv.sendResult(msg.ID, mockGrokSessionStatePayload(sid, "grok-4.7", "high"))
		})
		srv.on("session/load", func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			sid := str(p, "sessionId")
			loadCount.Add(1)
			// Emit replay chunk during LoadSession; must be drained while loading == true
			// and must NOT duplicate existing disk turns.
			_ = srv.sendNotify("session/update", map[string]any{
				"sessionId": sid,
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": "replayed duplicate"},
				},
			})
			_ = srv.sendResult(msg.ID, mockGrokSessionStatePayload(sid, "grok-4.7", "high"))
		})
		srv.start()
		t.Cleanup(srv.close)
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	h.mgr.refresh()
	drainFrames(h.client)

	// 1. Warm open: single frame Mode == "live", Connecting == false, HasOlder == true (15 of 20).
	h.send(h.client, map[string]any{"op": "open", "id": "sess-warm", "nonce": "open-warm"})
	warmOpen := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "sess-warm"
	})
	warmMeta := mapValue(warmOpen["meta"])
	if str(warmMeta, "mode") != "live" || warmMeta["connecting"] == true || warmMeta["hasOlder"] != true {
		t.Fatalf("expected warm frame-1 live open with hasOlder, got %+v", warmMeta)
	}
	if len(arrayValue(warmOpen["turns"])) != 15 {
		t.Fatalf("expected 15 initial turns, got %d", len(arrayValue(warmOpen["turns"])))
	}
	waitAck(t, h.client, "open-warm")

	// op: "more" loads all 20 turns while live.
	h.send(h.client, map[string]any{"op": "more", "id": "sess-warm", "nonce": "more-warm"})
	moreFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "sess-warm" && m["more"] == true
	})
	if len(arrayValue(moreFrame["turns"])) != 20 {
		t.Fatalf("expected 20 turns after more, got %d", len(arrayValue(moreFrame["turns"])))
	}
	waitAck(t, h.client, "more-warm")

	// 2. External TUI open: returns Mode == "external" without calling ResumeSession.
	h.send(h.client, map[string]any{"op": "open", "id": "sess-ext", "nonce": "open-ext"})
	extFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "sess-ext"
	})
	if str(mapValue(extFrame["meta"]), "mode") != "external" {
		t.Fatalf("expected mode=external for live TUI session, got %+v", extFrame["meta"])
	}
	waitAck(t, h.client, "open-ext")
	if extResumeCount.Load() != 0 {
		t.Fatalf("open on external TUI session must not fight TUI, resumes=%d", extResumeCount.Load())
	}

	// Explicit wake ("Take Over") attaches live and transitions to Mode == "live".
	h.send(h.client, map[string]any{"op": "wake", "id": "sess-ext", "nonce": "wake-ext"})
	takeOverFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "sess-ext"
	})
	if str(mapValue(takeOverFrame["meta"]), "mode") != "live" {
		t.Fatalf("expected mode=live after wake takeover, got %+v", takeOverFrame["meta"])
	}
	waitAck(t, h.client, "wake-ext")
	if extResumeCount.Load() != 1 {
		t.Fatalf("expected 1 resume on wake takeover, got %d", extResumeCount.Load())
	}

	// 3. Cold open (>openFastWait): frame 1 is tail+connecting, then upgrades to live via LoadSession fallback.
	h.send(h.client, map[string]any{"op": "open", "id": "sess-cold", "nonce": "open-cold"})
	coldFirst := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "sess-cold"
	})
	coldFirstMeta := mapValue(coldFirst["meta"])
	if str(coldFirstMeta, "mode") != "tail" || coldFirstMeta["connecting"] != true {
		t.Fatalf("expected cold frame 1 tail+connecting, got %+v", coldFirstMeta)
	}
	waitAck(t, h.client, "open-cold")

	close(coldGate)
	coldLive := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "sess-cold" && str(mapValue(m["meta"]), "mode") == "live"
	})
	if mapValue(coldLive["meta"])["connecting"] == true {
		t.Fatalf("expected connecting=false on live upgrade, got %+v", coldLive["meta"])
	}
	if len(arrayValue(coldLive["turns"])) != 1 {
		t.Fatalf("replay chunk must not duplicate existing disk turns, got %d turns", len(arrayValue(coldLive["turns"])))
	}
	if loadCount.Load() != 1 {
		t.Fatalf("expected fallback LoadSession call, got %d", loadCount.Load())
	}

	// 4. Generation guard: closing sess-warm before reopening does not emit stale frames.
	h.send(h.client, map[string]any{"op": "close", "id": "sess-warm"})
	h.send(h.client, map[string]any{"op": "close", "id": "sess-cold"})
	h.send(h.client, map[string]any{"op": "close", "id": "sess-ext"})
	if resumeCount.Load() < 2 {
		t.Fatalf("expected warm + ext resumes, got %d", resumeCount.Load())
	}
}

func TestGrokManagerSharedDaemonPoolAndDemux(t *testing.T) {
	h := newGrokTestHarness(t, true)
	cwd := t.TempDir()
	h.writeSession("sess-1", cwd, "Session One", "grok-4.7", "high", h.clock, nil, nil)
	h.writeSession("sess-2", cwd, "Session Two", "grok-4.7", "high", h.clock.Add(-time.Second), nil, nil)

	var mu sync.Mutex
	var defaultSrv *mockGrokACPServer
	var alwaysApproveSrv *mockGrokACPServer
	var dialDefaultCount int
	var dialAlwaysApproveCount int
	promptHold := map[string]chan struct{}{
		"sess-1": make(chan struct{}),
		"sess-2": make(chan struct{}),
	}

	h.mgr.dialACP = func(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
		srv, cli := newMockGrokACPServer(t, cfg)
		setupStandardMockGrokServer(t, srv)
		srv.on("session/resume", func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			_ = srv.sendResult(msg.ID, mockGrokSessionStatePayload(str(p, "sessionId"), "grok-4.7", "high"))
		})
		srv.on("session/new", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockGrokSessionStatePayload("sess-yolo", "grok-4.7", "high"))
		})
		srv.on("session/prompt", func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			sid := str(p, "sessionId")
			if ch := promptHold[sid]; ch != nil {
				<-ch
			}
			_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "end_turn"})
		})
		srv.start()
		t.Cleanup(srv.close)

		mu.Lock()
		if cfg.AlwaysApprove {
			dialAlwaysApproveCount++
			alwaysApproveSrv = srv
		} else {
			dialDefaultCount++
			defaultSrv = srv
		}
		mu.Unlock()

		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	h.mgr.refresh()
	r1, err := h.mgr.ensureRun(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ensureRun sess-1: %v", err)
	}
	r2, err := h.mgr.ensureRun(context.Background(), "sess-2")
	if err != nil {
		t.Fatalf("ensureRun sess-2: %v", err)
	}
	if r1.daemon != r2.daemon {
		t.Fatal("sess-1 and sess-2 must share one pooled grokDaemon")
	}
	mu.Lock()
	if dialDefaultCount != 1 {
		mu.Unlock()
		t.Fatalf("expected 1 default daemon dial, got %d", dialDefaultCount)
	}
	srv := defaultSrv
	mu.Unlock()

	// Strict SessionID demux for updates.
	_ = srv.sendNotify("session/update", map[string]any{
		"sessionId": "sess-1",
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "Only For Session One"},
		},
	})
	_ = srv.sendNotify("session/update", map[string]any{
		"sessionId": "sess-2",
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "Only For Session Two"},
		},
	})

	demuxDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(demuxDeadline) {
		_, t1 := r1.snapshot()
		_, t2 := r2.snapshot()
		if strings.Contains(string(mustJSON(t1)), "Only For Session One") &&
			strings.Contains(string(mustJSON(t2)), "Only For Session Two") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	r1.daemon.drainDispatchForSession(context.Background(), "sess-1")
	r2.daemon.drainDispatchForSession(context.Background(), "sess-2")

	_, turns1 := r1.snapshot()
	_, turns2 := r2.snapshot()
	if !strings.Contains(string(mustJSON(turns1)), "Only For Session One") || strings.Contains(string(mustJSON(turns1)), "Only For Session Two") {
		t.Fatalf("sess-1 demux mismatch: %+v", turns1)
	}
	if !strings.Contains(string(mustJSON(turns2)), "Only For Session Two") || strings.Contains(string(mustJSON(turns2)), "Only For Session One") {
		t.Fatalf("sess-2 demux mismatch: %+v", turns2)
	}

	// Empty-SessionID interactive request with 0 active prompts -> auto-cancelled!
	respZero, err := srv.sendServerRequest("perm-zero", "session/request_permission", map[string]any{
		"toolCall": map[string]any{"toolCallId": "tc0", "title": "ls"},
		"options":  []map[string]any{{"optionId": "allow_once", "name": "Allow", "kind": "allow_once"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgZero := <-respZero
	if !strings.Contains(string(msgZero.Result), `"cancelled"`) {
		t.Fatalf("expected auto-cancel when 0 active prompts, got %s", string(msgZero.Result))
	}

	// Start prompt on sess-1 ONLY -> empty-SessionID interactive request routes to sess-1!
	if _, err := r1.startPrompt("run 1", nil, []acpContentBlock{acpTextBlock("run 1")}); err != nil {
		t.Fatal(err)
	}
	_, err = srv.sendServerRequest("perm-one", "session/request_permission", map[string]any{
		"toolCall": map[string]any{"toolCallId": "tc1", "title": "echo ok"},
		"options":  []map[string]any{{"optionId": "allow_once", "name": "Allow", "kind": "allow_once"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	permDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(permDeadline) {
		m1Check, _ := r1.snapshot()
		if len(m1Check.Requests) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	r1.daemon.drainDispatchForSession(context.Background(), "sess-1")
	m1, _ := r1.snapshot()
	m2, _ := r2.snapshot()
	if len(m1.Requests) != 1 || m1.Requests[0].ID != "perm-one" || len(m2.Requests) != 0 {
		t.Fatalf("expected empty-SessionID permission routed only to single active run sess-1, m1=%+v m2=%+v", m1.Requests, m2.Requests)
	}

	// Start second concurrent prompt on sess-2 -> 2 active prompts -> empty-SessionID auto-cancelled!
	if _, err := r2.startPrompt("run 2", nil, []acpContentBlock{acpTextBlock("run 2")}); err != nil {
		t.Fatal(err)
	}
	respTwo, err := srv.sendServerRequest("ask-two", grokExtAskUserQuestion, map[string]any{
		"toolCallId": "tc2",
		"questions":  []map[string]any{{"id": "q", "question": "Ambiguous?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgTwo := <-respTwo
	if !strings.Contains(string(msgTwo.Result), `"cancelled"`) {
		t.Fatalf("expected auto-cancel when 2 active prompts, got %s", string(msgTwo.Result))
	}

	// Approve pending perm-one and release both prompts cleanly.
	if err := r1.approveRequest("perm-one", "allow_once", "accept", ""); err != nil {
		t.Fatalf("approve perm-one: %v", err)
	}
	close(promptHold["sess-1"])
	close(promptHold["sess-2"])

	// bypassPermissions creates/uses the separate alwaysApprove pooled daemon.
	h.send(h.client, map[string]any{
		"op":             "new",
		"cwd":            cwd,
		"text":           "Run with full access",
		"permissionMode": "bypassPermissions",
		"nonce":          "new-yolo",
	})
	ackYolo := waitAck(t, h.client, "new-yolo")
	if ackYolo["ok"] != true || str(ackYolo, "thread") != "sess-yolo" {
		t.Fatalf("bypassPermissions new failed: %+v", ackYolo)
	}
	mu.Lock()
	if dialAlwaysApproveCount != 1 || alwaysApproveSrv == nil {
		mu.Unlock()
		t.Fatalf("expected separate alwaysApprove daemon dial, count=%d", dialAlwaysApproveCount)
	}
	mu.Unlock()
}

func TestGrokManagerWatcherSafeReapWatchdogAndCrashAutoReconnect(t *testing.T) {
	h := newGrokTestHarness(t, true)
	cwd := t.TempDir()
	h.writeSession("sess-keep", cwd, "Keep Alive Chat", "grok-4.7", "high", h.clock, []map[string]any{
		{
			"timestamp": h.clock.Format(time.RFC3339),
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "sess-keep",
				"update": map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content":       map[string]any{"type": "text", "text": "Initial turn"},
				},
			},
		},
	}, nil)

	var mu sync.Mutex
	var servers []*mockGrokACPServer
	var closeSessionCalls atomic.Int32
	var listSessionCalls atomic.Int32
	var holdPrompt chan struct{}

	h.mgr.dialACP = func(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
		srv, cli := newMockGrokACPServer(t, cfg)
		setupStandardMockGrokServer(t, srv)
		srv.on("session/list", func(msg mockACPRPCMessage) {
			listSessionCalls.Add(1)
			_ = srv.sendResult(msg.ID, map[string]any{"sessions": []any{}})
		})
		srv.on("session/resume", func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			_ = srv.sendResult(msg.ID, mockGrokSessionStatePayload(str(p, "sessionId"), "grok-4.7", "high"))
		})
		srv.on("session/close", func(msg mockACPRPCMessage) {
			closeSessionCalls.Add(1)
			_ = srv.sendResult(msg.ID, map[string]any{"_meta": map[string]any{"x.ai/closeOutcome": "closed"}})
		})
		srv.on("session/prompt", func(msg mockACPRPCMessage) {
			mu.Lock()
			ch := holdPrompt
			mu.Unlock()
			if ch != nil {
				<-ch
			}
			_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "end_turn"})
		})
		srv.start()
		t.Cleanup(srv.close)
		mu.Lock()
		servers = append(servers, srv)
		mu.Unlock()
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	h.mgr.refresh()
	drainFrames(h.client)
	drainFrames(h.evClient)

	// Open & watch sess-keep.
	h.send(h.client, map[string]any{"op": "open", "id": "sess-keep", "nonce": "open-keep"})
	waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "sess-keep" && str(mapValue(m["meta"]), "mode") == "live"
	})
	waitAck(t, h.client, "open-keep")

	// 1. Even after 60 minutes, watched run is NEVER reaped.
	h.advance(60 * time.Minute)
	h.mgr.reapIdleSessions()
	h.mgr.mu.Lock()
	runKeep := h.mgr.runs["sess-keep"]
	h.mgr.mu.Unlock()
	if runKeep == nil || !runKeep.isAlive() {
		t.Fatal("watched run must NEVER be reaped")
	}

	// 2. 45s keepalive watchdog invokes session/list when idle.
	h.mgr.checkDaemonHealth()
	deadline := time.Now().Add(2 * time.Second)
	for listSessionCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if listSessionCalls.Load() == 0 {
		t.Fatal("expected checkDaemonHealth to invoke session/list watchdog")
	}

	// 3. External disk sync while live and idle: append new turn to updates.jsonl and pollWatchedThreads.
	h.writeSession("sess-keep", cwd, "Keep Alive Chat", "grok-4.7", "high", h.clock.Add(time.Second), []map[string]any{
		{
			"timestamp": h.clock.Format(time.RFC3339),
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "sess-keep",
				"update": map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content":       map[string]any{"type": "text", "text": "Initial turn"},
				},
			},
		},
		{
			"timestamp": h.clock.Add(time.Second).Format(time.RFC3339),
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "sess-keep",
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": "External disk reply"},
				},
			},
		},
	}, nil)
	h.mgr.refresh()
	drainFrames(h.client)
	h.mgr.pollWatchedThreads()
	syncFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "turn" && str(m, "id") == "sess-keep"
	})
	if !strings.Contains(string(mustJSON(syncFrame)), "External disk reply") {
		t.Fatalf("expected external disk reply synced while live, got %+v", syncFrame)
	}

	// 4. Unexpected daemon crash while running & watched:
	//    - emits single "failed" event (no duplicate from finishPrompt!)
	//    - broadcasts connecting: true
	//    - auto-reconnects to Mode: "live"!
	mu.Lock()
	holdPrompt = make(chan struct{})
	srv1 := servers[0]
	mu.Unlock()

	h.send(h.client, map[string]any{"op": "send", "id": "sess-keep", "text": "Crash during this turn", "nonce": "send-crash"})
	waitAck(t, h.client, "send-crash")
	waitFrame(t, h.evClient, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "event" && str(m, "kind") == "started" && str(m, "id") == "sess-keep"
	})
	drainFrames(h.client)
	drainFrames(h.evClient)

	// Drop the daemon transport abruptly!
	srv1.close()
	close(holdPrompt)

	// Expect Connecting: true meta frame AND single "failed" event, followed by Mode: "live" recovery!
	waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "meta" && str(m, "id") == "sess-keep" && mapValue(m["meta"])["connecting"] == true
	})
	waitFrame(t, h.evClient, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "event" && str(m, "kind") == "failed" && str(m, "id") == "sess-keep"
	})
	waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "sess-keep" && str(mapValue(m["meta"]), "mode") == "live"
	})
	time.Sleep(80 * time.Millisecond)
	for _, extra := range drainFrames(h.evClient) {
		if str(extra, "t") == "event" && str(extra, "kind") == "failed" {
			t.Fatalf("duplicate failed event emitted on transport drop: %+v", extra)
		}
	}

	// 5. Unwatch sess-keep: after 12m idle -> reaped via session/close ONLY, daemon stays alive!
	h.send(h.client, map[string]any{"op": "close", "id": "sess-keep"})
	h.advance(13 * time.Minute)
	h.mgr.reapIdleSessions()

	deadline = time.Now().Add(2 * time.Second)
	for closeSessionCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if closeSessionCalls.Load() == 0 {
		t.Fatal("expected session/close on post-unwatch idle reap")
	}
	h.mgr.mu.Lock()
	daemonStillAlive := len(h.mgr.daemons) == 1
	h.mgr.mu.Unlock()
	if !daemonStillAlive {
		t.Fatal("per-session reap must keep shared daemon alive")
	}

	// 6. After 30m with len(runs) == 0 and zero watchers -> idle daemon released!
	h.advance(1 * time.Minute)
	h.mgr.reapIdleSessions() // starts idleSince clock
	h.advance(31 * time.Minute)
	h.mgr.reapIdleSessions()
	h.mgr.mu.Lock()
	remainingDaemons := len(h.mgr.daemons)
	h.mgr.mu.Unlock()
	if remainingDaemons != 0 {
		t.Fatalf("expected idle daemon released after 30m, remaining=%d", remainingDaemons)
	}
}

func TestGrokManagerNewSendStreamingDiffsSteeringCompactStopAndSettings(t *testing.T) {
	// Configure temp HOME + USERPROFILE so uploadDir() stays inside t.TempDir() on POSIX and Windows.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	h := newGrokTestHarness(t, true)
	cwd := t.TempDir()

	// Prepare a valid 1x1 PNG in uploadDir() and a bad file for upload validation tests.
	if err := os.MkdirAll(uploadDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	pngBytes, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+X2S8AAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	goodPNG := filepath.Join(uploadDir(), "sample.png")
	if err := os.WriteFile(goodPNG, pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	badMIME := filepath.Join(uploadDir(), "not-an-image.png")
	if err := os.WriteFile(badMIME, []byte("plain text not png"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var srvRef *mockGrokACPServer
	var setModeCalls int
	var configCalls []map[string]any
	var modeNotifs []map[string]any
	var togglePlanCalls int
	var compactCalls []string
	var cancelCalls int
	var promptCount int
	prompt1Gate := make(chan struct{})
	prompt2Done := make(chan struct{})

	h.mgr.dialACP = func(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
		srv, cli := newMockGrokACPServer(t, cfg)
		setupStandardMockGrokServer(t, srv)
		srv.on("session/set_mode", func(msg mockACPRPCMessage) {
			mu.Lock()
			setModeCalls++
			mu.Unlock()
			_ = srv.sendError(msg.ID, -32601, "session/set_mode must never be called")
		})
		srv.on("session/new", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockGrokSessionStatePayload("sess-live", "grok-4.7", "high"))
		})
		srv.on("session/set_config_option", func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			// Verify PLAIN STRING value (never an object)!
			if _, ok := p["value"].(string); !ok {
				_ = srv.sendError(msg.ID, -32602, "expected plain string value")
				return
			}
			mu.Lock()
			configCalls = append(configCalls, p)
			mu.Unlock()
			_ = srv.sendResult(msg.ID, map[string]any{
				"configOptions": mockGrokSessionStatePayload("sess-live", "grok-4.7", "high")["configOptions"],
			})
		})
		srv.on(grokExtYoloModeChanged, func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			mu.Lock()
			modeNotifs = append(modeNotifs, p)
			mu.Unlock()
		})
		srv.on(grokExtTogglePlanMode, func(msg mockACPRPCMessage) {
			mu.Lock()
			togglePlanCalls++
			mu.Unlock()
		})
		srv.on(grokExtCompactConversation, func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			mu.Lock()
			compactCalls = append(compactCalls, str(p, "session_id"))
			mu.Unlock()
			_ = srv.sendResult(msg.ID, map[string]any{"ok": true})
		})
		srv.on("session/cancel", func(msg mockACPRPCMessage) {
			mu.Lock()
			cancelCalls++
			mu.Unlock()
		})
		srv.on("session/prompt", func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			sid := str(p, "sessionId")

			mu.Lock()
			promptCount++
			n := promptCount
			mu.Unlock()

			switch n {
			case 1:
				// Emit thought, message, diff tool call, plan, usage, and turn_completed!
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": sid,
					"update": map[string]any{
						"sessionUpdate": "agent_thought_chunk",
						"content":       map[string]any{"type": "text", "text": "Checking files..."},
					},
				})
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": sid,
					"update": map[string]any{
						"sessionUpdate": "tool_call",
						"toolCallId":    "tc-edit-1",
						"title":         "Edit main.go",
						"kind":          "edit",
						"status":        "completed",
						"content": []map[string]any{
							{
								"type":    "diff",
								"path":    "main.go",
								"oldText": "a := 1\n",
								"newText": "a := 2\n",
							},
						},
					},
				})
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": sid,
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": "Updated main.go cleanly."},
					},
				})
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": sid,
					"update": map[string]any{
						"sessionUpdate": "usage_update",
						"used":          42000,
						"size":          256000,
					},
				})
				_ = srv.sendNotify(grokMethodSessionUpdate, map[string]any{
					"sessionId": sid,
					"update": map[string]any{
						"type":        "turn_completed",
						"stop_reason": "end_turn",
						"elapsed_ms":  450,
					},
				})
				_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "end_turn"})
			case 2:
				// Hold prompt 2 so prompt 3 exercises concurrent live steering.
				<-prompt1Gate
				_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "end_turn"})
			case 3:
				close(prompt2Done)
				_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "end_turn"})
			case 4:
				// Wait for cancel
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					mu.Lock()
					cancelled := cancelCalls > 0
					mu.Unlock()
					if cancelled {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "cancelled"})
			}
		})
		srv.start()
		t.Cleanup(srv.close)
		mu.Lock()
		srvRef = srv
		mu.Unlock()
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	// 1. Upload validation rejections: outside uploadDir, >12 images, bad MIME.
	h.send(h.client, map[string]any{"op": "new", "cwd": cwd, "text": "bad", "images": []string{"/etc/passwd"}, "nonce": "img-outside"})
	if ack := waitAck(t, h.client, "img-outside"); ack["ok"] != false {
		t.Fatalf("expected upload outside dir rejected, got %+v", ack)
	}
	tooMany := make([]string, 13)
	for i := range tooMany {
		tooMany[i] = goodPNG
	}
	h.send(h.client, map[string]any{"op": "new", "cwd": cwd, "text": "too many", "images": tooMany, "nonce": "img-13"})
	if ack := waitAck(t, h.client, "img-13"); ack["ok"] != false {
		t.Fatalf("expected >12 images rejected, got %+v", ack)
	}
	h.send(h.client, map[string]any{"op": "new", "cwd": cwd, "text": "bad mime", "images": []string{badMIME}, "nonce": "img-mime"})
	if ack := waitAck(t, h.client, "img-mime"); ack["ok"] != false {
		t.Fatalf("expected non-image MIME rejected, got %+v", ack)
	}

	// 2. Valid op: "new" in plan mode with image, model, and reasoning_effort.
	h.send(h.client, map[string]any{
		"op":             "new",
		"cwd":            cwd,
		"text":           "Plan the change",
		"images":         []string{goodPNG},
		"model":          "grok-4.7-build-fast",
		"effort":         "xhigh",
		"permissionMode": "plan",
		"nonce":          "new-ok",
	})
	ackNew := waitAck(t, h.client, "new-ok")
	if ackNew["ok"] != true || str(ackNew, "thread") != "sess-live" {
		t.Fatalf("op:new failed: %+v", ackNew)
	}

	// Wait for started + completed events on evClient.
	waitFrame(t, h.evClient, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "event" && str(m, "provider") == "grok" && str(m, "kind") == "started" && str(m, "id") == "sess-live"
	})
	compEv := waitFrame(t, h.evClient, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "event" && str(m, "provider") == "grok" && str(m, "kind") == "completed" && str(m, "id") == "sess-live"
	})
	if !strings.Contains(str(compEv, "text"), "Updated main.go cleanly") {
		t.Fatalf("unexpected completed event text: %+v", compEv)
	}

	// Verify op: "diff" resolves via run.transcript.Diff(key) for toolCallId and empty key!
	h.send(h.client, map[string]any{"op": "diff", "id": "sess-live", "key": "tc-edit-1"})
	diffFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "diff" && str(m, "provider") == "grok" && str(m, "key") == "tc-edit-1"
	})
	if !strings.Contains(str(diffFrame, "diff"), "-a := 1") || !strings.Contains(str(diffFrame, "diff"), "+a := 2") {
		t.Fatalf("unexpected diff frame: %+v", diffFrame)
	}

	// 3. Live steering: send prompt 2, then prompt 3 while prompt 2 is running.
	h.send(h.client, map[string]any{"op": "send", "id": "sess-live", "text": "Second turn", "nonce": "send-2"})
	waitAck(t, h.client, "send-2")
	h.send(h.client, map[string]any{"op": "send", "id": "sess-live", "text": "Steer this", "nonce": "send-3"})
	ackSteer := waitAck(t, h.client, "send-3")
	if ackSteer["ok"] != true || ackSteer["queued"] != false {
		t.Fatalf("unexpected steering ack: %+v", ackSteer)
	}
	<-prompt2Done
	close(prompt1Gate)
	waitFrame(t, h.evClient, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "event" && str(m, "kind") == "completed" && str(m, "id") == "sess-live"
	})

	// 4. op: "compact" calls _x.ai/compact_conversation with session_id.
	h.send(h.client, map[string]any{"op": "compact", "id": "sess-live", "nonce": "cmp-1"})
	if ack := waitAck(t, h.client, "cmp-1"); ack["ok"] != true {
		t.Fatalf("compact failed: %+v", ack)
	}

	// 5. op: "stop" cancels active turn and marks it interrupted without failed event.
	drainFrames(h.evClient)
	h.send(h.client, map[string]any{"op": "send", "id": "sess-live", "text": "Stop me", "nonce": "send-4"})
	waitAck(t, h.client, "send-4")
	h.send(h.client, map[string]any{"op": "stop", "id": "sess-live", "nonce": "stop-4"})
	if ack := waitAck(t, h.client, "stop-4"); ack["ok"] != true {
		t.Fatalf("stop failed: %+v", ack)
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.mgr.isRunActive("sess-live") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.mgr.isRunActive("sess-live") {
		t.Fatal("expected sess-live stopped")
	}

	// 6. op: "settings":
	//    - omitted effort keeps existing effort ("xhigh")
	//    - explicit effort: "" resets back to Default!
	//    - mode change from "plan" -> "auto" uses notification-only _x.ai/toggle_plan_mode + _x.ai/yolo_mode_changed
	//    - ZERO calls to session/set_mode!
	h.send(h.client, map[string]any{"op": "settings", "id": "sess-live", "model": "grok-4.7", "nonce": "set-model"})
	if ack := waitAck(t, h.client, "set-model"); ack["ok"] != true {
		t.Fatalf("settings model failed: %+v", ack)
	}
	if h.mgr.state.Pref("sess-live").Effort != "xhigh" {
		t.Fatalf("omitted effort should preserve xhigh, got %q", h.mgr.state.Pref("sess-live").Effort)
	}

	h.send(h.client, map[string]any{"op": "settings", "id": "sess-live", "effort": "", "permissionMode": "auto", "nonce": "set-reset"})
	if ack := waitAck(t, h.client, "set-reset"); ack["ok"] != true {
		t.Fatalf("settings reset failed: %+v", ack)
	}
	if pref := h.mgr.state.Pref("sess-live"); pref.Effort != "" || pref.PermissionMode != "auto" {
		t.Fatalf("expected effort reset to empty and mode=auto, got %+v", pref)
	}

	// Wait briefly for notifications to be processed by mock server reader.
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if srvRef == nil {
		t.Fatal("expected mock server dialed")
	}
	if setModeCalls != 0 {
		t.Fatalf("session/set_mode MUST NEVER be called, got %d calls", setModeCalls)
	}
	if togglePlanCalls < 2 {
		t.Fatalf("expected at least 2 _x.ai/toggle_plan_mode notifications (enter + exit plan), got %d", togglePlanCalls)
	}
	if len(modeNotifs) == 0 || modeNotifs[len(modeNotifs)-1]["auto_mode"] != true {
		t.Fatalf("expected _x.ai/yolo_mode_changed with auto_mode=true, got %+v", modeNotifs)
	}
	if len(compactCalls) != 1 || compactCalls[0] != "sess-live" {
		t.Fatalf("expected 1 compact call for sess-live, got %v", compactCalls)
	}
	if len(configCalls) < 4 {
		t.Fatalf("expected plain-string SetConfigOption calls for model/effort, got %+v", configCalls)
	}
}

func TestGrokManagerInteractiveApprovalsPlanExitQuestionsAndElicitation(t *testing.T) {
	h := newGrokTestHarness(t, true)
	cwd := t.TempDir()
	h.writeSession("sess-int", cwd, "Interactive Chat", "grok-4.7", "high", h.clock, nil, nil)

	var srvRef *mockGrokACPServer
	h.mgr.dialACP = func(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
		srv, cli := newMockGrokACPServer(t, cfg)
		setupStandardMockGrokServer(t, srv)
		srv.on("session/resume", func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			_ = srv.sendResult(msg.ID, mockGrokSessionStatePayload(str(p, "sessionId"), "grok-4.7", "high"))
		})
		srv.start()
		t.Cleanup(srv.close)
		srvRef = srv
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	h.mgr.refresh()
	h.send(h.client, map[string]any{"op": "open", "id": "sess-int", "nonce": "open-int"})
	waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "sess-int"
	})
	waitAck(t, h.client, "open-int")
	drainFrames(h.client)
	drainFrames(h.evClient)

	// 1. session/request_permission: reject unknown optionId, then approve valid optionId.
	permResp, err := srvRef.sendServerRequest("perm-1", "session/request_permission", map[string]any{
		"sessionId": "sess-int",
		"toolCall": map[string]any{
			"toolCallId": "tc-cmd",
			"title":      "Run go test",
			"kind":       "execute",
			"rawInput":   map[string]any{"command": "go test ./..."},
		},
		"options": []map[string]any{
			{"optionId": "allow_once", "name": "Allow Once", "kind": "allow_once"},
			{"optionId": "reject_once", "name": "Reject Once", "kind": "reject_once"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFrame(t, h.evClient, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "event" && str(m, "kind") == "attention" && str(m, "id") == "sess-int"
	})

	h.send(h.client, map[string]any{"op": "approve", "id": "sess-int", "reqId": "perm-1", "optionId": "bogus_opt", "nonce": "perm-bad"})
	if ack := waitAck(t, h.client, "perm-bad"); ack["ok"] != false {
		t.Fatalf("expected unknown optionId rejected, got %+v", ack)
	}
	h.send(h.client, map[string]any{"op": "approve", "id": "sess-int", "reqId": "perm-1", "optionId": "allow_once", "decision": "accept", "nonce": "perm-ok"})
	if ack := waitAck(t, h.client, "perm-ok"); ack["ok"] != true {
		t.Fatalf("approve perm-1 failed: %+v", ack)
	}
	msgPerm := <-permResp
	if !strings.Contains(string(msgPerm.Result), `"optionId":"allow_once"`) {
		t.Fatalf("unexpected permission response: %s", string(msgPerm.Result))
	}

	// 2. _x.ai/exit_plan_mode: verifies Kind=="plan", Method=="_x.ai/exit_plan_mode", options, and {outcome, feedback}!
	planResp, err := srvRef.sendServerRequest("plan-1", grokExtExitPlanMode, map[string]any{
		"sessionId":   "sess-int",
		"toolCallId":  "tc-plan",
		"planContent": "# Plan\n1. Ship GrokManager\n2. Verify race tests",
	})
	if err != nil {
		t.Fatal(err)
	}
	metaPlanFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		if str(m, "t") != "meta" {
			return false
		}
		reqs := arrayValue(mapValue(m["meta"])["requests"])
		return len(reqs) == 1 && str(mapValue(reqs[0]), "id") == "plan-1"
	})
	planReqCard := mapValue(arrayValue(mapValue(metaPlanFrame["meta"])["requests"])[0])
	if str(planReqCard, "kind") != "plan" || str(planReqCard, "method") != grokExtExitPlanMode || !strings.Contains(str(planReqCard, "body"), "Ship GrokManager") {
		t.Fatalf("unexpected exit_plan_mode card: %+v", planReqCard)
	}
	if len(arrayValue(planReqCard["options"])) != 3 {
		t.Fatalf("expected 3 options (Approve Plan / Revise / Decline), got %+v", planReqCard["options"])
	}

	h.send(h.client, map[string]any{
		"op":       "approve",
		"id":       "sess-int",
		"reqId":    "plan-1",
		"optionId": "revise",
		"feedback": "Add Windows portability check",
		"nonce":    "plan-revise",
	})
	if ack := waitAck(t, h.client, "plan-revise"); ack["ok"] != true {
		t.Fatalf("approve exit_plan_mode failed: %+v", ack)
	}
	msgPlan := <-planResp
	var planOut struct {
		Outcome  string  `json:"outcome"`
		Feedback *string `json:"feedback"`
	}
	if err := json.Unmarshal(msgPlan.Result, &planOut); err != nil || planOut.Outcome != "revise" || planOut.Feedback == nil || *planOut.Feedback != "Add Windows portability check" {
		t.Fatalf("unexpected exit_plan_mode wire response: %s", string(msgPlan.Result))
	}

	// 3. _x.ai/ask_user_question: single + multi-select + free-text -> 6-field []grokAnsweredQuestion!
	askResp, err := srvRef.sendServerRequest("ask-1", grokExtAskUserQuestion, map[string]any{
		"sessionId":  "sess-int",
		"toolCallId": "tc-ask",
		"mode":       "plan",
		"questions": []map[string]any{
			{
				"id":          "targets",
				"question":    "Which targets?",
				"multiSelect": true,
				"options": []map[string]any{
					{"id": "mac", "label": "macOS", "description": "Darwin arm64"},
					{"id": "win", "label": "Windows", "description": "Win64"},
				},
			},
			{
				"id":       "note",
				"question": "Extra notes?",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		if str(m, "t") != "meta" {
			return false
		}
		reqs := arrayValue(mapValue(m["meta"])["requests"])
		return len(reqs) == 1 && str(mapValue(reqs[0]), "id") == "ask-1"
	})

	h.send(h.client, map[string]any{
		"op":    "answer",
		"id":    "sess-int",
		"reqId": "ask-1",
		"answers": map[string]any{
			"targets": []string{"macOS", "Windows", "Linux too"},
			"note":    []string{"Run with -race"},
		},
		"nonce": "ans-ok",
	})
	if ack := waitAck(t, h.client, "ans-ok"); ack["ok"] != true {
		t.Fatalf("answer ask_user_question failed: %+v", ack)
	}
	msgAsk := <-askResp
	var askOut struct {
		Outcome string                 `json:"outcome"`
		Answers []grokAnsweredQuestion `json:"answers"`
	}
	if err := json.Unmarshal(msgAsk.Result, &askOut); err != nil {
		t.Fatalf("unmarshal ask response: %v (%s)", err, string(msgAsk.Result))
	}
	if askOut.Outcome != "accepted" || len(askOut.Answers) != 2 ||
		len(askOut.Answers[0].Picked) != 2 || askOut.Answers[0].Typed != "Linux too" ||
		askOut.Answers[1].Typed != "Run with -race" {
		t.Fatalf("unexpected ask_user_question response: %+v", askOut)
	}

	// 3b. Cancelled ask_user_question.
	askCancelResp, err := srvRef.sendServerRequest("ask-cancel", grokExtAskUserQuestion, map[string]any{
		"sessionId": "sess-int",
		"questions": []map[string]any{{"id": "q1", "question": "Proceed?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		if str(m, "t") != "meta" {
			return false
		}
		reqs := arrayValue(mapValue(m["meta"])["requests"])
		return len(reqs) == 1 && str(mapValue(reqs[0]), "id") == "ask-cancel"
	})
	h.send(h.client, map[string]any{"op": "answer", "id": "sess-int", "reqId": "ask-cancel", "action": "cancel", "nonce": "ans-cancel"})
	if ack := waitAck(t, h.client, "ans-cancel"); ack["ok"] != true {
		t.Fatalf("cancel ask_user_question failed: %+v", ack)
	}
	msgAskCancel := <-askCancelResp
	if !strings.Contains(string(msgAskCancel.Result), `"outcome":"cancelled"`) {
		t.Fatalf("expected cancelled outcome, got %s", string(msgAskCancel.Result))
	}

	// 4. _x.ai/mcp/elicit round-trip via op: "answer".
	elicitResp, err := srvRef.sendServerRequest("el-1", grokExtMCPElicit, map[string]any{
		"sessionId": "sess-int",
		"message":   "Enter project key",
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{"type": "string", "title": "Key"},
			},
			"required": []string{"key"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		if str(m, "t") != "meta" {
			return false
		}
		reqs := arrayValue(mapValue(m["meta"])["requests"])
		return len(reqs) == 1 && str(mapValue(reqs[0]), "id") == "el-1"
	})
	h.send(h.client, map[string]any{
		"op":      "answer",
		"id":      "sess-int",
		"reqId":   "el-1",
		"answers": map[string]any{"key": []string{"FC-42"}},
		"nonce":   "el-ok",
	})
	if ack := waitAck(t, h.client, "el-ok"); ack["ok"] != true {
		t.Fatalf("answer elicitation failed: %+v", ack)
	}
	msgEl := <-elicitResp
	if !strings.Contains(string(msgEl.Result), `"action":"accept"`) || !strings.Contains(string(msgEl.Result), `"FC-42"`) {
		t.Fatalf("unexpected elicitation response: %s", string(msgEl.Result))
	}
}

func TestGrokManagerSubagentReadOnlyGuardsAndLiveWorkTail(t *testing.T) {
	h := newGrokTestHarness(t, true)
	cwd := t.TempDir()

	parentDir := h.writeSession("parent-1", cwd, "Parent Chat", "grok-4.7", "high", h.clock, nil, nil)
	childUpdates := []map[string]any{
		{
			"timestamp": h.clock.Format(time.RFC3339),
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "sub-1",
				"update": map[string]any{
					"sessionUpdate": "tool_call",
					"toolCallId":    "tc-sub-diff",
					"title":         "Edit worker.go",
					"kind":          "edit",
					"status":        "completed",
					"content": []map[string]any{
						{"type": "diff", "path": "worker.go", "oldText": "v1\n", "newText": "v2\n"},
					},
				},
			},
		},
		{
			"timestamp": h.clock.Add(time.Second).Format(time.RFC3339),
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "sub-1",
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": "Subagent step 1"},
				},
			},
		},
	}
	childDir := h.writeSession("sub-1", cwd, "Explore Worker", "grok-4.7", "high", h.clock, childUpdates, map[string]any{
		"session_kind":      "subagent",
		"parent_session_id": "parent-1",
		"agent_name":        "Explore Worker",
	})
	subMetaDir := filepath.Join(parentDir, "subagents", "sub-1")
	if err := os.MkdirAll(subMetaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metaJSON := mustJSON(map[string]any{
		"subagent_id":       "sub-1",
		"parent_session_id": "parent-1",
		"child_session_id":  "sub-1",
		"child_cwd":         cwd,
		"subagent_type":     "explore",
		"description":       "Explore Worker",
		"status":            "completed",
	})
	if err := os.WriteFile(filepath.Join(subMetaDir, "meta.json"), metaJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	// BEFORE calling h.mgr.refresh(), sub-1 is missing from m.records:
	// rejectSubagentControl MUST refresh and STILL reject every control op!
	for _, op := range []string{"send", "stop", "wake", "settings", "approve", "answer", "compact", "archive", "unarchive", "rename", "fork", "pin", "unpin"} {
		nonce := "guard-" + op
		h.send(h.client, map[string]any{"op": op, "id": "sub-1", "text": "nope", "model": "grok-4.7", "nonce": nonce})
		ack := waitAck(t, h.client, nonce)
		if ack["ok"] != false || !strings.Contains(str(ack, "msg"), "Subagent Chats Are Read Only") {
			t.Fatalf("op %q on subagent must be rejected as read-only, got %+v", op, ack)
		}
	}

	// Direct op: "open" and op: "diff" on subagent MUST be rejected!
	h.send(h.client, map[string]any{"op": "open", "id": "sub-1", "nonce": "direct-open"})
	if ack := waitAck(t, h.client, "direct-open"); ack["ok"] != false || !strings.Contains(str(ack, "msg"), "Parent Chat") {
		t.Fatalf("direct open on subagent must be rejected, got %+v", ack)
	}
	h.send(h.client, map[string]any{"op": "diff", "id": "sub-1", "key": "", "nonce": "direct-diff"})
	if ack := waitAck(t, h.client, "direct-diff"); ack["ok"] != false || !strings.Contains(str(ack, "msg"), "Parent Chat") {
		t.Fatalf("direct diff on subagent must be rejected, got %+v", ack)
	}

	// Top-level threads excludes sub-1; op: "agents" lists sub-1 under parent-1.
	h.send(h.client, map[string]any{"op": "agents", "id": "parent-1", "nonce": "list-agents"})
	ackAgents := waitAck(t, h.client, "list-agents")
	agents := arrayValue(ackAgents["agents"])
	if ackAgents["ok"] != true || len(agents) != 1 || str(mapValue(agents[0]), "id") != "sub-1" {
		t.Fatalf("unexpected agents list: %+v", ackAgents)
	}

	// op: "agentOpen" opens sub-1 read-only and enables workWatch live tail + diff.
	drainFrames(h.client)
	h.send(h.client, map[string]any{"op": "agentOpen", "parent": "parent-1", "id": "sub-1", "nonce": "open-agent"})
	agentOpenFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "provider") == "grok" && str(m, "id") == "sub-1"
	})
	agentMeta := mapValue(agentOpenFrame["meta"])
	if agentMeta["readOnly"] != true || str(agentMeta, "mode") != "tail" {
		t.Fatalf("expected readOnly=true mode=tail for agentOpen, got %+v", agentMeta)
	}
	if ack := waitAck(t, h.client, "open-agent"); ack["ok"] != true {
		t.Fatalf("agentOpen ack failed: %+v", ack)
	}

	// Diff works through active workWatch!
	h.send(h.client, map[string]any{"op": "diff", "id": "sub-1", "key": "tc-sub-diff"})
	subDiff := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "diff" && str(m, "id") == "sub-1"
	})
	if !strings.Contains(str(subDiff, "diff"), "+v2") {
		t.Fatalf("expected subagent diff via workWatch, got %+v", subDiff)
	}

	// Appending to child updates.jsonl pushes a live tail update via pollWork!
	f, err := os.OpenFile(filepath.Join(childDir, "updates.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(mustJSON(map[string]any{
		"timestamp": h.clock.Add(2 * time.Second).Format(time.RFC3339),
		"method":    "session/update",
		"params": map[string]any{
			"sessionId": "sub-1",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": " and step 2"},
			},
		},
	}))
	_, _ = f.Write([]byte("\n"))
	_ = f.Close()

	h.mgr.pollWork()
	updatedWork := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "sub-1" && strings.Contains(string(mustJSON(m)), "and step 2")
	})
	if mapValue(updatedWork["meta"])["readOnly"] != true {
		t.Fatalf("live tail update must remain readOnly=true: %+v", updatedWork["meta"])
	}
}

func TestGrokManagerPrewarmAndCleanRunCtxShutdown(t *testing.T) {
	h := newGrokTestHarness(t, true)
	cwd := t.TempDir()
	h.writeSession("sess-shut", cwd, "Shutdown Chat", "grok-4.7", "high", h.clock, nil, nil)

	var dialCount atomic.Int32
	var listSessionsCount atomic.Int32
	var closeSessionCount atomic.Int32
	var clientClosed atomic.Bool

	h.mgr.prewarmOnStart = true
	h.mgr.dialACP = func(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
		dialCount.Add(1)
		origOnClose := cfg.OnClose
		cfg.OnClose = func(err error) {
			clientClosed.Store(true)
			if origOnClose != nil {
				origOnClose(err)
			}
		}
		srv, cli := newMockGrokACPServer(t, cfg)
		setupStandardMockGrokServer(t, srv)
		srv.on("session/list", func(msg mockACPRPCMessage) {
			listSessionsCount.Add(1)
			_ = srv.sendResult(msg.ID, map[string]any{"sessions": []any{}})
		})
		srv.on("session/resume", func(msg mockACPRPCMessage) {
			var p map[string]any
			_ = json.Unmarshal(msg.Params, &p)
			_ = srv.sendResult(msg.ID, mockGrokSessionStatePayload(str(p, "sessionId"), "grok-4.7", "high"))
		})
		srv.on("session/close", func(msg mockACPRPCMessage) {
			closeSessionCount.Add(1)
			_ = srv.sendResult(msg.ID, map[string]any{"_meta": map[string]any{"x.ai/closeOutcome": "closed"}})
		})
		srv.start()
		t.Cleanup(srv.close)
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.mgr.run(runCtx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for (dialCount.Load() == 0 || listSessionsCount.Load() == 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if dialCount.Load() != 1 || listSessionsCount.Load() == 0 {
		t.Fatalf("expected prewarm to dial once and run session/list, dials=%d lists=%d", dialCount.Load(), listSessionsCount.Load())
	}

	// Attach a session on the prewarmed daemon, then cancel runCtx.
	if _, err := h.mgr.ensureRun(context.Background(), "sess-shut"); err != nil {
		t.Fatalf("ensureRun sess-shut: %v", err)
	}
	if dialCount.Load() != 1 {
		t.Fatalf("expected ensureRun to reuse prewarmed daemon, dials=%d", dialCount.Load())
	}

	cancelRun()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("GrokManager.run did not exit cleanly after runCtx cancel")
	}

	if closeSessionCount.Load() != 1 {
		t.Fatalf("expected shutdown to send session/close for loaded run, got %d", closeSessionCount.Load())
	}
	if !clientClosed.Load() {
		t.Fatal("expected shutdown to close pooled ACP client")
	}
}
