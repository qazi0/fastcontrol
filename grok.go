package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultGrokRecencyBroadcastInterval = 15 * time.Second
	defaultGrokIdleSessionTimeout       = 12 * time.Minute
	defaultGrokDaemonIdleTimeout        = 30 * time.Minute
	defaultGrokKeepaliveInterval        = 45 * time.Second
	defaultGrokKeepaliveTimeout         = 10 * time.Second
	defaultGrokOpenFastWait             = 150 * time.Millisecond
	defaultGrokModelsCacheTTL           = 5 * time.Minute
	defaultGrokPollInterval             = time.Second
	defaultGrokWorkPollInterval         = 500 * time.Millisecond
	defaultGrokFlushInterval            = 60 * time.Millisecond
	defaultGrokCallTimeout              = 30 * time.Second
	grokDaemonMaxEarlyUpdates           = 256
)

var defaultGrokReconnectBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
}

type grokDaemonKey struct {
	alwaysApprove bool
}

type grokDaemonDial struct {
	done   chan struct{}
	daemon *grokDaemon
	err    error
}

type grokEarlyEvent struct {
	update       *grokSessionUpdate
	extUpdate    *grokExtSessionUpdate
	sessionNotif *grokSessionNotification
}

type grokDaemon struct {
	manager *GrokManager
	key     grokDaemonKey
	client  *grokACPClient
	initRes *grokInitResult
	authRes *grokAuthResult

	mu           sync.Mutex
	runs         map[string]*grokRun
	earlyUpdates map[string][]grokEarlyEvent
	pendingNew   int
	lastRPCAt    time.Time
	lastProbeAt  time.Time
	idleSince    time.Time
	probing      bool
	closed       bool
	closeErr     error
}

type grokGate struct {
	mu    sync.Mutex
	users int
}

type grokWatchStamp struct {
	updatesMod  time.Time
	updatesSize int64
	summaryMod  time.Time
	summarySize int64
	running     bool
	external    bool
	title       string
}

type grokWorkWatch struct {
	Gate       *sync.Mutex
	Parent     string
	Window     int
	Generation int
	Mod        time.Time
	Size       int64
	State      string
	Digest     [32]byte
}

type grokModelCatalog struct {
	Models         []Model
	DefaultModel   string
	DefaultEffort  string
	DefaultMode    string
	ContextWindows map[string]int64
	LiveLoaded     bool
	UpdatedAt      time.Time
}

type grokPendingPerm struct {
	acpReq grokPermissionRequest
	req    Request
	at     time.Time
}

type grokPendingAsk struct {
	acpReq grokAskUserQuestionRequest
	req    Request
	at     time.Time
}

type grokPendingExitPlan struct {
	acpReq grokExitPlanModeRequest
	req    Request
	at     time.Time
}

type grokPendingElicit struct {
	acpReq grokElicitationRequest
	req    Request
	at     time.Time
}

type grokListState struct {
	alive        bool
	running      bool
	localRunning bool
	attention    bool
	turnAt       int64
	recency      int64
	title        string
	cwd          string
	model        string
	effort       string
	mode         string
	lastMsg      string
	lastErr      string
}

func grokListSignature(list []ThreadInfo, groups []OrganizationGroup) [32]byte {
	stripped := make([]ThreadInfo, len(list))
	extra := make([]string, len(list))
	for i, t := range list {
		t.Recency = 0
		stripped[i] = t
		if t.Model != "" || t.Effort != "" {
			extra[i] = t.Model + "\x00" + t.Effort
		}
	}
	return sha256.Sum256(mustJSON(struct {
		Threads []ThreadInfo        `json:"threads"`
		Groups  []OrganizationGroup `json:"groups"`
		Extra   []string            `json:"extra"`
	}{Threads: stripped, Groups: groups, Extra: extra}))
}

func grokEnvWithHome(grokHome string) []string {
	base := codexEnv()
	if strings.TrimSpace(grokHome) == "" {
		return base
	}
	out := make([]string, 0, len(base)+1)
	for _, entry := range base {
		if strings.HasPrefix(entry, "GROK_HOME=") {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, "GROK_HOME="+grokHome)
	return out
}

func isGrokAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"unauthenticated",
		"not authenticated",
		"not signed in",
		"not logged in",
		"grok login",
		"cached_token",
		"cached token",
		"no auth",
		"missing token",
		"auth required",
		"authentication required",
		"login required",
		"unauthorized",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	var rpcErr *acpRPCError
	if errors.As(err, &rpcErr) && rpcErr.Code == -32000 {
		return strings.Contains(msg, "auth") || strings.Contains(msg, "token") || strings.Contains(msg, "login")
	}
	return false
}

// GrokManager indexes local Grok CLI conversations, manages pooled persistent
// ACP daemons and live runs, streams updates to subscribed clients, and handles
// provider WebSocket operations.
//
// Lock ordering invariant (strictly enforced everywhere):
//
//	m.hub.mu -> m.mu -> d.mu / r.mu -> state.mu
type GrokManager struct {
	hub                      *Hub
	bin                      string
	grokHome                 string
	customGrokHome           bool
	state                    *grokState
	allowFullAccess          bool
	prewarmOnStart           bool
	supportsACP              func(string) bool
	checkAuth                func(string) bool
	dialACP                  func(context.Context, grokACPClientConfig) (*grokACPClient, *grokInitResult, error)
	now                      func() time.Time
	recencyBroadcastInterval time.Duration
	idleSessionTimeout       time.Duration
	daemonIdleTimeout        time.Duration
	keepaliveInterval        time.Duration
	keepaliveTimeout         time.Duration
	openFastWait             time.Duration
	reconnectBackoff         []time.Duration
	modelsCacheTTL           time.Duration
	pollInterval             time.Duration
	workPollInterval         time.Duration

	mu               sync.Mutex
	records          map[string]grokRecord
	recordSlice      []grokRecord
	runs             map[string]*grokRun
	daemons          map[grokDaemonKey]*grokDaemon
	daemonDial       map[grokDaemonKey]*grokDaemonDial
	watch            map[*Client]map[string]int
	watchGen         map[*Client]map[string]uint64
	watchSeq         uint64
	workWatch        map[*Client]map[string]grokWorkWatch
	watchedStamp     map[string]grokWatchStamp
	gates            map[string]*grokGate
	reconnecting     map[string]bool
	prewarmStarted   bool
	lastList         []byte
	lastBroadcast    []byte
	lastBroadcastSig [32]byte
	lastBroadcastAt  time.Time
	orgState         OrganizationSnapshot
	prevStates       map[string]threadState
	catalog          grokModelCatalog
	shuttingDown     bool
	stopCtx          context.Context
	stopCancel       context.CancelFunc

	bgWG       sync.WaitGroup
	refreshMu  sync.Mutex
	refreshJob atomic.Bool
	modelsMu   sync.Mutex
}

func newGrokManager(h *Hub) *GrokManager {
	state, err := newGrokState(defaultGrokStatePath())
	if err != nil {
		state = &grokState{
			path: defaultGrokStatePath(),
			data: emptyGrokStateData(),
		}
	}
	stopCtx, stopCancel := context.WithCancel(context.Background())
	return &GrokManager{
		hub:                      h,
		bin:                      grokBinary(),
		grokHome:                 grokHomeDir(""),
		state:                    state,
		allowFullAccess:          true,
		prewarmOnStart:           true,
		supportsACP:              grokSupportsACP,
		checkAuth:                grokHasAuth,
		dialACP:                  dialGrokACPClient,
		recencyBroadcastInterval: defaultGrokRecencyBroadcastInterval,
		idleSessionTimeout:       defaultGrokIdleSessionTimeout,
		daemonIdleTimeout:        defaultGrokDaemonIdleTimeout,
		keepaliveInterval:        defaultGrokKeepaliveInterval,
		keepaliveTimeout:         defaultGrokKeepaliveTimeout,
		openFastWait:             defaultGrokOpenFastWait,
		modelsCacheTTL:           defaultGrokModelsCacheTTL,
		pollInterval:             defaultGrokPollInterval,
		workPollInterval:         defaultGrokWorkPollInterval,
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
}

func (m *GrokManager) nowTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *GrokManager) grokRoot() string {
	if strings.TrimSpace(m.grokHome) != "" {
		return m.grokHome
	}
	return grokHomeDir("")
}

func (m *GrokManager) recencyThrottle() time.Duration {
	if m.recencyBroadcastInterval > 0 {
		return m.recencyBroadcastInterval
	}
	return defaultGrokRecencyBroadcastInterval
}

func (m *GrokManager) idleTimeout() time.Duration {
	if m.idleSessionTimeout > 0 {
		return m.idleSessionTimeout
	}
	return defaultGrokIdleSessionTimeout
}

func (m *GrokManager) daemonIdleDuration() time.Duration {
	if m.daemonIdleTimeout > 0 {
		return m.daemonIdleTimeout
	}
	return defaultGrokDaemonIdleTimeout
}

func (m *GrokManager) keepaliveEvery() time.Duration {
	if m.keepaliveInterval > 0 {
		return m.keepaliveInterval
	}
	return defaultGrokKeepaliveInterval
}

func (m *GrokManager) keepaliveCallTimeout() time.Duration {
	if m.keepaliveTimeout > 0 {
		return m.keepaliveTimeout
	}
	return defaultGrokKeepaliveTimeout
}

func (m *GrokManager) openFastWaitDuration() time.Duration {
	if m.openFastWait > 0 {
		return m.openFastWait
	}
	return defaultGrokOpenFastWait
}

func (m *GrokManager) reconnectSchedule() []time.Duration {
	if len(m.reconnectBackoff) > 0 {
		return m.reconnectBackoff
	}
	return defaultGrokReconnectBackoff
}

func (m *GrokManager) modelsTTL() time.Duration {
	if m.modelsCacheTTL > 0 {
		return m.modelsCacheTTL
	}
	return defaultGrokModelsCacheTTL
}

func (m *GrokManager) baseContextLocked() context.Context {
	if m.stopCtx == nil {
		m.stopCtx, m.stopCancel = context.WithCancel(context.Background())
		if m.shuttingDown {
			m.stopCancel()
		}
	}
	return m.stopCtx
}

func (m *GrokManager) baseContext() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.baseContextLocked()
}

func (m *GrokManager) checkSupportsACP(bin string) bool {
	if strings.TrimSpace(bin) == "" {
		return false
	}
	if m.supportsACP != nil {
		return m.supportsACP(bin)
	}
	return grokSupportsACP(bin)
}

func (m *GrokManager) hasAuth() bool {
	if m.checkAuth != nil {
		return m.checkAuth(m.grokRoot())
	}
	return grokHasAuth(m.grokRoot())
}

func (m *GrokManager) acpAvailable() bool {
	return strings.TrimSpace(m.bin) != "" && m.checkSupportsACP(m.bin)
}

func (m *GrokManager) canDialACP() bool {
	return m.acpAvailable() && m.hasAuth()
}

func (m *GrokManager) available() bool {
	if m.acpAvailable() {
		return true
	}
	m.mu.Lock()
	hasCached := len(m.records) > 0 || len(m.runs) > 0
	m.mu.Unlock()
	if hasCached {
		return true
	}
	return grokHasSessions(m.grokRoot())
}

func (m *GrokManager) requireACP() error {
	if strings.TrimSpace(m.bin) == "" {
		return errors.New("Install Grok CLI (grok) On This Computer")
	}
	if !m.checkSupportsACP(m.bin) {
		return errors.New("Update Grok CLI (grok) To A Version With ACP Support")
	}
	return nil
}

func (m *GrokManager) requireAuth() error {
	if !m.hasAuth() {
		return errors.New("Sign In With grok login")
	}
	return nil
}

func (m *GrokManager) dialClient(ctx context.Context, cfg grokACPClientConfig) (*grokACPClient, *grokInitResult, error) {
	if cfg.Binary == "" {
		cfg.Binary = m.bin
	}
	if len(cfg.Env) == 0 && (m.customGrokHome || (m.grokHome != "" && m.grokHome != grokHomeDir(""))) {
		cfg.Env = grokEnvWithHome(m.grokHome)
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = defaultGrokCallTimeout
	}
	if m.dialACP != nil {
		return m.dialACP(ctx, cfg)
	}
	return dialGrokACPClient(ctx, cfg)
}

