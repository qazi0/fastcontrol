package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	windowTurns  = 15
	flushEvery   = 60 * time.Millisecond
	pollEvery    = 300 * time.Millisecond
	followRetry  = 5 * time.Second
	idleShutdown = 60 * time.Second
	ipcTimeout   = 15 * time.Second
)

// ---- clients ----

type Client struct {
	hub    *Hub
	out    chan []byte
	subs   map[string]bool // guarded by hub.mu
	events bool            // wants {t:"event"} pushes; guarded by hub.mu
	// eventsOnly clients (the Android watch service) get events and nothing
	// else: no thread lists, which are large and change often.
	eventsOnly bool
	done   chan struct{}
}

func (c *Client) push(b []byte) {
	select {
	case c.out <- b:
	default:
		// Slow consumer; the reader side will notice the closed channel.
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ---- hub ----

type Hub struct {
	ipc *IPCClient
	db  string

	listMu sync.Mutex // serializes refreshList
	addrs  []string   // http://ip:port for every non-loopback listener, sent in hello

	mu       sync.Mutex
	clients  map[*Client]bool
	sessions map[string]*Session
	execs    map[string]*execRun
	threads  []ThreadInfo
	listJSON []byte
	prev     map[string]threadState // last state per thread, for events
}

func NewHub(ipc *IPCClient, db string) *Hub {
	h := &Hub{ipc: ipc, db: db, clients: map[*Client]bool{}, sessions: map[string]*Session{}, execs: map[string]*execRun{}}
	ipc.OnBroadcast = h.onBroadcast
	ipc.OnReset = h.onReset
	return h
}

func (h *Hub) onBroadcast(f *ipcFrame) {
	switch f.Method {
	case "thread-stream-state-changed", "thread-stream-following-changed":
		var p struct {
			ConversationID string `json:"conversationId"`
		}
		if json.Unmarshal(f.Params, &p) != nil {
			return
		}
		h.mu.Lock()
		s := h.sessions[p.ConversationID]
		h.mu.Unlock()
		if s != nil {
			select {
			case s.inbox <- f:
			default:
				log.Printf("session %s: inbox full, dropping %s", p.ConversationID, f.Method)
				s.requestResync()
			}
		}
	case "client-status-changed":
		var p struct {
			ClientID string `json:"clientId"`
			Status   string `json:"status"`
		}
		if json.Unmarshal(f.Params, &p) != nil || p.Status != "disconnected" {
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, s := range h.sessions {
			select {
			case s.ownerGone <- p.ClientID:
			default:
			}
		}
	}
}

func (h *Hub) onReset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		select {
		case s.ownerGone <- "*":
		default:
		}
	}
}

func (h *Hub) addClient(c *Client) {
	h.mu.Lock()
	h.clients[c] = true
	lj := h.listJSON
	h.mu.Unlock()
	if lj != nil && !c.eventsOnly {
		c.push(lj)
	}
}

func (h *Hub) removeClient(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	var subs []*Session
	for id := range c.subs {
		if s := h.sessions[id]; s != nil {
			subs = append(subs, s)
		}
	}
	h.mu.Unlock()
	for _, s := range subs {
		s.unsubscribe(c)
	}
}

// RunList refreshes the thread list and pushes it to clients when it changes.
func (h *Hub) RunList(ctx context.Context) {
	var lastMod time.Time
	var lastFull time.Time
	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()
	for {
		mod := time.Time{}
		for _, f := range []string{h.db, h.db + "-wal"} {
			if st, err := os.Stat(f); err == nil && st.ModTime().After(mod) {
				mod = st.ModTime()
			}
		}
		if mod != lastMod || time.Since(lastFull) > 5*time.Second || h.runningRolloutsChanged() {
			lastMod = mod
			lastFull = time.Now()
			h.refreshList()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *Hub) refreshList() {
	h.listMu.Lock()
	defer h.listMu.Unlock()
	ts, err := listThreads(h.db)
	if err != nil {
		log.Printf("thread list: %v", err)
		return
	}
	h.mu.Lock()
	for i := range ts {
		t := &ts[i]
		if s := h.sessions[t.ID]; s != nil {
			st := s.listState()
			t.Live, t.Running, t.Attention = st.live, st.running, st.attention
			if st.hasTurn {
				t.TurnAt = st.turnAt
			}
		}
		if x := h.execs[t.ID]; x != nil && x.running() {
			t.Exec = true
			// codex exec lingers for seconds after the turn ends; trust the
			// rollout once it records the end of the turn.
			if !t.Active || t.Running || t.TurnAt == 0 {
				t.Running = true
			}
		}
	}
	b := mustJSON(map[string]any{"t": "threads", "threads": ts})
	changed := !bytes.Equal(b, h.listJSON)
	h.threads = ts
	h.listJSON = b
	var cs []*Client
	if changed {
		for c := range h.clients {
			if !c.eventsOnly {
				cs = append(cs, c)
			}
		}
	}
	events, watch := h.diffStates(ts)
	h.mu.Unlock()
	for _, c := range cs {
		c.push(b)
	}
	h.emit(events)
	h.updateWatch(watch)
}

func (h *Hub) threadInfo(id string) (ThreadInfo, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range h.threads {
		if t.ID == id {
			return t, true
		}
	}
	return ThreadInfo{}, false
}

func (h *Hub) session(id string, create bool) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.sessions[id]; s != nil || !create {
		return s
	}
	info, ok := func() (ThreadInfo, bool) {
		for _, t := range h.threads {
			if t.ID == id {
				return t, true
			}
		}
		return ThreadInfo{}, false
	}()
	if !ok {
		// A codex exec run writes its rollout before the database lists it.
		r := findRollout(id)
		if r == "" {
			return nil
		}
		info = ThreadInfo{ID: id, Title: "New Thread", Rollout: r}
		if x := h.execs[id]; x != nil {
			info.Title = clipPlain(x.prompt, 80)
		}
	}
	s := newSession(h, info)
	h.sessions[id] = s
	go s.run()
	return s
}

func (h *Hub) dropSession(s *Session) {
	h.mu.Lock()
	if h.sessions[s.id] == s {
		delete(h.sessions, s.id)
	}
	h.mu.Unlock()
}

// handle processes one message from a phone.
func (h *Hub) handle(c *Client, raw []byte) {
	var m struct {
		Op             string              `json:"op"`
		ID             string              `json:"id"`
		Text           string              `json:"text"`
		ReqID          string              `json:"reqId"`
		Method         string              `json:"method"`
		Decision       string              `json:"decision"`
		Nonce          string              `json:"nonce"`
		Key            string              `json:"key"`
		Cwd            string              `json:"cwd"`
		Model          *string             `json:"model"`
		Effort         *string             `json:"effort"`
		ApprovalPolicy *string             `json:"approvalPolicy"`
		Sandbox        *string             `json:"sandbox"`
		Reviewer       *string             `json:"reviewer"`
		Answers        map[string][]string `json:"answers"`
		Images         []string            `json:"images"`
		On             bool                `json:"on"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	ack := func(err error, extra map[string]any) {
		r := map[string]any{"t": "ack", "op": m.Op, "id": m.ID, "nonce": m.Nonce, "ok": err == nil}
		if err != nil {
			r["msg"] = err.Error()
		}
		for k, v := range extra {
			r[k] = v
		}
		c.push(mustJSON(r))
	}
	sess := func() *Session {
		if _, ok := h.threadInfo(m.ID); !ok {
			h.refreshList()
		}
		s := h.session(m.ID, true)
		if s == nil {
			ack(errors.New("thread not found"), nil)
		}
		return s
	}
	switch m.Op {
	case "threads":
		h.refreshList()
	case "open":
		s := sess()
		if s == nil {
			return
		}
		h.mu.Lock()
		if c.subs == nil {
			c.subs = map[string]bool{}
		}
		c.subs[m.ID] = true
		h.mu.Unlock()
		s.subscribe(c)
	case "close":
		h.mu.Lock()
		delete(c.subs, m.ID)
		s := h.sessions[m.ID]
		h.mu.Unlock()
		if s != nil {
			s.unsubscribe(c)
		}
	case "events":
		h.mu.Lock()
		c.events = m.On
		h.mu.Unlock()
		ack(nil, nil)
		go h.refreshList() // starts or stops auto-following running threads
	case "models":
		def := defaultModel()
		c.push(mustJSON(map[string]any{"t": "models", "models": listModels(), "default": def}))
	case "projects":
		go func() {
			ps, err := listProjects(h.db)
			if err != nil {
				ack(err, nil)
				return
			}
			home, _ := os.UserHomeDir()
			c.push(mustJSON(map[string]any{"t": "projects", "projects": ps, "home": home}))
		}()
	case "project":
		go func() {
			ts, err := listProjectThreads(h.db, m.Cwd)
			if err != nil {
				ack(err, nil)
				return
			}
			// Overlay live state the database does not know about.
			h.mu.Lock()
			for i := range ts {
				for _, t := range h.threads {
					if t.ID == ts[i].ID {
						ts[i] = t
						break
					}
				}
			}
			h.mu.Unlock()
			c.push(mustJSON(map[string]any{"t": "project", "cwd": m.Cwd, "threads": ts}))
		}()
	case "archive", "unarchive":
		go func() {
			err := runCodex(m.Op, m.ID)
			if err == nil {
				h.refreshList()
			}
			ack(err, nil)
		}()
	case "new":
		go func() {
			id, err := h.newThread(m.Cwd, m.Text, deref(m.Model), deref(m.Effort), m.Images)
			ack(err, map[string]any{"thread": id})
		}()
	case "send":
		if x := h.execFor(m.ID); x != nil && x.queue(m.Text, m.Images) {
			ack(nil, map[string]any{"queued": true})
			return
		}
		s := sess()
		if s == nil {
			return
		}
		go func() { ack(s.sendMessage(m.Text, m.Images), nil) }()
	case "stop":
		if x := h.execFor(m.ID); x != nil && x.running() {
			x.stop()
			ack(nil, nil)
			return
		}
		s := sess()
		if s == nil {
			return
		}
		go func() { ack(s.interrupt(), nil) }()
	case "more":
		if s := sess(); s != nil {
			s.more()
		}
	case "diff":
		if s := sess(); s != nil {
			s.diff(c, m.Key)
		}
	case "approve", "wake", "compact", "answer", "settings":
		s := sess()
		if s == nil {
			return
		}
		go func() {
			var err error
			switch m.Op {
			case "approve":
				err = s.approve(m.ReqID, m.Method, m.Decision)
			case "wake":
				err = s.wake()
			case "compact":
				err = s.compact()
			case "answer":
				err = s.answer(m.ReqID, m.Answers)
			case "settings":
				err = s.updateSettings(m.Model, m.Effort, m.ApprovalPolicy, m.Sandbox, m.Reviewer)
			}
			ack(err, nil)
		}()
	default:
		ack(errors.New("unknown op "+m.Op), nil)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- sessions ----

type Session struct {
	hub     *Hub
	id      string
	title   string
	rollout string
	info    ThreadInfo

	inbox     chan *ipcFrame
	ownerGone chan string
	cmds      chan func()
	resync    chan struct{}

	mu      sync.Mutex // guards subs, the list-state fields and watch, which other goroutines read
	subs    map[*Client]bool
	live    bool
	run_    bool
	attn    bool   // waiting on an approval or a question
	attnMsg string // what it is waiting on, for notifications
	turnAt  int64
	watch   bool // kept following for notifications even with no viewer

	// Owned by the run goroutine.
	owner     string
	revision  int64
	state     map[string]any
	sentKeys  []string
	sentSet   map[string]bool
	dirtyT    map[string]bool
	dirtyI    map[string]map[int]bool
	dirtyH    map[string]bool
	dirtyMeta bool
	dirtyOrd  bool
	lastMeta  []byte
	lastOpen  []byte
	rv        *rolloutView
	following bool
	lastTry   time.Time
	waiters   []chan struct{} // notified when the session goes live
	window    int             // turns sent to clients
	moreAt    time.Time       // last Load Earlier request; full renders soon after keep the reader's place
}

func newSession(h *Hub, info ThreadInfo) *Session {
	return &Session{
		hub: h, id: info.ID, title: info.Title, rollout: info.Rollout, info: info, window: windowTurns,
		inbox: make(chan *ipcFrame, 4096), ownerGone: make(chan string, 8),
		cmds: make(chan func(), 64), resync: make(chan struct{}, 1),
		subs: map[*Client]bool{},
	}
}

type sessionState struct {
	live, running, attention, hasTurn bool
	turnAt                            int64
	attnMsg                           string
}

func (s *Session) listState() sessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sessionState{live: s.live, running: s.run_, attention: s.attn, hasTurn: s.turnAt != 0, turnAt: s.turnAt, attnMsg: s.attnMsg}
}

// publishMeta records the list-visible parts of meta and refreshes the list
// when they change. Runs on the session goroutine.
func (s *Session) publishMeta(m Meta) {
	attn := len(m.Requests) > 0
	msg := ""
	if attn {
		r := m.Requests[0]
		msg = r.Title
		if r.Body != "" {
			msg += " " + clipPlain(strings.TrimSpace(r.Body), 200)
		} else if len(r.Questions) > 0 {
			msg += " " + clipPlain(r.Questions[0].Question, 200)
		}
	}
	s.mu.Lock()
	changed := s.run_ != m.Running || s.attn != attn || (m.TurnAt != 0 && s.turnAt != m.TurnAt)
	s.run_, s.attn, s.attnMsg = m.Running, attn, msg
	if m.TurnAt != 0 {
		s.turnAt = m.TurnAt
	}
	s.mu.Unlock()
	if changed {
		go s.hub.refreshList()
	}
}

func (s *Session) setWatch(on bool) {
	s.mu.Lock()
	s.watch = on
	s.mu.Unlock()
}

func (s *Session) requestResync() {
	select {
	case s.resync <- struct{}{}:
	default:
	}
}

func (s *Session) subscribe(c *Client) {
	s.cmds <- func() {
		s.mu.Lock()
		s.subs[c] = true
		s.mu.Unlock()
		c.push(s.openMessage())
	}
}

func (s *Session) unsubscribe(c *Client) {
	select {
	case s.cmds <- func() {
		s.mu.Lock()
		delete(s.subs, c)
		s.mu.Unlock()
	}:
	default:
	}
}

func (s *Session) broadcast(b []byte) {
	s.mu.Lock()
	cs := make([]*Client, 0, len(s.subs))
	for c := range s.subs {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	for _, c := range cs {
		c.push(b)
	}
}

func (s *Session) setLive(live bool) {
	s.mu.Lock()
	changed := s.live != live
	s.live = live
	s.mu.Unlock()
	if live {
		for _, w := range s.waiters {
			close(w)
		}
		s.waiters = nil
	}
	if changed {
		go s.hub.refreshList()
	}
}

func (s *Session) meta() Meta {
	var m Meta
	if s.state != nil {
		m = desktopMeta(s.state, s.title)
		if m.Cwd == "" {
			m.Cwd = s.info.Cwd
		}
		if m.Branch == "" {
			m.Branch = s.info.Branch
		}
		if len(desktopTurnOrder(s.state)) > s.window {
			m.HasOlder = true
		}
	} else {
		i := s.info
		m = Meta{Mode: "tail", Title: s.title, Cwd: i.Cwd, Branch: i.Branch, Model: i.Model, Effort: i.Effort, Approval: i.Approval, Sandbox: i.Sandbox}
		if s.rv != nil {
			m.Running = s.rv.running()
			m.CtxUsed, m.CtxMax = s.rv.ctxUsed, s.rv.ctxMax
			m.HasOlder = s.rv.hasOlder(s.window)
			if t := s.rv.lastTurn(); t != nil {
				m.TurnAt = t.StartedAt
			}
		}
	}
	if x := s.hub.execFor(s.id); x != nil && x.running() {
		m.Exec = true
		if s.rv == nil || s.rv.lastTurn() == nil || s.rv.running() {
			m.Running = true
		}
	}
	return m
}

// openMessage renders the full window for a (re)subscribing client.
func (s *Session) openMessage() []byte {
	meta := s.meta()
	var turns []Turn
	if s.state != nil {
		keys := desktopTurnOrder(s.state)
		if len(keys) > s.window {
			keys = keys[len(keys)-s.window:]
		}
		ents := desktopEntities(s.state)
		for _, k := range keys {
			turns = append(turns, desktopTurn(k, ents[k]))
		}
		s.sentKeys = keys
		s.sentSet = map[string]bool{}
		for _, k := range keys {
			s.sentSet[k] = true
		}
	} else if s.rv != nil {
		turns = s.rv.snapshot(s.window)
	}
	if turns == nil {
		turns = []Turn{}
	}
	s.lastMeta = mustJSON(meta)
	s.publishMeta(meta)
	msg := map[string]any{"t": "open", "id": s.id, "meta": meta, "turns": turns}
	if time.Since(s.moreAt) < 8*time.Second {
		msg["more"] = true
	}
	return mustJSON(msg)
}

func (s *Session) run() {
	defer s.hub.dropSession(s)
	s.rv = newRolloutView(s.rollout)
	if err := s.rv.load(); err != nil {
		log.Printf("session %s: rollout: %v", s.id, err)
	}
	go s.tryFollow()

	flush := time.NewTicker(flushEvery)
	poll := time.NewTicker(pollEvery)
	defer flush.Stop()
	defer poll.Stop()
	idleSince := time.Time{}

	for {
		select {
		case f := <-s.inbox:
			s.onFrame(f)
		case who := <-s.ownerGone:
			if who == "*" || who == s.owner {
				s.dropLive()
			}
		case fn := <-s.cmds:
			fn()
		case <-s.resync:
			if s.owner != "" {
				s.follow(s.owner, true)
			}
		case <-flush.C:
			s.flush()
		case <-poll.C:
			s.mu.Lock()
			n := len(s.subs)
			if s.watch {
				n++
			}
			s.mu.Unlock()
			if n == 0 && len(s.waiters) == 0 {
				if idleSince.IsZero() {
					idleSince = time.Now()
				} else if time.Since(idleSince) > idleShutdown {
					s.unfollow()
					return
				}
				continue
			}
			idleSince = time.Time{}
			if s.state == nil {
				s.pollRollout()
				if time.Since(s.lastTry) > followRetry {
					go s.tryFollow()
				}
			}
		}
	}
}

func (s *Session) dropLive() {
	if s.state == nil && s.owner == "" {
		return
	}
	log.Printf("session %s: desktop owner went away, falling back to transcript tail", s.id)
	s.owner = ""
	s.state = nil
	s.lastOpen = nil
	s.following = false
	s.setLive(false)
	_ = s.rv.load()
	s.broadcast(s.openMessage())
}

// tryFollow asks the IPC router which desktop window owns the thread and, if
// one does, subscribes as a follower. Runs off the session goroutine.
func (s *Session) tryFollow() {
	s.cmds <- func() { s.lastTry = time.Now() }
	r, err := s.hub.ipc.Request("thread-owner-discovery", map[string]string{"hostId": "local", "conversationId": s.id}, "", 1500*time.Millisecond, nil)
	if err != nil || r.HandledBy == "" {
		return
	}
	owner := r.HandledBy
	s.cmds <- func() {
		if s.owner == owner && s.following {
			return
		}
		s.follow(owner, false)
	}
}

func (s *Session) follow(owner string, resync bool) {
	s.owner = owner
	s.following = true
	if resync {
		s.revision = -1
	}
	err := s.hub.ipc.Broadcast("thread-stream-following-changed", map[string]any{"conversationId": s.id, "hostId": "local", "following": true}, []string{owner})
	if err != nil {
		log.Printf("session %s: follow: %v", s.id, err)
	}
}

func (s *Session) unfollow() {
	if s.owner != "" && s.following {
		_ = s.hub.ipc.Broadcast("thread-stream-following-changed", map[string]any{"conversationId": s.id, "hostId": "local", "following": false}, []string{s.owner})
	}
	s.following = false
}

type streamChange struct {
	ConversationID string `json:"conversationId"`
	Following      *bool  `json:"following"`
	Change         *struct {
		Type         string          `json:"type"`
		Revision     int64           `json:"revision"`
		BaseRevision int64           `json:"baseRevision"`
		State        json.RawMessage `json:"conversationState"`
		Patches      []patch         `json:"patches"`
	} `json:"change"`
}

func (s *Session) onFrame(f *ipcFrame) {
	if f.Method != "thread-stream-state-changed" {
		return
	}
	var p streamChange
	if err := json.Unmarshal(f.Params, &p); err != nil || p.Change == nil {
		return
	}
	ch := p.Change
	switch ch.Type {
	case "snapshot":
		var st map[string]any
		if err := json.Unmarshal(ch.State, &st); err != nil {
			log.Printf("session %s: bad snapshot: %v", s.id, err)
			return
		}
		s.state = st
		s.owner = f.SourceClientID
		s.revision = ch.Revision
		s.clearDirty()
		s.setLive(true)
		log.Printf("session %s: live (snapshot rev %d, %d bytes)", s.id, ch.Revision, len(ch.State))
		// The owner re-sends snapshots after history loads; skip identical ones.
		if b := s.openMessage(); !bytes.Equal(b, s.lastOpen) {
			s.lastOpen = b
			s.broadcast(b)
		}
	case "patches":
		if s.state == nil || f.SourceClientID != s.owner {
			return
		}
		if ch.BaseRevision != s.revision {
			log.Printf("session %s: revision gap (have %d, base %d), resyncing", s.id, s.revision, ch.BaseRevision)
			s.follow(s.owner, true)
			return
		}
		var root any = s.state
		for _, pt := range ch.Patches {
			nr, err := applyPatch(root, pt)
			if err != nil {
				log.Printf("session %s: patch failed (%v), resyncing", s.id, err)
				s.follow(s.owner, true)
				return
			}
			root = nr
			s.markDirty(pt)
		}
		if m, ok := root.(map[string]any); ok {
			s.state = m
		}
		s.revision = ch.Revision
	}
}

func (s *Session) clearDirty() {
	s.dirtyT = map[string]bool{}
	s.dirtyI = map[string]map[int]bool{}
	s.dirtyH = map[string]bool{}
	s.dirtyMeta = false
	s.dirtyOrd = false
}

func pathStrings(path []json.RawMessage) []string {
	out := make([]string, len(path))
	for i, p := range path {
		out[i], _, _ = pathKey(p)
	}
	return out
}

func (s *Session) markDirty(pt patch) {
	if s.dirtyT == nil {
		s.clearDirty()
	}
	p := pathStrings(pt.Path)
	if len(p) == 0 {
		s.dirtyOrd = true
		return
	}
	if p[0] != "turnHistory" {
		s.dirtyMeta = true
		return
	}
	if len(p) >= 4 && p[1] == "history" && p[2] == "entitiesByKey" {
		key := p[3]
		s.dirtyMeta = true // turn status drives the running flag
		switch {
		case len(p) == 4:
			s.dirtyT[key] = true
			s.dirtyOrd = true
		case p[4] != "items":
			s.dirtyH[key] = true
		case len(p) == 5:
			s.dirtyT[key] = true
		default:
			idx, err := strconv.Atoi(p[5])
			if err != nil || (len(p) == 6 && pt.Op == "remove") {
				s.dirtyT[key] = true
				return
			}
			if len(p) == 6 && pt.Op == "add" {
				// Appends are cheap item updates; inserts in the middle shift indexes.
				if items := s.turnItems(key); idx != len(items)-1 {
					s.dirtyT[key] = true
					return
				}
			}
			if s.dirtyI[key] == nil {
				s.dirtyI[key] = map[int]bool{}
			}
			s.dirtyI[key][idx] = true
		}
		return
	}
	s.dirtyOrd = true
}

func (s *Session) turnItems(key string) []any {
	t, _ := desktopEntities(s.state)[key].(map[string]any)
	items, _ := t["items"].([]any)
	return items
}

func equalPrefix(a, b []string) bool {
	if len(b) < len(a) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Session) flush() {
	if s.state == nil || s.dirtyT == nil {
		return
	}
	if s.dirtyOrd || s.dirtyMeta || len(s.dirtyT) > 0 || len(s.dirtyI) > 0 || len(s.dirtyH) > 0 {
		s.lastOpen = nil // the client view has moved past the last full render
	}
	if !s.dirtyOrd && !s.dirtyMeta && len(s.dirtyT) == 0 && len(s.dirtyI) == 0 && len(s.dirtyH) == 0 {
		return
	}
	defer s.clearDirty()
	if s.dirtyOrd {
		keys := desktopTurnOrder(s.state)
		// Keep the client's window and append new turns; anything else is a full resend.
		start := len(keys) - len(s.sentKeys)
		if start < 0 || !equalPrefix(s.sentKeys, keys[start:]) {
			s.lastOpen = s.openMessage()
			s.broadcast(s.lastOpen)
			return
		}
		for _, k := range keys[start+len(s.sentKeys):] {
			s.sentKeys = append(s.sentKeys, k)
			s.sentSet[k] = true
			s.dirtyT[k] = true
		}
	}
	ents := desktopEntities(s.state)
	for _, k := range s.sentKeys {
		if s.dirtyT[k] {
			s.broadcast(mustJSON(map[string]any{"t": "turn", "id": s.id, "turn": desktopTurn(k, ents[k])}))
			continue
		}
		if s.dirtyH[k] {
			t := desktopTurn(k, ents[k])
			s.broadcast(mustJSON(map[string]any{"t": "head", "id": s.id, "key": k, "status": t.Status, "durationMs": t.DurationMs, "error": t.Error}))
		}
		if idxs := s.dirtyI[k]; idxs != nil {
			items := s.turnItems(k)
			for idx := range idxs {
				if idx < len(items) {
					s.broadcast(mustJSON(map[string]any{"t": "item", "id": s.id, "key": k, "idx": idx, "item": desktopItem(items[idx])}))
				}
			}
		}
	}
	s.sendMeta()
}

// sendMeta pushes meta to viewers when it changed since the last push.
func (s *Session) sendMeta() {
	meta := s.meta()
	mb := mustJSON(meta)
	if !bytes.Equal(mb, s.lastMeta) {
		s.lastMeta = mb
		s.publishMeta(meta)
		s.broadcast(mustJSON(map[string]any{"t": "meta", "id": s.id, "meta": meta}))
	}
}

func (s *Session) pollRollout() {
	ch, err := s.rv.poll()
	if err != nil || ch == nil {
		return
	}
	newSet := map[*Turn]bool{}
	for _, t := range ch.newTurns {
		newSet[t] = true
		s.broadcast(mustJSON(map[string]any{"t": "turn", "id": s.id, "turn": t}))
	}
	for t := range ch.status {
		if !newSet[t] {
			s.broadcast(mustJSON(map[string]any{"t": "head", "id": s.id, "key": t.Key, "status": t.Status, "durationMs": t.DurationMs}))
		}
	}
	for t, idxs := range ch.items {
		if newSet[t] {
			continue
		}
		for _, i := range idxs {
			s.broadcast(mustJSON(map[string]any{"t": "item", "id": s.id, "key": t.Key, "idx": i, "item": t.Items[i]}))
		}
	}
	if len(ch.status) > 0 || ch.ctx {
		s.sendMeta()
	}
}

// ---- control ----

type liveInfo struct {
	owner   string
	running bool
	turnID  string
}

func (s *Session) snapshotLive() (liveInfo, bool) {
	res := make(chan liveInfo, 1)
	s.cmds <- func() {
		if s.state == nil || s.owner == "" {
			res <- liveInfo{}
			return
		}
		m := desktopMeta(s.state, s.title)
		res <- liveInfo{owner: s.owner, running: m.Running, turnID: lastTurnID(s.state)}
	}
	li := <-res
	return li, li.owner != ""
}

// ensureLive makes the desktop load the thread (via its codex:// deep link)
// when no window owns it yet, then waits for the follower snapshot.
func (s *Session) ensureLive(timeout time.Duration) (liveInfo, error) {
	if li, ok := s.snapshotLive(); ok {
		return li, nil
	}
	w := make(chan struct{})
	s.cmds <- func() { s.waiters = append(s.waiters, w) }
	if err := exec.Command("/usr/bin/open", "-g", "codex://threads/"+s.id).Run(); err != nil {
		return liveInfo{}, err
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(700 * time.Millisecond)
	defer tick.Stop()
	go s.tryFollow()
	for {
		select {
		case <-w:
			if li, ok := s.snapshotLive(); ok {
				return li, nil
			}
		case <-tick.C:
			go s.tryFollow()
		case <-deadline:
			return liveInfo{}, errors.New("the desktop app did not open this thread; is it running?")
		}
	}
}

func (s *Session) wake() error {
	_, err := s.ensureLive(12 * time.Second)
	return err
}

func textInput(text string, images []string) []map[string]any {
	var in []map[string]any
	if text != "" {
		in = append(in, map[string]any{"type": "text", "text": text, "text_elements": []any{}})
	}
	for _, p := range images {
		in = append(in, map[string]any{"type": "localImage", "path": p})
	}
	return in
}

func (s *Session) sendMessage(text string, images []string) error {
	if text == "" && len(images) == 0 {
		return errors.New("empty message")
	}
	if err := checkUploads(images); err != nil {
		return err
	}
	li, err := s.ensureLive(12 * time.Second)
	if err != nil {
		return err
	}
	msgID := uuid()
	if li.running {
		_, err = s.hub.ipc.Request("thread-follower-steer-turn", map[string]any{
			"conversationId":      s.id,
			"input":               textInput(text, images),
			"restoreMessage":      map[string]any{"id": msgID, "text": text, "context": map[string]any{}},
			"attachments":         []any{},
			"clientUserMessageId": msgID,
		}, li.owner, ipcTimeout, nil)
		return err
	}
	_, err = s.hub.ipc.Request("thread-follower-start-turn", map[string]any{
		"conversationId": s.id,
		"turnStart": map[string]any{
			"request": map[string]any{"threadId": s.id, "input": textInput(text, images), "clientUserMessageId": msgID},
			"context": map[string]any{},
		},
	}, li.owner, ipcTimeout, nil)
	return err
}

func (s *Session) interrupt() error {
	li, ok := s.snapshotLive()
	if !ok {
		return errors.New("thread is not open in the desktop app")
	}
	params := map[string]any{"conversationId": s.id, "mode": "user-stop"}
	var version *int
	if li.turnID != "" {
		params["expectedTurnId"] = li.turnID
	} else {
		v := 3
		version = &v
	}
	_, err := s.hub.ipc.Request("thread-follower-interrupt-turn", params, li.owner, ipcTimeout, version)
	return err
}

func (s *Session) approve(reqID, method, decision string) error {
	li, ok := s.snapshotLive()
	if !ok {
		return errors.New("thread is not open in the desktop app")
	}
	var id any = reqID
	if n, err := strconv.ParseInt(reqID, 10, 64); err == nil {
		id = n
	}
	var m string
	switch {
	case bytes.Contains([]byte(method), []byte("commandExecution")):
		m = "thread-follower-command-approval-decision"
	case bytes.Contains([]byte(method), []byte("fileChange")):
		m = "thread-follower-file-approval-decision"
	default:
		return errors.New("unsupported request type: " + method)
	}
	_, err := s.hub.ipc.Request(m, map[string]any{"conversationId": s.id, "requestId": id, "decision": decision}, li.owner, ipcTimeout, nil)
	return err
}
