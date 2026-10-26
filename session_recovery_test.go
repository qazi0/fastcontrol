package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestConcurrentSlowClientWritersCloseOnce(t *testing.T) {
	c := &Client{out: make(chan []byte), done: make(chan struct{})}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); <-start; c.push([]byte("update")) }()
	}
	close(start)
	workers.Wait()
	select {
	case <-c.done:
	default:
		t.Fatal("slow client was left connected")
	}
}

func recoverySession(t *testing.T) (*Session, <-chan ipcFrame) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	ipc := NewIPCClient("")
	ipc.conn = client
	ipc.clientID = "relay"
	close(ipc.ready)
	hub := NewHub(ipc, "")
	session := newSession(hub, ThreadInfo{ID: "test"})
	frames := make(chan ipcFrame, 32)
	go func() {
		defer close(frames)
		for {
			var header [4]byte
			if _, err := io.ReadFull(server, header[:]); err != nil {
				return
			}
			body := make([]byte, binary.LittleEndian.Uint32(header[:]))
			if _, err := io.ReadFull(server, body); err != nil {
				return
			}
			var frame ipcFrame
			if json.Unmarshal(body, &frame) == nil {
				frames <- frame
			}
		}
	}()
	return session, frames
}
func nextRecoveryFrame(t *testing.T, frames <-chan ipcFrame) {
	t.Helper()
	select {
	case f := <-frames:
		if f.Method != "thread-stream-following-changed" {
			t.Fatal(f.Method)
		}
	case <-time.After(time.Second):
		t.Fatal("missing snapshot request")
	}
}
func noRecoveryFrame(t *testing.T, frames <-chan ipcFrame) {
	t.Helper()
	select {
	case f := <-frames:
		t.Fatalf("duplicate snapshot request: %s", f.Method)
	default:
	}
}
func recoverySnapshot(s *Session, owner string, revision int64, title string) {
	s.onFrame(&ipcFrame{Method: "thread-stream-state-changed", SourceClientID: owner, Params: mustJSON(map[string]any{"conversationId": "test", "change": map[string]any{"type": "snapshot", "revision": revision, "conversationState": map[string]any{"title": title}}})})
}
func recoveryPatch(s *Session, revision, base int64) {
	s.onFrame(&ipcFrame{Method: "thread-stream-state-changed", SourceClientID: "owner", Params: mustJSON(map[string]any{"conversationId": "test", "change": map[string]any{"type": "patches", "revision": revision, "baseRevision": base, "patches": []any{}}})})
}
func TestSlowSnapshotRequestsAreCoalesced(t *testing.T) {
	s, frames := recoverySession(t)
	s.follow("owner", false)
	nextRecoveryFrame(t, frames)
	for i := 0; i < 30; i++ {
		s.follow("owner", false)
	}
	noRecoveryFrame(t, frames)
	s.followAt = time.Now().Add(-ipcTimeout - time.Second)
	s.follow("owner", true)
	nextRecoveryFrame(t, frames)
	if !s.awaitingSnapshot {
		t.Fatal("snapshot was not marked in flight")
	}
}
func TestRevisionGapDoesNotCreateSnapshotStorm(t *testing.T) {
	s, frames := recoverySession(t)
	s.owner = "owner"
	recoverySnapshot(s, "owner", 10, "current")
	recoveryPatch(s, 12, 11)
	nextRecoveryFrame(t, frames)
	for i := int64(13); i < 30; i++ {
		recoveryPatch(s, i, i-1)
	}
	noRecoveryFrame(t, frames)
	recoverySnapshot(s, "owner", 10, "late")
	if s.revision != 10 || !s.awaitingSnapshot || str(s.state, "title") != "current" {
		t.Fatal("late snapshot ended recovery")
	}
	recoverySnapshot(s, "owner", 30, "fresh")
	if s.revision != 30 || s.awaitingSnapshot || str(s.state, "title") != "fresh" {
		t.Fatal("fresh snapshot did not recover")
	}
	recoveryPatch(s, 29, 28)
	noRecoveryFrame(t, frames)
	recoverySnapshot(s, "old-owner", 40, "wrong owner")
	if str(s.state, "title") != "fresh" {
		t.Fatal("old owner replaced current state")
	}
}