func (m *GrokManager) ensureDaemon(ctx context.Context, key grokDaemonKey) (*grokDaemon, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.requireACP(); err != nil {
		return nil, err
	}
	if err := m.requireAuth(); err != nil {
		return nil, err
	}

	for {
		m.mu.Lock()
		if m.shuttingDown {
			m.mu.Unlock()
			return nil, errors.New("Relay Is Shutting Down")
		}
		if m.daemons == nil {
			m.daemons = map[grokDaemonKey]*grokDaemon{}
		}
		if d := m.daemons[key]; d != nil && d.isAlive() {
			m.mu.Unlock()
			return d, nil
		}
		if m.daemonDial == nil {
			m.daemonDial = map[grokDaemonKey]*grokDaemonDial{}
		}
		if inflight := m.daemonDial[key]; inflight != nil {
			m.mu.Unlock()
			select {
			case <-inflight.done:
				if inflight.err != nil {
					return nil, inflight.err
				}
				if inflight.daemon != nil && inflight.daemon.isAlive() {
					return inflight.daemon, nil
				}
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		dial := &grokDaemonDial{done: make(chan struct{})}
		m.daemonDial[key] = dial
		baseCtx := m.baseContextLocked()
		m.bgWG.Add(1)
		m.mu.Unlock()

		go func() {
			defer m.bgWG.Done()
			defer close(dial.done)

			dialCtx, dialCancel := context.WithTimeout(baseCtx, defaultGrokCallTimeout)
			defer dialCancel()

			homeDir, _ := os.UserHomeDir()
			if homeDir != "" {
				if st, statErr := os.Stat(homeDir); statErr != nil || !st.IsDir() {
					homeDir = ""
				}
			}

			now := m.nowTime()
			d := &grokDaemon{
				manager:      m,
				key:          key,
				runs:         map[string]*grokRun{},
				earlyUpdates: map[string][]grokEarlyEvent{},
				lastRPCAt:    now,
			}

			client, initRes, err := m.dialClient(dialCtx, grokACPClientConfig{
				Cwd:                   homeDir,
				AlwaysApprove:         key.alwaysApprove,
				CallTimeout:           defaultGrokCallTimeout,
				OnUpdate:              d.onUpdate,
				OnExtUpdate:           d.onExtUpdate,
				OnSessionNotification: d.onSessionNotification,
				OnPermission:          d.onPermission,
				OnAskUserQuestion:     d.onAskUserQuestion,
				OnExitPlanMode:        d.onExitPlanMode,
				OnElicitation:         d.onElicitation,
				OnClose:               d.onClose,
			})
			if err != nil {
				m.mu.Lock()
				if m.daemonDial[key] == dial {
					delete(m.daemonDial, key)
				}
				m.mu.Unlock()
				if isGrokAuthError(err) {
					dial.err = errors.New("Sign In With grok login")
				} else {
					dial.err = err
				}
				return
			}

			authRes, authErr := client.Authenticate(dialCtx, "")
			if authErr != nil {
				allowSkip := isACPMethodNotFound(authErr) && initRes != nil && !initRes.AgentCapabilities.Auth && len(initRes.AuthMethods) == 0
				if !allowSkip {
					m.mu.Lock()
					if m.daemonDial[key] == dial {
						delete(m.daemonDial, key)
					}
					m.mu.Unlock()
					_ = client.Close()
					if isGrokAuthError(authErr) {
						dial.err = errors.New("Sign In With grok login")
					} else {
						dial.err = authErr
					}
					return
				}
			}

			d.mu.Lock()
			d.client = client
			d.initRes = initRes
			d.authRes = authRes
			d.lastRPCAt = m.nowTime()
			alreadyClosed := d.closed || client.Closed()
			d.mu.Unlock()

			m.mu.Lock()
			if m.daemonDial[key] == dial {
				delete(m.daemonDial, key)
			}
			if m.shuttingDown {
				m.mu.Unlock()
				_ = client.Close()
				dial.err = errors.New("Relay Is Shutting Down")
				return
			}
			if alreadyClosed {
				m.mu.Unlock()
				_ = client.Close()
				dial.err = errors.New("Grok ACP Daemon Closed During Startup")
				return
			}
			if m.daemons == nil {
				m.daemons = map[grokDaemonKey]*grokDaemon{}
			}
			m.daemons[key] = d
			m.mu.Unlock()

			if initRes != nil && len(initRes.ModelState.AvailableModels) > 0 {
				m.updateCatalogFromModelsState(&initRes.ModelState)
			}

			dial.daemon = d
		}()

		select {
		case <-dial.done:
			if dial.err != nil {
				return nil, dial.err
			}
			if dial.daemon != nil && dial.daemon.isAlive() {
				return dial.daemon, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (d *grokDaemon) isAlive() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	closed := d.closed
	client := d.client
	d.mu.Unlock()
	if closed || client == nil {
		return false
	}
	return !client.Closed()
}

func (d *grokDaemon) touchActivity(now time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if now.After(d.lastRPCAt) {
		d.lastRPCAt = now
	}
	d.idleSince = time.Time{}
	d.mu.Unlock()
}

func (d *grokDaemon) attachRun(id string, run *grokRun) bool {
	if d == nil || id == "" || run == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.client == nil || d.client.Closed() {
		return false
	}
	if d.runs == nil {
		d.runs = map[string]*grokRun{}
	}
	d.runs[id] = run
	d.idleSince = time.Time{}
	d.lastRPCAt = d.manager.nowTime()
	return true
}

func (d *grokDaemon) detachRun(id string, run *grokRun) {
	if d == nil || id == "" {
		return
	}
	d.mu.Lock()
	if d.runs[id] == run {
		delete(d.runs, id)
	}
	if d.earlyUpdates != nil {
		delete(d.earlyUpdates, id)
	}
	d.mu.Unlock()
}

func (d *grokDaemon) hasRun(id string) bool {
	if d == nil || id == "" {
		return false
	}
	d.mu.Lock()
	_, ok := d.runs[id]
	d.mu.Unlock()
	return ok
}

func (d *grokDaemon) beginNewSession() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.pendingNew++
	d.idleSince = time.Time{}
	d.mu.Unlock()
}

func (d *grokDaemon) endNewSession(keepSID string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.pendingNew > 0 {
		d.pendingNew--
	}
	if d.pendingNew == 0 && len(d.earlyUpdates) > 0 {
		for sid := range d.earlyUpdates {
			if sid != keepSID {
				delete(d.earlyUpdates, sid)
			}
		}
	}
	d.mu.Unlock()
}

func (d *grokDaemon) flushEarlyUpdates(sessionID string) {
	if d == nil || sessionID == "" {
		return
	}
	d.mu.Lock()
	target := d.runs[sessionID]
	var early []grokEarlyEvent
	if target != nil && len(d.earlyUpdates[sessionID]) > 0 {
		early = d.earlyUpdates[sessionID]
		delete(d.earlyUpdates, sessionID)
	}
	d.mu.Unlock()
	d.deliverEarlyEvents(target, early)
}

func (d *grokDaemon) deliverEarlyEvents(target *grokRun, early []grokEarlyEvent) {
	if target == nil || len(early) == 0 {
		return
	}
	for _, ev := range early {
		switch {
		case ev.update != nil:
			target.onUpdate(*ev.update)
		case ev.extUpdate != nil:
			target.onExtUpdate(*ev.extUpdate)
		case ev.sessionNotif != nil:
			target.onSessionNotification(*ev.sessionNotif)
		}
	}
}

// drainDispatchForSession flushes any early updates for sessionID on the
// dispatch goroutine and waits for grokACPClient.DrainDispatch(ctx) to drain
// all earlier wire notifications. Never call this from inside an ACP callback.
func (d *grokDaemon) drainDispatchForSession(ctx context.Context, sessionID string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	client := d.client
	closed := d.closed
	d.mu.Unlock()
	if closed || client == nil || client.Closed() {
		return
	}
	if sessionID != "" {
		client.EnqueueEvent(func() {
			d.flushEarlyUpdates(sessionID)
		})
	}
	_ = client.DrainDispatch(ctx)
}

func (d *grokDaemon) routeNotification(sessionID string, ev grokEarlyEvent) (*grokRun, []grokEarlyEvent) {
	now := d.manager.nowTime()
	var target *grokRun
	var early []grokEarlyEvent

	d.mu.Lock()
	d.lastRPCAt = now
	if sessionID != "" {
		target = d.runs[sessionID]
		if target != nil {
			if buf := d.earlyUpdates[sessionID]; len(buf) > 0 {
				early = buf
				delete(d.earlyUpdates, sessionID)
			}
		} else if d.pendingNew > 0 {
			if d.earlyUpdates == nil {
				d.earlyUpdates = map[string][]grokEarlyEvent{}
			}
			if len(d.earlyUpdates[sessionID]) < grokDaemonMaxEarlyUpdates {
				d.earlyUpdates[sessionID] = append(d.earlyUpdates[sessionID], ev)
			}
		}
	} else if len(d.runs) == 1 {
		for _, r := range d.runs {
			target = r
		}
	}
	d.mu.Unlock()
	return target, early
}

func (d *grokDaemon) onUpdate(u grokSessionUpdate) {
	cp := u
	target, early := d.routeNotification(u.SessionID, grokEarlyEvent{update: &cp})
	if target != nil {
		d.deliverEarlyEvents(target, early)
		target.onUpdate(u)
	}
}

func (d *grokDaemon) onExtUpdate(u grokExtSessionUpdate) {
	cp := u
	target, early := d.routeNotification(u.SessionID, grokEarlyEvent{extUpdate: &cp})
	if target != nil {
		d.deliverEarlyEvents(target, early)
		target.onExtUpdate(u)
	}
}

func (d *grokDaemon) onSessionNotification(n grokSessionNotification) {
	cp := n
	target, early := d.routeNotification(n.SessionID, grokEarlyEvent{sessionNotif: &cp})
	if target != nil {
		d.deliverEarlyEvents(target, early)
		target.onSessionNotification(n)
	}
}

func (d *grokDaemon) resolveInteractiveTarget(sessionID string) (*grokRun, *grokACPClient) {
	now := d.manager.nowTime()
	var target *grokRun
	var candidates []*grokRun

	d.mu.Lock()
	d.lastRPCAt = now
	client := d.client
	if sessionID != "" {
		target = d.runs[sessionID]
	} else {
		for _, r := range d.runs {
			candidates = append(candidates, r)
		}
	}
	d.mu.Unlock()

	if target == nil && sessionID == "" {
		var active []*grokRun
		for _, r := range candidates {
			if r.hasActivePrompts() {
				active = append(active, r)
			}
		}
		if len(active) == 1 {
			target = active[0]
		}
	}
	return target, client
}

func (d *grokDaemon) onPermission(req grokPermissionRequest) {
	target, client := d.resolveInteractiveTarget(req.SessionID)
	if target != nil {
		target.onPermission(req)
		return
	}
	if client != nil {
		_ = client.RespondPermission(req.RequestID, "", true)
	}
}

func (d *grokDaemon) onAskUserQuestion(req grokAskUserQuestionRequest) {
	target, client := d.resolveInteractiveTarget(req.SessionID)
	if target != nil {
		target.onAskUserQuestion(req)
		return
	}
	if client != nil {
		_ = client.RespondAskUserQuestion(req.RequestID, "cancelled", nil, nil)
	}
}

func (d *grokDaemon) onExitPlanMode(req grokExitPlanModeRequest) {
	target, client := d.resolveInteractiveTarget(req.SessionID)
	if target != nil {
		target.onExitPlanMode(req)
		return
	}
	if client != nil {
		_ = client.RespondExitPlanMode(req.RequestID, "cancelled", "")
	}
}

func (d *grokDaemon) onElicitation(req grokElicitationRequest) {
	target, client := d.resolveInteractiveTarget(req.SessionID)
	if target != nil {
		target.onElicitation(req)
		return
	}
	if client != nil {
		_ = client.RespondElicitation(req.RequestID, "cancel", nil)
	}
}

// onClose runs inside grokACPClient.dispatchLoop; NEVER call client.Close() here!
func (d *grokDaemon) onClose(closeErr error) {
	d.mu.Lock()
	d.closed = true
	d.closeErr = closeErr
	runs := make([]*grokRun, 0, len(d.runs))
	for _, r := range d.runs {
		runs = append(runs, r)
	}
	d.runs = map[string]*grokRun{}
	d.earlyUpdates = nil
	d.mu.Unlock()

	m := d.manager
	m.mu.Lock()
	if m.daemons[d.key] == d {
		delete(m.daemons, d.key)
	}
	shuttingDown := m.shuttingDown
	m.mu.Unlock()

	for _, r := range runs {
		r.onTransportClose(closeErr, shuttingDown)
	}
}

func (m *GrokManager) lockThread(id string) func() {
	m.mu.Lock()
	if m.gates == nil {
		m.gates = map[string]*grokGate{}
	}
	g := m.gates[id]
	if g == nil {
		g = &grokGate{}
		m.gates[id] = g
	}
	g.users++
	m.mu.Unlock()
	g.mu.Lock()
	return func() {
		g.mu.Unlock()
		m.mu.Lock()
		g.users--
		if g.users == 0 {
			delete(m.gates, id)
		}
		m.mu.Unlock()
	}
}

func (m *GrokManager) emit(c *Client, value map[string]any) {
	value["provider"] = "grok"
	c.push(mustJSON(value))
}

func (m *GrokManager) broadcast(id string, value map[string]any) {
	m.mu.Lock()
	var clients []*Client
	for c, ids := range m.watch {
		if _, ok := ids[id]; ok {
			clients = append(clients, c)
		}
	}
	m.mu.Unlock()
	value["provider"] = "grok"
	value["id"] = id
	b := mustJSON(value)
	for _, c := range clients {
		c.push(b)
	}
}

func (m *GrokManager) event(id, kind, title, text, cwd string) {
	if m.hub == nil {
		return
	}
	payload := map[string]any{
		"t":        "event",
		"provider": "grok",
		"id":       id,
		"kind":     kind,
		"title":    title,
		"text":     text,
	}
	if cwd != "" {
		payload["cwd"] = cwd
	}
	b := mustJSON(payload)
	m.hub.mu.Lock()
	var clients []*Client
	for c := range m.hub.clients {
		if c.events {
			clients = append(clients, c)
		}
	}
	m.hub.mu.Unlock()
	for _, c := range clients {
		c.push(b)
	}
}

func (m *GrokManager) hasWatchersLocked(id string) bool {
	if id == "" {
		return false
	}
	for _, ids := range m.watch {
		if _, ok := ids[id]; ok {
			return true
		}
	}
	return false
}

func (m *GrokManager) hasAnyWatchersLocked() bool {
	for _, ids := range m.watch {
		if len(ids) > 0 {
			return true
		}
	}
	return false
}

func (m *GrokManager) reserveWatchGen(c *Client, id string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watchSeq++
	gen := m.watchSeq
	if m.watchGen == nil {
		m.watchGen = map[*Client]map[string]uint64{}
	}
	if m.watchGen[c] == nil {
		m.watchGen[c] = map[string]uint64{}
	}
	m.watchGen[c][id] = gen
	return gen
}

func (m *GrokManager) clearWatchGen(c *Client, id string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watchGen[c] != nil && m.watchGen[c][id] == gen {
		delete(m.watchGen[c], id)
	}
}

func (m *GrokManager) isWatchCurrent(c *Client, id string, gen uint64) bool {
	if m.hub != nil {
		m.hub.mu.Lock()
		connected := m.hub.clients[c]
		if !connected {
			m.hub.mu.Unlock()
			return false
		}
		defer m.hub.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown || m.watch[c] == nil {
		return false
	}
	if _, ok := m.watch[c][id]; !ok {
		return false
	}
	if gen == 0 {
		return true
	}
	return m.watchGen[c] != nil && m.watchGen[c][id] == gen
}

func (m *GrokManager) removeClient(c *Client) {
	now := m.nowTime()
	var unwatched []*grokRun
	m.mu.Lock()
	watched := m.watch[c]
	delete(m.watch, c)
	delete(m.watchGen, c)
	delete(m.workWatch, c)
	for id := range watched {
		if !m.hasWatchersLocked(id) {
			if r := m.runs[id]; r != nil {
				unwatched = append(unwatched, r)
			}
		}
	}
	m.mu.Unlock()
	for _, r := range unwatched {
		r.markUnwatched(now)
	}
}

func (m *GrokManager) requestRefresh() {
	if !m.refreshJob.CompareAndSwap(false, true) {
		return
	}
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		m.refreshJob.Store(false)
		return
	}
	m.bgWG.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.bgWG.Done()
		defer m.refreshJob.Store(false)
		m.refresh()
	}()
}

func (m *GrokManager) snapshotRecords() []grokRecord {
	m.mu.Lock()
	out := append([]grokRecord(nil), m.recordSlice...)
	m.mu.Unlock()
	return out
}

func (m *GrokManager) isRunActive(id string) bool {
	m.mu.Lock()
	run := m.runs[id]
	m.mu.Unlock()
	return run != nil && run.isRunning()
}

func (m *GrokManager) effectiveRecordsLocked(diskRecords []grokRecord, runStates map[string]grokListState, now time.Time) []grokRecord {
	byID := make(map[string]grokRecord, len(diskRecords)+len(runStates))
	out := make([]grokRecord, 0, len(diskRecords)+len(runStates))
	for _, rec := range diskRecords {
		if st, ok := runStates[rec.ID]; ok {
			rec.Running = rec.Running || st.localRunning
			if st.recency > rec.Recency {
				rec.Recency = st.recency
			}
			if rec.Cwd == "" && st.cwd != "" {
				rec.Cwd = st.cwd
			}
			if st.title != "" && rec.Title == "" {
				rec.Title = st.title
			}
			if st.model != "" {
				rec.Model = st.model
			}
			if st.effort != "" {
				rec.ReasoningEffort = st.effort
			}
		}
		byID[rec.ID] = rec
		out = append(out, rec)
	}
	for id, st := range runStates {
		if _, ok := byID[id]; ok {
			continue
		}
		recency := st.recency
		if recency == 0 {
			recency = now.UnixMilli()
		}
		rec := grokRecord{
			ID:              id,
			Title:           grokFirstNonEmpty(st.title, "New Chat"),
			Cwd:             st.cwd,
			Recency:         recency,
			Updated:         recency / 1000,
			Model:           st.model,
			ReasoningEffort: st.effort,
			Running:         st.running,
		}
		out = append(out, rec)
	}
	return out
}

func (m *GrokManager) refresh() {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	diskRecords, err := grokLoadRecords(m.grokRoot(), m.isRunActive)
	if err != nil {
		return
	}

	m.mu.Lock()
	runs := make(map[string]*grokRun, len(m.runs))
	for id, run := range m.runs {
		runs[id] = run
	}
	m.mu.Unlock()

	runStates := make(map[string]grokListState, len(runs))
	for id, run := range runs {
		runStates[id] = run.listState()
	}

	now := m.nowTime()
	merged := m.effectiveRecordsLocked(diskRecords, runStates, now)

	recMap := make(map[string]grokRecord, len(merged))
	for _, rec := range merged {
		recMap[rec.ID] = rec
	}

	list, orgSnap := grokBuildOrganization(false, m.state, merged)
	for i := range list {
		t := &list[i]
		if st, ok := runStates[t.ID]; ok {
			t.Live = st.alive
			t.Running = t.Running || st.localRunning
			t.Attention = st.attention
			if st.alive || t.Running {
				t.Active = true
			}
			if st.turnAt > 0 {
				t.TurnAt = st.turnAt
			}
			if st.model != "" {
				t.Model = st.model
			}
			if st.effort != "" {
				t.Effort = st.effort
			}
			if st.lastMsg != "" {
				t.LastMsg = st.lastMsg
			}
			if st.lastErr != "" {
				t.LastErr = st.lastErr
			}
			if st.title != "" && t.DisplayTitle == "" {
				t.Title = clipPlain(st.title, 240)
			}
		}
	}

	b := mustJSON(map[string]any{
		"t":        "threads",
		"provider": "grok",
		"threads":  list,
		"groups":   orgSnap.Groups,
	})
	throttle := m.recencyThrottle()

	type pendingEvent struct {
		id, kind, title, text, cwd string
	}
	var externalEvents []pendingEvent

	m.mu.Lock()
	m.records = recMap
	m.recordSlice = merged
	m.lastList = b
	m.orgState = orgSnap

	// Track state transitions for non-managed (external) Grok chats.
	nextPrev := make(map[string]threadState, len(list))
	for _, t := range list {
		if t.Subagent {
			continue
		}
		st := threadState{running: t.Running, attention: t.Attention}
		nextPrev[t.ID] = st
		if _, managed := runStates[t.ID]; managed {
			continue
		}
		if m.prevStates != nil {
			old, seen := m.prevStates[t.ID]
			title := grokFirstNonEmpty(t.DisplayTitle, t.Title, t.Preview)
			switch {
			case st.running && (!seen || !old.running):
				externalEvents = append(externalEvents, pendingEvent{id: t.ID, kind: "started", title: title, cwd: t.Cwd})
			case !st.running && seen && old.running:
				externalEvents = append(externalEvents, pendingEvent{id: t.ID, kind: "completed", title: title, text: t.LastMsg, cwd: t.Cwd})
			}
		}
	}
	m.prevStates = nextPrev

	shouldBroadcast := false
	if len(m.lastBroadcast) == 0 {
		shouldBroadcast = true
		m.lastBroadcastSig = grokListSignature(list, orgSnap.Groups)
	} else if !bytes.Equal(b, m.lastBroadcast) {
		sig := grokListSignature(list, orgSnap.Groups)
		if sig != m.lastBroadcastSig || now.Sub(m.lastBroadcastAt) >= throttle {
			shouldBroadcast = true
			m.lastBroadcastSig = sig
		}
	}
	if shouldBroadcast {
		m.lastBroadcast = b
		m.lastBroadcastAt = now
	}
	m.mu.Unlock()

	if shouldBroadcast && m.hub != nil {
		m.hub.mu.Lock()
		var clients []*Client
		for c := range m.hub.clients {
			if !c.eventsOnly {
				clients = append(clients, c)
			}
		}
		m.hub.mu.Unlock()
		for _, c := range clients {
			c.push(b)
		}
	}
	for _, ev := range externalEvents {
		m.event(ev.id, ev.kind, ev.title, ev.text, ev.cwd)
	}
}

// maybePrewarm starts the default (alwaysApprove: false) Grok ACP daemon in the
// background ONLY when grok is installed, supports ACP, and auth.json exists.
func (m *GrokManager) maybePrewarm() {
	if !m.prewarmOnStart || !m.canDialACP() {
		return
	}
	m.mu.Lock()
	if m.shuttingDown || m.prewarmStarted {
		m.mu.Unlock()
		return
	}
	m.prewarmStarted = true
	baseCtx := m.baseContextLocked()
	m.bgWG.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.bgWG.Done()
		warmCtx, cancel := context.WithTimeout(baseCtx, 25*time.Second)
		defer cancel()
		d, err := m.ensureDaemon(warmCtx, grokDaemonKey{alwaysApprove: false})
		if err != nil || d == nil {
			return
		}
		if ms, err := d.client.ListModels(warmCtx); err == nil && ms != nil && len(ms.AvailableModels) > 0 {
			m.updateCatalogFromModelsState(ms)
		}
		_, _ = d.client.ListSessions(warmCtx, "")
		d.touchActivity(m.nowTime())
	}()
}

func (m *GrokManager) run(ctx context.Context) {
	go m.followWork(ctx)
	m.refresh()
	m.maybePrewarm()

	interval := m.pollInterval
	if interval <= 0 {
		interval = defaultGrokPollInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-tick.C:
			m.refresh()
			m.pollWatchedThreads()
			m.reapIdleSessions()
			m.checkDaemonHealth()
		}
	}
}

func (m *GrokManager) shutdown() {
	m.mu.Lock()
	m.shuttingDown = true
	if m.stopCancel != nil {
		m.stopCancel()
	}
	runs := make([]*grokRun, 0, len(m.runs))
	for id, r := range m.runs {
		runs = append(runs, r)
		delete(m.runs, id)
	}
	daemons := make([]*grokDaemon, 0, len(m.daemons))
	for k, d := range m.daemons {
		daemons = append(daemons, d)
		delete(m.daemons, k)
	}
	dials := make([]chan struct{}, 0, len(m.daemonDial))
	for _, dial := range m.daemonDial {
		if dial != nil && dial.done != nil {
			dials = append(dials, dial.done)
		}
	}
	m.mu.Unlock()

	// 1. Send session/close for all loaded runs within a 500ms budget.
	var runWG sync.WaitGroup
	for _, r := range runs {
		runWG.Add(1)
		go func(run *grokRun) {
			defer runWG.Done()
			run.detachAndCloseSession(500 * time.Millisecond)
		}(r)
	}
	runWG.Wait()

	// 2. Close every pooled ACP daemon cleanly so zero orphan grok processes remain.
	var daemonWG sync.WaitGroup
	for _, d := range daemons {
		daemonWG.Add(1)
		go func(daemon *grokDaemon) {
			defer daemonWG.Done()
			daemon.mu.Lock()
			daemon.closed = true
			client := daemon.client
			daemon.mu.Unlock()
			if client != nil {
				_ = client.Close()
			}
		}(d)
	}
	daemonWG.Wait()

	// 3. Wait for any cancelled in-flight dials and background tasks to finish.
	for _, ch := range dials {
		<-ch
	}
	m.bgWG.Wait()
}

func (m *GrokManager) pollWatchedThreads() {
	m.mu.Lock()
	watched := map[string]int{}
	for _, ids := range m.watch {
		for id, w := range ids {
			if w > watched[id] {
				watched[id] = w
			}
		}
	}
	m.mu.Unlock()

	for id, maxWindow := range watched {
		if maxWindow <= 0 {
			maxWindow = grokDefaultWindow
		}
		m.mu.Lock()
		run := m.runs[id]
		rec, ok := m.records[id]
		prevStamp := m.watchedStamp[id]
		m.mu.Unlock()
		if !ok {
			continue
		}
		if run != nil && run.isRunning() {
			continue
		}
		stamp := m.computeWatchStamp(rec)
		if stamp == prevStamp {
			continue
		}

		if run != nil && run.isAlive() {
			hist, err := m.readRecordTranscript(rec, maxWindow)
			if err != nil {
				continue
			}
			m.mu.Lock()
			if m.watchedStamp == nil {
				m.watchedStamp = map[string]grokWatchStamp{}
			}
			m.watchedStamp[id] = stamp
			m.mu.Unlock()

			changed, fullOpen := run.syncExternalDiskState(hist, rec.Running)
			if fullOpen {
				m.pushLiveToWatchers(id, run)
			} else if changed {
				run.publish(true)
			}
			continue
		}

		m.mu.Lock()
		if m.watchedStamp == nil {
			m.watchedStamp = map[string]grokWatchStamp{}
		}
		m.watchedStamp[id] = stamp
		var clients []*Client
		for c, ids := range m.watch {
			if _, ok := ids[id]; ok {
				clients = append(clients, c)
			}
		}
		m.mu.Unlock()
		for _, c := range clients {
			_ = m.emitTailOpen(c, id, false)
		}
	}
}

func (m *GrokManager) computeWatchStamp(rec grokRecord) grokWatchStamp {
	stamp := grokWatchStamp{
		running:  rec.Running,
		external: rec.External,
		title:    grokFirstNonEmpty(m.state.Title(rec.ID), rec.Title, rec.GeneratedTitle, rec.SessionSummary),
	}
	if rec.UpdatesPath != "" {
		if st, err := os.Stat(rec.UpdatesPath); err == nil && st.Mode().IsRegular() {
			stamp.updatesMod = st.ModTime()
			stamp.updatesSize = st.Size()
		}
	}
	if rec.SummaryPath != "" {
		if st, err := os.Stat(rec.SummaryPath); err == nil && st.Mode().IsRegular() {
			stamp.summaryMod = st.ModTime()
			stamp.summarySize = st.Size()
		}
	}
	return stamp
}

func (m *GrokManager) reapIdleSessions() {
	now := m.nowTime()
	timeout := m.idleTimeout()
	if timeout > 0 {
		m.mu.Lock()
		type reapCandidate struct {
			id  string
			run *grokRun
		}
		candidates := make([]reapCandidate, 0, len(m.runs))
		for id, run := range m.runs {
			if m.hasWatchersLocked(id) {
				continue
			}
			candidates = append(candidates, reapCandidate{id: id, run: run})
		}
		m.mu.Unlock()

		for _, cand := range candidates {
			if !cand.run.canReap(now, timeout) {
				continue
			}
			m.mu.Lock()
			if m.runs[cand.id] == cand.run && !m.hasWatchersLocked(cand.id) && cand.run.canReap(now, timeout) {
				delete(m.runs, cand.id)
				m.mu.Unlock()
				go func(r *grokRun) {
					r.detachAndCloseSession(2 * time.Second)
					m.requestRefresh()
				}(cand.run)
			} else {
				m.mu.Unlock()
			}
		}
	}

	m.reapIdleDaemons(now)
}

func (m *GrokManager) reapIdleDaemons(now time.Time) {
	daemonTimeout := m.daemonIdleDuration()
	if daemonTimeout <= 0 {
		return
	}

	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return
	}
	hasWatchers := m.hasAnyWatchersLocked()
	daemons := make([]*grokDaemon, 0, len(m.daemons))
	for _, d := range m.daemons {
		daemons = append(daemons, d)
	}
	m.mu.Unlock()

	if hasWatchers {
		for _, d := range daemons {
			d.mu.Lock()
			d.idleSince = time.Time{}
			d.mu.Unlock()
		}
		return
	}

	for _, d := range daemons {
		d.mu.Lock()
		if d.closed || len(d.runs) > 0 || d.pendingNew > 0 || d.probing {
			d.idleSince = time.Time{}
			d.mu.Unlock()
			continue
		}
		if d.idleSince.IsZero() {
			d.idleSince = now
			d.mu.Unlock()
			continue
		}
		idleFor := now.Sub(d.idleSince)
		d.mu.Unlock()

		if idleFor < daemonTimeout {
			continue
		}

		var clientToClose *grokACPClient
		m.mu.Lock()
		if !m.shuttingDown && !m.hasAnyWatchersLocked() && m.daemons[d.key] == d {
			d.mu.Lock()
			if !d.closed && len(d.runs) == 0 && d.pendingNew == 0 && !d.probing && !d.idleSince.IsZero() && now.Sub(d.idleSince) >= daemonTimeout {
				d.closed = true
				clientToClose = d.client
				delete(m.daemons, d.key)
			}
			d.mu.Unlock()
		}
		m.mu.Unlock()

		if clientToClose != nil {
			go func(cli *grokACPClient) {
				_ = cli.Close()
			}(clientToClose)
		}
	}
}

