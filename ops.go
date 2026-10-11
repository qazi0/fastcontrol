package main

// Thread control ops beyond send/stop/approve: Load Earlier, diffs, settings,
// compaction, question answers, and the codex CLI for archiving.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const moreTurns = 15

// more widens the window by moreTurns and re-sends the thread. In live mode
// the desktop only keeps recent turns in full, so it is asked to load the
// complete history, which arrives as a fresh snapshot.
func (s *Session) more() {
	s.cmds <- func() {
		s.window += moreTurns
		s.moreAt = time.Now()
		if s.state != nil {
			keys := desktopTurnOrder(s.state)
			needHistory := desktopMeta(s.state, s.title).HasOlder
			if !needHistory {
				ents := desktopEntities(s.state)
				start := len(keys) - s.window
				if start < 0 {
					start = 0
				}
				for _, k := range keys[start:] {
					t, _ := ents[k].(map[string]any)
					if items, _ := t["items"].([]any); len(items) == 0 {
						needHistory = true
						break
					}
				}
			}
			if needHistory && s.owner != "" {
				owner := s.owner
				go func() {
					_, err := s.hub.ipc.Request("thread-follower-load-complete-history", map[string]any{"conversationId": s.id}, owner, ipcTimeout, nil)
					if err != nil {
						s.broadcast(mustJSON(map[string]any{"t": "ack", "op": "more", "id": s.id, "ok": false, "msg": err.Error()}))
					}
				}()
			}
		} else if s.rv != nil {
			s.rv.grow(s.window)
		}
		s.lastOpen = s.openMessage()
		s.broadcast(s.lastOpen)
	}
}

// diff replies with the unified diff of one turn, or of the latest turn that
// changed files when key is empty.
func (s *Session) diff(c *Client, key string) {
	s.cmds <- func() {
		var d string
		if s.state != nil {
			keys := desktopTurnOrder(s.state)
			if key == "" {
				for i := len(keys) - 1; i >= 0 && d == ""; i-- {
					if d = desktopTurnDiff(s.state, keys[i]); d != "" {
						key = keys[i]
					}
				}
			} else {
				d = desktopTurnDiff(s.state, key)
			}
		} else if s.rv != nil {
			if key == "" {
				for i := len(s.rv.turns) - 1; i >= 0 && d == ""; i-- {
					if d = s.rv.diff(s.rv.turns[i].Key); d != "" {
						key = s.rv.turns[i].Key
					}
				}
			} else {
				d = s.rv.diff(key)
			}
		}
		c.push(mustJSON(map[string]any{"t": "diff", "id": s.id, "key": key, "diff": clipHead(d, 2<<20)}))
	}
}

func (s *Session) liveOwner() (liveInfo, error) {
	li, ok := s.snapshotLive()
	if !ok {
		return s.ensureLive(12 * time.Second)
	}
	return li, nil
}

func (s *Session) compact() error {
	li, err := s.liveOwner()
	if err != nil {
		return err
	}
	_, err = s.hub.ipc.Request("thread-follower-compact-thread", map[string]any{"conversationId": s.id}, li.owner, ipcTimeout, nil)
	return err
}

func requestID(id string) any {
	if n, err := strconv.ParseInt(id, 10, 64); err == nil {
		return n
	}
	return id
}

// answer submits replies to a request_user_input question set.
func (s *Session) answer(reqID string, answers map[string][]string) error {
	li, ok := s.snapshotLive()
	if !ok {
		return errors.New("thread is not open in the desktop app")
	}
	out := map[string]any{}
	for q, a := range answers {
		if a == nil {
			a = []string{}
		}
		out[q] = map[string]any{"answers": a}
	}
	_, err := s.hub.ipc.Request("thread-follower-submit-user-input", map[string]any{
		"conversationId": s.id,
		"requestId":      requestID(reqID),
		"response":       map[string]any{"answers": out},
	}, li.owner, ipcTimeout, nil)
	return err
}

var (
	approvalPolicies = map[string]bool{"never": true, "on-request": true, "untrusted": true}
	sandboxModes     = map[string]bool{"dangerFullAccess": true, "workspaceWrite": true, "readOnly": true}
)

