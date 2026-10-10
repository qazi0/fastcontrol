package main

// Client for the Codex desktop app's local IPC router (~/.codex/ipc/ipc.sock).
//
// Wire format: each frame is a 4-byte little-endian length followed by a UTF-8
// JSON object. Message types are "request", "response", "broadcast",
// "client-discovery-request" and "client-discovery-response". The router
// assigns a clientId on "initialize" and forwards requests to whichever client
// answers canHandle:true to its discovery probe (for thread-follower-* methods
// that is the desktop window that owns the thread).
//
// This protocol is private to the desktop app and versioned per method; see
// ipcVersions. It was reverse engineered from ChatGPT.app 154.0.8037.98.

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Per-method protocol versions copied from the desktop bundle (src-*.js, `lt={...}`).
// For hostId "local" the request is sent without hostId and uses these values.
var ipcVersions = map[string]int{
	"thread-stream-state-changed":                            11,
	"thread-stream-following-changed":                        1,
	"thread-stream-following-status-requested":               1,
	"ipc-connection-reset":                                   1,
	"thread-read-state-changed":                              3,
	"thread-archived":                                        2,
	"thread-unarchived":                                      1,
	"thread-owner-discovery":                                 1,
	"thread-follower-start-turn":                             2,
	"thread-follower-load-complete-history":                  1,
	"thread-follower-compact-thread":                         1,
	"thread-follower-steer-turn":                             1,
	"thread-follower-interrupt-turn":                         4,
	"thread-follower-update-thread-settings":                 2,
	"thread-follower-update-daybreak":                        1,
	"thread-follower-edit-last-user-turn":                    2,
	"thread-follower-command-approval-decision":              1,
	"thread-follower-file-approval-decision":                 1,
	"thread-follower-permissions-request-approval-response":  1,
	"thread-follower-submit-user-input":                      1,
	"thread-follower-submit-mcp-server-elicitation-response": 1,
	"thread-follower-set-queued-follow-ups-state":            1,
	"thread-follower-remove-queued-message":                  1,
	"thread-follower-clear-queued-messages":                  1,
	"thread-queued-followups-changed":                        2,
}