func (m *GrokManager) checkDaemonHealth() {
	now := m.nowTime()
	interval := m.keepaliveEvery()
	if interval <= 0 {
		return
	}

	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return
	}
	hasWatchers := m.hasAnyWatchersLocked()
	baseCtx := m.baseContextLocked()
	daemons := make([]*grokDaemon, 0, len(m.daemons))
	for _, d := range m.daemons {
		daemons = append(daemons, d)
	}
	m.mu.Unlock()

	for _, d := range daemons {
		d.mu.Lock()
		if d.closed || d.probing || d.client == nil || d.client.Closed() {
			d.mu.Unlock()
			continue
		}
		if len(d.runs) == 0 && !hasWatchers {
			d.mu.Unlock()
			continue
		}
		lastAct := d.lastRPCAt
		if d.lastProbeAt.After(lastAct) {
			lastAct = d.lastProbeAt
		}
		if now.Sub(lastAct) < interval {
			d.mu.Unlock()
			continue
		}
		d.probing = true
		d.lastProbeAt = now
		client := d.client
		d.mu.Unlock()

		m.mu.Lock()
		if m.shuttingDown {
			m.mu.Unlock()
			d.mu.Lock()
			d.probing = false
			d.mu.Unlock()
			return
		}
		m.bgWG.Add(1)
		m.mu.Unlock()

		go func(daemon *grokDaemon, cli *grokACPClient) {
			defer m.bgWG.Done()
			defer func() {
				daemon.mu.Lock()
				daemon.probing = false
				daemon.mu.Unlock()
			}()

			ctx, cancel := context.WithTimeout(baseCtx, m.keepaliveCallTimeout())
			_, err := cli.ListSessions(ctx, "")
			cancel()

			if err == nil {
				daemon.touchActivity(m.nowTime())
				return
			}
			var rpcErr *acpRPCError
			if errors.As(err, &rpcErr) {
				// Any valid JSON-RPC response proves the transport is responsive.
				daemon.touchActivity(m.nowTime())
				return
			}
			m.mu.Lock()
			shuttingDown := m.shuttingDown
			m.mu.Unlock()
			if shuttingDown || baseCtx.Err() != nil {
				return
			}
			// Transport or timeout error: close wedged daemon so OnClose drives recovery.
			_ = cli.Close()
		}(d, client)
	}
}

