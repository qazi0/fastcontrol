package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

type claudePermission struct {
	Tool        string
	Input       map[string]any
	Suggestions any
	At          time.Time
}
type claudeQueuedMessage struct {
	ID, Text string
	Images   []string
	Content  []any
}

type ClaudeRun struct {
	manager                  *ClaudeManager
	id, cwd                  string
	cmd                      *exec.Cmd
	stdin                    io.WriteCloser
	writeMu                  sync.Mutex
	sendMu                   sync.Mutex
	queue                    []claudeQueuedMessage
	interrupted              bool
	mu                       sync.Mutex
	view                     *claudeTranscript
	options                  claudePreference
	controls                 map[string]chan map[string]any
	permissions              map[string]claudePermission
	ready                    chan struct{}
	done                     chan struct{}
	alive, running, streamed bool
	reply                    string
	blocks                   map[int]string
	lastPublished            map[string][]byte
	lastMeta                 []byte
	lastFlush                time.Time
	flushScheduled           bool
	arguments                map[int]string
	blockDefs                map[int]map[string]any
}

func (m *ClaudeManager) start(id, cwd string, p claudePreference, resume bool) (*ClaudeRun, error) {
	if m.bin == "" {
		return nil, errors.New("Install Claude Code On This Mac")
	}
	if p.Permission != "" && !contains([]string{"default", "acceptEdits", "plan", "auto", "bypassPermissions", "dontAsk"}, p.Permission) {
		return nil, errors.New("Unknown Claude Permission Mode")
	}
	if p.Effort != "" && !contains([]string{"low", "medium", "high", "xhigh", "max"}, p.Effort) {
		return nil, errors.New("Unknown Claude Effort")
	}

	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return nil, errors.New("Project Folder Is Unavailable")
	}
	m.mu.Lock()
	if existing := m.runs[id]; existing != nil {
		m.mu.Unlock()
		return existing, nil
	}
	m.mu.Unlock()
	view := newClaudeTranscript()
	view.Meta.Cwd = cwd
	view.Meta.Model = p.Model
	view.Meta.Effort = p.Effort
	view.Meta.Mode = "live"
	if resume {
		m.mu.Lock()
		e := m.entries[id]
		m.mu.Unlock()
		if e.Path != "" {
			if old, err := readClaudeTranscript(e.Path, 15, 2<<20); err == nil {
				view = old
			}
		}
	}
	if p.Permission == "" {
		p.Permission = "default"
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--replay-user-messages", "--permission-prompt-tool", "stdio", "--permission-mode", p.Permission, "--allow-dangerously-skip-permissions"}
	if resume {
		args = append(args, "--resume", id)
	} else {
		args = append(args, "--session-id", id)
	}
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	if p.Effort != "" {
		args = append(args, "--effort", p.Effort)
	}
	cmd := exec.Command(m.bin, args...)
	cmd.Dir = cwd
	prepareProcess(cmd)
	for _, e := range codexEnv() {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	r := &ClaudeRun{manager: m, id: id, cwd: cwd, cmd: cmd, stdin: stdin, view: view, options: p, controls: map[string]chan map[string]any{}, permissions: map[string]claudePermission{}, ready: make(chan struct{}), done: make(chan struct{}), alive: true, blocks: map[int]string{}, lastPublished: map[string][]byte{}, arguments: map[int]string{}, blockDefs: map[int]map[string]any{}}
	m.mu.Lock()
	if m.runs[id] != nil {
		m.mu.Unlock()
		_ = stdin.Close()
		return nil, errors.New("Claude Session Is Already Starting")
	}
	m.runs[id] = r
	m.mu.Unlock()
	if err = cmd.Start(); err != nil {
		_ = stdin.Close()
		m.mu.Lock()
		delete(m.runs, id)
		m.mu.Unlock()
		return nil, err
	}
	go r.read(stdout)
	go func() {
		_, err := r.control(map[string]any{"subtype": "initialize"}, 60*time.Second)
		r.mu.Lock()
		if err != nil {
			r.alive = false
		}
		close(r.ready)
		r.mu.Unlock()
		if err != nil {
			r.kill()
		}
	}()
	return r, nil
}
func (r *ClaudeRun) write(value any) error {
	b := append(mustJSON(value), '\n')
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_, err := r.stdin.Write(b)
	return err
}
func (r *ClaudeRun) control(request map[string]any, timeout time.Duration) (map[string]any, error) {
	id := uuid()
	ch := make(chan map[string]any, 1)
	r.mu.Lock()
	if !r.alive {
		r.mu.Unlock()
		return nil, errors.New("Claude Process Has Stopped")
	}
	r.controls[id] = ch
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.controls, id); r.mu.Unlock() }()
	if err := r.write(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		return nil, err
	}
	select {
	case response := <-ch:
		if str(response, "subtype") == "error" {
			return nil, errors.New(str(response, "error"))
		}
		return mapValue(response["response"]), nil
	case <-r.done:
		return nil, errors.New("Claude Process Has Stopped")
	case <-time.After(timeout):
		return nil, errors.New("Claude Control Request Timed Out")
	}
}
func (r *ClaudeRun) snapshot() (Meta, []Turn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}
func (r *ClaudeRun) snapshotLocked() (Meta, []Turn) {
	meta := r.view.Meta
	meta.Provider = "claude"
	meta.Cwd = r.cwd
	meta.Effort = r.options.Effort
	meta.PermissionMode = r.options.Permission
	meta.Running = r.running
	meta.Mode = "live"
	if !r.alive {
		meta.Mode = "tail"
	}
	meta.Requests = nil
	var ids []string
	for id := range r.permissions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return r.permissions[ids[i]].At.Before(r.permissions[ids[j]].At) })
	for _, id := range ids {
		p := r.permissions[id]
		q := Request{ID: id, Method: "claude", Kind: "command", Title: p.Tool, Body: compactJSON(p.Input, 2000)}
		if p.Tool == "Bash" {
			q.Title = "Run Command?"
			q.Body = str(p.Input, "command")
		}
		if p.Tool == "Edit" || p.Tool == "Write" || p.Tool == "MultiEdit" {
			q.Kind = "file"
			q.Title = "Apply File Changes?"
		}
		if p.Tool == "AskUserQuestion" {
			q.Kind = "question"
			q.Title = "Claude Has a Question"
			for i, raw := range arrayValue(p.Input["questions"]) {
				v := mapValue(raw)
				question := Question{ID: fmt.Sprint(i), Header: str(v, "header"), Question: str(v, "question"), Other: true}
				for _, opt := range arrayValue(v["options"]) {
					question.Options = append(question.Options, str(mapValue(opt), "label"))
					question.Descriptions = append(question.Descriptions, str(mapValue(opt), "description"))
				}
				q.Questions = append(q.Questions, question)
			}
		}
		meta.Requests = append(meta.Requests, q)
	}
	// Marshal while holding the lock so callers get independent slices and maps.
	var turns []Turn
	_ = json.Unmarshal(mustJSON(r.view.Turns), &turns)
	return meta, turns
}
func (r *ClaudeRun) publish() {
	r.mu.Lock()
	if delay := 60*time.Millisecond - time.Since(r.lastFlush); delay > 0 {
		if !r.flushScheduled {
			r.flushScheduled = true
			time.AfterFunc(delay, func() { r.mu.Lock(); r.flushScheduled = false; r.mu.Unlock(); r.publish() })
		}
		r.mu.Unlock()
		return
	}
	r.lastFlush = time.Now()
	meta, turns := r.snapshotLocked()
	var messages []map[string]any
	for _, t := range turns {
		b := mustJSON(t)
		if string(b) != string(r.lastPublished[t.Key]) {
			r.lastPublished[t.Key] = b
			messages = append(messages, map[string]any{"t": "turn", "turn": t})
		}
	}
	b := mustJSON(meta)
	if string(b) != string(r.lastMeta) {
		r.lastMeta = b
		messages = append(messages, map[string]any{"t": "meta", "meta": meta})
	}
	r.mu.Unlock()
	for _, value := range messages {
		r.manager.broadcast(r.id, value)
	}
}
func (r *ClaudeRun) send(text string, images []string) (bool, error) {
	select {
	case <-r.ready:
	case <-r.done:
		return false, errors.New("Claude Could Not Start")
	case <-time.After(60 * time.Second):
		return false, errors.New("Claude Is Still Starting")
	}
	if text == "" && len(images) == 0 {
		return false, errors.New("Message Is Empty")
	}
	if err := checkUploads(images); err != nil {
		return false, err
	}
	content := []any{}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, path := range images {
		b, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		mime := imageMIME(b)
		if mime == "" {
			return false, errors.New("Unsupported Image Format")
		}
		content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mime, "data": base64.StdEncoding.EncodeToString(b)}})
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	r.mu.Lock()
	if !r.alive {
		r.mu.Unlock()
		return false, errors.New("Claude Process Has Stopped")
	}
	if len(r.queue) >= 20 {
		r.mu.Unlock()
		return false, errors.New("Follow-Up Queue Is Full")
	}
	queued := r.running
	r.queue = append(r.queue, claudeQueuedMessage{ID: uuid(), Text: text, Images: images, Content: content})
	r.mu.Unlock()
	return queued, r.dispatchNext()
}

