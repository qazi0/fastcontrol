package main

// New threads. The desktop's IPC cannot start a thread (codex://threads/new
// only pre-fills its composer), so the first turn runs headless through
// `codex exec --json`. The relay reads the thread id from the first event,
// tails the rollout like any other thread, and hands follow-up messages to the
// desktop app once the exec run exits.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type queuedMsg struct {
	text   string
	images []string
}

type execRun struct {
	id     string
	cmd    *exec.Cmd
	prompt string

	mu     sync.Mutex
	done   bool
	queued []queuedMsg
}

func (x *execRun) running() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return !x.done
}

// queue holds a message until the exec run exits. It returns false when the
// run has already finished, so the caller should send normally.
func (x *execRun) queue(text string, images []string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.done {
		return false
	}
	x.queued = append(x.queued, queuedMsg{text, images})
	return true
}

// stop interrupts the run like Ctrl-C, then kills it if it lingers.
func (x *execRun) stop() {
	x.mu.Lock()
	x.queued = nil
	x.mu.Unlock()
	if p := x.cmd.Process; p != nil {
		_ = syscall.Kill(-p.Pid, syscall.SIGINT)
		go func() {
			time.Sleep(5 * time.Second)
			if x.running() {
				_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
			}
		}()
	}
}

func (h *Hub) execFor(id string) *execRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.execs[id]
}

// newThread starts `codex exec` in cwd and returns the new thread id once the
// CLI reports it.
func (h *Hub) newThread(cwd, text, model, effort string, images []string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("empty prompt")
	}
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	if strings.HasPrefix(cwd, "~/") {
		home, _ := os.UserHomeDir()
		cwd = filepath.Join(home, cwd[2:])
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return "", fmt.Errorf("folder %s does not exist", cwd)
	}
	if err := checkUploads(images); err != nil {
		return "", err
	}
	bin, err := codexBin()
	if err != nil {
		return "", err
	}
	args := []string{"exec", "--json", "--skip-git-repo-check", "-C", cwd}
	if model != "" {
		args = append(args, "-m", model)
	}
	if effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", effort))
	}
	for _, img := range images {
		args = append(args, "-i", img)
	}
	args = append(args, "--", text)

	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Env = codexEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	log.Printf("exec: started codex exec in %s (pid %d)", cwd, cmd.Process.Pid)

	idc := make(chan string, 1)
	failc := make(chan string, 1)
	x := &execRun{cmd: cmd, prompt: text}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		sent := false
		for sc.Scan() {
			var ev struct {
				Type     string `json:"type"`
				ThreadID string `json:"thread_id"`
				Message  string `json:"message"`
				Error    *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue
			}
			switch {
			case ev.Type == "thread.started" && !sent:
				sent = true
				idc <- ev.ThreadID
			case ev.Type == "turn.failed" && ev.Error != nil:
				select {
				case failc <- errorText(ev.Error.Message):
				default:
				}
			}
		}
		err := cmd.Wait()
		if !sent {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" && err != nil {
				msg = err.Error()
			}
			select {
			case failc <- "codex exec exited: " + clipPlain(msg, 400):
			default:
			}
			close(idc)
			return
		}
		h.execDone(x)
	}()

	select {
	case id, ok := <-idc:
		if !ok {
			return "", errors.New(<-failc)
		}
		x.mu.Lock()
		x.id = id
		x.mu.Unlock()
		h.mu.Lock()
		h.execs[id] = x
		h.mu.Unlock()
		log.Printf("exec: thread %s", id)
		// The rollout appears a moment after thread.started; wait so the
		// phone can open the thread as soon as it gets the id.
		for i := 0; i < 80 && findRollout(id) == ""; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		go h.refreshList()
		return id, nil
	case <-time.After(45 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return "", errors.New("codex exec did not start a thread within 45 s")
	}
}

// execDone runs after the CLI exits: refresh the thread and hand any queued
// follow-ups to the desktop app.
func (h *Hub) execDone(x *execRun) {
	x.mu.Lock()
	x.done = true
	q := x.queued
	x.queued = nil
	id := x.id
	x.mu.Unlock()
	log.Printf("exec: thread %s finished", id)
	// Give the rollout and database a moment to settle before the list refresh.
	time.Sleep(300 * time.Millisecond)
	h.refreshList()
	h.mu.Lock()
	s := h.sessions[id]
	h.mu.Unlock()
	if s != nil {
		s.cmds <- func() { s.sendMeta() }
	}
	if len(q) == 0 {
		go func() {
			time.Sleep(10 * time.Minute)
			h.mu.Lock()
			if h.execs[id] == x {
				delete(h.execs, id)
			}
			h.mu.Unlock()
		}()
		return
	}
	var texts []string
	var images []string
	for _, m := range q {
		if m.text != "" {
			texts = append(texts, m.text)
		}
		images = append(images, m.images...)
	}
	if s == nil {
		s = h.session(id, true)
	}
	if s == nil {
		log.Printf("exec: thread %s: cannot deliver %d queued messages", id, len(q))
		return
	}
	if err := s.sendMessage(strings.Join(texts, "\n\n"), images); err != nil {
		log.Printf("exec: thread %s: queued message: %v", id, err)
		s.broadcast(mustJSON(map[string]any{"t": "ack", "op": "send", "id": id, "ok": false, "msg": "Queued message failed: " + err.Error()}))
	}
}

// tailBuffer keeps the last 4 KB written to it (stderr of a child process).
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