func (m *GrokManager) startReconnect(id string) {
	if strings.TrimSpace(id) == "" || !m.canDialACP() {
		return
	}
	m.mu.Lock()
	if m.shuttingDown || !m.hasWatchersLocked(id) {
		m.mu.Unlock()
		return
	}
	if existing := m.runs[id]; existing != nil && existing.isAlive() {
		m.mu.Unlock()
		return
	}
	if m.reconnecting == nil {
		m.reconnecting = map[string]bool{}
	}
	if m.reconnecting[id] {
		m.mu.Unlock()
		return
	}
	m.reconnecting[id] = true
	baseCtx := m.baseContextLocked()
	m.bgWG.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.bgWG.Done()
		defer func() {
			m.mu.Lock()
			delete(m.reconnecting, id)
			m.mu.Unlock()
		}()

		backoffs := m.reconnectSchedule()
		for _, delay := range backoffs {
			if delay > 0 {
				t := time.NewTimer(delay)
				select {
				case <-baseCtx.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}

			m.mu.Lock()
			if m.shuttingDown || !m.hasWatchersLocked(id) {
				m.mu.Unlock()
				return
			}
			existing := m.runs[id]
			m.mu.Unlock()
			if existing != nil && existing.isAlive() {
				existing.touch()
				m.pushLiveToWatchers(id, existing)
				return
			}

			attemptCtx, cancel := context.WithTimeout(baseCtx, defaultGrokCallTimeout)
			run, err := m.ensureRun(attemptCtx, id)
			cancel()
			if err == nil && run != nil {
				run.touch()
				m.pushLiveToWatchers(id, run)
				return
			}
			if baseCtx.Err() != nil {
				return
			}
		}

		m.clearConnectingForWatchers(id)
	}()
}

func (m *GrokManager) pushLiveToWatchers(id string, run *grokRun) {
	if run == nil {
		return
	}
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return
	}
	rec, exists := m.records[id]
	var clients []*Client
	maxWindow := grokDefaultWindow
	for c, ids := range m.watch {
		if w, ok := ids[id]; ok {
			clients = append(clients, c)
			if w > maxWindow {
				maxWindow = w
			}
		}
	}
	m.mu.Unlock()

	if len(clients) == 0 {
		return
	}
	if maxWindow > grokDefaultWindow && exists && rec.SessionDir != "" {
		if hist, err := readGrokTranscriptForRecord(rec, maxWindow); err == nil {
			run.mergeOlderDiskHistory(hist)
		}
	}
	if exists {
		stamp := m.computeWatchStamp(rec)
		m.mu.Lock()
		if m.watchedStamp == nil {
			m.watchedStamp = map[string]grokWatchStamp{}
		}
		m.watchedStamp[id] = stamp
		m.mu.Unlock()
	}

	meta, turns := m.snapshotRunForEmit(id, run)
	payload := mustJSON(map[string]any{
		"t":        "open",
		"provider": "grok",
		"id":       id,
		"meta":     meta,
		"turns":    turns,
		"more":     false,
	})
	for _, c := range clients {
		c.push(payload)
	}
}

func (m *GrokManager) clearConnectingForWatchers(id string) {
	m.mu.Lock()
	if m.shuttingDown || !m.hasWatchersLocked(id) {
		m.mu.Unlock()
		return
	}
	if existing := m.runs[id]; existing != nil && existing.isAlive() {
		m.mu.Unlock()
		return
	}
	rec, exists := m.records[id]
	m.mu.Unlock()

	mode := "tail"
	if exists && rec.External {
		mode = "external"
	}
	meta := Meta{
		Provider:   "grok",
		Mode:       mode,
		Connecting: false,
	}
	if exists {
		meta.Title = grokFirstNonEmpty(m.state.Title(id), grokRecordTitle(rec, ""))
		meta.Cwd = rec.Cwd
		meta.Branch = rec.HeadBranch
		meta.Model = rec.Model
		meta.Effort = rec.ReasoningEffort
		meta.CtxUsed = rec.CtxUsed
		meta.CtxMax = rec.CtxMax
		meta.Running = rec.Running
	}
	pref := m.state.Pref(id)
	if pref.Model != "" {
		meta.Model = pref.Model
	}
	if pref.Effort != "" {
		meta.Effort = pref.Effort
	}
	if pref.PermissionMode != "" {
		meta.PermissionMode = pref.PermissionMode
	}
	if meta.PermissionMode == "" {
		meta.PermissionMode = "default"
	}
	m.broadcast(id, map[string]any{"t": "meta", "meta": meta})
}

func (m *GrokManager) followWork(ctx context.Context) {
	interval := m.workPollInterval
	if interval <= 0 {
		interval = defaultGrokWorkPollInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.pollWork()
		}
	}
}

func (m *GrokManager) pollWork() {
	type sub struct {
		client *Client
		id     string
		watch  grokWorkWatch
	}
	var subs []sub
	m.mu.Lock()
	for c, items := range m.workWatch {
		for id, w := range items {
			subs = append(subs, sub{client: c, id: id, watch: w})
		}
	}
	m.mu.Unlock()
	for _, s := range subs {
		_ = m.refreshWork(s.client, s.id, s.watch, false)
	}
}

// ---- model catalog helpers ----

func grokModelsFromInfos(infos []grokModelInfo) ([]Model, map[string]int64) {
	if len(infos) == 0 {
		return nil, nil
	}
	models := make([]Model, 0, len(infos))
	ctxWindows := map[string]int64{}
	seen := map[string]bool{}
	for _, info := range infos {
		slug := strings.TrimSpace(info.ModelID)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		var efforts []string
		defEffort := strings.ToLower(strings.TrimSpace(info.DefaultReasoningEffort))
		for _, opt := range info.ReasoningEfforts {
			val := strings.ToLower(strings.TrimSpace(opt.Value))
			if val == "" {
				val = strings.ToLower(strings.TrimSpace(opt.ID))
			}
			if val != "" {
				efforts = append(efforts, val)
				if opt.Default && defEffort == "" {
					defEffort = val
				}
			}
		}
		efforts = sortGrokEfforts(efforts)
		if len(efforts) == 0 && info.SupportsReasoningEffort {
			efforts = []string{"minimal", "low", "medium", "high", "xhigh"}
		}
		if defEffort == "" && len(efforts) > 0 {
			for _, candidate := range []string{"high", "medium"} {
				if contains(efforts, candidate) {
					defEffort = candidate
					break
				}
			}
			if defEffort == "" {
				defEffort = efforts[0]
			}
		}
		if info.TotalContextTokens > 0 {
			ctxWindows[slug] = info.TotalContextTokens
		}
		models = append(models, Model{
			Slug:          slug,
			Name:          grokFirstNonEmpty(strings.TrimSpace(info.Name), slug),
			Description:   strings.TrimSpace(info.Description),
			DefaultEffort: defEffort,
			Efforts:       efforts,
		})
	}
	return models, ctxWindows
}

func mergeGrokModels(primary, fallback []Model) []Model {
	if len(primary) == 0 {
		out := make([]Model, len(fallback))
		for i, m := range fallback {
			m.Efforts = sortGrokEfforts(m.Efforts)
			m.DefaultEffort = strings.ToLower(strings.TrimSpace(m.DefaultEffort))
			out[i] = m
		}
		return out
	}
	fallbackBySlug := make(map[string]Model, len(fallback))
	for _, m := range fallback {
		fallbackBySlug[m.Slug] = m
	}
	out := make([]Model, 0, len(primary))
	seen := make(map[string]bool, len(primary))
	for _, m := range primary {
		if m.Slug == "" || seen[m.Slug] {
			continue
		}
		seen[m.Slug] = true
		m.Efforts = sortGrokEfforts(m.Efforts)
		m.DefaultEffort = strings.ToLower(strings.TrimSpace(m.DefaultEffort))
		if fb, ok := fallbackBySlug[m.Slug]; ok {
			if m.Name == "" || m.Name == m.Slug {
				if fb.Name != "" {
					m.Name = fb.Name
				}
			}
			if m.Description == "" {
				m.Description = fb.Description
			}
			if len(m.Efforts) == 0 && len(fb.Efforts) > 0 {
				m.Efforts = sortGrokEfforts(fb.Efforts)
			}
			if m.DefaultEffort == "" {
				m.DefaultEffort = strings.ToLower(strings.TrimSpace(fb.DefaultEffort))
			}
		}
		out = append(out, m)
	}
	return out
}

func (m *GrokManager) updateCatalogFromModelsState(ms *grokModelsState) {
	if ms == nil {
		return
	}
	models, ctxWindows := grokModelsFromInfos(ms.AvailableModels)
	if len(models) == 0 && strings.TrimSpace(ms.CurrentModelID) == "" {
		return
	}
	now := m.nowTime()
	m.mu.Lock()
	defer m.mu.Unlock()
	cat := m.catalog
	if len(models) > 0 {
		cat.Models = mergeGrokModels(models, cat.Models)
		cat.LiveLoaded = true
		cat.UpdatedAt = now
	}
	if cur := strings.TrimSpace(ms.CurrentModelID); cur != "" {
		cat.DefaultModel = cur
	}
	if len(ctxWindows) > 0 {
		if cat.ContextWindows == nil {
			cat.ContextWindows = map[string]int64{}
		}
		for k, v := range ctxWindows {
			cat.ContextWindows[k] = v
		}
	}
	if cat.DefaultMode == "" {
		cat.DefaultMode = "default"
	}
	m.catalog = cat
}

func (m *GrokManager) updateCatalogFromSessionState(state *grokSessionState, initRes *grokInitResult) {
	if initRes != nil && len(initRes.ModelState.AvailableModels) > 0 {
		m.updateCatalogFromModelsState(&initRes.ModelState)
	}
	if state == nil {
		return
	}
	if len(state.Models.AvailableModels) > 0 || state.Models.CurrentModelID != "" {
		m.updateCatalogFromModelsState(&state.Models)
	}
	if len(state.ConfigOptions) > 0 {
		m.updateCatalogFromConfigOptions(state.ConfigOptions)
	}
}

func (m *GrokManager) updateCatalogFromConfigOptions(opts []acpConfigOption) {
	if len(opts) == 0 {
		return
	}
	now := m.nowTime()
	m.mu.Lock()
	defer m.mu.Unlock()
	cat := m.catalog

	var efforts []string
	defaultEffort := cat.DefaultEffort
	for _, opt := range opts {
		if opt.ID == "reasoning_effort" || opt.ID == "thought_level" {
			for _, item := range opt.Options {
				if v := strings.TrimSpace(item.Value); v != "" {
					efforts = append(efforts, v)
				}
			}
			if v := strings.ToLower(strings.TrimSpace(opt.CurrentValue)); v != "" {
				defaultEffort = v
			}
		}
	}
	efforts = sortGrokEfforts(efforts)

	for _, opt := range opts {
		if opt.ID != "model" {
			continue
		}
		if len(opt.Options) > 0 {
			var models []Model
			for _, item := range opt.Options {
				slug := strings.TrimSpace(item.Value)
				if slug == "" {
					continue
				}
				models = append(models, Model{
					Slug:          slug,
					Name:          grokFirstNonEmpty(strings.TrimSpace(item.Name), slug),
					Description:   strings.TrimSpace(item.Description),
					DefaultEffort: defaultEffort,
					Efforts:       append([]string(nil), efforts...),
				})
			}
			if len(models) > 0 {
				cat.Models = mergeGrokModels(models, cat.Models)
				cat.LiveLoaded = true
				cat.UpdatedAt = now
			}
		}
		if v := strings.TrimSpace(opt.CurrentValue); v != "" {
			cat.DefaultModel = v
		}
	}

	if len(efforts) > 0 && len(cat.Models) > 0 {
		for i := range cat.Models {
			if len(cat.Models[i].Efforts) == 0 {
				cat.Models[i].Efforts = append([]string(nil), efforts...)
			} else {
				cat.Models[i].Efforts = sortGrokEfforts(cat.Models[i].Efforts)
			}
			if cat.Models[i].DefaultEffort == "" && defaultEffort != "" {
				cat.Models[i].DefaultEffort = defaultEffort
			}
		}
	}
	if defaultEffort != "" {
		cat.DefaultEffort = defaultEffort
	}
	if cat.DefaultMode == "" {
		cat.DefaultMode = "default"
	}
	m.catalog = cat
}

func (m *GrokManager) catalogSnapshot() grokModelCatalog {
	m.mu.Lock()
	defer m.mu.Unlock()
	cat := m.catalog
	cat.Models = make([]Model, len(m.catalog.Models))
	for i, model := range m.catalog.Models {
		model.Efforts = append([]string(nil), model.Efforts...)
		cat.Models[i] = model
	}
	if len(m.catalog.ContextWindows) > 0 {
		cat.ContextWindows = make(map[string]int64, len(m.catalog.ContextWindows))
		for k, v := range m.catalog.ContextWindows {
			cat.ContextWindows[k] = v
		}
	}
	if cat.DefaultMode == "" {
		cat.DefaultMode = "default"
	}
	return cat
}

func (m *GrokManager) defaultModes() []string {
	modes := []string{"default", "acceptEdits", "auto", "plan"}
	if m.allowFullAccess {
		modes = append(modes, "bypassPermissions")
	}
	return modes
}