// The caller holds sendMu, including the result reader, so two senders cannot
// publish one turn while writing another turn to the process.
func (r *ClaudeRun) dispatchNext() error {
	r.mu.Lock()
	if r.running || len(r.queue) == 0 || !r.alive {
		r.mu.Unlock()
		return nil
	}
	next := r.queue[0]
	r.queue = r.queue[1:]
	r.running = true
	r.interrupted = false
	if len(r.view.Turns) >= 1000 {
		for _, turn := range r.view.Turns[:len(r.view.Turns)-999] {
			delete(r.view.Diffs, turn.Key)
			delete(r.lastPublished, turn.Key)
		}
		r.view.Turns = r.view.Turns[len(r.view.Turns)-999:]
		r.view.Meta.HasOlder = true
		r.view.reindex()
	}
	r.view.ensureTurn(next.ID, time.Now().UnixMilli())
	r.view.current().Status = "inProgress"
	r.view.Meta.TurnAt = time.Now().UnixMilli()
	r.streamed = false
	r.view.upsert(Item{ID: next.ID, K: "user", Text: next.Text, Images: next.Images})
	if r.view.Meta.Title == "" {
		r.view.Meta.Title = clipPlain(next.Text, 120)
	}
	r.mu.Unlock()
	err := r.write(map[string]any{"type": "user", "uuid": next.ID, "session_id": r.id, "message": map[string]any{"role": "user", "content": next.Content}, "parent_tool_use_id": nil})
	if err != nil {
		r.mu.Lock()
		r.running = false
		r.view.current().Status = "failed"
		r.view.current().Error = "Could Not Send To Claude"
		r.mu.Unlock()
	}
	r.publish()
	if err == nil {
		r.manager.event(r.id, "started", clipPlain(next.Text, 120), "")
	}
	return err
}
func (r *ClaudeRun) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var value map[string]any
		if json.Unmarshal(scanner.Bytes(), &value) == nil {
			r.consume(value)
		}
	}
	if scanner.Err() != nil {
		r.kill()
	}
	_ = r.cmd.Wait()
	r.mu.Lock()
	wasRunning := r.running
	r.alive = false
	r.running = false
	r.permissions = map[string]claudePermission{}
	if wasRunning {
		r.view.current().Status = "failed"
		r.view.current().Error = "Claude Process Stopped"
	}
	r.mu.Unlock()
	r.publish()
	r.manager.mu.Lock()
	if r.manager.runs[r.id] == r {
		delete(r.manager.runs, r.id)
	}
	r.manager.mu.Unlock()
	close(r.done)
}
func (r *ClaudeRun) consume(m map[string]any) {
	if str(m, "type") == "result" {
		r.sendMu.Lock()
		defer r.sendMu.Unlock()
		defer func() { _ = r.dispatchNext() }()
	}
	switch str(m, "type") {
	case "control_response":
		response := mapValue(m["response"])
		r.mu.Lock()
		ch := r.controls[str(response, "request_id")]
		r.mu.Unlock()
		if ch != nil {
			select {
			case ch <- response:
			default:
			}
		}
		payload := mapValue(response["response"])
		if models := arrayValue(payload["models"]); len(models) > 0 {
			r.manager.setModels(models)
		}
		return
	case "control_request":
		request := mapValue(m["request"])
		if str(request, "subtype") == "can_use_tool" {
			r.mu.Lock()
			r.permissions[str(m, "request_id")] = claudePermission{Tool: str(request, "tool_name"), Input: mapValue(request["input"]), Suggestions: request["permission_suggestions"], At: time.Now()}
			r.mu.Unlock()
			r.publish()
			r.manager.event(r.id, "attention", r.id, "Approval Required")
		} else {
			_ = r.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": str(m, "request_id"), "error": "Unsupported Relay Control Request"}})
		}

		return
	case "control_cancel_request":
		r.mu.Lock()
		delete(r.permissions, str(m, "request_id"))
		r.mu.Unlock()
		r.publish()
		return
	}
	r.mu.Lock()
	switch str(m, "type") {
	case "system":
		if str(m, "subtype") == "init" {
			r.view.Meta.Model = str(m, "model")
			if mode := str(m, "permissionMode"); mode != "" {
				r.options.Permission = mode
			}
		}
	case "stream_event":
		e := mapValue(m["event"])
		index, _ := num(e, "index")
		switch str(e, "type") {
		case "message_start":
			r.reply = str(mapValue(e["message"]), "id")
			if model := str(mapValue(e["message"]), "model"); model != "" {
				r.view.Meta.Model = model
			}
			r.blocks = map[int]string{}
			r.arguments = map[int]string{}
			r.blockDefs = map[int]map[string]any{}
		case "content_block_start":
			id := fmt.Sprintf("%s:%d", r.reply, index)
			block := mapValue(e["content_block"])
			r.blockDefs[int(index)] = block
			it := claudeBlock(block, id)
			if it.K != "" {
				r.view.upsert(it)
				r.blocks[int(index)] = it.ID
			}
			r.streamed = true
		case "content_block_delta":
			id := r.blocks[int(index)]
			delta := mapValue(e["delta"])
			if str(delta, "type") == "input_json_delta" {
				r.arguments[int(index)] += str(delta, "partial_json")
				var input map[string]any
				if json.Unmarshal([]byte(r.arguments[int(index)]), &input) == nil {
					block := r.blockDefs[int(index)]
					block["input"] = input
					r.view.upsert(claudeBlock(block, id))
				}
			}
			t := r.view.current()
			for i := range t.Items {
				if t.Items[i].ID == id {
					if str(delta, "type") == "text_delta" {
						t.Items[i].Text += str(delta, "text")
					}
					if str(delta, "type") == "thinking_delta" {
						t.Items[i].Text += str(delta, "thinking")
					}
				}
			}
		}
	case "assistant":
		r.view.apply(m)
	case "user":
		message := mapValue(m["message"])
		content := arrayValue(message["content"])
		toolResult := false
		for _, b := range content {
			if str(mapValue(b), "type") == "tool_result" {
				toolResult = true
			}
		}
		if toolResult {
			r.view.apply(m)
		}
	case "result":
		r.running = false
		r.permissions = map[string]claudePermission{}
		t := r.view.current()
		t.Status = "completed"
		t.DurationMs, _ = num(m, "duration_ms")
		if bad, _ := m["is_error"].(bool); bad {
			t.Status = "failed"
			t.Error = clipPlain(str(m, "result"), 200)
		}
		if r.interrupted {
			t.Status = "interrupted"
			t.Error = ""
		}
		if !r.streamed && !r.interrupted && str(m, "result") != "" {
			r.view.upsert(Item{ID: uuid(), K: "assistant", Text: str(m, "result")})
		}
		for _, usage := range mapValue(m["modelUsage"]) {
			if n, ok := num(mapValue(usage), "contextWindow"); ok {
				r.view.Meta.CtxMax = n
			}
		}
	}
	meta, _ := r.snapshotLocked()
	kind := ""
	if str(m, "type") == "result" {
		kind = "completed"
		if bad, _ := m["is_error"].(bool); bad && !r.interrupted {
			kind = "failed"
		}
	}
	r.mu.Unlock()
	r.publish()
	if kind != "" {
		r.manager.event(r.id, kind, meta.Title, clipPlain(str(m, "result"), 160))
	}
}
func (r *ClaudeRun) approve(id, decision string, answers map[string][]string) error {
	r.mu.Lock()
	permission, ok := r.permissions[id]
	if ok && answers != nil {
		if permission.Tool != "AskUserQuestion" {
			r.mu.Unlock()
			return errors.New("Request Is Not A Question")
		}
		questions := arrayValue(permission.Input["questions"])
		if len(answers) != len(questions) {
			r.mu.Unlock()
			return errors.New("Answer Every Question")
		}
		for i := range questions {
			if len(answers[fmt.Sprint(i)]) == 0 {
				r.mu.Unlock()
				return errors.New("Answer Every Question")
			}
		}
	}
	if ok {
		delete(r.permissions, id)
	}
	r.mu.Unlock()
	if !ok {
		return errors.New("Request Is No Longer Pending")
	}
	response := map[string]any{"behavior": "deny", "message": "Declined By The User"}
	if decision == "accept" || decision == "acceptForSession" || answers != nil {
		input := map[string]any{}
		_ = json.Unmarshal(mustJSON(permission.Input), &input)
		if answers != nil {
			values := map[string]string{}
			for i, q := range arrayValue(input["questions"]) {
				values[str(mapValue(q), "question")] = strings.Join(answers[fmt.Sprint(i)], ", ")
			}
			input["answers"] = values
		}
		response = map[string]any{"behavior": "allow", "updatedInput": input}
		if decision == "acceptForSession" {
			suggestions := []any{}
			for _, raw := range arrayValue(permission.Suggestions) {
				suggestion := mapValue(raw)
				clone := map[string]any{}
				for key, value := range suggestion {
					clone[key] = value
				}
				clone["destination"] = "session"
				suggestions = append(suggestions, clone)
			}
			response["updatedPermissions"] = suggestions
		}
	}
	err := r.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": response}})
	r.publish()
	return err
}
func (r *ClaudeRun) kill() {
	if r.cmd.Process != nil {
		terminateProcess(r.cmd.Process)
	}
}
func (m *ClaudeManager) setModels(raw []any) {
	var models []Model
	for _, v := range raw {
		x := mapValue(v)
		efforts := []string{}
		for _, e := range arrayValue(x["supportedEffortLevels"]) {
			if s, ok := e.(string); ok {
				efforts = append(efforts, s)
			}
		}
		if len(efforts) == 0 {
			efforts = []string{"low"}
		}
		models = append(models, Model{Slug: str(x, "value"), Resolved: str(x, "resolvedModel"), Name: str(x, "displayName"), Description: str(x, "description"), DefaultEffort: "low", Efforts: efforts})
	}
	m.mu.Lock()
	m.models = models
	m.mu.Unlock()
}
