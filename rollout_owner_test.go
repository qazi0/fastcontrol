package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRolloutOwnerProcess(t *testing.T) {
	if os.Getenv("FASTCONTROL_OWNER_HELPER") != "1" {
		return
	}
	f, err := os.OpenFile(os.Getenv("FASTCONTROL_OWNER_PATH"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(2)
	}
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	f.Close()
	os.Exit(0)
}

func TestNativeRolloutOwnerSurvivesLongSilenceAndDetectsExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte("started\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-177 * time.Hour)
	os.Chtimes(path, old, old)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRolloutOwnerProcess$")
	cmd.Env = append(os.Environ(), "FASTCONTROL_OWNER_HELPER=1", "FASTCONTROL_OWNER_PATH="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	ready := make(chan bool, 1)
	go func() { scanner := bufio.NewScanner(stdout); ready <- scanner.Scan() && scanner.Text() == "ready" }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("owner did not open file")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner helper timed out")
	}
	cache := &rolloutOwnerCache{entries: map[string]rolloutOwnerEvidence{}, pending: map[string]os.FileInfo{}, probe: platformRolloutOwners}
	info, _ := os.Stat(path)
	cache.refresh(map[string]os.FileInfo{path: info})
	if entry := cache.entries[path]; !entry.known || !entry.alive || !cache.running(path, info) {
		t.Fatalf("live owner of 177-hour-old transcript lost: %+v", entry)
	}
	stdin.Close()
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	entry := cache.entries[path]
	entry.checked = time.Time{}
	cache.entries[path] = entry
	cache.refresh(map[string]os.FileInfo{path: info})
	if entry := cache.entries[path]; !entry.known || entry.alive || cache.running(path, info) {
		t.Fatalf("exited owner remained active without file changes: %+v", entry)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	proof := platformRolloutOwners([]string{path})[path]
	if !proof.known || proof.alive {
		t.Fatal("relay's read-only descriptor counted as live work", proof)
	}
}

func TestRolloutOwnerDoesNotPublishEvidenceForReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	os.WriteFile(path, []byte("old"), 0600)
	info, _ := os.Stat(path)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cache := &rolloutOwnerCache{entries: map[string]rolloutOwnerEvidence{}, pending: map[string]os.FileInfo{}, probe: func([]string) map[string]rolloutOwnerEvidence {
		close(started)
		<-release
		return map[string]rolloutOwnerEvidence{path: {known: true, alive: false}}
	}}
	go func() { cache.refresh(map[string]os.FileInfo{path: info}); close(done) }()
	<-started
	os.Rename(path, path+".old")
	os.WriteFile(path, []byte("new"), 0600)
	close(release)
	<-done
	if len(cache.entries) != 0 {
		t.Fatal("replacement inherited old process evidence")
	}
}

func TestRolloutOwnerProbeFailureKeepsLastKnownState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	os.WriteFile(path, []byte("started"), 0600)
	info, _ := os.Stat(path)
	cache := &rolloutOwnerCache{entries: map[string]rolloutOwnerEvidence{path: {file: info, known: true, alive: false}}, pending: map[string]os.FileInfo{}, probe: func([]string) map[string]rolloutOwnerEvidence { return nil }}
	cache.refresh(map[string]os.FileInfo{path: info})
	if cache.running(path, info) {
		t.Fatal("temporary process-probe failure resurrected abandoned work")
	}
}