func (m *GrokManager) validEffort(effort string) bool {
	effort = strings.ToLower(strings.TrimSpace(effort))
	switch effort {
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, model := range m.catalog.Models {
		if contains(model.Efforts, effort) {
			return true
		}
	}
	return false
}

func (m *GrokManager) validPermissionMode(mode string) bool {
	switch mode {
	case "default", "acceptEdits", "auto", "plan":
		return true
	case "bypassPermissions":
		return m.allowFullAccess
	default:
		return false
	}
}

// ---- live ACP run ----

type grokRun struct {
	manager *GrokManager
	id      string
	cwd     string

	daemon *grokDaemon
	client *grokACPClient

	mu               sync.Mutex
	alive            bool
	running          bool
	externalRunning  bool
	connecting       bool
	loading          bool
	activePrompts    int
	cancelled        bool
	lastActivity     time.Time
	unwatchedAt      time.Time
	recency          int64
	liveTurnSeq      int64
	activeTurnKey    string
	turnStartedAt    int64
	transcript       *grokTranscript
	replayTranscript *grokTranscript
	liveTurnKeys     map[string]bool
	optimisticUsers  []string
	optimisticImages int
	title            string
	branch           string
	model            string
	effort           string
	permissionMode   string
	ctxUsed          int64
	ctxMax           int64
	commands         []SlashCommand
	lastMsg          string
	lastErr          string
	permissions      map[string]grokPendingPerm
	askQuestions     map[string]grokPendingAsk
	exitPlans        map[string]grokPendingExitPlan
	elicitations     map[string]grokPendingElicit
	reqOrder         []string
	lastPublished    map[string][]byte
	lastMeta         []byte
	lastFlush        time.Time
	flushScheduled   bool
	flushTimer       *time.Timer
	closeOnce        sync.Once
	done             chan struct{}
}

func newGrokRun(m *GrokManager, id, cwd string) *grokRun {
	now := m.nowTime()
	tr := newGrokTranscript()
	tr.Meta.Provider = "grok"
	tr.Meta.Mode = "live"
	tr.Meta.Cwd = cwd
	tr.Meta.PermissionMode = "default"
	return &grokRun{
		manager:        m,
		id:             id,
		cwd:            cwd,
		alive:          true,
		lastActivity:   now,
		recency:        now.UnixMilli(),
		transcript:     tr,
		liveTurnKeys:   map[string]bool{},
		permissionMode: "default",
		permissions:    map[string]grokPendingPerm{},
		askQuestions:   map[string]grokPendingAsk{},
		exitPlans:      map[string]grokPendingExitPlan{},
		elicitations:   map[string]grokPendingElicit{},
		lastPublished:  map[string][]byte{},
		done:           make(chan struct{}),
	}
}

func (r *grokRun) isAlive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.alive
}

func (r *grokRun) isRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

func (r *grokRun) isExternalRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.externalRunning
}

func (r *grokRun) hasActivePrompts() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activePrompts > 0
}

func (r *grokRun) touch() {
	now := r.manager.nowTime()
	r.mu.Lock()
	if now.After(r.lastActivity) {
		r.lastActivity = now
	}
	r.unwatchedAt = time.Time{}
	r.connecting = false
	r.mu.Unlock()
}

func (r *grokRun) markUnwatched(now time.Time) {
	r.mu.Lock()
	if r.unwatchedAt.IsZero() {
		r.unwatchedAt = now
	}
	r.mu.Unlock()
}

func (r *grokRun) canReap(now time.Time, timeout time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.alive || r.running || r.externalRunning || r.activePrompts > 0 || r.loading {
		return false
	}
	if len(r.permissions) > 0 || len(r.askQuestions) > 0 || len(r.exitPlans) > 0 || len(r.elicitations) > 0 {
		return false
	}
	since := r.lastActivity
	if !r.unwatchedAt.IsZero() && r.unwatchedAt.After(since) {
		since = r.unwatchedAt
	}
	return now.Sub(since) >= timeout
}

func (r *grokRun) listState() grokListState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return grokListState{
		alive:        r.alive,
		running:      r.running || r.externalRunning,
		localRunning: r.running,
		attention:    len(r.permissions) > 0 || len(r.askQuestions) > 0 || len(r.exitPlans) > 0 || len(r.elicitations) > 0,
		turnAt:       r.turnStartedAt,
		recency:      r.recency,
		title:        r.title,
		cwd:          r.cwd,
		model:        r.model,
		effort:       r.effort,
		mode:         r.permissionMode,
		lastMsg:      r.lastMsg,
		lastErr:      r.lastErr,
	}
}

func (r *grokRun) seedFromTranscript(v *grokTranscript) {
	if v == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v.ensureInit()
	r.transcript = v
	r.transcript.Meta.Provider = "grok"
	r.transcript.Meta.Mode = "live"
	r.lastPublished = map[string][]byte{}
	for _, t := range v.Turns {
		r.lastPublished[t.Key] = mustJSON(t)
	}
	if r.title == "" && v.Meta.Title != "" {
		r.title = v.Meta.Title
	}
	if r.cwd == "" && v.Meta.Cwd != "" {
		r.cwd = v.Meta.Cwd
	}
	if r.branch == "" && v.Meta.Branch != "" {
		r.branch = v.Meta.Branch
	}
	if r.model == "" && v.Meta.Model != "" {
		r.model = v.Meta.Model
	}
	if r.effort == "" && v.Meta.Effort != "" {
		r.effort = v.Meta.Effort
	}
	if v.Meta.PermissionMode != "" {
		r.permissionMode = normalizeGrokPermissionMode(v.Meta.PermissionMode)
	}
	if r.ctxUsed == 0 && v.Meta.CtxUsed > 0 {
		r.ctxUsed = v.Meta.CtxUsed
	}
	if r.ctxMax == 0 && v.Meta.CtxMax > 0 {
		r.ctxMax = v.Meta.CtxMax
	}
	if len(v.Meta.Commands) > 0 && len(r.commands) == 0 {
		r.commands = append([]SlashCommand(nil), v.Meta.Commands...)
	}
	if v.LastMsg != "" {
		r.lastMsg = clipPlain(v.LastMsg, 200)
	}
	if v.LastErr != "" {
		r.lastErr = clipPlain(v.LastErr, 240)
	}
	if v.Meta.TurnAt > 0 && r.turnStartedAt == 0 {
		r.turnStartedAt = v.Meta.TurnAt
	}
}

func (r *grokRun) syncTranscriptMetaLocked() {
	if r.transcript == nil {
		return
	}
	m := r.transcript.Meta
	if m.Title != "" {
		r.title = m.Title
	}
	if m.Cwd != "" && r.cwd == "" {
		r.cwd = m.Cwd
	}
	if m.Branch != "" {
		r.branch = m.Branch
	}
	if m.Model != "" {
		r.model = m.Model
	}
	if m.Effort != "" {
		r.effort = m.Effort
	}
	if m.PermissionMode != "" {
		if norm := normalizeGrokPermissionMode(m.PermissionMode); norm != "" {
			r.permissionMode = norm
		}
	}
	if m.CtxUsed > 0 {
		r.ctxUsed = m.CtxUsed
	}
	if m.CtxMax > 0 {
		r.ctxMax = m.CtxMax
	}
	if len(m.Commands) > 0 {
		r.commands = append([]SlashCommand(nil), m.Commands...)
	}
	if r.transcript.LastMsg != "" {
		r.lastMsg = clipPlain(r.transcript.LastMsg, 200)
	}
	if r.transcript.LastErr != "" {
		r.lastErr = clipPlain(r.transcript.LastErr, 240)
	}
}

func (r *grokRun) applySessionState(state *grokSessionState) {
	if state == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if model := strings.TrimSpace(state.CurrentModel()); model != "" {
		r.model = model
		if r.transcript != nil {
			r.transcript.Meta.Model = model
		}
	}
	if effort := strings.TrimSpace(state.CurrentReasoningEffort()); effort != "" {
		r.effort = effort
		if r.transcript != nil {
			r.transcript.Meta.Effort = effort
		}
	}
	if state.Detail != nil {
		if title := cleanGrokTitle(state.Detail.Title); title != "" && r.title == "" {
			r.title = title
		}
		if cwd := strings.TrimSpace(state.Detail.Cwd); cwd != "" && r.cwd == "" {
			r.cwd = filepath.Clean(cwd)
		}
	}
}

func (r *grokRun) splitDiskAndLiveTurnsLocked() (disk []Turn, live []Turn) {
	if r.transcript == nil {
		return nil, nil
	}
	for _, t := range r.transcript.Turns {
		if r.liveTurnKeys[t.Key] {
			live = append(live, t)
		} else {
			disk = append(disk, t)
		}
	}
	return disk, live
}

func (r *grokRun) rebuildTranscriptIndexesLocked() {
	if r.transcript == nil {
		return
	}
	v := r.transcript
	v.ensureInit()
	v.toolIndex = make(map[string][2]int, len(v.Turns)*2)
	v.toolTurnKey = make(map[string]string, len(v.Turns)*2)
	v.planIndex = make(map[int]int, len(v.Turns))
	for ti, t := range v.Turns {
		for ii, it := range t.Items {
			if it.ID == t.Key+":plan" {
				v.planIndex[ti] = ii
			} else if it.K == "cmd" || it.K == "file" || it.K == "search" || it.K == "agent" || it.K == "tool" {
				v.toolIndex[it.ID] = [2]int{ti, ii}
				v.toolTurnKey[it.ID] = t.Key
			}
		}
	}
	v.cur = len(v.Turns) - 1
	v.streamKind = ""
	v.streamIdx = -1
}

func (r *grokRun) mergeDiffMapsLocked(src *grokTranscript) {
	if r.transcript == nil || src == nil {
		return
	}
	r.transcript.ensureInit()
	for k, d := range src.ToolDiffs {
		if _, exists := r.transcript.ToolDiffs[k]; !exists && d != "" {
			r.transcript.ToolDiffs[k] = d
		}
	}
	for k, d := range src.TurnDiffs {
		if _, exists := r.transcript.TurnDiffs[k]; !exists && d != "" {
			r.transcript.TurnDiffs[k] = d
		}
	}
	for k, d := range src.Diffs {
		if _, exists := r.transcript.Diffs[k]; !exists && d != "" {
			r.transcript.Diffs[k] = d
		}
	}
}

func (r *grokRun) mergeOlderDiskHistory(v *grokTranscript) {
	if v == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.transcript == nil {
		r.transcript = newGrokTranscript()
	}
	disk, live := r.splitDiskAndLiveTurnsLocked()
	if len(live) == 0 {
		r.transcript.Turns = cloneTurns(v.Turns)
	} else {
		cutoff := live[0].StartedAt
		var older []Turn
		for _, t := range v.Turns {
			if cutoff > 0 && t.StartedAt > 0 && t.StartedAt >= cutoff {
				continue
			}
			older = append(older, t)
		}
		if len(older) > len(disk) {
			combined := make([]Turn, 0, len(older)+len(live))
			combined = append(combined, cloneTurns(older)...)
			combined = append(combined, live...)
			r.transcript.Turns = combined
		}
	}
	r.mergeDiffMapsLocked(v)
	r.rebuildTranscriptIndexesLocked()
	r.transcript.More = v.More
	r.transcript.Meta.HasOlder = v.More
	if r.lastPublished == nil {
		r.lastPublished = map[string][]byte{}
	}
	for _, t := range r.transcript.Turns {
		if !r.liveTurnKeys[t.Key] {
			r.lastPublished[t.Key] = mustJSON(t)
		}
	}
}

func (r *grokRun) syncExternalDiskState(v *grokTranscript, externalRunning bool) (bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.alive || r.running {
		return false, false
	}
	changed := false
	fullOpen := false
	if r.externalRunning != externalRunning {
		r.externalRunning = externalRunning
		changed = true
	}
	if v == nil {
		return changed, false
	}
	if r.transcript == nil {
		r.transcript = newGrokTranscript()
	}
	if v.Meta.Title != "" && r.title == "" {
		r.title = v.Meta.Title
		changed = true
	}
	if v.Meta.Branch != "" && r.branch == "" {
		r.branch = v.Meta.Branch
		changed = true
	}
	if v.Meta.Model != "" && r.model == "" {
		r.model = v.Meta.Model
		changed = true
	}
	if v.Meta.Effort != "" && r.effort == "" {
		r.effort = v.Meta.Effort
		changed = true
	}
	if v.Meta.CtxUsed > r.ctxUsed {
		r.ctxUsed = v.Meta.CtxUsed
		r.transcript.Meta.CtxUsed = v.Meta.CtxUsed
		changed = true
	}
	if v.Meta.CtxMax > r.ctxMax {
		r.ctxMax = v.Meta.CtxMax
		r.transcript.Meta.CtxMax = v.Meta.CtxMax
		changed = true
	}
	if r.transcript.More != v.More {
		r.transcript.More = v.More
		r.transcript.Meta.HasOlder = v.More
		changed = true
	}
	for k, d := range v.ToolDiffs {
		if r.transcript.ToolDiffs[k] != d {
			r.transcript.ToolDiffs[k] = d
			r.transcript.Diffs[k] = d
			changed = true
		}
	}
	for k, d := range v.TurnDiffs {
		if r.transcript.TurnDiffs[k] != d {
			r.transcript.TurnDiffs[k] = d
			r.transcript.Diffs[k] = d
			changed = true
		}
	}
	if len(v.Turns) > 0 {
		disk, live := r.splitDiskAndLiveTurnsLocked()
		if len(live) == 0 {
			newDisk := cloneTurns(v.Turns)
			if !bytes.Equal(mustJSON(disk), mustJSON(newDisk)) {
				r.transcript.Turns = newDisk
				r.rebuildTranscriptIndexesLocked()
				if last := newDisk[len(newDisk)-1]; last.StartedAt > r.recency {
					r.recency = last.StartedAt
				}
				if msg := antigravityLastAssistantText(newDisk); msg != "" {
					r.lastMsg = clipPlain(msg, 200)
				}
				changed = true
			}
		} else {
			lastDisk := v.Turns[len(v.Turns)-1]
			lastLive := live[len(live)-1]
			grewBeyondLive := len(v.Turns) > len(disk)+len(live)
			newerExternal := lastDisk.StartedAt > lastLive.StartedAt &&
				antigravityTurnUserText(lastDisk) != "" &&
				antigravityTurnUserText(lastDisk) != antigravityTurnUserText(lastLive)
			if grewBeyondLive || newerExternal {
				newDisk := cloneTurns(v.Turns)
				r.transcript.Turns = newDisk
				r.liveTurnKeys = map[string]bool{}
				r.activeTurnKey = ""
				r.rebuildTranscriptIndexesLocked()
				r.lastPublished = map[string][]byte{}
				for _, t := range newDisk {
					r.lastPublished[t.Key] = mustJSON(t)
				}
				if last := newDisk[len(newDisk)-1]; last.StartedAt > r.recency {
					r.recency = last.StartedAt
				}
				if msg := antigravityLastAssistantText(newDisk); msg != "" {
					r.lastMsg = clipPlain(msg, 200)
				}
				changed = true
				fullOpen = true
			}
		}
	}
	return changed, fullOpen
}

