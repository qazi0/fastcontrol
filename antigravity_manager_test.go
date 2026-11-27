package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type agyTestHarness struct {
	t         *testing.T
	home      string
	geminiDir string
	statePath string
	hub       *Hub
	mgr       *AntigravityManager
	client    *Client
	evClient  *Client

	mu    sync.Mutex
	clock time.Time
}

func newAgyTestHarness(t *testing.T) *agyTestHarness {
	t.Helper()
	home := t.TempDir()
	geminiDir := filepath.Join(home, ".gemini")
	statePath := filepath.Join(home, ".fastcontrol", "antigravity-state.json")
	state, err := newAntigravityState(statePath)
	if err != nil {
		t.Fatalf("newAntigravityState: %v", err)
	}

	ipc := NewIPCClient(filepath.Join(home, "unused.sock"))
	hub := NewHub(ipc, "")

	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	h := &agyTestHarness{
		t:         t,
		home:      home,
		geminiDir: geminiDir,
		statePath: statePath,
		hub:       hub,
		clock:     now,
	}

	mgr := &AntigravityManager{
		hub:                      hub,
		bin:                      "/fake/agy",
		geminiDir:                geminiDir,
		customGeminiDir:          true,
		state:                    state,
		supportsACP:              func(string) bool { return true },
		now:                      func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.clock },
		recencyBroadcastInterval: 15 * time.Second,
		idleSessionTimeout:       12 * time.Minute,
		modelsCacheTTL:           5 * time.Minute,
		pollInterval:             20 * time.Millisecond,
		workPollInterval:         20 * time.Millisecond,
		records:                  map[string]antigravityRecord{},
		runs:                     map[string]*antigravityRun{},
		watch:                    map[*Client]map[string]int{},
		workWatch:                map[*Client]map[string]antigravityWorkWatch{},
		watchedStamp:             map[string]antigravityWatchStamp{},
		gates:                    map[string]*antigravityGate{},
		orgState:                 emptyOrganization(),
	}
	hub.antigravity = mgr
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

func (h *agyTestHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.clock = h.clock.Add(d)
	h.mu.Unlock()
}

func (h *agyTestHarness) send(c *Client, payload map[string]any) {
	h.t.Helper()
	if _, ok := payload["provider"]; !ok {
		payload["provider"] = "antigravity"
	}
	h.hub.handle(c, mustJSON(payload))
}

func waitFrame(t *testing.T, c *Client, timeout time.Duration, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case raw := <-c.out:
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil && match(m) {
				return m
			}
		case <-deadline:
			t.Fatalf("timed out waiting for matching frame")
		}
	}
}

func waitAck(t *testing.T, c *Client, nonce string) map[string]any {
	t.Helper()
	return waitFrame(t, c, 5*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "ack" && str(m, "nonce") == nonce
	})
}

func drainFrames(c *Client) []map[string]any {
	var out []map[string]any
	for {
		select {
		case raw := <-c.out:
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				out = append(out, m)
			}
		default:
			return out
		}
	}
}

func mockStandardSessionState(sessionID, model, thought, mode string) map[string]any {
	return map[string]any{
		"sessionId": sessionID,
		"models": map[string]any{
			"currentModelId": model + "-" + thought,
			"availableModels": []map[string]any{
				{"modelId": "gemini-4-neon-high", "name": "Gemini 4 Neon (High)"},
			},
		},
		"modes": map[string]any{
			"currentModeId": mode,
			"availableModes": []map[string]any{
				{"id": "default", "name": "Default", "description": "Asks before edits and commands"},
				{"id": "accept-edits", "name": "Accept Edits", "description": "Approves file edits automatically"},
				{"id": "auto", "name": "Auto", "description": "Runs approved actions automatically"},
				{"id": "plan", "name": "Plan", "description": "Plans without making edits"},
			},
		},
		"configOptions": []map[string]any{
			{
				"id":           "model",
				"name":         "Model",
				"category":     "model",
				"type":         "select",
				"currentValue": model,
				"options": []map[string]any{
					{"value": "gemini-4-neon", "name": "Gemini 4 Neon", "description": "Flagship model"},
					{"value": "gemini-3.8-flash", "name": "Gemini 3.8 Flash", "description": "Fast model"},
				},
			},
			{
				"id":           "thought_level",
				"name":         "Thought Level",
				"category":     "thought_level",
				"type":         "select",
				"currentValue": thought,
				"options": []map[string]any{
					{"value": "low", "name": "Low"},
					{"value": "medium", "name": "Medium"},
					{"value": "high", "name": "High"},
					{"value": "xhigh", "name": "Extra High"},
				},
			},
			{
				"id":           "mode",
				"name":         "Mode",
				"category":     "mode",
				"type":         "select",
				"currentValue": mode,
				"options": []map[string]any{
					{"value": "default", "name": "Default"},
					{"value": "accept-edits", "name": "Accept Edits"},
					{"value": "auto", "name": "Auto"},
					{"value": "plan", "name": "Plan"},
				},
			},
		},
		"_meta": map[string]any{
			"antigravity": map[string]any{
				"tier": map[string]any{
					"id":   "g1-ultra-tier",
					"name": "Google AI Ultra",
				},
			},
		},
	}
}

func mockInitResultPayload() map[string]any {
	return map[string]any{
		"protocolVersion": 1,
		"agentInfo":       map[string]any{"name": "antigravity", "version": "1.5.18"},
		"agentCapabilities": map[string]any{
			"loadSession": true,
			"promptCapabilities": map[string]any{
				"image": true,
			},
			"sessionCapabilities": map[string]any{
				"load":   map[string]any{},
				"resume": map[string]any{},
				"close":  map[string]any{},
				"list":   map[string]any{},
			},
		},
		"_meta": map[string]any{
			"antigravity": map[string]any{
				"tier": map[string]any{
					"id":   "g1-ultra-tier",
					"name": "Google AI Ultra",
				},
			},
		},
	}
}

func TestAntigravityManagerAvailabilityAndHubRouting(t *testing.T) {
	h := newAgyTestHarness(t)

	// Available when agy supports ACP.
	if !h.mgr.available() {
		t.Fatalf("expected available=true when binary supports ACP")
	}

	// Not available when binary is missing and no DB exists.
	h.mgr.bin = ""
	if h.mgr.available() {
		t.Fatalf("expected available=false with missing binary and no DB")
	}

	// Available for read-only browsing when local DB exists even without agy binary.
	dbPath := antigravityDBPath(h.geminiDir, antigravityAppDirDesktop)
	createAntigravityTestDB(t, dbPath, []antigravityTestRow{
		{id: "conv-ro", title: "Read Only Chat", lastMod: "2026-11-27T14:55:00Z", status: "CASCADE_RUN_STATUS_IDLE"},
	})
	if !h.mgr.available() {
		t.Fatalf("expected available=true when local Antigravity DB exists")
	}

	// Unsupported binary cannot run live ops, returns clear error on wake/new.
	h.send(h.client, map[string]any{"op": "wake", "id": "conv-ro", "nonce": "wake-no-bin"})
	ack := waitAck(t, h.client, "wake-no-bin")
	if ok, _ := ack["ok"].(bool); ok {
		t.Fatalf("expected wake to fail without agy binary")
	}

	// Unknown provider rejected by Hub.
	h.hub.handle(h.client, mustJSON(map[string]any{"provider": "unknown-agent", "op": "threads", "nonce": "bad-prov"}))
	ack = waitAck(t, h.client, "bad-prov")
	if ok, _ := ack["ok"].(bool); ok || str(ack, "msg") != "Unknown Provider" {
		t.Fatalf("expected Unknown Provider ack, got %+v", ack)
	}

	// op: "events" is handled by Hub directly even when provider is "antigravity".
	h.send(h.client, map[string]any{"op": "events", "on": true, "nonce": "ev-on"})
	ack = waitAck(t, h.client, "ev-on")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("expected events ack ok, got %+v", ack)
	}

	// When hub.antigravity is nil, Hub returns clean unavailable ack.
	saved := h.hub.antigravity
	h.hub.antigravity = nil
	h.hub.handle(h.client, mustJSON(map[string]any{"provider": "antigravity", "op": "threads", "nonce": "nil-mgr"}))
	ack = waitAck(t, h.client, "nil-mgr")
	h.hub.antigravity = saved
	if ok, _ := ack["ok"].(bool); ok || str(ack, "provider") != "antigravity" {
		t.Fatalf("expected antigravity unavailable ack, got %+v", ack)
	}

	// removeClient cleans up watch and workWatch subscriptions.
	h.mgr.mu.Lock()
	h.mgr.watch[h.client] = map[string]int{"conv-ro": 15}
	h.mgr.workWatch[h.client] = map[string]antigravityWorkWatch{"sub-1": {Parent: "conv-ro"}}
	h.mgr.mu.Unlock()
	h.hub.removeClient(h.client)
	h.mgr.mu.Lock()
	_, hasWatch := h.mgr.watch[h.client]
	_, hasWork := h.mgr.workWatch[h.client]
	h.mgr.mu.Unlock()
	if hasWatch || hasWork {
		t.Fatalf("expected removeClient to clear watch and workWatch")
	}
}

