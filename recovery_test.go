package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const recoveryFixture = `[{"id":"m5p","name":"Other Mac","urls":["http://100.64.0.2:8787"],"user":"owner","hosts":["100.64.0.2","192.168.1.20"],"hostKeyAlias":"192.168.1.20"}]`

func TestRecoveryConfigurationRejectsUntrustedInputs(t *testing.T) {
	if _, err := parseRecoveryPeers([]byte(recoveryFixture)); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		strings.Replace(recoveryFixture, `"owner"`, `"-oProxyCommand=bad"`, 1),
		strings.Replace(recoveryFixture, `"100.64.0.2","192.168.1.20"`, `"host; bad"`, 1),
		strings.Replace(recoveryFixture, `"192.168.1.20"}`, `"-oProxyCommand=bad"}`, 1),
		strings.Replace(recoveryFixture, `:8787"`, `:8787?t=secret"`, 1),
		strings.Replace(recoveryFixture, `"id":`, `"command":"bad","id":`, 1),
		recoveryFixture + `{}`,
		`[` + strings.Trim(recoveryFixture, "[]") + `,` + strings.Trim(recoveryFixture, "[]") + `]`,
	} {
		if _, err := parseRecoveryPeers([]byte(data)); err == nil {
			t.Fatalf("accepted invalid configuration: %s", data)
		}
	}
}

func TestRecoveryPublicMetadataExcludesSSHDetails(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".fastcontrol")
	os.Mkdir(dir, 0700)
	os.WriteFile(filepath.Join(dir, "recovery-peers.json"), []byte(recoveryFixture), 0600)
	m := newRecoveryManager(home)
	data, _ := json.Marshal(m.public)
	if len(m.peers) != 1 || strings.Contains(string(data), "hostKeyAlias") || strings.Contains(string(data), "owner") || strings.Contains(string(data), "192.168.1.20") {
		t.Fatal(string(data))
	}
	args := strings.Join(sshRecoveryArgs(m.peers["m5p"], "100.64.0.2", remoteRecoveryCommand), " ")
	for _, required := range []string{"-F none", "BatchMode=yes", "StrictHostKeyChecking=yes", "HostKeyAlias=192.168.1.20", remoteRecoveryCommand} {
		if !strings.Contains(args, required) {
			t.Fatal("missing SSH constraint", required)
		}
	}
}

func TestConcurrentRecoveryIsNotReplayed(t *testing.T) {
	m := newRecoveryManager(t.TempDir())
	peers, _ := parseRecoveryPeers([]byte(recoveryFixture))
	m.peers["m5p"] = peers[0]
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var calls atomic.Int32
	m.run = func(context.Context, recoveryPeer) error { calls.Add(1); close(started); <-release; return nil }
	go func() { done <- m.recover("m5p") }()
	<-started
	if err := m.recover("m5p"); err == nil {
		t.Fatal("duplicate recovery started")
	}
	if err := m.recover("unknown"); err == nil {
		t.Fatal("unknown target accepted")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := m.recover("m5p"); err == nil {
		t.Fatal("recent completed request replayed")
	}
	if calls.Load() != 1 {
		t.Fatal("more than one recovery executed")
	}
}

func TestRecoveryIngressRejectsExtraFieldsAndRoutesAcrossProviders(t *testing.T) {
	m := newRecoveryManager(t.TempDir())
	peers, _ := parseRecoveryPeers([]byte(recoveryFixture))
	m.peers["m5p"] = peers[0]
	var calls atomic.Int32
	m.run = func(context.Context, recoveryPeer) error { calls.Add(1); return nil }
	h := &Hub{recovery: m}
	c := &Client{out: make(chan []byte, 8), done: make(chan struct{})}
	h.handle(c, []byte(`{"op":"recover","id":"m5p","nonce":"bad","host":"different"}`))
	var ack map[string]any
	json.Unmarshal(<-c.out, &ack)
	if ack["ok"] != false || calls.Load() != 0 {
		t.Fatal("unknown request field reached SSH")
	}
	h.handle(c, []byte(`{"op":"recover","id":"m5p","nonce":"good","provider":"claude"}`))
	select {
	case data := <-c.out:
		json.Unmarshal(data, &ack)
		if ack["ok"] != true || ack["nonce"] != "good" || calls.Load() != 1 {
			t.Fatal(string(data))
		}
	case <-time.After(time.Second):
		t.Fatal("no provider-independent recovery response")
	}
}