func (r *grokRun) snapshot() (Meta, []Turn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *grokRun) snapshotLocked() (Meta, []Turn) {
	title := r.title
	if custom := r.manager.state.Title(r.id); custom != "" {
		title = custom
	}
	mode := "live"
	if !r.alive {
		mode = "tail"
	}
	hasOlder := false
	var turns []Turn
	if r.transcript != nil {
		hasOlder = r.transcript.More || r.transcript.Meta.HasOlder
		turns = cloneTurns(r.transcript.Turns)
	} else {
		turns = []Turn{}
	}
	meta := Meta{
		Connecting:     r.connecting,
		Provider:       "grok",
		Mode:           mode,
		Title:          title,
		Cwd:            r.cwd,
		Branch:         r.branch,
		Model:          r.model,
		Effort:         r.effort,
		PermissionMode: grokFirstNonEmpty(r.permissionMode, "default"),
		CtxUsed:        r.ctxUsed,
		CtxMax:         r.ctxMax,
		TurnAt:         r.turnStartedAt,
		HasOlder:       hasOlder,
		Running:        r.running || r.externalRunning,
	}
	if len(r.commands) > 0 {
		meta.Commands = append([]SlashCommand(nil), r.commands...)
	}
	if len(r.reqOrder) > 0 {
		meta.Requests = make([]Request, 0, len(r.reqOrder))
		for _, reqID := range r.reqOrder {
			if p, ok := r.permissions[reqID]; ok {
				meta.Requests = append(meta.Requests, cloneRequest(p.req))
			} else if a, ok := r.askQuestions[reqID]; ok {
				meta.Requests = append(meta.Requests, cloneRequest(a.req))
			} else if ep, ok := r.exitPlans[reqID]; ok {
				meta.Requests = append(meta.Requests, cloneRequest(ep.req))
			} else if e, ok := r.elicitations[reqID]; ok {
				meta.Requests = append(meta.Requests, cloneRequest(e.req))
			}
		}
	}
	return meta, turns
}

func (r *grokRun) publish(immediate bool) {
	r.mu.Lock()
	sid := r.id
	if sid == "" {
		r.mu.Unlock()
		return
	}
	if !immediate {
		if delay := defaultGrokFlushInterval - time.Since(r.lastFlush); delay > 0 {
			if !r.flushScheduled {
				r.flushScheduled = true
				r.flushTimer = time.AfterFunc(delay, func() {
					r.mu.Lock()
					r.flushScheduled = false
					r.flushTimer = nil
					r.mu.Unlock()
					r.publish(true)
				})
			}
			r.mu.Unlock()
			return
		}
	}
	r.lastFlush = time.Now()
	meta, turns := r.snapshotLocked()
	var messages []map[string]any
	for _, t := range turns {
		b := mustJSON(t)
		if !bytes.Equal(b, r.lastPublished[t.Key]) {
			r.lastPublished[t.Key] = b
			messages = append(messages, map[string]any{"t": "turn", "turn": t})
		}
	}
	metaBytes := mustJSON(meta)
	if !bytes.Equal(metaBytes, r.lastMeta) {
		r.lastMeta = metaBytes
		messages = append(messages, map[string]any{"t": "meta", "meta": meta})
	}
	r.mu.Unlock()

	for _, msg := range messages {
		r.manager.broadcast(sid, msg)
	}
}

func (r *grokRun) ensureLiveTurnLocked(nowMs int64) *Turn {
	if r.transcript == nil {
		r.transcript = newGrokTranscript()
	}
	r.transcript.ensureInit()
	if len(r.transcript.Turns) > 0 && r.activeTurnKey != "" {
		last := &r.transcript.Turns[len(r.transcript.Turns)-1]
		if last.Key == r.activeTurnKey {
			r.transcript.cur = len(r.transcript.Turns) - 1
			return last
		}
	}
	r.liveTurnSeq++
	key := fmt.Sprintf("live-%d", r.liveTurnSeq)
	r.activeTurnKey = key
	if nowMs == 0 {
		nowMs = r.manager.nowTime().UnixMilli()
	}
	r.turnStartedAt = nowMs
	r.transcript.Turns = append(r.transcript.Turns, Turn{
		Key:       key,
		TurnID:    key,
		Status:    "inProgress",
		StartedAt: nowMs,
		Items:     []Item{},
	})
	r.transcript.cur = len(r.transcript.Turns) - 1
	r.transcript.streamKind = ""
	r.transcript.streamIdx = -1
	r.transcript.Meta.TurnAt = nowMs
	if r.liveTurnKeys == nil {
		r.liveTurnKeys = map[string]bool{}
	}
	r.liveTurnKeys[key] = true
	return &r.transcript.Turns[r.transcript.cur]
}

func (r *grokRun) drainDispatch(ctx context.Context, client *grokACPClient) {
	r.mu.Lock()
	daemon := r.daemon
	sid := r.id
	r.mu.Unlock()
	if daemon != nil {
		daemon.drainDispatchForSession(ctx, sid)
		return
	}
	if client != nil && !client.Closed() {
		_ = client.DrainDispatch(ctx)
	}
}

func grokSessionUpdateRaw(u grokSessionUpdate) json.RawMessage {
	if len(bytes.TrimSpace(u.Raw)) > 0 {
		return u.Raw
	}
	payload := map[string]any{"sessionUpdate": u.Kind}
	if u.Text != "" {
		payload["text"] = u.Text
	}
	if u.Content != nil {
		payload["content"] = u.Content
	}
	if u.ToolCall != nil {
		payload["toolCallId"] = u.ToolCall.ToolCallID
		payload["title"] = u.ToolCall.Title
		payload["kind"] = u.ToolCall.Kind
		payload["status"] = u.ToolCall.Status
	}
	if len(u.Plan) > 0 {
		payload["entries"] = u.Plan
	}
	if u.Usage != nil {
		payload["used"] = u.Usage.Used
		payload["size"] = u.Usage.Size
	}
	if len(u.AvailableCommands) > 0 {
		payload["availableCommands"] = u.AvailableCommands
	}
	if u.Title != "" {
		payload["title"] = u.Title
	}
	if u.CurrentModeID != "" {
		payload["currentModeId"] = u.CurrentModeID
	}
	if len(u.ConfigOptions) > 0 {
		payload["configOptions"] = u.ConfigOptions
	}
	return mustJSON(payload)
}

func grokExtUpdateRaw(u grokExtSessionUpdate) json.RawMessage {
	if len(bytes.TrimSpace(u.Raw)) > 0 && grokUpdateKind(u.Raw) != "" {
		return u.Raw
	}
	payload := map[string]any{"type": u.Kind}
	if u.StopReason != "" {
		payload["stop_reason"] = u.StopReason
	}
	if u.ElapsedMs > 0 {
		payload["elapsed_ms"] = u.ElapsedMs
	}
	if u.Usage != nil {
		payload["usage"] = u.Usage
	}
	if u.SubagentID != "" {
		payload["subagent_id"] = u.SubagentID
	}
	if u.ChildSessionID != "" {
		payload["child_session_id"] = u.ChildSessionID
	}
	if u.Status != "" {
		payload["status"] = u.Status
	}
	if u.Summary != "" {
		payload["summary"] = u.Summary
	}
	return mustJSON(payload)
}

func grokSessionNotificationRaw(n grokSessionNotification) json.RawMessage {
	if len(bytes.TrimSpace(n.Raw)) > 0 && grokUpdateKind(n.Raw) != "" {
		return n.Raw
	}
	payload := map[string]any{"type": n.Kind}
	if n.ModelID != "" {
		payload["modelId"] = n.ModelID
	}
	if n.Message != "" {
		payload["message"] = n.Message
	}
	return mustJSON(payload)
}

func (r *grokRun) onUpdate(u grokSessionUpdate) {
	now := r.manager.nowTime()
	nowMs := now.UnixMilli()
	tsMs := u.AgentTimestampMs
	if tsMs <= 0 {
		tsMs = nowMs
	}

	var refreshList bool
	var updatedConfig []acpConfigOption
	var shouldPublish bool

	r.mu.Lock()
	if u.SessionID != "" && r.id != "" && u.SessionID != r.id {
		r.mu.Unlock()
		return
	}
	r.lastActivity = now
	if !r.loading {
		r.recency = nowMs
	}
	if r.transcript == nil {
		r.transcript = newGrokTranscript()
	}

	rawUpdate := grokSessionUpdateRaw(u)
	switch u.Kind {
	case "usage_update", "available_commands_update", "session_info_update", "current_mode_update", "config_option_update", "config_options_update":
		applied := r.transcript.ApplyUpdate("session/update", rawUpdate, tsMs)
		if u.Kind == "config_option_update" || u.Kind == "config_options_update" {
			for _, opt := range u.ConfigOptions {
				val := strings.TrimSpace(opt.CurrentValue)
				switch opt.ID {
				case "model":
					if val != "" {
						r.model = val
						r.transcript.Meta.Model = val
					}
				case "reasoning_effort", "thought_level":
					r.effort = val
					r.transcript.Meta.Effort = val
				case "mode", "permission_mode":
					if norm := normalizeGrokPermissionMode(val); norm != "" {
						r.permissionMode = norm
						r.transcript.Meta.PermissionMode = norm
					}
				}
			}
			updatedConfig = append([]acpConfigOption(nil), u.ConfigOptions...)
			applied = true
		}
		r.syncTranscriptMetaLocked()
		if u.Kind == "session_info_update" && applied {
			refreshList = true
		}
		shouldPublish = !r.loading && applied

	case "user_message_chunk":
		text := u.Text
		if text == "" && u.Content != nil {
			text = u.Content.Text
		}
		hasImage := u.Content != nil && strings.Contains(strings.ToLower(u.Content.Type), "image")
		if r.loading {
			if r.replayTranscript == nil {
				r.replayTranscript = newGrokTranscript()
			}
			r.replayTranscript.ApplyUpdate("session/update", rawUpdate, tsMs)
		} else if hasImage && r.optimisticImages > 0 && (text == "" || r.consumeOptimisticUserLocked(text)) {
			r.optimisticImages--
		} else if !hasImage && text != "" && r.consumeOptimisticUserLocked(text) {
			// Suppress echo of optimistic user bubble already rendered by startPrompt.
		} else {
			if r.transcript.ApplyUpdate("session/update", rawUpdate, tsMs) {
				if cur := r.transcript.cur; cur >= 0 && cur < len(r.transcript.Turns) {
					key := r.transcript.Turns[cur].Key
					r.liveTurnKeys[key] = true
					if r.running {
						r.transcript.Turns[cur].Status = "inProgress"
						r.activeTurnKey = key
					}
				}
				shouldPublish = true
			}
		}

	case "agent_thought_chunk", "agent_message_chunk", "tool_call", "tool_call_update", "plan":
		if r.loading {
			if r.replayTranscript == nil {
				r.replayTranscript = newGrokTranscript()
			}
			r.replayTranscript.ApplyUpdate("session/update", rawUpdate, tsMs)
		} else {
			if r.activeTurnKey == "" {
				r.ensureLiveTurnLocked(tsMs)
			}
			if r.transcript.ApplyUpdate("session/update", rawUpdate, tsMs) {
				if cur := r.transcript.cur; cur >= 0 && cur < len(r.transcript.Turns) {
					r.liveTurnKeys[r.transcript.Turns[cur].Key] = true
					if r.running {
						r.transcript.Turns[cur].Status = "inProgress"
					}
				}
				r.syncTranscriptMetaLocked()
				shouldPublish = true
			}
		}
	}
	r.mu.Unlock()

	if len(updatedConfig) > 0 {
		r.manager.updateCatalogFromConfigOptions(updatedConfig)
	}
	if shouldPublish {
		r.publish(false)
	}
	if refreshList {
		r.manager.requestRefresh()
	}
}

func (r *grokRun) onExtUpdate(u grokExtSessionUpdate) {
	now := r.manager.nowTime()
	nowMs := now.UnixMilli()
	tsMs := u.AgentTimestampMs
	if tsMs <= 0 {
		tsMs = nowMs
	}

	var shouldPublish bool
	var refreshList bool

	r.mu.Lock()
	if u.SessionID != "" && r.id != "" && u.SessionID != r.id {
		r.mu.Unlock()
		return
	}
	r.lastActivity = now
	if !r.loading {
		r.recency = nowMs
	}
	if r.transcript == nil {
		r.transcript = newGrokTranscript()
	}

	raw := grokExtUpdateRaw(u)
	if r.loading {
		if r.replayTranscript == nil {
			r.replayTranscript = newGrokTranscript()
		}
		r.replayTranscript.ApplyUpdate(grokMethodSessionUpdate, raw, tsMs)
	} else {
		if r.transcript.ApplyUpdate(grokMethodSessionUpdate, raw, tsMs) {
			r.syncTranscriptMetaLocked()
			shouldPublish = true
		}
		if u.Kind == "subagent_spawned" || u.Kind == "subagent_finished" {
			refreshList = true
		}
	}
	r.mu.Unlock()

	if shouldPublish {
		r.publish(false)
	}
	if refreshList {
		r.manager.requestRefresh()
	}
}

func (r *grokRun) onSessionNotification(n grokSessionNotification) {
	now := r.manager.nowTime()
	nowMs := now.UnixMilli()

	var shouldPublish bool
	var refreshList bool

	r.mu.Lock()
	if n.SessionID != "" && r.id != "" && n.SessionID != r.id {
		r.mu.Unlock()
		return
	}
	r.lastActivity = now
	if !r.loading {
		r.recency = nowMs
	}
	if r.transcript == nil {
		r.transcript = newGrokTranscript()
	}

	raw := grokSessionNotificationRaw(n)
	applied := r.transcript.ApplyUpdate(grokMethodSessionNotification, raw, nowMs)
	if strings.EqualFold(strings.TrimSpace(n.Kind), "model_changed") && strings.TrimSpace(n.ModelID) != "" {
		r.model = strings.TrimSpace(n.ModelID)
		r.transcript.Meta.Model = r.model
		applied = true
		refreshList = true
	}
	r.syncTranscriptMetaLocked()
	shouldPublish = !r.loading && applied
	r.mu.Unlock()

	if shouldPublish {
		r.publish(false)
	}
	if refreshList {
		r.manager.requestRefresh()
	}
}

func (r *grokRun) consumeOptimisticUserLocked(chunk string) bool {
	if len(r.optimisticUsers) == 0 {
		return false
	}
	want := r.optimisticUsers[0]
	if chunk == want {
		r.optimisticUsers = r.optimisticUsers[1:]
		return true
	}
	if strings.HasPrefix(want, chunk) {
		rest := strings.TrimPrefix(want, chunk)
		if rest == "" {
			r.optimisticUsers = r.optimisticUsers[1:]
		} else {
			r.optimisticUsers[0] = rest
		}
		return true
	}
	return false
}