func TestAntigravityManagerListProjectsGroupsOrgAndThrottle(t *testing.T) {
	h := newAgyTestHarness(t)
	wsA := filepath.Join(h.home, "repo-alpha")
	wsB := filepath.Join(h.home, "repo-beta")
	_ = os.MkdirAll(wsA, 0700)
	_ = os.MkdirAll(wsB, 0700)

	dbPath := antigravityDBPath(h.geminiDir, antigravityAppDirDesktop)
	createAntigravityTestDB(t, dbPath, []antigravityTestRow{
		{
			id:            "conv-1",
			title:         "Alpha Chat",
			preview:       "Initial alpha preview",
			lastMod:       "2026-11-27T14:58:00Z",
			workspaceURIs: fmt.Sprintf("[%q]", "file://"+filepath.ToSlash(wsA)),
			status:        "CASCADE_RUN_STATUS_IDLE",
			groupID:       "grp-manual",
		},
		{
			id:            "conv-2",
			title:         "Beta Chat",
			preview:       "Initial beta preview",
			lastMod:       "2026-11-27T14:59:00Z",
			workspaceURIs: fmt.Sprintf("[%q]", "file://"+filepath.ToSlash(wsB)),
			status:        "CASCADE_RUN_STATUS_IDLE",
		},
		{
			id:           "sub-1",
			title:        "Hidden Subagent",
			lastMod:      "2026-11-27T14:59:30Z",
			parentID:     "conv-1",
			nestingDepth: 1,
			agentName:    "Worker",
			status:       "CASCADE_RUN_STATUS_IDLE",
		},
	})

	// Initial refresh broadcasts threads list excluding subagents.
	h.mgr.refresh()
	frame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "threads" && str(m, "provider") == "antigravity"
	})
	threads := arrayValue(frame["threads"])
	if len(threads) != 2 {
		t.Fatalf("expected 2 top-level threads (excluding subagent), got %d", len(threads))
	}
	if str(mapValue(threads[0]), "id") != "conv-2" {
		t.Fatalf("expected newest thread conv-2 first, got %s", str(mapValue(threads[0]), "id"))
	}

	// Pure Recency-only update within 15s throttle must NOT broadcast, though lastList updates.
	drainFrames(h.client)
	h.advance(5 * time.Second)
	createAntigravityTestDB(t, dbPath+".tmp", nil) // unused helper check not needed; update in-place via new DB
	_ = os.Remove(dbPath + ".tmp")
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	createAntigravityTestDB(t, dbPath, []antigravityTestRow{
		{
			id:            "conv-1",
			title:         "Alpha Chat",
			preview:       "Initial alpha preview",
			lastMod:       "2026-11-27T14:58:10Z", // slightly newer, same order
			workspaceURIs: fmt.Sprintf("[%q]", "file://"+filepath.ToSlash(wsA)),
			status:        "CASCADE_RUN_STATUS_IDLE",
			groupID:       "grp-manual",
		},
		{
			id:            "conv-2",
			title:         "Beta Chat",
			preview:       "Initial beta preview",
			lastMod:       "2026-11-27T14:59:00Z",
			workspaceURIs: fmt.Sprintf("[%q]", "file://"+filepath.ToSlash(wsB)),
			status:        "CASCADE_RUN_STATUS_IDLE",
		},
		{
			id:           "sub-1",
			title:        "Hidden Subagent",
			lastMod:      "2026-11-27T14:59:30Z",
			parentID:     "conv-1",
			nestingDepth: 1,
			agentName:    "Worker",
			status:       "CASCADE_RUN_STATUS_IDLE",
		},
	})
	h.mgr.refresh()
	if frames := drainFrames(h.client); len(frames) != 0 {
		t.Fatalf("expected pure recency-only update within 15s to be throttled, got %d frames", len(frames))
	}

	// Advancing past 15s flushes the recency update.
	h.advance(16 * time.Second)
	h.mgr.refresh()
	_ = waitFrame(t, h.client, 2*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "threads" && str(m, "provider") == "antigravity"
	})

	// Explicit op:"threads" returns cached list immediately.
	drainFrames(h.client)
	h.send(h.client, map[string]any{"op": "threads"})
	_ = waitFrame(t, h.client, 2*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "threads" && str(m, "provider") == "antigravity"
	})

	// Pin conv-1: immediate broadcast (within throttle window!) and reordered to top.
	drainFrames(h.client)
	h.advance(1 * time.Second)
	h.send(h.client, map[string]any{"op": "pin", "id": "conv-1", "nonce": "pin-1"})
	pinnedFrame := waitFrame(t, h.client, 2*time.Second, func(m map[string]any) bool {
		if str(m, "t") != "threads" {
			return false
		}
		ts := arrayValue(m["threads"])
		return len(ts) == 2 && str(mapValue(ts[0]), "id") == "conv-1" && mapValue(ts[0])["pinned"] == true
	})
	if len(arrayValue(pinnedFrame["threads"])) != 2 {
		t.Fatalf("unexpected thread count after pin")
	}
	ack := waitAck(t, h.client, "pin-1")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("pin failed: %+v", ack)
	}

	// Rename conv-1 immediately broadcasts displayTitle.
	h.send(h.client, map[string]any{"op": "rename", "id": "conv-1", "text": "Renamed Alpha", "nonce": "ren-1"})
	ack = waitAck(t, h.client, "ren-1")
	if ok, _ := ack["ok"].(bool); !ok || str(ack, "title") != "Renamed Alpha" {
		t.Fatalf("rename failed: %+v", ack)
	}

	// Projects and project filtering.
	h.send(h.client, map[string]any{"op": "projects"})
	projFrame := waitFrame(t, h.client, 2*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "projects" && str(m, "provider") == "antigravity"
	})
	if len(arrayValue(projFrame["projects"])) != 2 {
		t.Fatalf("expected 2 projects, got %+v", projFrame["projects"])
	}

	h.send(h.client, map[string]any{"op": "project", "cwd": wsA})
	projThreads := waitFrame(t, h.client, 2*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "project" && str(m, "cwd") == wsA
	})
	if ts := arrayValue(projThreads["threads"]); len(ts) != 1 || str(mapValue(ts[0]), "id") != "conv-1" {
		t.Fatalf("expected only conv-1 for project wsA, got %+v", ts)
	}

	// Group query via organizationMembers.
	h.send(h.client, map[string]any{"op": "group", "id": "antigravity:group:grp-manual", "nonce": "grp-1"})
	ack = waitAck(t, h.client, "grp-1")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("group failed: %+v", ack)
	}
	if ts := arrayValue(ack["threads"]); len(ts) != 1 || str(mapValue(ts[0]), "id") != "conv-1" {
		t.Fatalf("expected conv-1 in manual group, got %+v", ts)
	}

	// Archive blocked when a session is actively running.
	liveRun := newAntigravityRun(h.mgr, "conv-2", antigravityAppDirDesktop, wsB)
	liveRun.mu.Lock()
	liveRun.running = true
	liveRun.mu.Unlock()
	h.mgr.mu.Lock()
	h.mgr.runs["conv-2"] = liveRun
	h.mgr.mu.Unlock()

	h.send(h.client, map[string]any{"op": "archive", "id": "conv-2", "nonce": "arch-run"})
	ack = waitAck(t, h.client, "arch-run")
	if ok, _ := ack["ok"].(bool); ok || !strings.Contains(str(ack, "msg"), "Stop") {
		t.Fatalf("expected archive to be blocked while running, got %+v", ack)
	}

	// Stop running flag, then archive and unarchive succeed.
	h.mgr.mu.Lock()
	delete(h.mgr.runs, "conv-2")
	h.mgr.mu.Unlock()

	h.send(h.client, map[string]any{"op": "archive", "id": "conv-2", "nonce": "arch-ok"})
	ack = waitAck(t, h.client, "arch-ok")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("expected archive ok, got %+v", ack)
	}
	if !h.mgr.state.IsArchived("conv-2") {
		t.Fatalf("expected conv-2 archived in state")
	}

	h.send(h.client, map[string]any{"op": "unarchive", "id": "conv-2", "nonce": "unarch-ok"})
	ack = waitAck(t, h.client, "unarch-ok")
	if ok, _ := ack["ok"].(bool); !ok || h.mgr.state.IsArchived("conv-2") {
		t.Fatalf("expected unarchive ok, got %+v", ack)
	}

	// Unpin conv-1.
	h.send(h.client, map[string]any{"op": "unpin", "id": "conv-1", "nonce": "unpin-1"})
	ack = waitAck(t, h.client, "unpin-1")
	if ok, _ := ack["ok"].(bool); !ok || h.mgr.state.IsPinned("conv-1") {
		t.Fatalf("expected unpin ok, got %+v", ack)
	}
}

