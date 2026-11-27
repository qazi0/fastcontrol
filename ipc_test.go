package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIPCLogStateDeduplicationAndTransitions(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	var logs []string
	state := &ipcLogState{
		now:            func() time.Time { return now },
		logf:           func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		repeatInterval: time.Hour,
	}

	errRefused := errors.New("dial unix /tmp/ipc.sock: connect: connection refused")
	errMissing := errors.New("dial unix /tmp/ipc.sock: connect: no such file or directory")

	// First failure logs once.
	state.onError(errRefused, false)
	if len(logs) != 1 || !strings.Contains(logs[0], "connection refused") {
		t.Fatalf("expected initial failure log, got %v", logs)
	}

	// Identical retries within the suppression window emit nothing.
	for i := 0; i < 100; i++ {
		now = now.Add(30 * time.Second)
		state.onError(errRefused, false)
	}
	if len(logs) != 1 {
		t.Fatalf("expected identical retries to be suppressed, got %d logs: %v", len(logs), logs)
	}

	// Crossing the 1-hour reminder interval emits one periodic summary.
	now = now.Add(15 * time.Minute)
	state.onError(errRefused, false)
	if len(logs) != 2 || !strings.Contains(logs[1], "100 identical retries suppressed") {
		t.Fatalf("expected hourly reminder log, got %v", logs)
	}

	// Further identical errors stay suppressed until the error reason changes.
	for i := 0; i < 5; i++ {
		now = now.Add(5 * time.Second)
		state.onError(errRefused, false)
	}
	if len(logs) != 2 {
		t.Fatalf("expected post-reminder suppression, got %v", logs)
	}

	// Changing the error reason logs immediately with suppressed count.
	now = now.Add(5 * time.Second)
	state.onError(errMissing, false)
	if len(logs) != 3 || !strings.Contains(logs[2], "no such file or directory") || !strings.Contains(logs[2], "5 suppressed retries") {
		t.Fatalf("expected error transition log, got %v", logs)
	}

	// Suppress a few retries of the new error, then recover.
	for i := 0; i < 3; i++ {
		now = now.Add(5 * time.Second)
		state.onError(errMissing, false)
	}
	state.onConnected("client-1")
	if len(logs) != 4 || !strings.Contains(logs[3], "client-1") || !strings.Contains(logs[3], "recovered after 3 suppressed retries") {
		t.Fatalf("expected recovery log, got %v", logs)
	}

	// Disconnecting from an established session logs immediately.
	state.onError(io.EOF, true)
	if len(logs) != 5 || !strings.Contains(logs[4], "disconnected from desktop app: EOF") {
		t.Fatalf("expected disconnect log, got %v", logs)
	}

	// Subsequent dial refusal transitions from EOF once, then suppresses repeats.
	state.onError(errRefused, false)
	state.onError(errRefused, false)
	state.onError(errRefused, false)
	if len(logs) != 6 || !strings.Contains(logs[5], "connection refused") {
		t.Fatalf("expected single post-disconnect refusal log, got %v", logs)
	}

	// Clean reconnect logs recovery with suppressed count.
	state.onConnected("client-2")
	if len(logs) != 7 || !strings.Contains(logs[6], "client-2") || !strings.Contains(logs[6], "recovered after 2 suppressed retries") {
		t.Fatalf("expected reconnect recovery log, got %v", logs)
	}
}

func serveFakeDesktopIPC(t *testing.T, server net.Conn, clientID string, release <-chan struct{}) {
	t.Helper()
	defer server.Close()
	var hdr [4]byte
	if _, err := io.ReadFull(server, hdr[:]); err != nil {
		return
	}
	body := make([]byte, binary.LittleEndian.Uint32(hdr[:]))
	if _, err := io.ReadFull(server, body); err != nil {
		return
	}
	var req ipcFrame
	if err := json.Unmarshal(body, &req); err != nil {
		return
	}
	res, _ := json.Marshal(map[string]string{"clientId": clientID})
	resp := &ipcFrame{
		Type:       "response",
		RequestID:  req.RequestID,
		ResultType: "success",
		Result:     res,
	}
	b, _ := json.Marshal(resp)
	buf := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, _ = server.Write(buf)
	<-release
}