func (r *grokRun) onPermission(req grokPermissionRequest) {
	now := r.manager.nowTime()
	diffText := req.ToolCall.DiffText()
	toolKind := classifyGrokToolKind(req.ToolCall, diffText)

	reqKind := "command"
	defaultTitle := "Run Command?"
	if toolKind == "file" {
		reqKind = "file"
		defaultTitle = "Apply File Changes?"
	} else if toolKind != "cmd" {
		defaultTitle = "Permission Required"
	}

	title := grokFirstNonEmpty(strings.TrimSpace(req.ToolCall.Title), strings.TrimSpace(req.ToolCall.Name), defaultTitle)
	inp := req.ToolCall.InputMap()
	var body string
	switch reqKind {
	case "file":
		if diffText != "" {
			body = clipHead(diffText, maxDiff)
		} else if p := grokFirstNonEmpty(str(inp, "path"), str(inp, "file_path"), str(inp, "Path"), str(inp, "TargetFile")); p != "" {
			body = p
		} else if len(req.ToolCall.Locations) > 0 {
			body = req.ToolCall.Locations[0].Path
		} else {
			body = compactJSON(inp, 1200)
		}
	default:
		cmdText := grokFirstNonEmpty(str(inp, "command"), str(inp, "CommandLine"), str(inp, "cmd"))
		if cmdText != "" {
			body = shellScript(cmdText)
		} else if len(inp) > 0 {
			body = compactJSON(inp, 1200)
		} else {
			body = strings.TrimSpace(req.ToolCall.Title)
		}
	}

	options := make([]RequestOption, 0, len(req.Options))
	for _, opt := range req.Options {
		options = append(options, RequestOption{
			ID:    opt.OptionID,
			Label: grokFirstNonEmpty(opt.Name, opt.OptionID),
			Kind:  opt.Kind,
		})
	}

	wireReq := Request{
		ID:      req.RequestID,
		Method:  "session/request_permission",
		Kind:    reqKind,
		Title:   title,
		Body:    body,
		Options: options,
	}

	r.mu.Lock()
	if req.SessionID != "" && r.id != "" && req.SessionID != r.id {
		r.mu.Unlock()
		return
	}
	sid := r.id
	r.lastActivity = now
	if _, exists := r.permissions[req.RequestID]; !exists {
		r.reqOrder = append(r.reqOrder, req.RequestID)
	}
	r.permissions[req.RequestID] = grokPendingPerm{
		acpReq: req,
		req:    wireReq,
		at:     now,
	}
	if diffText != "" && req.ToolCall.ToolCallID != "" {
		turn := r.ensureLiveTurnLocked(now.UnixMilli())
		r.transcript.ensureInit()
		turn.HasDiff = true
		r.transcript.ToolDiffs[req.ToolCall.ToolCallID] = diffText
		r.transcript.Diffs[req.ToolCall.ToolCallID] = diffText
		r.transcript.toolTurnKey[req.ToolCall.ToolCallID] = turn.Key
		existing := r.transcript.TurnDiffs[turn.Key]
		if existing == "" {
			r.transcript.TurnDiffs[turn.Key] = diffText
			r.transcript.Diffs[turn.Key] = diffText
		} else if !strings.Contains(existing, diffText) {
			if !strings.HasSuffix(existing, "\n") {
				existing += "\n"
			}
			combined := existing + diffText
			r.transcript.TurnDiffs[turn.Key] = combined
			r.transcript.Diffs[turn.Key] = combined
		}
	}
	threadTitle := grokFirstNonEmpty(r.manager.state.Title(sid), r.title, "Grok")
	cwd := r.cwd
	r.mu.Unlock()

	r.publish(true)
	r.manager.requestRefresh()
	if sid != "" {
		eventText := clipPlain(grokFirstNonEmpty(body, title, "Approval Required"), 200)
		r.manager.event(sid, "attention", threadTitle, eventText, cwd)
	}
}

func (r *grokRun) onExitPlanMode(req grokExitPlanModeRequest) {
	now := r.manager.nowTime()
	wireReq := Request{
		ID:     req.RequestID,
		Method: grokExtExitPlanMode,
		Kind:   "plan",
		Title:  "Review Plan",
		Body:   req.PlanContent,
		Options: []RequestOption{
			{ID: "approved", Label: "Approve Plan", Kind: "allow_once"},
			{ID: "revise", Label: "Revise", Kind: "other"},
			{ID: "abandoned", Label: "Decline", Kind: "reject_once"},
		},
	}

	r.mu.Lock()
	if req.SessionID != "" && r.id != "" && req.SessionID != r.id {
		r.mu.Unlock()
		return
	}
	sid := r.id
	r.lastActivity = now
	if _, exists := r.exitPlans[req.RequestID]; !exists {
		r.reqOrder = append(r.reqOrder, req.RequestID)
	}
	r.exitPlans[req.RequestID] = grokPendingExitPlan{
		acpReq: req,
		req:    wireReq,
		at:     now,
	}
	threadTitle := grokFirstNonEmpty(r.manager.state.Title(sid), r.title, "Grok")
	cwd := r.cwd
	r.mu.Unlock()

	r.publish(true)
	r.manager.requestRefresh()
	if sid != "" {
		eventText := clipPlain(grokFirstNonEmpty(strings.TrimSpace(req.PlanContent), "Review Plan"), 200)
		r.manager.event(sid, "attention", threadTitle, eventText, cwd)
	}
}

func (r *grokRun) onAskUserQuestion(req grokAskUserQuestionRequest) {
	now := r.manager.nowTime()
	questions := make([]Question, 0, len(req.Questions))
	for i, q := range req.Questions {
		qID := grokFirstNonEmpty(strings.TrimSpace(q.ID), fmt.Sprintf("q%d", i+1))
		header := ""
		if len(req.Questions) > 1 {
			header = fmt.Sprintf("Question %d", i+1)
		}
		item := Question{
			ID:          qID,
			Header:      header,
			Question:    strings.TrimSpace(q.Question),
			MultiSelect: q.MultiSelect,
			Other:       len(q.Options) > 0,
			FreeText:    len(q.Options) == 0,
		}
		for _, opt := range q.Options {
			label := grokFirstNonEmpty(strings.TrimSpace(opt.Label), strings.TrimSpace(opt.ID))
			if label == "" {
				continue
			}
			item.Options = append(item.Options, label)
			item.Descriptions = append(item.Descriptions, strings.TrimSpace(opt.Description))
		}
		if len(item.Options) == 0 {
			item.Other = false
			item.FreeText = true
		}
		questions = append(questions, item)
	}
	if len(questions) == 0 {
		questions = append(questions, Question{
			ID:       "response",
			Header:   "Response",
			Question: "Provide Your Answer",
			FreeText: true,
		})
	}

	wireReq := Request{
		ID:        req.RequestID,
		Method:    grokExtAskUserQuestion,
		Kind:      "question",
		Title:     "Grok Has A Question",
		Questions: questions,
	}

	r.mu.Lock()
	if req.SessionID != "" && r.id != "" && req.SessionID != r.id {
		r.mu.Unlock()
		return
	}
	sid := r.id
	r.lastActivity = now
	if _, exists := r.askQuestions[req.RequestID]; !exists {
		r.reqOrder = append(r.reqOrder, req.RequestID)
	}
	r.askQuestions[req.RequestID] = grokPendingAsk{
		acpReq: req,
		req:    wireReq,
		at:     now,
	}
	threadTitle := grokFirstNonEmpty(r.manager.state.Title(sid), r.title, "Grok")
	cwd := r.cwd
	r.mu.Unlock()

	r.publish(true)
	r.manager.requestRefresh()
	if sid != "" {
		eventText := clipPlain(grokFirstNonEmpty(questions[0].Question, "Input Required"), 200)
		r.manager.event(sid, "attention", threadTitle, eventText, cwd)
	}
}

func (r *grokRun) onElicitation(req grokElicitationRequest) {
	now := r.manager.nowTime()
	questions := make([]Question, 0, len(req.Fields))
	for _, f := range req.Fields {
		q := Question{
			ID:          f.Name,
			Header:      grokFirstNonEmpty(strings.TrimSpace(f.Title), humanize(strings.ReplaceAll(f.Name, "_", " "))),
			Question:    grokFirstNonEmpty(strings.TrimSpace(f.Description), strings.TrimSpace(f.Title), f.Name),
			Secret:      f.Secret,
			MultiSelect: f.MultiSelect,
		}
		for _, opt := range f.Options {
			label := grokFirstNonEmpty(opt.Label, opt.Value)
			q.Options = append(q.Options, label)
			q.Descriptions = append(q.Descriptions, opt.Description)
		}
		if len(q.Options) == 0 {
			q.FreeText = true
		}
		questions = append(questions, q)
	}
	if len(questions) == 0 {
		questions = append(questions, Question{
			ID:       "response",
			Header:   "Response",
			Question: grokFirstNonEmpty(strings.TrimSpace(req.Message), strings.TrimSpace(req.URL), "Provide Your Answer"),
			FreeText: true,
		})
	}

	title := grokFirstNonEmpty(clipPlain(strings.TrimSpace(req.Message), 120), "Grok Has A Question")
	wireReq := Request{
		ID:        req.RequestID,
		Method:    grokExtMCPElicit,
		Kind:      "question",
		Title:     title,
		Body:      strings.TrimSpace(req.Message),
		Questions: questions,
	}

	r.mu.Lock()
	if req.SessionID != "" && r.id != "" && req.SessionID != r.id {
		r.mu.Unlock()
		return
	}
	sid := r.id
	r.lastActivity = now
	if _, exists := r.elicitations[req.RequestID]; !exists {
		r.reqOrder = append(r.reqOrder, req.RequestID)
	}
	r.elicitations[req.RequestID] = grokPendingElicit{
		acpReq: req,
		req:    wireReq,
		at:     now,
	}
	threadTitle := grokFirstNonEmpty(r.manager.state.Title(sid), r.title, "Grok")
	cwd := r.cwd
	r.mu.Unlock()

	r.publish(true)
	r.manager.requestRefresh()
	if sid != "" {
		eventText := clipPlain(grokFirstNonEmpty(req.Message, questions[0].Question, "Input Required"), 200)
		r.manager.event(sid, "attention", threadTitle, eventText, cwd)
	}
}

// onTransportClose runs inside grokACPClient.dispatchLoop; NEVER call client.Close() here!
func (r *grokRun) onTransportClose(closeErr error, shuttingDown bool) {
	r.mu.Lock()
	sid := r.id
	wasRunning := r.running
	wasCancelled := r.cancelled
	r.alive = false
	r.running = false
	r.externalRunning = false
	r.activePrompts = 0
	r.permissions = map[string]grokPendingPerm{}
	r.askQuestions = map[string]grokPendingAsk{}
	r.exitPlans = map[string]grokPendingExitPlan{}
	r.elicitations = map[string]grokPendingElicit{}
	r.reqOrder = nil
	if r.flushTimer != nil {
		r.flushTimer.Stop()
		r.flushTimer = nil
		r.flushScheduled = false
	}
	if wasRunning && r.transcript != nil && len(r.transcript.Turns) > 0 {
		last := &r.transcript.Turns[len(r.transcript.Turns)-1]
		if last.Status == "inProgress" {
			if wasCancelled {
				last.Status = "interrupted"
			} else {
				last.Status = "failed"
				if closeErr != nil {
					last.Error = clipPlain(closeErr.Error(), 240)
				} else {
					last.Error = "Grok Session Stopped"
				}
				r.lastErr = last.Error
			}
		}
	}
	title := grokFirstNonEmpty(r.manager.state.Title(sid), r.title, "Grok")
	errText := r.lastErr
	cwd := r.cwd
	r.mu.Unlock()

	r.manager.mu.Lock()
	if sid != "" && r.manager.runs[sid] == r {
		delete(r.manager.runs, sid)
	}
	if r.manager.shuttingDown {
		shuttingDown = true
	}
	watched := !shuttingDown && sid != "" && r.manager.hasWatchersLocked(sid)
	r.manager.mu.Unlock()

	shouldReconnect := watched && r.manager.canDialACP()
	if shouldReconnect {
		r.mu.Lock()
		r.connecting = true
		r.mu.Unlock()
	}

	r.closeOnce.Do(func() { close(r.done) })

	if !shuttingDown && sid != "" {
		go func() {
			r.publish(true)
			r.manager.requestRefresh()
			if wasRunning && !wasCancelled {
				r.manager.event(sid, "failed", title, grokFirstNonEmpty(errText, "Grok Session Stopped"), cwd)
			}
			if shouldReconnect {
				r.manager.startReconnect(sid)
			}
		}()
	}
}

// detachAndCloseSession releases only this session via session/close without
// terminating the shared ACP daemon process.
func (r *grokRun) detachAndCloseSession(closeSessionTimeout time.Duration) {
	r.mu.Lock()
	r.alive = false
	r.running = false
	r.externalRunning = false
	if r.flushTimer != nil {
		r.flushTimer.Stop()
		r.flushTimer = nil
		r.flushScheduled = false
	}
	daemon := r.daemon
	client := r.client
	sid := r.id
	r.mu.Unlock()

	if daemon != nil && sid != "" {
		daemon.detachRun(sid, r)
	}
	if client != nil && !client.Closed() {
		if sid != "" && closeSessionTimeout > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), closeSessionTimeout)
			_ = client.CloseSession(ctx, sid)
			cancel()
		}
		if daemon == nil {
			_ = client.Close()
		}
	}
	r.closeOnce.Do(func() { close(r.done) })
}

func (r *grokRun) startPrompt(text string, images []string, blocks []acpContentBlock) (bool, error) {
	now := r.manager.nowTime()
	nowMs := now.UnixMilli()

	r.mu.Lock()
	if !r.alive || r.client == nil {
		r.mu.Unlock()
		return false, errors.New("Grok Session Has Stopped")
	}
	steering := r.running
	r.running = true
	r.externalRunning = false
	r.activePrompts++
	r.cancelled = false
	r.lastActivity = now
	r.recency = nowMs
	r.lastErr = ""

	if r.transcript == nil {
		r.transcript = newGrokTranscript()
	}
	var turn *Turn
	if steering && len(r.transcript.Turns) > 0 && r.activeTurnKey != "" {
		turn = &r.transcript.Turns[len(r.transcript.Turns)-1]
		r.transcript.cur = len(r.transcript.Turns) - 1
	} else {
		r.activeTurnKey = ""
		turn = r.ensureLiveTurnLocked(nowMs)
	}
	turn.Status = "inProgress"
	turn.Error = ""
	r.transcript.streamKind = ""
	r.transcript.streamIdx = -1
	userItem := Item{
		ID:     fmt.Sprintf("%s:user:%d", turn.Key, len(turn.Items)),
		K:      "user",
		Text:   text,
		Images: append([]string(nil), images...),
	}
	turn.Items = append(turn.Items, userItem)
	if text != "" {
		r.optimisticUsers = append(r.optimisticUsers, text)
	}
	r.optimisticImages += len(images)
	if r.title == "" && text != "" {
		r.title = clipPlain(text, 120)
	}
	title := grokFirstNonEmpty(r.manager.state.Title(r.id), r.title, clipPlain(text, 120))
	cwd := r.cwd
	daemon := r.daemon
	client := r.client
	sid := r.id
	turnKey := turn.Key
	startedAt := turn.StartedAt
	r.mu.Unlock()

	daemon.touchActivity(now)
	r.publish(true)
	r.manager.requestRefresh()
	if !steering {
		r.manager.event(sid, "started", title, "", cwd)
	}

	go func() {
		res, err := client.Prompt(context.Background(), sid, blocks)
		daemon.touchActivity(r.manager.nowTime())
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
		r.drainDispatch(drainCtx, client)
		drainCancel()
		r.finishPrompt(turnKey, startedAt, res, err)
	}()
	return false, nil
}