func TestAntigravityManagerModelsDiscoveryProbeAndCaching(t *testing.T) {
	h := newAgyTestHarness(t)
	ws := filepath.Join(h.home, "workspace")
	_ = os.MkdirAll(ws, 0700)

	dbPath := antigravityDBPath(h.geminiDir, antigravityAppDirDesktop)
	createAntigravityTestDB(t, dbPath, []antigravityTestRow{
		{
			id:            "idle-conv",
			title:         "Idle Chat",
			lastMod:       "2026-11-27T14:50:00Z",
			workspaceURIs: fmt.Sprintf("[%q]", "file://"+filepath.ToSlash(ws)),
			status:        "CASCADE_RUN_STATUS_IDLE",
		},
	})
	h.mgr.refresh()

	var dials, loadCalls, newCalls, closeCalls, promptCalls atomic.Int32
	h.mgr.dialACP = func(ctx context.Context, cfg acpClientConfig) (*acpClient, *acpInitResult, error) {
		dials.Add(1)
		srv, cli := newMockACPServer(t, cfg)
		srv.on("initialize", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockInitResultPayload())
		})
		srv.on("session/load", func(msg mockACPRPCMessage) {
			loadCalls.Add(1)
			_ = srv.sendResult(msg.ID, mockStandardSessionState("idle-conv", "gemini-4-neon", "high", "default"))
		})
		srv.on("session/new", func(msg mockACPRPCMessage) {
			newCalls.Add(1)
			_ = srv.sendResult(msg.ID, mockStandardSessionState("temp-session", "gemini-4-neon", "medium", "default"))
		})
		srv.on("session/close", func(msg mockACPRPCMessage) {
			closeCalls.Add(1)
			_ = srv.sendResult(msg.ID, map[string]any{})
		})
		srv.on("session/prompt", func(msg mockACPRPCMessage) {
			promptCalls.Add(1)
			_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "end_turn"})
		})
		srv.start()
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	// First models query should probe via session/load + session/close without session/new or session/prompt!
	h.send(h.client, map[string]any{"op": "models"})
	mFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "models" && str(m, "provider") == "antigravity"
	})
	if loadCalls.Load() != 1 || newCalls.Load() != 0 || closeCalls.Load() != 1 || promptCalls.Load() != 0 {
		t.Fatalf("unexpected probe calls: load=%d new=%d close=%d prompt=%d",
			loadCalls.Load(), newCalls.Load(), closeCalls.Load(), promptCalls.Load())
	}
	if models := arrayValue(mFrame["models"]); len(models) != 2 {
		t.Fatalf("expected 2 models from configOptions, got %+v", models)
	}
	def := mapValue(mFrame["default"])
	if str(def, "model") != "gemini-4-neon" || str(def, "effort") != "high" || str(def, "mode") != "default" {
		t.Fatalf("unexpected default: %+v", def)
	}
	if mFrame["allowFullAccess"] != false {
		t.Fatalf("expected allowFullAccess=false, got %v", mFrame["allowFullAccess"])
	}
	if len(arrayValue(mFrame["modes"])) != 4 {
		t.Fatalf("expected 4 modes, got %+v", mFrame["modes"])
	}
	if tier := mapValue(mFrame["tier"]); str(tier, "name") != "Google AI Ultra" {
		t.Fatalf("expected tier Google AI Ultra, got %+v", tier)
	}

	// Cached within 5 minutes: second request must not dial again.
	h.advance(2 * time.Minute)
	h.send(h.client, map[string]any{"op": "models"})
	_ = waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "models" && str(m, "provider") == "antigravity"
	})
	if dials.Load() != 1 {
		t.Fatalf("expected cached models without re-dial, got dials=%d", dials.Load())
	}

	// When zero sessions exist and cache expires, falls back to session/new in TempDir + session/close.
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	h.mgr.refresh()
	h.advance(6 * time.Minute)
	h.send(h.client, map[string]any{"op": "models"})
	_ = waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "models" && str(m, "provider") == "antigravity"
	})
	if newCalls.Load() != 1 || closeCalls.Load() != 2 || promptCalls.Load() != 0 {
		t.Fatalf("expected fallback session/new+close without prompt, got new=%d close=%d prompt=%d",
			newCalls.Load(), closeCalls.Load(), promptCalls.Load())
	}
}