func TestIPCClientRunDeduplicatesErrorsAndRecovers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu       sync.Mutex
		logs     []string
		attempts int32
		resets   int32
	)
	connectedCh := make(chan struct{}, 1)
	releaseServer := make(chan struct{})
	postDropRetries := make(chan struct{}, 1)

	client := NewIPCClient("test-ipc")
	client.initialBackoff = 2 * time.Millisecond
	client.maxBackoff = 4 * time.Millisecond
	client.repeatLogInterval = time.Hour
	client.logf = func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		mu.Lock()
		logs = append(logs, msg)
		mu.Unlock()
		if strings.Contains(msg, "desktop-42") {
			select {
			case connectedCh <- struct{}{}:
			default:
			}
		}
	}
	client.OnReset = func() {
		atomic.AddInt32(&resets, 1)
	}
	client.dial = func(string) (net.Conn, error) {
		n := atomic.AddInt32(&attempts, 1)
		switch {
		case n <= 8:
			return nil, errors.New("dial unix /tmp/ipc.sock: connect: connection refused")
		case n <= 11:
			return nil, errors.New("dial unix /tmp/ipc.sock: connect: no such file or directory")
		case n == 12:
			cConn, sConn := net.Pipe()
			go serveFakeDesktopIPC(t, sConn, "desktop-42", releaseServer)
			return cConn, nil
		default:
			if n >= 16 {
				select {
				case postDropRetries <- struct{}{}:
				default:
				}
			}
			return nil, errors.New("dial unix /tmp/ipc.sock: connect: connection refused")
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		client.run(ctx)
	}()

	select {
	case <-connectedCh:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for IPC recovery")
	}

	if got := client.ClientID(); got != "desktop-42" {
		cancel()
		t.Fatalf("expected clientID desktop-42, got %q", got)
	}
	if got := atomic.LoadInt32(&resets); got != 0 {
		cancel()
		t.Fatalf("expected 0 OnReset calls during failed dials, got %d", got)
	}

	// Drop the connected server and allow several post-drop dial retries.
	close(releaseServer)
	select {
	case <-postDropRetries:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for post-drop retries")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("IPCClient.run did not exit after cancel")
	}

	if got := atomic.LoadInt32(&resets); got != 1 {
		t.Fatalf("expected 1 OnReset after established session dropped, got %d", got)
	}

	mu.Lock()
	gotLogs := append([]string(nil), logs...)
	mu.Unlock()

	// Expected sequence:
	// 1. First connection refused (7 suppressed)
	// 2. Error reason changed to no such file or directory (2 suppressed)
	// 3. Connected as desktop-42 (recovered after 2 suppressed retries)
	// 4. Disconnected from desktop app: EOF
	// 5. Post-drop connection refused (further identical retries suppressed)
	if len(gotLogs) != 5 {
		t.Fatalf("expected exactly 5 transition logs across >=16 attempts, got %d:\n%s", len(gotLogs), strings.Join(gotLogs, "\n"))
	}
	if !strings.Contains(gotLogs[0], "connection refused") {
		t.Fatalf("log[0]: %s", gotLogs[0])
	}
	if !strings.Contains(gotLogs[1], "no such file or directory") || !strings.Contains(gotLogs[1], "7 suppressed retries") {
		t.Fatalf("log[1]: %s", gotLogs[1])
	}
	if !strings.Contains(gotLogs[2], "desktop-42") || !strings.Contains(gotLogs[2], "recovered after 2 suppressed retries") {
		t.Fatalf("log[2]: %s", gotLogs[2])
	}
	if !strings.Contains(gotLogs[3], "disconnected from desktop app: EOF") {
		t.Fatalf("log[3]: %s", gotLogs[3])
	}
	if !strings.Contains(gotLogs[4], "connection refused") {
		t.Fatalf("log[4]: %s", gotLogs[4])
	}
}