func (r *grokRun) finishPrompt(turnKey string, startedAt int64, res *acpPromptResult, promptErr error) {
	now := r.manager.nowTime()
	nowMs := now.UnixMilli()

	var eventKind, eventTitle, eventText, cwd string

	r.mu.Lock()
	r.lastActivity = now
	r.recency = nowMs
	wasRunning := r.running
	if r.activePrompts > 0 {
		r.activePrompts--
	}
	if res != nil && res.Usage != nil {
		if res.Usage.Used > 0 {
			r.ctxUsed = res.Usage.Used
			if r.transcript != nil {
				r.transcript.Meta.CtxUsed = res.Usage.Used
			}
		}
		if res.Usage.Size > 0 {
			r.ctxMax = res.Usage.Size
			if r.transcript != nil {
				r.transcript.Meta.CtxMax = res.Usage.Size
			}
		}
	}

	stillRunning := r.activePrompts > 0
	if wasRunning && !stillRunning {
		r.running = false
		var target *Turn
		if r.transcript != nil {
			for i := len(r.transcript.Turns) - 1; i >= 0; i-- {
				if r.transcript.Turns[i].Key == turnKey {
					target = &r.transcript.Turns[i]
					break
				}
			}
			if target == nil && len(r.transcript.Turns) > 0 {
				target = &r.transcript.Turns[len(r.transcript.Turns)-1]
			}
		}
		if target != nil {
			if target.DurationMs == 0 && startedAt > 0 && nowMs >= startedAt {
				target.DurationMs = nowMs - startedAt
			}
			stopReason := ""
			if res != nil {
				stopReason = strings.ToLower(strings.TrimSpace(res.StopReason))
			}
			switch {
			case r.cancelled || stopReason == "cancelled" || stopReason == "canceled":
				target.Status = "interrupted"
				target.Error = ""
				eventKind = ""
			case promptErr != nil:
				target.Status = "failed"
				target.Error = clipPlain(promptErr.Error(), 240)
				r.lastErr = target.Error
				eventKind = "failed"
				eventText = target.Error
			default:
				target.Status = "completed"
				eventKind = "completed"
				eventText = r.lastMsg
			}
		}
		r.activeTurnKey = ""
		if r.transcript != nil {
			r.transcript.streamKind = ""
			r.transcript.streamIdx = -1
		}
		r.optimisticUsers = nil
		r.optimisticImages = 0
		eventTitle = grokFirstNonEmpty(r.manager.state.Title(r.id), r.title, "Grok")
		cwd = r.cwd
	}
	r.mu.Unlock()

	r.publish(true)
	r.manager.requestRefresh()
	if eventKind != "" {
		r.manager.event(r.id, eventKind, eventTitle, eventText, cwd)
	}
}

func (r *grokRun) cancelTurn(ctx context.Context) error {
	r.mu.Lock()
	if !r.alive || r.client == nil {
		r.mu.Unlock()
		return errors.New("Grok Is Not Running")
	}
	r.cancelled = true
	r.permissions = map[string]grokPendingPerm{}
	r.askQuestions = map[string]grokPendingAsk{}
	r.exitPlans = map[string]grokPendingExitPlan{}
	r.elicitations = map[string]grokPendingElicit{}
	r.reqOrder = nil
	r.lastActivity = r.manager.nowTime()
	client := r.client
	daemon := r.daemon
	sid := r.id
	r.mu.Unlock()

	err := client.Cancel(ctx, sid)
	daemon.touchActivity(r.manager.nowTime())
	r.publish(true)
	r.manager.requestRefresh()
	return err
}

func (r *grokRun) removeRequestLocked(reqID string) {
	delete(r.permissions, reqID)
	delete(r.askQuestions, reqID)
	delete(r.exitPlans, reqID)
	delete(r.elicitations, reqID)
	filtered := r.reqOrder[:0]
	for _, id := range r.reqOrder {
		if id != reqID {
			filtered = append(filtered, id)
		}
	}
	r.reqOrder = filtered
}

func resolveGrokExitPlanOutcome(optionID, decision string) (string, error) {
	optionID = strings.ToLower(strings.TrimSpace(optionID))
	decision = strings.ToLower(strings.TrimSpace(decision))

	if optionID != "" {
		switch optionID {
		case "approve", "approved", "accept", "allow_once", "allow_always":
			return "approved", nil
		case "revise", "feedback", "edit":
			return "revise", nil
		case "decline", "reject", "reject_once", "reject_always", "abandoned", "cancel", "cancelled", "canceled":
			return "abandoned", nil
		default:
			return "", errors.New("Unknown Approval Option")
		}
	}

	switch decision {
	case "accept", "acceptforsession", "approve", "approved":
		return "approved", nil
	case "revise":
		return "revise", nil
	case "decline", "cancel", "abandoned", "reject":
		return "abandoned", nil
	default:
		return "", errors.New("Unknown Approval Decision")
	}
}

func (r *grokRun) approveRequest(reqID, optionID, decision, feedback string) error {
	reqID = strings.TrimSpace(reqID)
	r.mu.Lock()
	perm, isPerm := r.permissions[reqID]
	_, isExitPlan := r.exitPlans[reqID]
	daemon := r.daemon
	client := r.client
	r.mu.Unlock()

	if !isPerm && !isExitPlan {
		return errors.New("Request Is No Longer Pending")
	}
	if client == nil {
		return errors.New("Grok Session Is Not Active")
	}

	if isExitPlan {
		outcome, err := resolveGrokExitPlanOutcome(optionID, decision)
		if err != nil {
			return err
		}
		if err := client.RespondExitPlanMode(reqID, outcome, feedback); err != nil {
			return err
		}
		now := r.manager.nowTime()
		r.mu.Lock()
		r.removeRequestLocked(reqID)
		if outcome == "approved" && r.permissionMode == "plan" {
			r.permissionMode = "default"
			if r.transcript != nil {
				r.transcript.Meta.PermissionMode = "default"
			}
		}
		r.lastActivity = now
		r.mu.Unlock()
		daemon.touchActivity(now)
		r.publish(true)
		r.manager.requestRefresh()
		return nil
	}

	chosen, cancelled, err := resolvePermissionOption(perm.acpReq.Options, optionID, decision)
	if err != nil {
		return err
	}
	if err := client.RespondPermission(reqID, chosen, cancelled); err != nil {
		return err
	}
	now := r.manager.nowTime()
	r.mu.Lock()
	r.removeRequestLocked(reqID)
	r.lastActivity = now
	r.mu.Unlock()
	daemon.touchActivity(now)
	r.publish(true)
	r.manager.requestRefresh()
	return nil
}

func buildGrokAnsweredQuestions(
	req grokAskUserQuestionRequest,
	answers map[string][]string,
) ([]grokAnsweredQuestion, map[string]grokQuestionAnnotation, int) {
	answered := make([]grokAnsweredQuestion, 0, len(req.Questions))
	annotations := map[string]grokQuestionAnnotation{}
	totalAnswered := 0

	for i, q := range req.Questions {
		qID := grokFirstNonEmpty(strings.TrimSpace(q.ID), fmt.Sprintf("q%d", i+1))
		rawVals := answers[qID]
		if len(rawVals) == 0 && q.Question != "" {
			rawVals = answers[q.Question]
		}
		if len(rawVals) == 0 && len(req.Questions) == 1 && len(answers["response"]) > 0 {
			rawVals = answers["response"]
		}

		optLabels := make([]string, 0, len(q.Options))
		optDescs := make([]string, 0, len(q.Options))
		byLabel := map[string]grokQuestionOption{}
		byID := map[string]grokQuestionOption{}
		for _, opt := range q.Options {
			label := grokFirstNonEmpty(strings.TrimSpace(opt.Label), strings.TrimSpace(opt.ID))
			if label == "" {
				continue
			}
			optLabels = append(optLabels, label)
			optDescs = append(optDescs, strings.TrimSpace(opt.Description))
			byLabel[label] = opt
			if id := strings.TrimSpace(opt.ID); id != "" {
				byID[id] = opt
			}
		}

		picked := []string{}
		pickedSeen := map[string]bool{}
		var typedParts []string
		for _, rawVal := range rawVals {
			val := strings.TrimSpace(rawVal)
			if val == "" {
				continue
			}
			if opt, ok := byLabel[val]; ok {
				label := grokFirstNonEmpty(strings.TrimSpace(opt.Label), strings.TrimSpace(opt.ID))
				if !pickedSeen[label] {
					pickedSeen[label] = true
					picked = append(picked, label)
					if opt.Preview != "" {
						annotations[qID] = grokQuestionAnnotation{Preview: opt.Preview}
					}
				}
			} else if opt, ok := byID[val]; ok {
				label := grokFirstNonEmpty(strings.TrimSpace(opt.Label), strings.TrimSpace(opt.ID))
				if !pickedSeen[label] {
					pickedSeen[label] = true
					picked = append(picked, label)
					if opt.Preview != "" {
						annotations[qID] = grokQuestionAnnotation{Preview: opt.Preview}
					}
				}
			} else {
				typedParts = append(typedParts, val)
			}
		}

		typed := strings.Join(typedParts, ", ")
		if len(picked) > 0 || typed != "" {
			totalAnswered++
		}
		answered = append(answered, grokAnsweredQuestion{
			Question:     q.Question,
			Options:      optLabels,
			Descriptions: optDescs,
			MultiSelect:  q.MultiSelect,
			Picked:       picked,
			Typed:        typed,
		})
	}

	if len(req.Questions) == 0 && len(answers) > 0 {
		keys := make([]string, 0, len(answers))
		for k := range answers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			var parts []string
			for _, v := range answers[k] {
				if s := strings.TrimSpace(v); s != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				totalAnswered++
				answered = append(answered, grokAnsweredQuestion{
					Question:     k,
					Options:      []string{},
					Descriptions: []string{},
					Picked:       []string{},
					Typed:        strings.Join(parts, ", "),
				})
			}
		}
	}

	return answered, annotations, totalAnswered
}

func (r *grokRun) answerRequest(reqID, action string, answers map[string][]string) error {
	reqID = strings.TrimSpace(reqID)
	action = strings.ToLower(strings.TrimSpace(action))

	r.mu.Lock()
	ask, isAsk := r.askQuestions[reqID]
	elicit, isElicit := r.elicitations[reqID]
	daemon := r.daemon
	client := r.client
	r.mu.Unlock()

	if !isAsk && !isElicit {
		return errors.New("Request Is No Longer Pending")
	}
	if client == nil {
		return errors.New("Grok Session Is Not Active")
	}

	if isAsk {
		var outcome string
		switch action {
		case "", "accept", "accepted":
			outcome = "accepted"
		case "cancel", "cancelled", "canceled", "decline":
			outcome = "cancelled"
		case "chat", "chat_about_this":
			outcome = "chat_about_this"
		case "skip", "skip_interview":
			outcome = "skip_interview"
		default:
			return errors.New("Unknown Question Action")
		}

		if outcome == "cancelled" || outcome == "skip_interview" {
			if err := client.RespondAskUserQuestion(reqID, outcome, nil, nil); err != nil {
				return err
			}
		} else {
			answered, annotations, totalAnswered := buildGrokAnsweredQuestions(ask.acpReq, answers)
			if outcome == "accepted" && len(ask.acpReq.Questions) > 0 && totalAnswered == 0 {
				return errors.New("Answer Every Question")
			}
			if err := client.RespondAskUserQuestion(reqID, outcome, answered, annotations); err != nil {
				return err
			}
		}

		now := r.manager.nowTime()
		r.mu.Lock()
		r.removeRequestLocked(reqID)
		r.lastActivity = now
		r.mu.Unlock()
		daemon.touchActivity(now)
		r.publish(true)
		r.manager.requestRefresh()
		return nil
	}

	// _x.ai/mcp/elicit or elicitation/create
	if action == "cancel" || action == "decline" {
		if err := client.RespondElicitation(reqID, action, nil); err != nil {
			return err
		}
		now := r.manager.nowTime()
		r.mu.Lock()
		r.removeRequestLocked(reqID)
		r.lastActivity = now
		r.mu.Unlock()
		daemon.touchActivity(now)
		r.publish(true)
		r.manager.requestRefresh()
		return nil
	}
	if action != "" && action != "accept" {
		return errors.New("Unknown Elicitation Action")
	}

	content := map[string]any{}
	if len(elicit.acpReq.Fields) > 0 {
		for _, f := range elicit.acpReq.Fields {
			rawVals := answers[f.Name]
			cleaned := make([]string, 0, len(rawVals))
			for _, item := range rawVals {
				if strings.TrimSpace(item) != "" {
					cleaned = append(cleaned, item)
				}
			}
			if f.Required && len(cleaned) == 0 {
				return errors.New("Answer Every Required Question")
			}
			if len(cleaned) == 0 {
				continue
			}
			if f.MultiSelect || strings.EqualFold(f.Type, "array") {
				mapped := make([]string, 0, len(cleaned))
				for _, item := range cleaned {
					mapped = append(mapped, resolveElicitationOptionValue(f.Options, item))
				}
				content[f.Name] = mapped
			} else {
				val := resolveElicitationOptionValue(f.Options, cleaned[0])
				content[f.Name] = coerceElicitationScalar(f.Type, val)
			}
		}
	} else {
		for k, vals := range answers {
			if len(vals) == 1 {
				content[k] = vals[0]
			} else if len(vals) > 1 {
				content[k] = append([]string(nil), vals...)
			}
		}
	}

	if err := client.RespondElicitation(reqID, "accept", content); err != nil {
		return err
	}
	now := r.manager.nowTime()
	r.mu.Lock()
	r.removeRequestLocked(reqID)
	r.lastActivity = now
	r.mu.Unlock()
	daemon.touchActivity(now)
	r.publish(true)
	r.manager.requestRefresh()
	return nil
}

func (r *grokRun) diffByKey(key string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.transcript == nil {
		return "", false
	}
	d := r.transcript.Diff(key)
	return d, d != ""
}
