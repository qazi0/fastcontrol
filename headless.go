package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// HeadlessCodex controls Linux sessions through the installed Codex app-server.
// mu guards thread state and pending RPCs; writeMu serializes transport writes.
// The reader publishes immutable JSON to each Session's existing command queue.
// opMu orders start/resume and user controls without blocking the reader.
type HeadlessCodex struct {
	hub                        *Hub
	opMu, startMu, writeMu, mu sync.Mutex
	write                      func([]byte) error
	pending                    map[string]chan rpcResult
	threads                    map[string]map[string]any
	settings                   map[string]map[string]any
	requests                   map[string]map[string]any
	sequence                   uint64
}
type rpcResult struct {
	value map[string]any
	err   error
}

func newHeadlessCodex(h *Hub) *HeadlessCodex {
	return &HeadlessCodex{hub: h, pending: map[string]chan rpcResult{}, threads: map[string]map[string]any{}, settings: map[string]map[string]any{}, requests: map[string]map[string]any{}}
}
func (a *HeadlessCodex) start() error {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	a.mu.Lock()
	running := a.write != nil
	a.mu.Unlock()
	if running {
		return nil
	}
	// Reuse a shared daemon when one exists. Never start or stop the user's daemon.
	socket := filepath.Join(codexHome(), "app-server-control", "app-server-control.sock")
	var read func() ([]byte, error)
	var closeTransport func()
	var write func([]byte) error
	if st, err := os.Stat(socket); err == nil && st.Mode()&os.ModeSocket != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
		connection, _, err := websocket.Dial(ctx, "ws://localhost/rpc", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
		if err != nil {
			return fmt.Errorf("Connect To The Codex Daemon: %w", err)
		}
		connection.SetReadLimit(32 << 20)
		read = func() ([]byte, error) { _, data, err := connection.Read(context.Background()); return data, err }
		write = func(data []byte) error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return connection.Write(ctx, websocket.MessageText, data)
		}
		closeTransport = func() { _ = connection.CloseNow(); transport.CloseIdleConnections() }
	} else {
		binary, err := codexBin()
		if err != nil {
			return err
		}
		cmd := exec.Command(binary, "app-server", "--listen", "stdio://")
		cmd.Env = codexEnv()
		prepareProcess(cmd)
		input, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			_ = input.Close()
			return err
		}
		var stderr tailBuffer
		cmd.Stderr = &stderr
		if err = cmd.Start(); err != nil {
			_ = input.Close()
			_ = output.Close()
			return err
		}
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 64<<10), 32<<20)
		read = func() ([]byte, error) {
			if scanner.Scan() {
				return append([]byte(nil), scanner.Bytes()...), nil
			}
			if err := scanner.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		write = func(data []byte) error { _, err := input.Write(append(data, '\n')); return err }
		closeTransport = func() { _ = input.Close(); killProcess(cmd.Process); _ = cmd.Wait() }
	}
	closeOnce := sync.Once{}
	closeConnection := func() { closeOnce.Do(closeTransport) }
	a.mu.Lock()
	a.write = write
	a.mu.Unlock()
	go func() {
		defer closeConnection()
		for {
			data, err := read()
			if err != nil {
				break
			}
			a.receive(data)
		}
		a.mu.Lock()
		a.write = nil
		for id, ch := range a.pending {
			ch <- rpcResult{err: errors.New("Codex Connection Closed. Retry The Message")}
			delete(a.pending, id)
		}
		for id, t := range a.threads {
			turns, _ := t["turns"].([]any)
			for _, raw := range turns {
				turn, _ := raw.(map[string]any)
				if str(turn, "status") == "inProgress" {
					turn["status"] = "interrupted"
				}
			}
			t["status"] = map[string]any{"type": "notLoaded"}
			a.publishLocked(id)
		}
		a.threads = map[string]map[string]any{}
		a.requests = map[string]map[string]any{}
		a.mu.Unlock()
	}()
	_, err := a.call("initialize", map[string]any{"clientInfo": map[string]any{"name": "fastcontrol", "title": "fastcontrol", "version": "0.1.8"}, "capabilities": map[string]any{"experimentalApi": true}})
	if err == nil {
		err = a.send(map[string]any{"method": "initialized"})
	}
	if err != nil {
		closeConnection()
		return err
	}
	return nil
}
func (a *HeadlessCodex) send(value any) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.mu.Lock()
	write := a.write
	a.mu.Unlock()
	if write == nil {
		return errors.New("Codex Is Disconnected")
	}
	return write(mustJSON(value))
}
func (a *HeadlessCodex) call(method string, params any) (map[string]any, error) {
	a.mu.Lock()
	a.sequence++
	id := fmt.Sprintf("fastcontrol-%d", a.sequence)
	ch := make(chan rpcResult, 1)
	a.pending[id] = ch
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.pending, id); a.mu.Unlock() }()
	if err := a.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case response := <-ch:
		return response.value, response.err
	case <-time.After(30 * time.Second):
		return nil, errors.New("Codex Request Timed Out")
	}
}
func (a *HeadlessCodex) receive(data []byte) {
	var f struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params map[string]any  `json:"params"`
		Result map[string]any  `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &f) != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(f.ID) > 0 && f.Method == "" {
		if thread, ok := f.Result["thread"].(map[string]any); ok {
			id := str(thread, "id")
			a.threads[id] = thread
			a.settings[id] = map[string]any{"model": f.Result["model"], "effort": f.Result["reasoningEffort"], "approvalPolicy": f.Result["approvalPolicy"], "approvalsReviewer": f.Result["approvalsReviewer"], "sandboxPolicy": f.Result["sandbox"]}
			a.publishLocked(id)
		}
		var id string
		_ = json.Unmarshal(f.ID, &id)
		if ch := a.pending[id]; ch != nil {
			response := rpcResult{value: f.Result}
			if f.Error != nil {
				response.err = errors.New(f.Error.Message)
			}
			select {
			case ch <- response:
			default:
			}
		}
		return
	}
	p := f.Params
	id := str(p, "threadId")
	if f.Method == "thread/started" {
		if t, ok := p["thread"].(map[string]any); ok {
			id = str(t, "id")
			a.threads[id] = t
		}
	}
	t := a.threads[id]
	if t == nil {
		return
	}
	if len(f.ID) > 0 {
		key := string(f.ID)
		a.requests[key] = map[string]any{"id": json.RawMessage(append([]byte(nil), f.ID...)), "method": f.Method, "params": p}
	} else {
		switch f.Method {
		case "turn/started", "turn/completed":
			if turn, ok := p["turn"].(map[string]any); ok {
				old := headlessTurn(t, str(turn, "id"))
				if old != nil && lenAny(turn["items"]) == 0 {
					turn["items"] = old["items"]
				}
				putHeadlessTurn(t, turn)
			}
		case "item/started", "item/completed":
			if turn := headlessTurn(t, str(p, "turnId")); turn != nil {
				if item, ok := p["item"].(map[string]any); ok {
					putHeadlessItem(turn, item)
				}
			}
		case "item/agentMessage/delta", "item/commandExecution/outputDelta", "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded":
			if turn := headlessTurn(t, str(p, "turnId")); turn != nil {
				for _, raw := range anySlice(turn["items"]) {
					item, _ := raw.(map[string]any)
					if str(item, "id") != str(p, "itemId") {
						continue
					}
					switch f.Method {
					case "item/agentMessage/delta":
						item["text"] = str(item, "text") + str(p, "delta")
					case "item/commandExecution/outputDelta":
						item["aggregatedOutput"] = clipTail(str(item, "aggregatedOutput")+str(p, "delta"), maxOutput)
					default:
						index, _ := num(p, "summaryIndex")
						if index < 0 || index > 100 {
							break
						}
						parts := anySlice(item["summary"])
						for len(parts) <= int(index) {
							parts = append(parts, "")
						}
						parts[index] = fmt.Sprint(parts[index]) + str(p, "delta")
						item["summary"] = parts
					}
				}
			}
		case "turn/diff/updated":
			if turn := headlessTurn(t, str(p, "turnId")); turn != nil {
				turn["diff"] = clipHead(str(p, "diff"), 2<<20)
			}
		case "thread/tokenUsage/updated":
			t["tokenUsage"] = p["tokenUsage"]
		case "thread/status/changed":
			t["status"] = p["status"]
		case "thread/name/updated":
			t["name"] = p["threadName"]
		case "serverRequest/resolved":
			delete(a.requests, string(mustJSON(p["requestId"])))
		}
	}
	a.publishLocked(id)
}
func anySlice(value any) []any { result, _ := value.([]any); return result }
func lenAny(value any) int     { return len(anySlice(value)) }
func headlessTurn(thread map[string]any, id string) map[string]any {
	for _, raw := range anySlice(thread["turns"]) {
		turn, _ := raw.(map[string]any)
		if str(turn, "id") == id {
			return turn
		}
	}
	return nil
}
func putHeadlessTurn(thread, turn map[string]any) {
	turns := anySlice(thread["turns"])
	for i, raw := range turns {
		previous, _ := raw.(map[string]any)
		if str(previous, "id") == str(turn, "id") {
			turns[i] = turn
			thread["turns"] = turns
			return
		}
	}
	thread["turns"] = append(turns, turn)
}
func putHeadlessItem(turn, item map[string]any) {
	items := anySlice(turn["items"])
	for i, raw := range items {
		previous, _ := raw.(map[string]any)
		if str(previous, "id") == str(item, "id") {
			items[i] = item
			turn["items"] = items
			return
		}
	}
	turn["items"] = append(items, item)
}
func (a *HeadlessCodex) stateLocked(id string) []byte {
	t := a.threads[id]
	if t == nil {
		return nil
	}
	entities := map[string]any{}
	entries := []any{}
	for _, raw := range anySlice(t["turns"]) {
		turn, _ := raw.(map[string]any)
		if turn == nil {
			continue
		}
		key := str(turn, "id")
		copyTurn := map[string]any{}
		for k, v := range turn {
			copyTurn[k] = v
		}
		copyTurn["turnId"] = key
		if start, ok := num(turn, "startedAt"); ok {
			copyTurn["turnStartedAtMs"] = start * 1000
		}
		entities[key] = copyTurn
		entries = append(entries, map[string]any{"key": key})
	}
	requests := []any{}
	for _, request := range a.requests {
		p, _ := request["params"].(map[string]any)
		if str(p, "threadId") == id {
			requests = append(requests, request)
		}
	}
	return mustJSON(map[string]any{"title": firstNonEmpty(str(t, "name"), str(t, "preview"), "New Chat"), "cwd": t["cwd"], "latestModel": t["model"], "latestReasoningEffort": t["reasoningEffort"], "latestThreadSettings": a.settings[id], "latestTokenUsageInfo": t["tokenUsage"], "gitInfo": t["gitInfo"], "threadRuntimeStatus": t["status"], "requests": requests, "turnHistory": map[string]any{"history": map[string]any{"entitiesByKey": entities, "islands": []any{map[string]any{"entries": entries}}}}})
}
func (a *HeadlessCodex) publishLocked(id string) {
	a.hub.mu.Lock()
	s := a.hub.sessions[id]
	a.hub.mu.Unlock()
	if s != nil {
		s.applyHeadless(a.stateLocked(id))
	}
}
func (a *HeadlessCodex) attach(s *Session) {
	a.mu.Lock()
	data := a.stateLocked(s.id)
	a.mu.Unlock()
	if data != nil {
		s.applyHeadless(data)
	}
}
func (s *Session) applyHeadless(data []byte) {
	if data == nil {
		return
	}
	select {
	case s.cmds <- func() {
		var state map[string]any
		if json.Unmarshal(data, &state) != nil {
			return
		}
		old := s.state
		s.state = state
		s.owner = "fastcontrol-app-server"
		s.setLive(true)
		if old == nil {
			s.clearDirty()
			s.lastOpen = s.openMessage()
			s.broadcast(s.lastOpen)
			return
		}
		if s.dirtyT == nil {
			s.clearDirty()
		}
		oldKeys, keys := desktopTurnOrder(old), desktopTurnOrder(state)
		if !bytes.Equal(mustJSON(oldKeys), mustJSON(keys)) {
			s.dirtyOrd = true
		}
		previous, current := desktopEntities(old), desktopEntities(state)
		for _, key := range keys {
			if bytes.Equal(mustJSON(previous[key]), mustJSON(current[key])) {
				continue
			}
			before, _ := previous[key].(map[string]any)
			after, _ := current[key].(map[string]any)
			oldItems, newItems := anySlice(before["items"]), anySlice(after["items"])
			if before == nil || len(oldItems) != len(newItems) {
				s.dirtyT[key] = true
				continue
			}
			s.dirtyH[key] = true
			for i, item := range newItems {
				if !bytes.Equal(mustJSON(oldItems[i]), mustJSON(item)) {
					if s.dirtyI[key] == nil {
						s.dirtyI[key] = map[int]bool{}
					}
					s.dirtyI[key][i] = true
				}
			}
		}
		s.dirtyMeta = true
	}:
	default:
		s.requestResync()
	}
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func codexHome() string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(userHome(), ".codex")
}

func (a *HeadlessCodex) remember(result map[string]any) (string, error) {
	thread, ok := result["thread"].(map[string]any)
	if !ok || str(thread, "id") == "" {
		return "", errors.New("Codex Returned No Thread")
	}
	id := str(thread, "id")
	a.mu.Lock()
	if a.threads[id] == nil {
		a.threads[id] = thread
	}
	a.publishLocked(id)
	a.mu.Unlock()
	return id, nil
}
func (a *HeadlessCodex) resume(id string) error {
	a.mu.Lock()
	loaded := a.threads[id] != nil
	if status, ok := a.threads[id]["status"].(map[string]any); ok && str(status, "type") == "notLoaded" {
		loaded = false
	}
	a.mu.Unlock()
	if loaded {
		return nil
	}
	result, err := a.call("thread/resume", map[string]any{"threadId": id})
	if err != nil {
		return err
	}
	_, err = a.remember(result)
	return err
}
func (a *HeadlessCodex) listThreads() ([]ThreadInfo, error) {
	if _, err := codexBin(); err != nil {
		return []ThreadInfo{}, nil
	}
	if err := a.start(); err != nil {
		return nil, err
	}
	result, err := a.call("thread/list", map[string]any{"limit": 400, "useStateDbOnly": true, "sourceKinds": []string{}, "sortKey": "updated_at"})
	if err != nil {
		return nil, err
	}
	list := []ThreadInfo{}
	seen := map[string]bool{}
	for _, raw := range anySlice(result["data"]) {
		t, _ := raw.(map[string]any)
		id := str(t, "id")
		if id == "" {
			continue
		}
		updated, _ := num(t, "updatedAt")
		title := firstNonEmpty(str(t, "name"), str(t, "preview"), "New Chat")
		info := ThreadInfo{ID: id, Title: clipPlain(title, 100), Preview: clipPlain(str(t, "preview"), 160), Cwd: str(t, "cwd"), Rollout: str(t, "path"), Source: "codex", Recency: updated * 1000, Model: str(t, "model"), Effort: str(t, "reasoningEffort")}
		status, _ := t["status"].(map[string]any)
		info.Running = str(status, "type") == "active"
		list = append(list, info)
		seen[id] = true
	}
	a.mu.Lock()
	for id, thread := range a.threads {
		if seen[id] {
			continue
		}
		list = append(list, ThreadInfo{ID: id, Title: firstNonEmpty(str(thread, "name"), str(thread, "preview"), "New Chat"), Cwd: str(thread, "cwd"), Rollout: str(thread, "path"), Source: "codex", Recency: time.Now().UnixMilli()})
	}
	a.mu.Unlock()
	return list, nil
}
func (a *HeadlessCodex) handle(c *Client, raw []byte) bool {
	var m struct {
		Op, ID, Text, Cwd, Model, Effort, ApprovalPolicy, Sandbox, Reviewer, ReqID, Method, Decision, Nonce string
		Images                                                                                              []string
		Answers                                                                                             map[string][]string
	}
	if json.Unmarshal(raw, &m) != nil {
		return true
	}
	switch m.Op {
	case "new", "send", "stop", "wake", "settings", "approve", "answer", "compact", "archive", "unarchive", "models", "projects", "project":
	default:
		return false
	}
	go func() {
		a.opMu.Lock()
		defer a.opMu.Unlock()
		ack := func(err error, extra map[string]any) {
			value := map[string]any{"t": "ack", "op": m.Op, "id": m.ID, "nonce": m.Nonce, "ok": err == nil}
			if err != nil {
				value["msg"] = err.Error()
			}
			for k, v := range extra {
				value[k] = v
			}
			c.push(mustJSON(value))
		}
		if err := a.start(); err != nil {
			ack(err, nil)
			return
		}
		switch m.Op {
		case "models":
			result, err := a.call("model/list", map[string]any{})
			if err != nil {
				ack(err, nil)
				return
			}
			models := []Model{}
			for _, raw := range anySlice(result["data"]) {
				model, _ := raw.(map[string]any)
				value := Model{Slug: str(model, "model"), Name: str(model, "displayName"), Description: str(model, "description"), DefaultEffort: str(model, "defaultReasoningEffort")}
				for _, raw := range anySlice(model["supportedReasoningEfforts"]) {
					effort, _ := raw.(map[string]any)
					value.Efforts = append(value.Efforts, str(effort, "reasoningEffort"))
				}
				models = append(models, value)
			}
			c.push(mustJSON(map[string]any{"t": "models", "models": models, "default": defaultModel()}))
			return
		case "projects", "project":
			list, err := a.listThreads()
			if err != nil {
				ack(err, nil)
				return
			}
			if m.Op == "project" {
				filtered := []ThreadInfo{}
				for _, t := range list {
					if t.Cwd == m.Cwd {
						filtered = append(filtered, t)
					}
				}
				c.push(mustJSON(map[string]any{"t": "project", "cwd": m.Cwd, "threads": filtered}))
				return
			}
			projects := map[string]Project{}
			for _, t := range list {
				project := projects[t.Cwd]
				project.Cwd = t.Cwd
				project.Threads++
				if t.Recency > project.Recency {
					project.Recency = t.Recency
				}
				projects[t.Cwd] = project
			}
			values := []Project{}
			for _, p := range projects {
				values = append(values, p)
			}
			sort.Slice(values, func(i, j int) bool { return values[i].Recency > values[j].Recency })
			c.push(mustJSON(map[string]any{"t": "projects", "projects": values, "home": userHome()}))
			return
		case "new":
			if strings.TrimSpace(m.Text) == "" {
				ack(errors.New("Empty Prompt"), nil)
				return
			}
			if err := checkUploads(m.Images); err != nil {
				ack(err, nil)
				return
			}
			cwd := m.Cwd
			if cwd == "" {
				cwd = userHome()
			}
			if strings.HasPrefix(cwd, "~/") {
				cwd = filepath.Join(userHome(), cwd[2:])
			}
			if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
				ack(errors.New("The Folder Does Not Exist"), nil)
				return
			}
			params := map[string]any{"cwd": cwd}
			if m.Model != "" {
				params["model"] = m.Model
			}
			if m.Effort != "" {
				params["config"] = map[string]any{"model_reasoning_effort": m.Effort}
			}
			result, err := a.call("thread/start", params)
			if err != nil {
				ack(err, nil)
				return
			}
			m.ID, err = a.remember(result)
			if err != nil {
				ack(err, nil)
				return
			}
			a.mu.Lock()
			a.threads[m.ID]["preview"] = clipPlain(m.Text, 160)
			a.mu.Unlock()
			err = a.sendTurn(m.ID, m.Text, m.Images)
			a.hub.refreshList()
			ack(err, map[string]any{"thread": m.ID})
			return
		case "archive", "unarchive":
			_, err := a.call("thread/"+m.Op, map[string]any{"threadId": m.ID})
			if err == nil {
				a.mu.Lock()
				delete(a.threads, m.ID)
				delete(a.settings, m.ID)
				a.mu.Unlock()
				a.hub.refreshList()
			}
			ack(err, nil)
			return
		case "approve", "answer":
			a.mu.Lock()
			var key string
			var request map[string]any
			for k, r := range a.requests {
				p, _ := r["params"].(map[string]any)
				if str(p, "threadId") == m.ID && fmt.Sprint(pendingRequestID(r)) == m.ReqID {
					key = k
					request = r
					break
				}
			}
			a.mu.Unlock()
			if request == nil {
				ack(errors.New("This Request Is No Longer Pending"), nil)
				return
			}
			method := str(request, "method")
			result := map[string]any{}
			if m.Op == "answer" && strings.Contains(method, "requestUserInput") {
				answers := map[string]any{}
				for k, v := range m.Answers {
					answers[k] = map[string]any{"answers": v}
				}
				result["answers"] = answers
			} else if m.Op == "approve" && (strings.Contains(method, "commandExecution") || strings.Contains(method, "fileChange")) {
				if m.Decision != "accept" && m.Decision != "acceptForSession" && m.Decision != "decline" && m.Decision != "cancel" {
					ack(errors.New("Unknown Approval Decision"), nil)
					return
				}
				result["decision"] = m.Decision
			} else {
				ack(errors.New("Unsupported Request Type"), nil)
				return
			}
			err := a.send(map[string]any{"id": request["id"], "result": result})
			if err == nil {
				a.mu.Lock()
				delete(a.requests, key)
				a.publishLocked(m.ID)
				a.mu.Unlock()
			}
			ack(err, nil)
			return
		}
		if err := a.resume(m.ID); err != nil {
			ack(err, nil)
			return
		}
		var err error
		switch m.Op {
		case "send":
			err = a.sendTurn(m.ID, m.Text, m.Images)
		case "wake":
		case "stop":
			a.mu.Lock()
			id := lastTurnIDJSON(a.threads[m.ID])
			a.mu.Unlock()
			if id == "" {
				err = errors.New("No Active Turn")
			} else {
				_, err = a.call("turn/interrupt", map[string]any{"threadId": m.ID, "turnId": id})
			}
		case "compact":
			_, err = a.call("thread/compact/start", map[string]any{"threadId": m.ID})
		case "settings":
			if m.ApprovalPolicy != "" && !approvalPolicies[m.ApprovalPolicy] {
				err = errors.New("Unknown Approval Policy")
			}
			if m.Sandbox != "" && !sandboxModes[m.Sandbox] {
				err = errors.New("Unknown Sandbox")
			}
			if m.Reviewer != "" && m.Reviewer != "user" && m.Reviewer != "auto_review" {
				err = errors.New("Unknown Reviewer")
			}
			if err == nil {
				a.mu.Lock()
				settings := a.settings[m.ID]
				for key, value := range map[string]string{"model": m.Model, "effort": m.Effort, "approvalPolicy": m.ApprovalPolicy, "approvalsReviewer": m.Reviewer} {
					if value != "" {
						settings[key] = value
					}
				}
				if m.Sandbox != "" {
					settings["sandboxPolicy"] = headlessSandbox(m.Sandbox)
				}
				a.publishLocked(m.ID)
				a.mu.Unlock()
			}
		}
		ack(err, nil)
	}()
	return true
}
func pendingRequestID(request map[string]any) any {
	var id any
	data, _ := request["id"].(json.RawMessage)
	_ = json.Unmarshal(data, &id)
	return id
}
func headlessSandbox(mode string) map[string]any {
	policy := map[string]any{"type": mode}
	if mode == "readOnly" || mode == "workspaceWrite" {
		policy["networkAccess"] = false
	}
	if mode == "workspaceWrite" {
		policy["writableRoots"] = []string{}
		policy["excludeTmpdirEnvVar"] = false
		policy["excludeSlashTmp"] = false
	}
	return policy
}
func lastTurnIDJSON(thread map[string]any) string {
	turns := anySlice(thread["turns"])
	if len(turns) == 0 {
		return ""
	}
	turn, _ := turns[len(turns)-1].(map[string]any)
	if str(turn, "status") != "inProgress" {
		return ""
	}
	return str(turn, "id")
}
func (a *HeadlessCodex) sendTurn(id, text string, images []string) error {
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return errors.New("Empty Message")
	}
	if err := checkUploads(images); err != nil {
		return err
	}
	input := textInput(text, images)
	for _, item := range input {
		if item["type"] == "text" {
			delete(item, "text_elements")
			item["textElements"] = []any{}
		}
	}
	params := map[string]any{"threadId": id, "input": input}
	a.mu.Lock()
	turn := lastTurnIDJSON(a.threads[id])
	for key, value := range a.settings[id] {
		params[key] = value
	}
	a.mu.Unlock()
	method := "turn/start"
	if turn != "" {
		method = "turn/steer"
		params = map[string]any{"threadId": id, "input": input, "expectedTurnId": turn}
	}
	messageID := uuid()
	params["clientUserMessageId"] = messageID
	result, err := a.call(method, params)
	if err == nil {
		a.mu.Lock()
		turnID := str(result, "turnId")
		if turnResult, ok := result["turn"].(map[string]any); ok {
			turnID = str(turnResult, "id")
		}
		if current := headlessTurn(a.threads[id], turnID); current != nil {
			items := anySlice(current["items"])
			found := false
			for _, raw := range items {
				item, _ := raw.(map[string]any)
				if str(item, "type") == "userMessage" && bytes.Equal(mustJSON(item["content"]), mustJSON(input)) {
					found = true
					break
				}
			}
			if !found {
				item := map[string]any{"id": messageID, "type": "userMessage", "content": input}
				current["items"] = append([]any{item}, items...)
			}
			a.publishLocked(id)
		}
		a.mu.Unlock()
	}
	return err
}