type ipcFrame struct {
	Type            string          `json:"type"`
	RequestID       string          `json:"requestId,omitempty"`
	SourceClientID  string          `json:"sourceClientId,omitempty"`
	TargetClientID  string          `json:"targetClientId,omitempty"`
	TargetClientIDs []string        `json:"targetClientIds,omitempty"`
	Version         *int            `json:"version,omitempty"`
	Method          string          `json:"method,omitempty"`
	Params          json.RawMessage `json:"params,omitempty"`
	TimeoutMs       int             `json:"timeoutMs,omitempty"`
	ResultType      string          `json:"resultType,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           json.RawMessage `json:"error,omitempty"`
	HandledBy       string          `json:"handledByClientId,omitempty"`
	Request         *ipcFrame       `json:"request,omitempty"`
	Response        json.RawMessage `json:"response,omitempty"`
}

type IPCClient struct {
	path string

	mu       sync.Mutex
	conn     net.Conn
	clientID string
	pending  map[string]chan *ipcFrame
	ready    chan struct{}

	// OnBroadcast receives every broadcast addressed to this client. It runs on
	// the reader goroutine, so it must not block for long.
	OnBroadcast func(f *ipcFrame)
	// OnReset fires after the socket drops; followers must re-subscribe.
	OnReset func()
}

func defaultIPCPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "ipc", "ipc.sock")
}

func NewIPCClient(path string) *IPCClient {
	return &IPCClient{path: path, pending: map[string]chan *ipcFrame{}, ready: make(chan struct{})}
}

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Run connects and reconnects forever.
func (c *IPCClient) Run() {
	backoff := 500 * time.Millisecond
	for {
		err := c.session()
		if err != nil {
			log.Printf("ipc: %v (desktop app not running?), retrying", err)
		}
		c.mu.Lock()
		c.conn = nil
		c.clientID = ""
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.ready = make(chan struct{})
		c.mu.Unlock()
		if c.OnReset != nil {
			c.OnReset()
		}
		time.Sleep(backoff)
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func (c *IPCClient) session() error {
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return err
	}
	defer conn.Close()
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	initID := uuid()
	ch := make(chan *ipcFrame, 1)
	c.mu.Lock()
	c.pending[initID] = ch
	c.mu.Unlock()
	params, _ := json.Marshal(map[string]string{"clientType": "fastcontrol"})
	zero := 0
	if err := c.write(&ipcFrame{Type: "request", RequestID: initID, Version: &zero, Method: "initialize", Params: params}); err != nil {
		return err
	}

	errc := make(chan error, 1)
	go func() { errc <- c.readLoop(conn) }()

	select {
	case f := <-ch:
		if f == nil || f.ResultType != "success" {
			return errors.New("initialize failed")
		}
		var r struct {
			ClientID string `json:"clientId"`
		}
		_ = json.Unmarshal(f.Result, &r)
		c.mu.Lock()
		c.clientID = r.ClientID
		close(c.ready)
		c.mu.Unlock()
		log.Printf("ipc: connected to desktop app as client %s", r.ClientID)
	case err := <-errc:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("initialize timed out")
	}
	return <-errc
}

func (c *IPCClient) write(f *ipcFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	buf := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = conn.Write(buf)
	return err
}

func (c *IPCClient) readLoop(conn net.Conn) error {
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return err
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		if n == 0 || n > 256<<20 {
			return fmt.Errorf("bad frame length %d", n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(conn, body); err != nil {
			return err
		}
		var f ipcFrame
		if err := json.Unmarshal(body, &f); err != nil {
			log.Printf("ipc: bad frame: %v", err)
			continue
		}
		switch f.Type {
		case "response":
			c.mu.Lock()
			ch := c.pending[f.RequestID]
			delete(c.pending, f.RequestID)
			c.mu.Unlock()
			if ch != nil {
				ch <- &f
			}
		case "broadcast":
			if c.OnBroadcast != nil {
				c.OnBroadcast(&f)
			}
		case "client-discovery-request":
			// We never handle requests from other clients.
			resp, _ := json.Marshal(map[string]bool{"canHandle": false})
			_ = c.write(&ipcFrame{Type: "client-discovery-response", RequestID: f.RequestID, Response: resp})
		case "request":
			errMsg, _ := json.Marshal("no-handler-for-request")
			_ = c.write(&ipcFrame{Type: "response", RequestID: f.RequestID, ResultType: "error", Error: errMsg})
		}
	}
}

func (c *IPCClient) waitReady(timeout time.Duration) (string, error) {
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	select {
	case <-ready:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.clientID, nil
	case <-time.After(timeout):
		return "", errors.New("desktop app IPC not connected")
	}
}

func (c *IPCClient) ClientID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clientID
}

// Request sends a request and waits for its response. version overrides the
// method's default when non-nil.
func (c *IPCClient) Request(method string, params any, targetClientID string, timeout time.Duration, version *int) (*ipcFrame, error) {
	id, err := c.waitReady(3 * time.Second)
	if err != nil {
		return nil, err
	}
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	v := ipcVersions[method]
	if version != nil {
		v = *version
	}
	reqID := uuid()
	ch := make(chan *ipcFrame, 1)
	c.mu.Lock()
	c.pending[reqID] = ch
	c.mu.Unlock()
	f := &ipcFrame{Type: "request", RequestID: reqID, SourceClientID: id, Version: &v, Method: method, Params: p, TargetClientID: targetClientID, TimeoutMs: int(timeout / time.Millisecond)}
	if err := c.write(f); err != nil {
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok || r == nil {
			return nil, errors.New("connection closed")
		}
		if r.ResultType != "success" {
			var msg string
			_ = json.Unmarshal(r.Error, &msg)
			if msg == "" {
				msg = string(r.Error)
			}
			return r, fmt.Errorf("%s: %s", method, msg)
		}
		return r, nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: timeout", method)
	}
}

func (c *IPCClient) Broadcast(method string, params any, targets []string) error {
	id, err := c.waitReady(3 * time.Second)
	if err != nil {
		return err
	}
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	v := ipcVersions[method]
	return c.write(&ipcFrame{Type: "broadcast", Method: method, SourceClientID: id, TargetClientIDs: targets, Params: p, Version: &v})
}

// checkProtocolDrift compares ipcVersions with the map embedded in the
// installed desktop app and returns the methods whose versions differ.
// app.asar stores files uncompressed, so a byte search is enough.
func checkProtocolDrift(asar string) ([]string, error) {
	data, err := os.ReadFile(asar)
	if err != nil {
		return nil, err
	}
	start := bytes.Index(data, []byte(`{"thread-stream-state-changed":`))
	if start < 0 {
		return nil, errors.New("version map not found in desktop bundle")
	}
	end := bytes.IndexByte(data[start:], '}')
	if end < 0 {
		return nil, errors.New("version map not terminated")
	}
	var installed map[string]int
	if err := json.Unmarshal(data[start:start+end+1], &installed); err != nil {
		return nil, err
	}
	var drift []string
	for m, v := range ipcVersions {
		if iv, ok := installed[m]; !ok || iv != v {
			drift = append(drift, fmt.Sprintf("%s: pinned %d, installed %d", m, v, iv))
		}
	}
	return drift, nil
}
