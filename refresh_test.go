package main

import (
	"testing"
	"time"
)

func TestSlowListRefreshDoesNotBlockSocketControl(t *testing.T) {
	hub := &Hub{clients: map[*Client]bool{}, sessions: map[string]*Session{}, execs: map[string]*execRun{}}
	client := &Client{hub: hub, subs: map[string]bool{"old": true}, out: make(chan []byte, 8), done: make(chan struct{})}
	hub.clients[client] = true
	hub.listMu.Lock()
	released := false
	defer func() {
		if !released {
			hub.listMu.Unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		hub.handle(client, []byte(`{"op":"threads"}`))
		hub.handle(client, []byte(`{"op":"close","id":"old"}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked list refresh held later socket commands")
	}
	hub.mu.Lock()
	subscribed := client.subs["old"]
	hub.mu.Unlock()
	if subscribed {
		t.Fatal("close was not processed")
	}
	if !hub.listRefresh.Load() {
		t.Fatal("refresh was not queued")
	}
	for i := 0; i < 20; i++ {
		hub.requestListRefresh()
	}
	hub.listMu.Unlock()
	released = true
	deadline := time.Now().Add(2 * time.Second)
	for hub.listRefresh.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if hub.listRefresh.Load() {
		t.Fatal("refresh did not finish")
	}
}
