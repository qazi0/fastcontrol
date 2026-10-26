package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

func sessionTitle(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) {
		return "", errors.New("Enter a chat title")
	}
	if len([]rune(value)) > 4096 {
		return "", errors.New("Use a shorter chat title")
	}
	for _, c := range value {
		if unicode.IsControl(c) {
			return "", errors.New("Use a single line for the title")
		}
	}
	return value, nil
}

// Metadata actions do not submit a model turn. An isolated stdio connection
// releases its thread writers on exit, so the desktop can open a new fork.
func codexSessionAction(method string, params map[string]any) (map[string]any, error) {
	bin, err := codexBin()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "app-server", "--listen", "stdio://")
	cmd.Cancel = func() error { killProcess(cmd.Process); return nil }
	cmd.Env = codexEnv()
	cmd.Stderr = io.Discard
	prepareProcess(cmd)
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		input.Close()
		output.Close()
		return nil, err
	}
	defer func() {
		_ = input.Close()
		go func() { _, _ = io.Copy(io.Discard, output) }()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			killProcess(cmd.Process)
			<-done
		}
	}()
	decoder := json.NewDecoder(output)
	sequence := 0
	call := func(name string, value map[string]any) (map[string]any, error) {
		sequence++
		id := fmt.Sprintf("control-%d", sequence)
		if _, err := input.Write(append(mustJSON(map[string]any{"id": id, "method": name, "params": value}), '\n')); err != nil {
			return nil, err
		}
		for {
			var frame struct {
				ID     json.RawMessage `json:"id"`
				Result map[string]any  `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := decoder.Decode(&frame); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, errors.New("Codex closed the control connection")
			}
			var received string
			if json.Unmarshal(frame.ID, &received) != nil || received != id {
				continue
			}
			if frame.Error != nil {
				return nil, errors.New(clipPlain(frame.Error.Message, 500))
			}
			return frame.Result, nil
		}
	}
	if _, err = call("initialize", map[string]any{"clientInfo": map[string]any{"name": "fastcontrol", "title": "fastcontrol", "version": "0.2.10"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		return nil, err
	}
	if _, err = input.Write([]byte("{\"method\":\"initialized\"}\n")); err != nil {
		return nil, err
	}
	result, err := call(method, params)
	if err == nil && method == "thread/fork" {
		if id := str(mapValue(result["thread"]), "id"); id != "" {
			_, _ = call("thread/unsubscribe", map[string]any{"threadId": id})
		}
	}
	return result, err
}

func (h *Hub) sessionAction(method string, params map[string]any) (map[string]any, error) {
	if h.headless == nil {
		return codexSessionAction(method, params)
	}
	if err := h.headless.start(); err != nil {
		return nil, err
	}
	return h.headless.call(method, params)
}
func (h *Hub) renameSession(id, title string) error {
	title, err := sessionTitle(title)
	if err != nil {
		return err
	}
	_, err = h.sessionAction("thread/name/set", map[string]any{"threadId": id, "name": title})
	if err == nil {
		h.requestListRefresh()
	}
	return err
}
func (h *Hub) forkSession(id string) (string, error) {
	result, err := h.sessionAction("thread/fork", map[string]any{"threadId": id, "excludeTurns": true, "deferGoalContinuation": true})
	if err != nil {
		return "", err
	}
	child := str(mapValue(result["thread"]), "id")
	if child == "" {
		return "", errors.New("Codex did not return the new chat")
	}
	if h.links != nil {
		_ = h.links.put(child, id)
	}
	h.requestListRefresh()
	return child, nil
}

type sessionLinks struct {
	mu      sync.Mutex
	path    string
	parents map[string]string
}

func newSessionLinks(path string) *sessionLinks {
	s := &sessionLinks{path: path, parents: map[string]string{}}
	if info, err := os.Stat(path); err == nil && info.Size() <= 2<<20 {
		if data, err := os.ReadFile(path); err == nil {
			var values map[string]string
			if json.Unmarshal(data, &values) == nil && values != nil {
				s.parents = values
			}
		}
	}
	return s
}
func (s *sessionLinks) put(child, parent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Keep a successful native fork visible if saving its supplemental link fails.
	s.parents[child] = parent
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".session-links-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(s.parents)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), s.path)
}
func (s *sessionLinks) apply(snapshot *OrganizationSnapshot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for child, parent := range s.parents {
		p, ok := snapshot.Placements[child]
		if !ok || p.Archived || p.Relation == "subagent" {
			continue
		}
		if source, ok := snapshot.Placements[parent]; !ok || source.Archived || child == parent {
			continue
		}
		p.ParentID, p.Relation = parent, "fork"
		snapshot.Placements[child] = p
	}
}
