package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (m *ClaudeManager) discoverModels() error {
	m.modelsMu.Lock()
	defer m.modelsMu.Unlock()
	m.mu.Lock()
	ready := len(m.models) > 0
	m.mu.Unlock()
	if ready {
		return nil
	}
	if m.bin == "" {
		return errors.New("Install Claude Code On This Mac")
	}
	home, _ := os.UserHomeDir()
	cwd := filepath.Join(home, ".fastcontrol/model-catalog")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.bin, "-p", "--safe-mode", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--model", "claude-haiku-5-5", "--effort", "low")
	cmd.Dir = cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	_, err = io.WriteString(stdin, string(mustJSON(map[string]any{"type": "control_request", "request_id": "models", "request": map[string]any{"subtype": "initialize"}}))+"\n")
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		var value map[string]any
		if json.Unmarshal(scanner.Bytes(), &value) != nil {
			continue
		}
		if str(value, "type") == "control_response" {
			response := mapValue(value["response"])
			if str(response, "subtype") == "error" {
				return errors.New(str(response, "error"))
			}
			models := arrayValue(mapValue(response["response"])["models"])
			if len(models) > 0 {
				m.setModels(models)
				return nil
			}
		}
	}
	return errors.New("Could Not Read Claude Models")
}
func (m *ClaudeManager) getRun(id string, takeover bool) (*ClaudeRun, error) {
	m.refreshActive()
	m.mu.Lock()
	run := m.runs[id]
	entry, ok := m.entries[id]
	active := m.active[id]
	m.mu.Unlock()
	if run != nil {
		return run, nil
	}
	if !ok {
		return nil, errors.New("Claude Session Not Found")
	}
	if active.ID != "" {
		if !takeover {
			return nil, errors.New("This Session Is Managed On The Mac. Use Take Over First")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := exec.CommandContext(ctx, m.bin, "stop", active.ID).Run(); err != nil {
			return nil, errors.New("Could Not Stop The Existing Claude Process")
		}
		m.refreshActive()
		m.mu.Lock()
		_, stillActive := m.active[id]
		m.mu.Unlock()
		if stillActive {
			return nil, errors.New("Claude Is Still Stopping. Try Again")
		}
	}
	p := m.preference(id)
	if p.Model == "" {
		p.Model = entry.Meta.Model
	}
	if p.Effort == "" {
		p.Effort = entry.Meta.Effort
	}
	if p.Permission == "" {
		p.Permission = entry.Meta.PermissionMode
	}
	return m.start(id, entry.Info.Cwd, p, true)
}
func (m *ClaudeManager) handle(c *Client, raw []byte) {
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	op, id := str(v, "op"), str(v, "id")
	ack := func(err error, extra map[string]any) {
		value := map[string]any{"t": "ack", "op": op, "id": id, "nonce": v["nonce"], "ok": err == nil}
		if err != nil {
			value["msg"] = err.Error()
		}
		for k, x := range extra {
			value[k] = x
		}
		m.emit(c, value)
	}
	switch op {
	case "threads":
		m.refresh()
		m.mu.Lock()
		b := append([]byte(nil), m.lastList...)
		m.mu.Unlock()
		c.push(b)
	case "open", "more":
		ack(m.open(c, id, op == "more"), nil)
	case "close":
		m.mu.Lock()
		delete(m.watch[c], id)
		m.mu.Unlock()
	case "projects":
		m.projects(c, "")
	case "project":
		m.projects(c, str(v, "cwd"))
	case "models":
		go func() {
			err := m.discoverModels()
			if err != nil {
				ack(err, nil)
				return
			}
			m.mu.Lock()
			models := append([]Model(nil), m.models...)
			m.mu.Unlock()
			m.emit(c, map[string]any{"t": "models", "models": models, "default": map[string]any{"model": "default", "effort": "low"}})
		}()
	case "new":
		go func() {
			sid := uuid()
			cwd := str(v, "cwd")
			if cwd == "" {
				cwd, _ = os.UserHomeDir()
			}
			p := claudePreference{Model: str(v, "model"), Effort: str(v, "effort"), Permission: str(v, "permissionMode")}
			if p.Permission == "" {
				p.Permission = "default"
			}
			run, err := m.start(sid, cwd, p, false)
			if err == nil {
				_, err = run.send(str(v, "text"), stringArray(v["images"]))
			}
			ack(err, map[string]any{"thread": sid})
			m.refresh()
		}()
	case "wake", "send", "compact":
		go func() {
			run, err := m.getRun(id, op == "wake")
			queued := false
			if err == nil {
				if op == "send" {
					queued, err = run.send(str(v, "text"), stringArray(v["images"]))
				}
				if op == "compact" {
					queued, err = run.send("/compact", nil)
				}
				if op == "wake" {
					err = m.open(c, id, false)
				}
			}
			ack(err, map[string]any{"queued": queued})
		}()
	case "stop":
		go func() {
			m.mu.Lock()
			run := m.runs[id]
			active := m.active[id]
			m.mu.Unlock()
			var err error
			if run != nil {
				run.sendMu.Lock()
				run.mu.Lock()
				run.interrupted = true
				run.queue = nil
				run.mu.Unlock()
				run.sendMu.Unlock()
				_, err = run.control(map[string]any{"subtype": "interrupt"}, 10*time.Second)
			} else if active.ID != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				err = exec.CommandContext(ctx, m.bin, "stop", active.ID).Run()
			} else {
				err = errors.New("Claude Is Not Running")
			}
			ack(err, nil)
		}()
	case "approve", "answer":
		go func() {
			m.mu.Lock()
			run := m.runs[id]
			m.mu.Unlock()
			if run == nil {
				ack(errors.New("Claude Session Is Not Managed Here"), nil)
				return
			}
			decision := str(v, "decision")
			if op == "approve" && decision != "accept" && decision != "acceptForSession" && decision != "decline" && decision != "cancel" {
				ack(errors.New("Unknown Approval Decision"), nil)
				return
			}
			var answers map[string][]string
			if op == "answer" {
				answers = map[string][]string{}
				for key, value := range mapValue(v["answers"]) {
					answers[key] = stringArray(value)
				}
			}
			ack(run.approve(str(v, "reqId"), decision, answers), nil)
		}()
	case "settings":
		go func() {
			p := m.preference(id)
			if model := str(v, "model"); model != "" {
				p.Model = model
			}
			if effort := str(v, "effort"); effort != "" {
				if !contains([]string{"low", "medium", "high", "xhigh", "max"}, effort) {
					ack(errors.New("Unknown Claude Effort"), nil)
					return
				}
				p.Effort = effort
			}
			if permission := str(v, "permissionMode"); permission != "" {
				if !contains([]string{"default", "acceptEdits", "plan", "auto", "bypassPermissions", "dontAsk"}, permission) {
					ack(errors.New("Unknown Claude Permission Mode"), nil)
					return
				}
				p.Permission = permission
			}
			m.mu.Lock()
			run := m.runs[id]
			m.mu.Unlock()
			if run != nil {
				run.sendMu.Lock()
				defer run.sendMu.Unlock()
				run.mu.Lock()
				running := run.running
				oldEffort := run.options.Effort
				run.mu.Unlock()
				if running {
					ack(errors.New("Change Settings After The Turn Finishes"), nil)
					return
				}
				if p.Effort != oldEffort {
					if running {
						ack(errors.New("Change Effort After The Turn Finishes"), nil)
						return
					}
					_ = run.stdin.Close()
					select {
					case <-run.done:
					case <-time.After(5 * time.Second):
						run.kill()
					}
				}
				if p.Model != "" && p.Effort == oldEffort {
					if _, err := run.control(map[string]any{"subtype": "set_model", "model": p.Model}, 10*time.Second); err != nil && p.Effort == oldEffort {
						ack(err, nil)
						return
					}
				}
				if p.Permission != "" && p.Effort == oldEffort {
					if _, err := run.control(map[string]any{"subtype": "set_permission_mode", "mode": p.Permission}, 10*time.Second); err != nil {
						ack(err, nil)
						return
					}
					run.mu.Lock()
					run.options.Permission = p.Permission
					run.mu.Unlock()
				}
			}
			if run != nil && p.Effort == oldEffortFor(run) {
				run.mu.Lock()
				run.options = p
				if p.Model != "" {
					run.view.Meta.Model = m.resolvedModel(p.Model)
				}
				run.mu.Unlock()
				run.publish()
			}
			if err := m.setPreference(id, p); err != nil {
				ack(err, nil)
				return
			}
			ack(nil, nil)
			_ = m.open(c, id, false)
		}()
	case "archive", "unarchive":
		m.mu.Lock()
		run := m.runs[id]
		active := m.active[id]
		m.mu.Unlock()
		if active.ID != "" {
			ack(errors.New("Stop This Session Before Archiving"), nil)
			return
		}
		if run != nil {
			meta, _ := run.snapshot()
			if meta.Running {
				ack(errors.New("Stop This Session Before Archiving"), nil)
				return
			}
		}
		p := m.preference(id)
		p.Archived = op == "archive"
		ack(m.setPreference(id, p), nil)
		m.refresh()
	case "diff":
		m.mu.Lock()
		run := m.runs[id]
		entry := m.entries[id]
		m.mu.Unlock()
		var diffs map[string]string
		if run != nil {
			run.mu.Lock()
			diffs = map[string]string{}
			for k, d := range run.view.Diffs {
				diffs[k] = d
			}
			run.mu.Unlock()
		} else if view, err := readClaudeTranscript(entry.Path, 100, 8<<20); err == nil {
			diffs = view.Diffs
		}
		text := diffs[str(v, "key")]
		if str(v, "key") == "" {
			var out strings.Builder
			keys := make([]string, 0, len(diffs))
			for k := range diffs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				out.WriteString(diffs[k])
			}
			text = out.String()
		}
		m.emit(c, map[string]any{"t": "diff", "id": id, "text": text, "key": str(v, "key")})
	default:
		ack(errors.New("Unsupported Claude Operation"), nil)
	}
}
func stringArray(value any) []string {
	var out []string
	for _, v := range arrayValue(value) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func oldEffortFor(r *ClaudeRun) string { r.mu.Lock(); defer r.mu.Unlock(); return r.options.Effort }
