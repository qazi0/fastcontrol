package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type recoveryPeer struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	URLs         []string `json:"urls"`
	User         string   `json:"user"`
	Hosts        []string `json:"hosts"`
	HostKeyAlias string   `json:"hostKeyAlias"`
}
type publicRecoveryPeer struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	URLs []string `json:"urls"`
}
type recoveryManager struct {
	// Configuration is immutable after startup. Only jobs/timestamps are mutable.
	peers    map[string]recoveryPeer
	public   []publicRecoveryPeer
	mu       sync.Mutex
	jobs     map[string]bool
	finished map[string]time.Time
	run      func(context.Context, recoveryPeer) error
}

var recoveryID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
var recoveryHost = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.:-]{0,252}$`)

func newRecoveryManager(home string) *recoveryManager {
	m := &recoveryManager{peers: map[string]recoveryPeer{}, jobs: map[string]bool{}, finished: map[string]time.Time{}, public: []publicRecoveryPeer{}, run: recoverOverSSH}
	path := filepath.Join(home, ".fastcontrol", "recovery-peers.json")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return m
	}
	if err != nil {
		log.Printf("recovery configuration: %v", err)
		return m
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 || info.Mode().Perm()&0022 != 0 {
		log.Print("recovery configuration must be a regular private file under 64 KiB")
		return m
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(data) > 64<<10 {
		log.Print("could not read recovery configuration")
		return m
	}
	peers, err := parseRecoveryPeers(data)
	if err != nil {
		log.Printf("recovery configuration: %v", err)
		return m
	}
	for _, peer := range peers {
		m.peers[peer.ID] = peer
		m.public = append(m.public, publicRecoveryPeer{peer.ID, peer.Name, append([]string(nil), peer.URLs...)})
	}
	return m
}

func parseRecoveryPeers(data []byte) ([]recoveryPeer, error) {
	var peers []recoveryPeer
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&peers); err != nil {
		return nil, errors.New("invalid peer configuration")
	}
	if decoder.Decode(new(any)) != io.EOF || len(peers) > 16 {
		return nil, errors.New("invalid peer configuration")
	}
	seen := map[string]bool{}
	identities := map[string]bool{}
	for _, peer := range peers {
		if !recoveryID.MatchString(peer.ID) || seen[peer.ID] || identities[peer.User+"@"+peer.HostKeyAlias] || strings.TrimSpace(peer.Name) == "" || len(peer.Name) > 120 || !recoveryID.MatchString(peer.User) || !recoveryHost.MatchString(peer.HostKeyAlias) || len(peer.Hosts) == 0 || len(peer.Hosts) > 4 || len(peer.URLs) == 0 || len(peer.URLs) > 12 {
			return nil, errors.New("invalid or duplicate recovery peer")
		}
		seen[peer.ID] = true
		identities[peer.User+"@"+peer.HostKeyAlias] = true
		for _, host := range peer.Hosts {
			if !recoveryHost.MatchString(host) {
				return nil, errors.New("invalid SSH host")
			}
		}
		for _, address := range peer.URLs {
			u, err := url.Parse(address)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
				return nil, errors.New("invalid recovery peer URL")
			}
		}
	}
	return peers, nil
}

func (m *recoveryManager) recover(id string) error {
	if m == nil {
		return errors.New("Remote recovery is not configured on this computer")
	}
	peer, ok := m.peers[id]
	if !ok {
		return errors.New("This computer is not configured for remote recovery")
	}
	m.mu.Lock()
	if m.jobs[id] || time.Since(m.finished[id]) < 10*time.Second {
		m.mu.Unlock()
		return errors.New("Recovery already requested. Wait for the computer to reconnect")
	}
	m.jobs[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.jobs, id); m.finished[id] = time.Now(); m.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	return m.run(ctx, peer)
}

func sshRecoveryArgs(peer recoveryPeer, host, command string) []string {
	return []string{"-F", "none", "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=4", "-o", "ConnectionAttempts=1", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=1", "-o", "HostKeyAlias=" + peer.HostKeyAlias, peer.User + "@" + host, command}
}

const remoteRecoveryCommand = `bash "$HOME/.fastcontrol/repair.command" --remote`

func recoverOverSSH(ctx context.Context, peer recoveryPeer) error {
	if _, err := exec.LookPath("ssh"); err != nil {
		return errors.New("Install the SSH client on the helper computer to enable recovery")
	}
	// Probe all configured routes, then execute the fixed recovery command once.
	probe, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		host string
		err  error
	}
	results := make(chan result, len(peer.Hosts))
	for _, host := range peer.Hosts {
		go func(host string) {
			err := exec.CommandContext(probe, "ssh", sshRecoveryArgs(peer, host, "true")...).Run()
			results <- result{host, err}
		}(host)
	}
	host := ""
	for range peer.Hosts {
		result := <-results
		if result.err == nil {
			host = result.host
			break
		}
	}
	cancel()
	if host == "" {
		return errors.New("SSH could not reach the computer. It must be awake, connected and allow SSH access from this helper")
	}
	cmd := exec.CommandContext(ctx, "ssh", sshRecoveryArgs(peer, host, remoteRecoveryCommand)...)
	// No command, path, SSH option or credential comes from the phone request.
	err := cmd.Run()
	if ctx.Err() != nil {
		return errors.New("Recovery timed out. The Mac may need a network connection or local attention")
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 75 {
			return errors.New("A voice upload is in progress. Try recovery after it finishes")
		}
		return fmt.Errorf("The recovery script did not complete. Check SSH access and fastcontrol installation on %s", peer.Name)
	}
	return nil
}