func TestAntigravityManagerNewStreamDiffSteeringAndSettings(t *testing.T) {
	h := newAgyTestHarness(t)
	t.Setenv("HOME", h.home)
	t.Setenv("USERPROFILE", h.home)

	ws := filepath.Join(h.home, "project")
	_ = os.MkdirAll(ws, 0700)

	// Prepare a valid uploaded PNG image in ~/.fastcontrol/uploads.
	upDir := uploadDir()
	_ = os.MkdirAll(upDir, 0700)
	pngBytes := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}
	imgPath := filepath.Join(upDir, "test-shot.png")
	if err := os.WriteFile(imgPath, pngBytes, 0600); err != nil {
		t.Fatal(err)
	}

	// Reject non-upload image path before touching ACP.
	outsideImg := filepath.Join(h.home, "outside.png")
	_ = os.WriteFile(outsideImg, pngBytes, 0600)
	h.send(h.client, map[string]any{"op": "new", "cwd": ws, "text": "hi", "images": []string{outsideImg}, "nonce": "bad-img"})
	ack := waitAck(t, h.client, "bad-img")
	if ok, _ := ack["ok"].(bool); ok {
		t.Fatalf("expected non-upload image to be rejected")
	}

	// Reject full-access mode when allowFullAccess is false.
	h.send(h.client, map[string]any{"op": "new", "cwd": ws, "text": "hi", "permissionMode": "full-access", "nonce": "bad-mode"})
	ack = waitAck(t, h.client, "bad-mode")
	if ok, _ := ack["ok"].(bool); ok {
		t.Fatalf("expected full-access to be rejected when disabled")
	}

	var (
		mu             sync.Mutex
		setConfigs     []string
		setModes       []string
		promptCount    int
		promptBlocks   [][]acpContentBlock
		releasePrompt1 = make(chan struct{})
		prompt1Started = make(chan struct{})
		prompt2Seen    = make(chan struct{})
		activeSrv      *mockACPServer
	)

	h.mgr.dialACP = func(ctx context.Context, cfg acpClientConfig) (*acpClient, *acpInitResult, error) {
		srv, cli := newMockACPServer(t, cfg)
		mu.Lock()
		activeSrv = srv
		mu.Unlock()

		srv.on("initialize", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockInitResultPayload())
		})
		srv.on("session/new", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockStandardSessionState("sess-new-1", "gemini-4-neon", "high", "default"))
		})
		srv.on("session/set_config_option", func(msg mockACPRPCMessage) {
			var p struct {
				ConfigID string `json:"configId"`
				Value    string `json:"value"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			mu.Lock()
			setConfigs = append(setConfigs, p.ConfigID+"="+p.Value)
			mu.Unlock()
			_ = srv.sendResult(msg.ID, map[string]any{})
		})
		srv.on("session/set_mode", func(msg mockACPRPCMessage) {
			var p struct {
				ModeID string `json:"modeId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			mu.Lock()
			setModes = append(setModes, p.ModeID)
			mu.Unlock()
			_ = srv.sendResult(msg.ID, map[string]any{})
		})
		srv.on("session/prompt", func(msg mockACPRPCMessage) {
			var p struct {
				SessionID string            `json:"sessionId"`
				Prompt    []acpContentBlock `json:"prompt"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			mu.Lock()
			promptCount++
			n := promptCount
			promptBlocks = append(promptBlocks, p.Prompt)
			mu.Unlock()

			if n == 1 {
				// Stream rich updates while prompt 1 is running.
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": p.SessionID,
					"update": map[string]any{
						"sessionUpdate": "available_commands_update",
						"availableCommands": []map[string]any{
							{"name": "compact", "description": "Compact context"},
							{"name": "goal", "description": "Run until done", "input": map[string]any{"hint": "<goal>"}},
						},
					},
				})
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": p.SessionID,
					"update": map[string]any{
						"sessionUpdate": "session_info_update",
						"title":         "Streamed Title",
					},
				})
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": p.SessionID,
					"update": map[string]any{
						"sessionUpdate": "usage_update",
						"used":          42000,
						"size":          1048576,
					},
				})
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": p.SessionID,
					"update": map[string]any{
						"sessionUpdate": "agent_thought_chunk",
						"content":       map[string]any{"type": "text", "text": "Thinking part 1. "},
					},
				})
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": p.SessionID,
					"update": map[string]any{
						"sessionUpdate": "agent_thought_chunk",
						"content":       map[string]any{"type": "text", "text": "Part 2."},
					},
				})
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": p.SessionID,
					"update": map[string]any{
						"sessionUpdate": "tool_call",
						"toolCallId":    "tc-cmd-1",
						"name":          "run_command",
						"kind":          "execute",
						"status":        "in_progress",
						"rawInput":      map[string]any{"command": "echo ok"},
					},
				})
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": p.SessionID,
					"update": map[string]any{
						"sessionUpdate": "tool_call_update",
						"toolCallId":    "tc-cmd-1",
						"status":        "completed",
						"content":       []map[string]any{{"type": "text", "text": "ok\n"}},
					},
				})
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": p.SessionID,
					"update": map[string]any{
						"sessionUpdate": "tool_call",
						"toolCallId":    "tc-diff-1",
						"name":          "edit_file",
						"kind":          "edit",
						"status":        "completed",
						"content": []map[string]any{{
							"type":    "diff",
							"path":    "main.go",
							"oldText": "line1\nold\n",
							"newText": "line1\nnew\n",
						}},
					},
				})
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": p.SessionID,
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": "Hello "},
					},
				})
				close(prompt1Started)
				<-releasePrompt1
				_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "end_turn"})
				return
			}

			// Prompt 2 (live steering while prompt 1 is running).
			_ = srv.sendNotify("session/update", map[string]any{
				"sessionId": p.SessionID,
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": "Steered reply!"},
				},
			})
			close(prompt2Seen)
			<-releasePrompt1
			_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "end_turn"})
		})
		srv.on("session/close", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, map[string]any{})
		})
		srv.start()
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	// Create new chat with model, effort, permissionMode, and uploaded image.
	h.send(h.client, map[string]any{
		"op":             "new",
		"cwd":            ws,
		"text":           "Explain this screenshot",
		"model":          "gemini-3.8-flash",
		"effort":         "medium",
		"permissionMode": "accept-edits",
		"images":         []string{imgPath},
		"nonce":          "new-1",
	})
	ack = waitAck(t, h.client, "new-1")
	if ok, _ := ack["ok"].(bool); !ok || str(ack, "thread") != "sess-new-1" {
		t.Fatalf("new failed: %+v", ack)
	}

	// Verify started event arrived on events client.
	ev := waitFrame(t, h.evClient, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "event" && str(m, "provider") == "antigravity" && str(m, "kind") == "started"
	})
	if str(ev, "id") != "sess-new-1" || str(ev, "cwd") != ws {
		t.Fatalf("unexpected started event: %+v", ev)
	}

	// Open the live session and wait for prompt 1 streamed chunks.
	h.send(h.client, map[string]any{"op": "open", "id": "sess-new-1", "nonce": "open-1"})
	ack = waitAck(t, h.client, "open-1")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("open failed: %+v", ack)
	}
	<-prompt1Started

	// Send live steering message while prompt 1 is still active!
	h.send(h.client, map[string]any{"op": "send", "id": "sess-new-1", "text": "Also add tests", "nonce": "steer-1"})
	ack = waitAck(t, h.client, "steer-1")
	if ok, _ := ack["ok"].(bool); !ok || ack["queued"] != false {
		t.Fatalf("expected live steering queued=false, got %+v", ack)
	}
	<-prompt2Seen

	// Verify image block was sent in prompt 1.
	mu.Lock()
	if len(promptBlocks) < 2 || len(promptBlocks[0]) != 2 || promptBlocks[0][1].Type != "image" || promptBlocks[0][1].MimeType != "image/png" {
		mu.Unlock()
		t.Fatalf("expected text+image blocks in prompt 1, got %+v", promptBlocks)
	}
	mu.Unlock()

	// Finish both prompts and verify completed event!
	close(releasePrompt1)
	compEv := waitFrame(t, h.evClient, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "event" && str(m, "provider") == "antigravity" && str(m, "kind") == "completed"
	})
	if str(compEv, "id") != "sess-new-1" {
		t.Fatalf("unexpected completed event: %+v", compEv)
	}

	// Re-open to inspect full merged live state (commands, ctx, title, reasoning, cmd, file diff, assistant).
	drainFrames(h.client)
	h.send(h.client, map[string]any{"op": "open", "id": "sess-new-1", "nonce": "open-2"})
	openFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "sess-new-1"
	})
	meta := mapValue(openFrame["meta"])
	if str(meta, "title") != "Streamed Title" {
		t.Fatalf("expected title Streamed Title, got %q", str(meta, "title"))
	}
	if used, _ := num(meta, "ctxUsed"); used != 42000 {
		t.Fatalf("expected ctxUsed 42000, got %d", used)
	}
	if maxCtx, _ := num(meta, "ctxMax"); maxCtx != 1048576 {
		t.Fatalf("expected ctxMax 1048576, got %d", maxCtx)
	}
	if cmds := arrayValue(meta["commands"]); len(cmds) != 2 {
		t.Fatalf("expected 2 slash commands, got %+v", cmds)
	}

	turns := arrayValue(openFrame["turns"])
	if len(turns) != 1 {
		t.Fatalf("expected 1 combined live turn, got %d", len(turns))
	}
	turnMap := mapValue(turns[0])
	turnKey := str(turnMap, "key")
	if turnMap["hasDiff"] != true {
		t.Fatalf("expected turn hasDiff=true")
	}
	items := arrayValue(turnMap["items"])
	var hasThought, hasCmd, hasFile, hasAssistant bool
	for _, rawItem := range items {
		it := mapValue(rawItem)
		switch str(it, "k") {
		case "reasoning":
			if str(it, "text") == "Thinking part 1. Part 2." {
				hasThought = true
			}
		case "cmd":
			if str(it, "status") == "completed" && strings.Contains(str(it, "body"), "ok") {
				hasCmd = true
			}
		case "file":
			if strings.Contains(str(it, "body"), "-old") && strings.Contains(str(it, "body"), "+new") {
				hasFile = true
			}
		case "assistant":
			if strings.Contains(str(it, "text"), "Steered reply!") {
				hasAssistant = true
			}
		}
	}
	if !hasThought || !hasCmd || !hasFile || !hasAssistant {
		t.Fatalf("missing expected streamed items: thought=%v cmd=%v file=%v assistant=%v items=%+v",
			hasThought, hasCmd, hasFile, hasAssistant, items)
	}

	// Diff lookup by toolCallId, turnKey, and empty key returns both diff and text fields.
	for _, key := range []string{"tc-diff-1", turnKey, ""} {
		h.send(h.client, map[string]any{"op": "diff", "id": "sess-new-1", "key": key})
		df := waitFrame(t, h.client, 2*time.Second, func(m map[string]any) bool {
			return str(m, "t") == "diff" && str(m, "id") == "sess-new-1" && str(m, "key") == key
		})
		if !strings.Contains(str(df, "diff"), "+new") || str(df, "diff") != str(df, "text") {
			t.Fatalf("unexpected diff frame for key %q: %+v", key, df)
		}
	}

	// Settings: update model & mode, then reset effort explicitly to "" (Default) without clobbering model/mode.
	h.send(h.client, map[string]any{"op": "settings", "id": "sess-new-1", "model": "gemini-4-neon", "permissionMode": "plan", "nonce": "set-1"})
	ack = waitAck(t, h.client, "set-1")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("settings 1 failed: %+v", ack)
	}

	// Explicit empty effort resets thought_level back to Default while preserving model & mode.
	drainFrames(h.client)
	h.send(h.client, map[string]any{"op": "settings", "id": "sess-new-1", "effort": "", "nonce": "set-reset-effort"})
	ack = waitAck(t, h.client, "set-reset-effort")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("settings reset effort failed: %+v", ack)
	}
	pref := h.mgr.state.Pref("sess-new-1")
	if pref.Effort != "" || pref.Model != "gemini-4-neon" || pref.PermissionMode != "plan" {
		t.Fatalf("unexpected saved pref after clearing effort: %+v", pref)
	}

	mu.Lock()
	gotConfigs := append([]string(nil), setConfigs...)
	gotModes := append([]string(nil), setModes...)
	_ = activeSrv
	mu.Unlock()
	if !contains(gotConfigs, "thought_level=") || !contains(gotConfigs, "model=gemini-4-neon") || !contains(gotModes, "plan") {
		t.Fatalf("expected live ACP settings updates, configs=%v modes=%v", gotConfigs, gotModes)
	}
}

func TestAntigravityManagerPermissionsElicitationsAndCancel(t *testing.T) {
	h := newAgyTestHarness(t)
	ws := filepath.Join(h.home, "ws-int")
	_ = os.MkdirAll(ws, 0700)

	var (
		perm1Resp   chan mockACPRPCMessage
		perm2Resp   chan mockACPRPCMessage
		elicit1Resp chan mockACPRPCMessage
		elicit2Resp chan mockACPRPCMessage
		cancelSeen  = make(chan struct{}, 2)
		promptDone  = make(chan struct{})
		srvReady    = make(chan *mockACPServer, 1)
	)

	h.mgr.dialACP = func(ctx context.Context, cfg acpClientConfig) (*acpClient, *acpInitResult, error) {
		srv, cli := newMockACPServer(t, cfg)
		srv.on("initialize", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockInitResultPayload())
		})
		srv.on("session/new", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockStandardSessionState("sess-int", "gemini-4-neon", "high", "default"))
		})
		srv.on("session/prompt", func(msg mockACPRPCMessage) {
			srvReady <- srv
			<-promptDone
			_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "cancelled"})
		})
		srv.on("session/cancel", func(msg mockACPRPCMessage) {
			select {
			case cancelSeen <- struct{}{}:
			default:
			}
			select {
			case <-promptDone:
			default:
				close(promptDone)
			}
		})
		srv.on("session/close", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, map[string]any{})
		})
		srv.start()
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	h.send(h.client, map[string]any{"op": "new", "cwd": ws, "text": "Run interactive flow", "nonce": "int-new"})
	ack := waitAck(t, h.client, "int-new")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("new failed: %+v", ack)
	}
	h.send(h.client, map[string]any{"op": "open", "id": "sess-int", "nonce": "int-open"})
	_ = waitAck(t, h.client, "int-open")

	srv := <-srvReady

	// 1. Permission request with explicit optionId (including reject_once!).
	var reqErr error
	perm1Resp, reqErr = srv.sendServerRequest("perm-1", "session/request_permission", map[string]any{
		"sessionId": "sess-int",
		"toolCall": map[string]any{
			"toolCallId": "tc-rm",
			"name":       "run_command",
			"kind":       "execute",
			"title":      "Run rm -rf",
			"rawInput":   map[string]any{"command": "rm -rf /tmp/demo"},
		},
		"options": []map[string]any{
			{"optionId": "opt-allow-once", "name": "Allow Once", "kind": "allow_once"},
			{"optionId": "opt-allow-always", "name": "Always Allow", "kind": "allow_always"},
			{"optionId": "opt-reject", "name": "Reject", "kind": "reject_once"},
		},
	})
	if reqErr != nil {
		t.Fatalf("send perm-1: %v", reqErr)
	}

	metaFrame := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		if str(m, "t") != "meta" {
			return false
		}
		reqs := arrayValue(mapValue(m["meta"])["requests"])
		return len(reqs) == 1 && str(mapValue(reqs[0]), "id") == "perm-1"
	})
	req0 := mapValue(arrayValue(mapValue(metaFrame["meta"])["requests"])[0])
	if str(req0, "kind") != "command" || len(arrayValue(req0["options"])) != 3 {
		t.Fatalf("unexpected permission request wire shape: %+v", req0)
	}
	_ = waitFrame(t, h.evClient, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "event" && str(m, "kind") == "attention" && str(m, "id") == "sess-int"
	})

	// Reply using explicit optionId (opt-reject).
	h.send(h.client, map[string]any{
		"op":       "approve",
		"id":       "sess-int",
		"reqId":    "perm-1",
		"decision": "decline",
		"optionId": "opt-reject",
		"nonce":    "app-1",
	})
	ack = waitAck(t, h.client, "app-1")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("approve 1 failed: %+v", ack)
	}
	resp1 := <-perm1Resp
	if !strings.Contains(string(resp1.Result), `"optionId":"opt-reject"`) {
		t.Fatalf("expected selected opt-reject, got %s", string(resp1.Result))
	}

	// 2. Permission request (file kind) answered via fallback decision="acceptForSession" (no optionId).
	perm2Resp, reqErr = srv.sendServerRequest("perm-2", "session/request_permission", map[string]any{
		"sessionId": "sess-int",
		"toolCall": map[string]any{
			"toolCallId": "tc-edit",
			"name":       "edit_file",
			"kind":       "edit",
			"title":      "Edit config.go",
		},
		"options": []map[string]any{
			{"optionId": "opt-once", "name": "Allow Once", "kind": "allow_once"},
			{"optionId": "opt-always", "name": "Allow Always", "kind": "allow_always"},
		},
	})
	if reqErr != nil {
		t.Fatalf("send perm-2: %v", reqErr)
	}
	metaFrame = waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		if str(m, "t") != "meta" {
			return false
		}
		reqs := arrayValue(mapValue(m["meta"])["requests"])
		return len(reqs) == 1 && str(mapValue(reqs[0]), "id") == "perm-2"
	})
	if str(mapValue(arrayValue(mapValue(metaFrame["meta"])["requests"])[0]), "kind") != "file" {
		t.Fatalf("expected file permission kind")
	}
	h.send(h.client, map[string]any{
		"op":       "approve",
		"id":       "sess-int",
		"reqId":    "perm-2",
		"decision": "acceptForSession",
		"nonce":    "app-2",
	})
	ack = waitAck(t, h.client, "app-2")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("approve 2 failed: %+v", ack)
	}
	resp2 := <-perm2Resp
	if !strings.Contains(string(resp2.Result), `"optionId":"opt-always"`) {
		t.Fatalf("expected allow_always fallback optionId, got %s", string(resp2.Result))
	}

	// 3. Elicitation form with single-select, multi-select, free-text secret, and integer.
	elicit1Resp, reqErr = srv.sendServerRequest("elicit-1", "elicitation/create", map[string]any{
		"sessionId": "sess-int",
		"mode":      "form",
		"message":   "Configure Deployment",
		"requestedSchema": map[string]any{
			"type":     "object",
			"required": []string{"env", "token"},
			"properties": map[string]any{
				"env": map[string]any{
					"type":  "string",
					"title": "Environment",
					"oneOf": []map[string]any{
						{"const": "prod", "title": "Production", "description": "Live traffic"},
						{"const": "staging", "title": "Staging", "description": "Pre-release"},
					},
				},
				"features": map[string]any{
					"type":  "array",
					"title": "Features",
					"items": map[string]any{
						"oneOf": []map[string]any{
							{"const": "auth", "title": "Authentication"},
							{"const": "metrics", "title": "Metrics"},
						},
					},
				},
				"token": map[string]any{
					"type":   "string",
					"title":  "API Password",
					"format": "password",
				},
				"replicas": map[string]any{
					"type":  "integer",
					"title": "Replicas",
				},
			},
		},
	})
	if reqErr != nil {
		t.Fatalf("send elicit-1: %v", reqErr)
	}

	metaFrame = waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		if str(m, "t") != "meta" {
			return false
		}
		reqs := arrayValue(mapValue(m["meta"])["requests"])
		return len(reqs) == 1 && str(mapValue(reqs[0]), "id") == "elicit-1"
	})
	elicitReq := mapValue(arrayValue(mapValue(metaFrame["meta"])["requests"])[0])
	qs := arrayValue(elicitReq["questions"])
	if str(elicitReq, "kind") != "question" || len(qs) != 4 {
		t.Fatalf("unexpected elicitation questions: %+v", elicitReq)
	}
	qByID := map[string]map[string]any{}
	for _, rawQ := range qs {
		qm := mapValue(rawQ)
		qByID[str(qm, "id")] = qm
	}
	if qByID["features"]["multiSelect"] != true || qByID["token"]["freeText"] != true || qByID["token"]["secret"] != true || qByID["replicas"]["freeText"] != true {
		t.Fatalf("expected multiSelect, freeText, and secret flags: %+v", qs)
	}

	// Missing required field returns error without popping pending request.
	h.send(h.client, map[string]any{
		"op":      "answer",
		"id":      "sess-int",
		"reqId":   "elicit-1",
		"answers": map[string]any{"env": []string{"Production"}},
		"nonce":   "ans-missing",
	})
	ack = waitAck(t, h.client, "ans-missing")
	if ok, _ := ack["ok"].(bool); ok {
		t.Fatalf("expected missing required answer to fail")
	}

	// Submit complete answers using UI display labels -> verifies label-to-value mapping and typed JSON!
	h.send(h.client, map[string]any{
		"op":    "answer",
		"id":    "sess-int",
		"reqId": "elicit-1",
		"answers": map[string]any{
			"env":      []string{"Production"},
			"features": []string{"Authentication", "Metrics"},
			"token":    []string{"s3cr3t"},
			"replicas": []string{"3"},
		},
		"nonce": "ans-ok",
	})
	ack = waitAck(t, h.client, "ans-ok")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("answer failed: %+v", ack)
	}
	eResp1 := <-elicit1Resp
	var parsedElicit struct {
		Action  string         `json:"action"`
		Content map[string]any `json:"content"`
	}
	if err := json.Unmarshal(eResp1.Result, &parsedElicit); err != nil {
		t.Fatalf("unmarshal elicitation response: %v", err)
	}
	if parsedElicit.Action != "accept" ||
		parsedElicit.Content["env"] != "prod" ||
		parsedElicit.Content["token"] != "s3cr3t" ||
		parsedElicit.Content["replicas"] != float64(3) {
		t.Fatalf("unexpected elicitation content: %+v", parsedElicit)
	}
	feats := stringArray(parsedElicit.Content["features"])
	if len(feats) != 2 || feats[0] != "auth" || feats[1] != "metrics" {
		t.Fatalf("expected typed multi-select []string [auth metrics], got %+v", parsedElicit.Content["features"])
	}

	// 4. Elicitation cancelled explicitly via op:"answer" action="cancel".
	elicit2Resp, reqErr = srv.sendServerRequest("elicit-2", "elicitation/create", map[string]any{
		"sessionId": "sess-int",
		"mode":      "form",
		"message":   "Optional note",
		"requestedSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"note": map[string]any{"type": "string"}},
		},
	})
	if reqErr != nil {
		t.Fatalf("send elicit-2: %v", reqErr)
	}
	_ = waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		if str(m, "t") != "meta" {
			return false
		}
		reqs := arrayValue(mapValue(m["meta"])["requests"])
		return len(reqs) == 1 && str(mapValue(reqs[0]), "id") == "elicit-2"
	})
	h.send(h.client, map[string]any{"op": "answer", "id": "sess-int", "reqId": "elicit-2", "action": "cancel", "nonce": "ans-cancel"})
	ack = waitAck(t, h.client, "ans-cancel")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("cancel elicitation failed: %+v", ack)
	}
	eResp2 := <-elicit2Resp
	if !strings.Contains(string(eResp2.Result), `"action":"cancel"`) {
		t.Fatalf("expected action cancel, got %s", string(eResp2.Result))
	}

	// 5. Stop cancels pending permission AND sends session/cancel.
	perm3Resp, reqErr := srv.sendServerRequest("perm-3", "session/request_permission", map[string]any{
		"sessionId": "sess-int",
		"toolCall":  map[string]any{"toolCallId": "tc-3", "name": "run_command", "kind": "execute"},
		"options":   []map[string]any{{"optionId": "allow", "name": "Allow", "kind": "allow_once"}},
	})
	if reqErr != nil {
		t.Fatalf("send perm-3: %v", reqErr)
	}
	_ = waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		if str(m, "t") != "meta" {
			return false
		}
		reqs := arrayValue(mapValue(m["meta"])["requests"])
		return len(reqs) == 1 && str(mapValue(reqs[0]), "id") == "perm-3"
	})

	h.send(h.client, map[string]any{"op": "stop", "id": "sess-int", "nonce": "stop-1"})
	ack = waitAck(t, h.client, "stop-1")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("stop failed: %+v", ack)
	}
	<-cancelSeen
	p3 := <-perm3Resp
	if !strings.Contains(string(p3.Result), `"cancelled"`) {
		t.Fatalf("expected pending permission cancelled on stop, got %s", string(p3.Result))
	}
}

func TestAntigravityManagerWakeAppDataDirMoreAndIdleReap(t *testing.T) {
	h := newAgyTestHarness(t)
	ws := filepath.Join(h.home, "cli-ws")
	_ = os.MkdirAll(ws, 0700)

	cliDB := antigravityDBPath(h.geminiDir, antigravityAppDirCLI)
	createAntigravityTestDB(t, cliDB, []antigravityTestRow{
		{
			id:            "conv-cli-1",
			title:         "CLI Session",
			lastMod:       "2026-11-27T14:55:00Z",
			workspaceURIs: fmt.Sprintf("[%q]", "file://"+filepath.ToSlash(ws)),
			status:        "CASCADE_RUN_STATUS_IDLE",
			appDataDir:    antigravityAppDirCLI,
		},
	})

	// Write 20 turns to transcript.jsonl so pagination ("more") is exercised.
	var lines []string
	base := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		ts := base.Add(time.Duration(i*2) * time.Minute).Format(time.RFC3339)
		ts2 := base.Add(time.Duration(i*2+1) * time.Minute).Format(time.RFC3339)
		lines = append(lines,
			fmt.Sprintf(`{"step_index":%d,"type":"USER_INPUT","status":"DONE","created_at":%q,"content":"Question %d"}`, i*2, ts, i+1),
			fmt.Sprintf(`{"step_index":%d,"type":"PLANNER_RESPONSE","status":"DONE","created_at":%q,"content":"Answer %d","model":"gemini-4-neon"}`, i*2+1, ts2, i+1),
		)
	}
	writeTestTranscript(t, antigravityTranscriptPath(h.geminiDir, antigravityAppDirCLI, "conv-cli-1"), lines, base.Add(45*time.Minute))
	h.mgr.refresh()

	// Offline open returns last 15 turns with hasOlder=true.
	h.send(h.client, map[string]any{"op": "open", "id": "conv-cli-1", "nonce": "open-off"})
	openFrame := waitFrame(t, h.client, 2*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "conv-cli-1"
	})
	_ = waitAck(t, h.client, "open-off")
	if len(arrayValue(openFrame["turns"])) != 15 || mapValue(openFrame["meta"])["hasOlder"] != true {
		t.Fatalf("expected 15 turns with hasOlder=true, got turns=%d meta=%+v", len(arrayValue(openFrame["turns"])), openFrame["meta"])
	}

	// Wake loads ACP using exact appDataDir ("antigravity-cli") and does NOT duplicate replayed history!
	var gotAppDataDir string
	var closedSession atomic.Int32
	h.mgr.dialACP = func(ctx context.Context, cfg acpClientConfig) (*acpClient, *acpInitResult, error) {
		gotAppDataDir = cfg.AppDataDir
		srv, cli := newMockACPServer(t, cfg)
		srv.on("initialize", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockInitResultPayload())
		})
		srv.on("session/load", func(msg mockACPRPCMessage) {
			// Simulate replay notifications during session/load.
			_ = srv.sendNotify("session/update", map[string]any{
				"sessionId": "conv-cli-1",
				"update":    map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "Question 20"}},
			})
			_ = srv.sendNotify("session/update", map[string]any{
				"sessionId": "conv-cli-1",
				"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Answer 20"}},
			})
			_ = srv.sendResult(msg.ID, mockStandardSessionState("conv-cli-1", "gemini-4-neon", "high", "default"))
		})
		srv.on("session/close", func(msg mockACPRPCMessage) {
			closedSession.Add(1)
			_ = srv.sendResult(msg.ID, map[string]any{})
		})
		srv.start()
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	drainFrames(h.client)
	h.send(h.client, map[string]any{"op": "wake", "id": "conv-cli-1", "nonce": "wake-cli"})
	openLive := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "conv-cli-1"
	})
	ack := waitAck(t, h.client, "wake-cli")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("wake failed: %+v", ack)
	}
	if gotAppDataDir != antigravityAppDirCLI {
		t.Fatalf("expected appDataDir=%q, got %q", antigravityAppDirCLI, gotAppDataDir)
	}
	if str(mapValue(openLive["meta"]), "mode") != "live" || len(arrayValue(openLive["turns"])) != 15 {
		t.Fatalf("expected live mode with 15 non-duplicated turns, got mode=%s turns=%d",
			str(mapValue(openLive["meta"]), "mode"), len(arrayValue(openLive["turns"])))
	}

	// op: "more" while live loads all 20 disk turns with hasOlder=false.
	h.send(h.client, map[string]any{"op": "more", "id": "conv-cli-1", "nonce": "more-live"})
	openMore := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "conv-cli-1" && m["more"] == true
	})
	_ = waitAck(t, h.client, "more-live")
	if len(arrayValue(openMore["turns"])) != 20 || mapValue(openMore["meta"])["hasOlder"] == true {
		t.Fatalf("expected 20 turns and hasOlder=false after more, got turns=%d meta=%+v",
			len(arrayValue(openMore["turns"])), openMore["meta"])
	}

	// Idle reaper closes idle session after idleSessionTimeout.
	h.mgr.idleSessionTimeout = 10 * time.Minute
	h.advance(5 * time.Minute)
	h.mgr.reapIdleSessions()
	h.mgr.mu.Lock()
	stillPresent := h.mgr.runs["conv-cli-1"] != nil
	h.mgr.mu.Unlock()
	if !stillPresent {
		t.Fatalf("session should not be reaped before idle timeout")
	}

	h.advance(6 * time.Minute)
	h.mgr.reapIdleSessions()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if closedSession.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if closedSession.Load() != 1 {
		t.Fatalf("expected idle session to be reaped and closed, closedSession=%d", closedSession.Load())
	}
}

func TestAntigravityManagerSubagentsReadOnlyAndFilesServing(t *testing.T) {
	h := newAgyTestHarness(t)
	t.Setenv("HOME", h.home)
	t.Setenv("USERPROFILE", h.home)

	dbPath := antigravityDBPath(h.geminiDir, antigravityAppDirDesktop)
	createAntigravityTestDB(t, dbPath, []antigravityTestRow{
		{id: "parent-1", title: "Parent Chat", lastMod: "2026-11-27T14:55:00Z", status: "CASCADE_RUN_STATUS_IDLE"},
		{id: "parent-2", title: "Other Chat", lastMod: "2026-11-27T14:54:00Z", status: "CASCADE_RUN_STATUS_IDLE"},
		{
			id:           "child-1",
			title:        "Research Worker",
			lastMod:      "2026-11-27T14:56:00Z",
			parentID:     "parent-1",
			nestingDepth: 1,
			agentName:    "Researcher",
			status:       "CASCADE_RUN_STATUS_IDLE",
		},
	})
	subPath := antigravityTranscriptPath(h.geminiDir, antigravityAppDirDesktop, "child-1")
	writeTestTranscript(t, subPath, []string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":"2026-11-27T14:55:00Z","content":"Inspect files"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-27T14:55:10Z","content":"Done checking"}`,
	}, time.Now().Add(-2*time.Minute))
	h.mgr.refresh()

	// Strict read-only ingress guard blocks ALL control ops + pin/unpin on subagent ID.
	for _, op := range []string{"send", "stop", "wake", "settings", "approve", "answer", "compact", "archive", "unarchive", "rename", "fork", "pin", "unpin"} {
		nonce := "ro-" + op
		h.send(h.client, map[string]any{"op": op, "id": "child-1", "text": "mutate", "nonce": nonce})
		ack := waitAck(t, h.client, nonce)
		if ok, _ := ack["ok"].(bool); ok {
			t.Fatalf("expected subagent op %q to be rejected", op)
		}
		if !strings.Contains(str(ack, "msg"), "Read Only") {
			t.Fatalf("expected Read Only error for op %q, got %+v", op, ack)
		}
	}

	// op:"agents" lists child-1 under parent-1.
	h.send(h.client, map[string]any{"op": "agents", "id": "parent-1", "nonce": "ag-list"})
	ack := waitAck(t, h.client, "ag-list")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("agents list failed: %+v", ack)
	}
	if ags := arrayValue(ack["agents"]); len(ags) != 1 || str(mapValue(ags[0]), "id") != "child-1" {
		t.Fatalf("expected child-1 in agents list, got %+v", ags)
	}

	// Wrong parent rejected on agentOpen.
	h.send(h.client, map[string]any{"op": "agentOpen", "parent": "parent-2", "id": "child-1", "nonce": "ag-wrong"})
	ack = waitAck(t, h.client, "ag-wrong")
	if ok, _ := ack["ok"].(bool); ok {
		t.Fatalf("expected agentOpen with mismatched parent to fail")
	}

	// Valid agentOpen emits read-only open frame and follows transcript updates.
	drainFrames(h.client)
	h.send(h.client, map[string]any{"op": "agentOpen", "parent": "parent-1", "id": "child-1", "nonce": "ag-open"})
	openSub := waitFrame(t, h.client, 2*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "child-1"
	})
	ack = waitAck(t, h.client, "ag-open")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("agentOpen failed: %+v", ack)
	}
	subMeta := mapValue(openSub["meta"])
	if subMeta["readOnly"] != true || str(subMeta, "mode") != "tail" {
		t.Fatalf("expected readOnly=true mode=tail for subagent, got %+v", subMeta)
	}

	// Append to subagent transcript and run pollWork -> subscriber receives updated open frame!
	drainFrames(h.client)
	writeTestTranscript(t, subPath, []string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":"2026-11-27T14:55:00Z","content":"Inspect files"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-27T14:55:10Z","content":"Done checking"}`,
		`{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-27T14:55:20Z","content":"Follow up note"}`,
	}, time.Now())
	h.mgr.pollWork()
	updatedSub := waitFrame(t, h.client, 2*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "child-1"
	})
	if !strings.Contains(string(mustJSON(updatedSub)), "Follow up note") {
		t.Fatalf("expected pollWork to stream updated subagent transcript, got %+v", updatedSub)
	}

	// Verify serveFile supports file:// URIs and conversation-scoped brain artifact containment.
	brainDir := antigravityBrainDir(h.geminiDir, antigravityAppDirDesktop, "parent-1")
	_ = os.MkdirAll(brainDir, 0700)
	planFile := filepath.Join(brainDir, "plan.md")
	planBody := []byte("# Plan\n\nShip Antigravity cleanly.\n")
	if err := os.WriteFile(planFile, planBody, 0600); err != nil {
		t.Fatal(err)
	}

	// file:// URI download succeeds.
	slashed := filepath.ToSlash(planFile)
	if len(slashed) == 0 || slashed[0] != '/' {
		slashed = "/" + slashed
	}
	fileURI := (&url.URL{Scheme: "file", Path: slashed}).String()
	w := httptest.NewRecorder()
	serveFile(w, httptest.NewRequest("GET", "/file?path="+url.QueryEscape(fileURI), nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), planBody) {
		t.Fatalf("expected 200 for local file:// URI, got %d (%s)", w.Code, w.Body.String())
	}

	// Non-local file:// host rejected.
	w = httptest.NewRecorder()
	serveFile(w, httptest.NewRequest("GET", "/file?path="+url.QueryEscape("file://evil.example/etc/passwd"), nil))
	if w.Code != 400 {
		t.Fatalf("expected 400 for non-local file:// URI, got %d", w.Code)
	}

	// Conversation-scoped relative brain artifact succeeds, traversal rejected.
	w = httptest.NewRecorder()
	serveFile(w, httptest.NewRequest("GET", "/file?conversation=parent-1&path="+url.QueryEscape("plan.md"), nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), planBody) {
		t.Fatalf("expected 200 for conversation brain artifact, got %d (%s)", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	serveFile(w, httptest.NewRequest("GET", "/file?conversation=parent-1&path="+url.QueryEscape("../../escape.txt"), nil))
	if w.Code != 400 {
		t.Fatalf("expected 400 for brain traversal attempt, got %d", w.Code)
	}
}

func TestAntigravityManagerAsyncOnCloseAndCleanRunShutdown(t *testing.T) {
	h := newAgyTestHarness(t)
	ws := filepath.Join(h.home, "ws-life")
	_ = os.MkdirAll(ws, 0700)

	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		h.mgr.run(runCtx)
	}()

	var (
		mu       sync.Mutex
		servers  []*mockACPServer
		dialNum  int
		closedCh = make(chan struct{}, 2)
	)
	h.mgr.dialACP = func(ctx context.Context, cfg acpClientConfig) (*acpClient, *acpInitResult, error) {
		srv, cli := newMockACPServer(t, cfg)
		mu.Lock()
		dialNum++
		n := dialNum
		servers = append(servers, srv)
		mu.Unlock()

		sid := fmt.Sprintf("sess-life-%d", n)
		srv.on("initialize", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockInitResultPayload())
		})
		srv.on("session/new", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockStandardSessionState(sid, "gemini-4-neon", "high", "default"))
		})
		srv.on("session/prompt", func(msg mockACPRPCMessage) {
			if n == 1 {
				// Simulate abrupt transport drop while prompt is running; OnClose must NOT deadlock!
				time.Sleep(25 * time.Millisecond)
				srv.close()
				return
			}
			// Session 2 stays open until runCtx shutdown.
			<-srv.done
		})
		srv.on("session/close", func(msg mockACPRPCMessage) {
			select {
			case closedCh <- struct{}{}:
			default:
			}
			_ = srv.sendResult(msg.ID, map[string]any{})
		})
		srv.start()
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	// Session 1 drops abruptly mid-prompt -> OnClose cleans up without deadlock and emits failed event.
	h.send(h.client, map[string]any{"op": "new", "cwd": ws, "text": "Crash test", "nonce": "life-1"})
	ack := waitAck(t, h.client, "life-1")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("new session 1 failed: %+v", ack)
	}
	failEv := waitFrame(t, h.evClient, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "event" && str(m, "id") == "sess-life-1" && str(m, "kind") == "failed"
	})
	if str(failEv, "provider") != "antigravity" {
		t.Fatalf("unexpected failed event: %+v", failEv)
	}

	// Session 2 stays running, then runCtx cancels -> run() shuts down cleanly and closes active ACP client!
	h.send(h.client, map[string]any{"op": "new", "cwd": ws, "text": "Stay alive", "nonce": "life-2"})
	ack = waitAck(t, h.client, "life-2")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("new session 2 failed: %+v", ack)
	}

	cancelRun()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("AntigravityManager.run did not shut down cleanly on context cancel")
	}
	select {
	case <-closedCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected active ACP session to receive session/close on shutdown")
	}
	h.mgr.mu.Lock()
	remaining := len(h.mgr.runs)
	h.mgr.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected 0 remaining runs after shutdown, got %d", remaining)
	}

	// Ensure errors package is exercised if needed.
	if err := h.mgr.requireACP(); errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAntigravityManagerDispatchBarrierReplayAndTrailingChunks(t *testing.T) {
	h := newAgyTestHarness(t)
	ws := filepath.Join(h.home, "ws-barrier")
	_ = os.MkdirAll(ws, 0700)

	dbPath := antigravityDBPath(h.geminiDir, antigravityAppDirDesktop)
	createAntigravityTestDB(t, dbPath, []antigravityTestRow{
		{
			id:            "conv-barrier",
			title:         "Barrier Chat",
			lastMod:       "2026-11-27T14:55:00Z",
			workspaceURIs: fmt.Sprintf("[%q]", "file://"+filepath.ToSlash(ws)),
			status:        "CASCADE_RUN_STATUS_IDLE",
		},
	})
	writeTestTranscript(t, antigravityTranscriptPath(h.geminiDir, antigravityAppDirDesktop, "conv-barrier"), []string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":"2026-11-27T14:50:00Z","content":"Disk Q1"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-27T14:50:10Z","content":"Disk A1"}`,
	}, time.Now().Add(-5*time.Minute))
	h.mgr.refresh()

	h.mgr.dialACP = func(ctx context.Context, cfg acpClientConfig) (*acpClient, *acpInitResult, error) {
		// Wrap OnUpdate with a small delay to make readLoop -> dispatchLoop lag deterministic!
		origUpdate := cfg.OnUpdate
		cfg.OnUpdate = func(u acpSessionUpdate) {
			if u.Kind != antigravityDrainBarrierKind {
				time.Sleep(2 * time.Millisecond)
			}
			if origUpdate != nil {
				origUpdate(u)
			}
		}
		srv, cli := newMockACPServer(t, cfg)
		srv.on("initialize", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, mockInitResultPayload())
		})
		srv.on("session/load", func(msg mockACPRPCMessage) {
			// Burst 20 replay notifications immediately before session/load result.
			for i := 0; i < 20; i++ {
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": "conv-barrier",
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": fmt.Sprintf("replay-%d ", i)},
					},
				})
			}
			_ = srv.sendResult(msg.ID, mockStandardSessionState("conv-barrier", "gemini-4-neon", "high", "default"))
		})
		srv.on("session/prompt", func(msg mockACPRPCMessage) {
			// Burst 20 trailing chunks immediately before prompt end_turn result.
			for i := 0; i < 20; i++ {
				_ = srv.sendNotify("session/update", map[string]any{
					"sessionId": "conv-barrier",
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": fmt.Sprintf("chunk-%d ", i)},
					},
				})
			}
			_ = srv.sendNotify("session/update", map[string]any{
				"sessionId": "conv-barrier",
				"update": map[string]any{
					"sessionUpdate": "usage_update",
					"used":          9876,
					"size":          1048576,
				},
			})
			_ = srv.sendResult(msg.ID, map[string]any{"stopReason": "end_turn"})
		})
		srv.on("session/close", func(msg mockACPRPCMessage) {
			_ = srv.sendResult(msg.ID, map[string]any{})
		})
		srv.start()
		initRes, err := cli.Initialize(ctx)
		if err != nil {
			_ = cli.Close()
			return nil, nil, err
		}
		return cli, initRes, nil
	}

	// 1. Wake must drain all 20 replay chunks while loading==true -> 0 leaked liveTurns!
	h.send(h.client, map[string]any{"op": "wake", "id": "conv-barrier", "nonce": "bar-wake"})
	openAfterWake := waitFrame(t, h.client, 5*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "conv-barrier"
	})
	ack := waitAck(t, h.client, "bar-wake")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("wake failed: %+v", ack)
	}
	if turns := arrayValue(openAfterWake["turns"]); len(turns) != 1 {
		t.Fatalf("expected exactly 1 disk turn (no leaked replay turns), got %d: %+v", len(turns), turns)
	}

	// 2. Send prompt -> drain all 20 trailing chunks + usage before finalize -> NO phantom inProgress turn!
	h.send(h.client, map[string]any{"op": "send", "id": "conv-barrier", "text": "Go", "nonce": "bar-send"})
	ack = waitAck(t, h.client, "bar-send")
	if ok, _ := ack["ok"].(bool); !ok {
		t.Fatalf("send failed: %+v", ack)
	}
	_ = waitFrame(t, h.evClient, 5*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "event" && str(m, "id") == "conv-barrier" && str(m, "kind") == "completed"
	})
	// Sleep briefly to catch any late-arriving chunk that could create a phantom turn.
	time.Sleep(50 * time.Millisecond)

	drainFrames(h.client)
	h.send(h.client, map[string]any{"op": "open", "id": "conv-barrier", "nonce": "bar-open2"})
	openFinal := waitFrame(t, h.client, 3*time.Second, func(m map[string]any) bool {
		return str(m, "t") == "open" && str(m, "id") == "conv-barrier"
	})
	_ = waitAck(t, h.client, "bar-open2")
	turns := arrayValue(openFinal["turns"])
	if len(turns) != 2 {
		t.Fatalf("expected exactly 2 turns (1 disk + 1 completed live, no phantom turn), got %d: %+v", len(turns), turns)
	}
	liveTurn := mapValue(turns[1])
	if str(liveTurn, "status") != "completed" {
		t.Fatalf("expected completed live turn, got %+v", liveTurn)
	}
	if used, _ := num(mapValue(openFinal["meta"]), "ctxUsed"); used != 9876 {
		t.Fatalf("expected trailing usage_update ctxUsed=9876, got %d", used)
	}
	if !strings.Contains(string(mustJSON(liveTurn)), "chunk-19 ") {
		t.Fatalf("expected last trailing chunk-19 inside completed live turn, got %+v", liveTurn)
	}
}

func TestAntigravityManagerLateSubagentGuardDirectOpenAndHardening(t *testing.T) {
	h := newAgyTestHarness(t)
	ws := filepath.Join(h.home, "ws-late")
	_ = os.MkdirAll(ws, 0700)

	// Step 1: Populate DB with ONLY a top-level chat and refresh so m.records is non-empty!
	dbPath := antigravityDBPath(h.geminiDir, antigravityAppDirDesktop)
	createAntigravityTestDB(t, dbPath, []antigravityTestRow{
		{
			id:            "parent-existing",
			title:         "Parent Chat",
			lastMod:       "2026-11-27T14:55:00Z",
			workspaceURIs: fmt.Sprintf("[%q]", "file://"+filepath.ToSlash(ws)),
			status:        "CASCADE_RUN_STATUS_IDLE",
		},
	})
	h.mgr.refresh()

	// Step 2: Insert a NEW subagent into SQLite WITHOUT calling m.refresh()!
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	createAntigravityTestDB(t, dbPath, []antigravityTestRow{
		{
			id:            "parent-existing",
			title:         "Parent Chat",
			lastMod:       "2026-11-27T14:55:00Z",
			workspaceURIs: fmt.Sprintf("[%q]", "file://"+filepath.ToSlash(ws)),
			status:        "CASCADE_RUN_STATUS_IDLE",
		},
		{
			id:           "late-sub-1",
			title:        "Late Subagent",
			lastMod:      "2026-11-27T14:56:00Z",
			parentID:     "parent-existing",
			nestingDepth: 1,
			agentName:    "LateWorker",
			status:       "CASCADE_RUN_STATUS_IDLE",
		},
	})

	// Every mutating op on the uncached late subagent MUST be rejected as read-only!
	for _, op := range []string{"send", "wake", "settings", "rename", "archive", "unarchive", "pin", "unpin", "stop"} {
		nonce := "late-" + op
		h.send(h.client, map[string]any{
			"op":    op,
			"id":    "late-sub-1",
			"text":  "try mutate",
			"model": "gemini-4-neon",
			"nonce": nonce,
		})
		ack := waitAck(t, h.client, nonce)
		if ok, _ := ack["ok"].(bool); ok || !strings.Contains(str(ack, "msg"), "Read Only") {
			t.Fatalf("expected late subagent op %q rejected as Read Only, got %+v", op, ack)
		}
	}

	// Direct op:"open" on a subagent MUST be rejected (must use agentOpen with parent validation).
	h.send(h.client, map[string]any{"op": "open", "id": "late-sub-1", "nonce": "direct-open-sub"})
	ack := waitAck(t, h.client, "direct-open-sub")
	if ok, _ := ack["ok"].(bool); ok || !strings.Contains(str(ack, "msg"), "Parent") {
		t.Fatalf("expected direct open on subagent rejected, got %+v", ack)
	}

	// Archiving a nonexistent conversation returns not found.
	h.send(h.client, map[string]any{"op": "archive", "id": "does-not-exist", "nonce": "arch-missing"})
	ack = waitAck(t, h.client, "arch-missing")
	if ok, _ := ack["ok"].(bool); ok {
		t.Fatalf("expected archive on nonexistent conversation to fail, got %+v", ack)
	}

	// Image count cap (>12 images rejected).
	tooMany := make([]string, 13)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("/tmp/img-%d.png", i)
	}
	if _, err := buildAntigravityPromptBlocks("hello", tooMany); err == nil || !strings.Contains(err.Error(), "12") {
		t.Fatalf("expected >12 images error, got %v", err)
	}

	// Unknown explicit optionId rejected when options are advertised.
	opts := []acpPermissionOption{
		{OptionID: "allow-once", Name: "Allow Once", Kind: "allow_once"},
		{OptionID: "reject-once", Name: "Reject", Kind: "reject_once"},
	}
	if _, _, err := resolvePermissionOption(opts, "fabricated-opt", "accept"); err == nil {
		t.Fatalf("expected unknown optionId to return error")
	}
}

func TestAntigravityManagerNoDuplicateFailedEventOnAbruptDrop(t *testing.T) {
	for iter := 0; iter < 5; iter++ {
		h := newAgyTestHarness(t)
		ws := filepath.Join(h.home, fmt.Sprintf("ws-drop-%d", iter))
		_ = os.MkdirAll(ws, 0700)

		sid := fmt.Sprintf("sess-drop-%d", iter)
		h.mgr.dialACP = func(ctx context.Context, cfg acpClientConfig) (*acpClient, *acpInitResult, error) {
			srv, cli := newMockACPServer(t, cfg)
			srv.on("initialize", func(msg mockACPRPCMessage) {
				_ = srv.sendResult(msg.ID, mockInitResultPayload())
			})
			srv.on("session/new", func(msg mockACPRPCMessage) {
				_ = srv.sendResult(msg.ID, mockStandardSessionState(sid, "gemini-4-neon", "high", "default"))
			})
			srv.on("session/prompt", func(msg mockACPRPCMessage) {
				// Abruptly close transport to race onClose vs finishPrompt.
				srv.close()
			})
			srv.start()
			initRes, err := cli.Initialize(ctx)
			if err != nil {
				_ = cli.Close()
				return nil, nil, err
			}
			return cli, initRes, nil
		}

		h.send(h.client, map[string]any{"op": "new", "cwd": ws, "text": "Drop test", "nonce": fmt.Sprintf("drop-%d", iter)})
		ack := waitAck(t, h.client, fmt.Sprintf("drop-%d", iter))
		if ok, _ := ack["ok"].(bool); !ok {
			t.Fatalf("iter %d: new failed: %+v", iter, ack)
		}

		// Wait for the single expected failed event.
		_ = waitFrame(t, h.evClient, 3*time.Second, func(m map[string]any) bool {
			return str(m, "t") == "event" && str(m, "id") == sid && str(m, "kind") == "failed"
		})
		// Give both goroutines ample time to settle and confirm ZERO duplicate failed events!
		time.Sleep(40 * time.Millisecond)
		for _, extra := range drainFrames(h.evClient) {
			if str(extra, "t") == "event" && str(extra, "id") == sid && str(extra, "kind") == "failed" {
				t.Fatalf("iter %d: received duplicate failed event: %+v", iter, extra)
			}
		}
	}
}