// updateSettings changes model, effort, approvals or sandbox for the next turn.
// Nil fields are left unchanged.
func (s *Session) updateSettings(model, effort, approval, sandbox, reviewer *string) error {
	ts := map[string]any{}
	if model != nil && *model != "" {
		ts["model"] = *model
	}
	if effort != nil && *effort != "" {
		ts["effort"] = *effort
	}
	if approval != nil && *approval != "" {
		if !approvalPolicies[*approval] {
			return fmt.Errorf("unknown approval policy %q", *approval)
		}
		ts["approvalPolicy"] = *approval
	}
	if sandbox != nil && *sandbox != "" {
		if !sandboxModes[*sandbox] {
			return fmt.Errorf("unknown sandbox %q", *sandbox)
		}
		ts["sandboxPolicy"] = map[string]any{"type": *sandbox}
	}
	if reviewer != nil && *reviewer != "" {
		if *reviewer != "user" && *reviewer != "auto_review" {
			return fmt.Errorf("unknown reviewer %q", *reviewer)
		}
		ts["approvalsReviewer"] = *reviewer
	}
	if len(ts) == 0 {
		return errors.New("nothing to change")
	}
	li, err := s.liveOwner()
	if err != nil {
		return err
	}
	r, err := s.hub.ipc.Request("thread-follower-update-thread-settings", map[string]any{"conversationId": s.id, "threadSettings": ts}, li.owner, ipcTimeout, nil)
	if err != nil {
		return err
	}
	if strings.Contains(string(r.Result), `"applied":false`) {
		return errors.New("the desktop app did not apply the change")
	}
	return nil
}

// ---- codex CLI ----

// codexBin prefers the CLI bundled with the desktop app, which matches its
// models and protocol. LaunchAgents have a minimal PATH, so look in fixed places.
func codexBin() (string, error) {
	if runtime.GOOS == "windows" {
		return windowsCodexBin()
	}

	for _, p := range []string{
		"/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex",
		"/Applications/Codex.app/Contents/Resources/codex-cli/bin/codex",
		"/opt/homebrew/bin/codex",
		"/usr/local/bin/codex",
		filepath.Join(userHome(), ".local", "bin", "codex"),
	} {
		if speechExecutable(p) {
			return p, nil
		}
	}
	if p, err := exec.LookPath("codex"); err == nil {
		return p, nil
	}
	return "", errors.New("codex CLI not found")
}

func userHome() string { home, _ := os.UserHomeDir(); return home }

func codexEnv() []string {
	env := os.Environ()
	if runtime.GOOS == "windows" {
		return env
	}
	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			env[i] = e + ":" + filepath.Join(userHome(), ".local", "bin") + ":/opt/homebrew/bin:/usr/local/bin"
			return env
		}
	}
	return append(env, "PATH=/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin")
}

// runCodex runs `codex <verb> <thread id>` (archive, unarchive).
func runCodex(verb, id string) error {
	if id == "" {
		return errors.New("missing thread id")
	}
	bin, err := codexBin()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, verb, id)
	cmd.Env = codexEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The desktop app holds a writer lock on threads it has loaded and
		// archives them only through its own app server.
		home, _ := os.UserHomeDir()
		if _, lerr := os.Stat(filepath.Join(home, ".codex", "thread-writer-locks", id+".lock")); lerr == nil {
			return errors.New("Archive This In The Desktop App, Which Has It Open")
		}
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("codex %s: %s", verb, clipPlain(msg, 300))
	}
	return nil
}

// defaultModel reads the top-level model settings from ~/.codex/config.toml.
func defaultModel() map[string]string {
	f, err := os.Open(filepath.Join(codexHome(), "config.toml"))
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			break // only top-level keys
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`)
		switch k {
		case "model":
			out["model"] = v
		case "model_reasoning_effort":
			out["effort"] = v
		case "approval_policy":
			out["approval"] = v
		case "sandbox_mode":
			out["sandbox"] = v
		}
	}
	return out
}

// ---- uploads ----

func uploadDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fastcontrol", "uploads")
}

// checkUploads makes sure image paths point into the upload folder, so a
// client cannot attach arbitrary files from the Mac.
func checkUploads(paths []string) error {
	dir := uploadDir()
	for _, p := range paths {
		c := filepath.Clean(p)
		if filepath.Dir(c) != dir {
			return fmt.Errorf("image %q is not an upload", p)
		}
		if _, err := os.Stat(c); err != nil {
			return fmt.Errorf("image %q: %v", filepath.Base(p), err)
		}
	}
	return nil
}
