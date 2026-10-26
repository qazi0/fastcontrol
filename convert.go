package main

// Converts the desktop app's thread state and rollout JSONL records into a
// compact wire format for the phone. Large fields (command output, diffs,
// encrypted reasoning) are trimmed so that opening a thread costs kilobytes,
// not the 10 MB the desktop keeps in memory.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type Item struct {
	ID        string   `json:"id"`
	K         string   `json:"k"` // user, assistant, reasoning, cmd, file, tool, search, agent, error, info
	Text      string   `json:"text,omitempty"`
	Title     string   `json:"title,omitempty"`
	Body      string   `json:"body,omitempty"`
	Status    string   `json:"status,omitempty"`
	Exit      *int     `json:"exit,omitempty"`
	Phase     string   `json:"phase,omitempty"`
	Images    []string `json:"images,omitempty"`
	Audio     []string `json:"audio,omitempty"`
	AgentPath string   `json:"agentPath,omitempty"`
	AgentID   string   `json:"agentId,omitempty"`
	AgentKind string   `json:"agentKind,omitempty"`
	// Questions the assistant asked without blocking the turn; the answer is the next message.
	Asks []Ask `json:"asks,omitempty"`
}

type Ask struct {
	Title        string   `json:"title"`
	Options      []string `json:"options,omitempty"`
	Descriptions []string `json:"descriptions,omitempty"`
}

func parseAsks(v any) []Ask {
	qs, _ := v.([]any)
	var out []Ask
	for _, q := range qs {
		qm, _ := q.(map[string]any)
		if qm == nil {
			continue
		}
		a := Ask{Title: str(qm, "title")}
		if a.Title == "" {
			a.Title = str(qm, "question")
		}
		opts, _ := qm["options"].([]any)
		for _, o := range opts {
			switch ov := o.(type) {
			case string:
				a.Options = append(a.Options, ov)
				a.Descriptions = append(a.Descriptions, "")
			case map[string]any:
				a.Options = append(a.Options, str(ov, "label"))
				a.Descriptions = append(a.Descriptions, str(ov, "description"))
			}
		}
		out = append(out, a)
	}
	return out
}

type Turn struct {
	Key        string `json:"key"`
	HasDiff    bool   `json:"hasDiff,omitempty"`
	TurnID     string `json:"turnId,omitempty"`
	Status     string `json:"status,omitempty"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Error      string `json:"error,omitempty"`
	Items      []Item `json:"items"`
}

type Question struct {
	ID           string   `json:"id"`
	Header       string   `json:"header"`
	Question     string   `json:"question"`
	Options      []string `json:"options,omitempty"`
	Descriptions []string `json:"descriptions,omitempty"`
	Other        bool     `json:"other,omitempty"`
	Secret       bool     `json:"secret,omitempty"`
}

type Request struct {
	ID        string     `json:"id"`
	Method    string     `json:"method"`
	Kind      string     `json:"kind"` // command, file, question, other
	Title     string     `json:"title"`
	Body      string     `json:"body,omitempty"`
	Questions []Question `json:"questions,omitempty"`
}

type Meta struct {
	Connecting     bool      `json:"connecting,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	PermissionMode string    `json:"permissionMode,omitempty"`
	Title          string    `json:"title"`
	Cwd            string    `json:"cwd,omitempty"`
	Branch         string    `json:"branch,omitempty"`
	Model          string    `json:"model,omitempty"`
	Effort         string    `json:"effort,omitempty"`
	Approval       string    `json:"approval,omitempty"`
	Reviewer       string    `json:"reviewer,omitempty"` // who reviews approval requests: user or auto_review
	Sandbox        string    `json:"sandbox,omitempty"`
	CtxUsed        int64     `json:"ctxUsed,omitempty"`
	CtxMax         int64     `json:"ctxMax,omitempty"`
	TurnAt         int64     `json:"turnAt,omitempty"`
	Exec           bool      `json:"exec,omitempty"` // first turn is running headless via codex exec
	HasOlder       bool      `json:"hasOlder,omitempty"`
	Mode           string    `json:"mode"` // live (desktop owns it, full control) or tail (read-only rollout tail)
	Running        bool      `json:"running"`
	Runtime        string    `json:"runtime,omitempty"`
	ReadOnly       bool      `json:"readOnly,omitempty"`
	AgentState     string    `json:"agentState,omitempty"`
	Requests       []Request `json:"requests,omitempty"`
}

const (
	maxOutput = 1500
	maxDiff   = 4000
	maxArgs   = 600
)

func clipHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n… (%d more bytes)", len(s)-cut)
}

// clipPlain shortens s to about n bytes with a trailing ellipsis, for previews.
func clipPlain(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

func clipTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return fmt.Sprintf("… (%d earlier bytes)\n", cut) + s[cut:]
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func num(m map[string]any, k string) (int64, bool) {
	if v, ok := m[k].(float64); ok {
		return int64(v), true
	}
	return 0, false
}

func compactJSON(v any, n int) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return clipHead(string(b), n)
}

// shellScript strips the `/bin/zsh -lc '...'` wrapper the desktop adds.
func shellScript(cmd string) string {
	for _, p := range []string{"/bin/zsh -lc ", "/bin/bash -lc ", "bash -lc ", "zsh -lc "} {
		if strings.HasPrefix(cmd, p) {
			s := strings.TrimPrefix(cmd, p)
			if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
				s = s[1 : len(s)-1]
				s = strings.ReplaceAll(s, `'\''`, `'`)
			}
			return s
		}
	}
	return cmd
}

func joinTexts(parts []any, key string) string {
	var out []string
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			if ty := str(v, "type"); ty == "image" || ty == "localImage" || ty == "local_image" || ty == "input_image" || ty == "Image" || ty == "LocalImage" {
				out = append(out, "[Image]")
			} else if t := str(v, key); t != "" {
				out = append(out, t)
			} else if t := str(v, "text"); t != "" {
				out = append(out, t)
			}
		}
	}
	return strings.Join(out, "\n")
}

func intPtr(v int64, ok bool) *int {
	if !ok {
		return nil
	}
	i := int(v)
	return &i
}

// desktopItem converts one item from the desktop app's conversation state
// (app-server v2 ThreadItem, camelCase).
func desktopItem(raw any) Item {
	m, _ := raw.(map[string]any)
	if m == nil {
		return Item{K: "info", Text: "?"}
	}
	t := str(m, "type")
	it := Item{ID: str(m, "id"), Status: str(m, "status"), Images: collectImages(m), Audio: collectAudio(m)}
	switch t {
	case "userMessage":
		it.K = "user"
		c, _ := m["content"].([]any)
		it.Text = joinTexts(c, "text")
	case "steeringUserMessage":
		it.K = "user"
		c, _ := m["input"].([]any)
		it.Text = joinTexts(c, "text")
	case "agentMessage":
		it.K = "assistant"
		it.Text = str(m, "text")
		it.Phase = str(m, "phase")
		it.Asks = parseAsks(m["questions"])
	case "imageView", "imageGeneration":
		it.K = "image"
		it.Title = map[string]string{"imageView": "Viewed Image", "imageGeneration": "Generated Image"}[t]
	case "functionCallOutput":
		it.K = "tool"
		it.Title = str(m, "name")
	case "reasoning":
		it.K = "reasoning"
		s, _ := m["summary"].([]any)
		it.Text = joinTexts(s, "text")
	case "commandExecution":
		it.K = "cmd"
		it.Title = clipHead(shellScript(str(m, "command")), 800)
		it.Body = clipTail(str(m, "aggregatedOutput"), maxOutput)
		it.Exit = intPtr(num(m, "exitCode"))
	case "fileChange":
		it.K = "file"
		ch, _ := m["changes"].([]any)
		var paths []string
		var diffs strings.Builder
		for _, c := range ch {
			cm, _ := c.(map[string]any)
			kind := ""
			if km, ok := cm["kind"].(map[string]any); ok {
				kind = str(km, "type")
			}
			paths = append(paths, strings.ToUpper(firstChar(kind))+" "+str(cm, "path"))
			d := str(cm, "diff")
			if (kind == "add" || kind == "delete") && !strings.HasPrefix(d, "@@") && !strings.HasPrefix(d, "---") {
				d = prefixLines(d, map[string]string{"add": "+", "delete": "-"}[kind])
			}
			diffs.WriteString(d)
			diffs.WriteString("\n")
		}
		it.Title = strings.Join(paths, "\n")
		it.Body = clipHead(diffs.String(), maxDiff)
	case "mcpToolCall":
		it.K = "tool"
		it.Title = str(m, "server") + "." + str(m, "tool")
		it.Body = compactJSON(m["arguments"], maxArgs)
		if e, ok := m["error"]; ok && e != nil {
			it.Body += "\nerror: " + compactJSON(e, maxArgs)
		}
	case "dynamicToolCall", "collabAgentToolCall":
		it.K = "tool"
		it.Title = str(m, "tool")
		if p := str(m, "prompt"); p != "" {
			it.Body = clipHead(p, maxArgs)
		}
	case "webSearch":
		it.K = "search"
		it.Title = str(m, "query")
	case "subAgentActivity":
		it.K = "agent"
		it.Title = str(m, "kind") + " " + str(m, "agentPath")
		it.AgentPath = str(m, "agentPath")
		it.AgentID = str(m, "agentThreadId")
	case "error":
		it.K = "error"
		it.Text = str(m, "message")
	case "contextCompaction":
		it.K = "info"
		it.Text = "Context compacted"
	case "steered":
		it.K = "info"
		it.Text = "Steered"
	case "sleep":
		it.K = "info"
		d, _ := num(m, "durationMs")
		it.Text = fmt.Sprintf("Waited %ds", d/1000)
	default:
		it.K = "info"
		it.Text = humanize(t)
	}
	return it
}

// humanize turns "exitedReviewMode" or "ExitedReviewMode" into "Exited Review Mode".
func humanize(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte(' ')
		}
		if i == 0 && r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func firstChar(s string) string {
	if s == "" {
		return "?"
	}
	return s[:1]
}

func desktopTurn(key string, raw any) Turn {
	m, _ := raw.(map[string]any)
	t := Turn{Key: key}
	if m == nil {
		return t
	}
	t.TurnID = str(m, "turnId")
	t.Status = str(m, "status")
	t.StartedAt, _ = num(m, "turnStartedAtMs")
	t.DurationMs, _ = num(m, "durationMs")
	if e, ok := m["error"].(map[string]any); ok {
		t.Error = str(e, "message")
	}
	t.HasDiff = str(m, "diff") != ""
	items, _ := m["items"].([]any)
	t.Items = make([]Item, len(items))
	for i, r := range items {
		t.Items[i] = desktopItem(r)
		if t.Items[i].K == "file" {
			t.HasDiff = true
		}
	}
	if len(items) == 0 {
		// Older turns arrive without items until history is paged in; show the prompt.
		if p, ok := m["params"].(map[string]any); ok {
			in, _ := p["input"].([]any)
			if txt := joinTexts(in, "text"); txt != "" {
				t.Items = []Item{{ID: key + ":input", K: "user", Text: txt}}
			}
		}
		if pg, ok := m["itemsPagination"].(map[string]any); ok && len(t.Items) == 0 {
			in, _ := pg["oldestUserInput"].([]any)
			if txt := joinTexts(in, "text"); txt != "" {
				t.Items = []Item{{ID: key + ":input", K: "user", Text: txt}}
			}
		}
	}
	return t
}

// desktopTurnOrder returns turn keys oldest first.
func desktopTurnOrder(state map[string]any) []string {
	th, _ := state["turnHistory"].(map[string]any)
	h, _ := th["history"].(map[string]any)
	if h == nil {
		return nil
	}
	var keys []string
	islands, _ := h["islands"].([]any)
	for _, is := range islands {
		im, _ := is.(map[string]any)
		entries, _ := im["entries"].([]any)
		for _, e := range entries {
			em, _ := e.(map[string]any)
			if k := str(em, "key"); k != "" {
				keys = append(keys, k)
			}
		}
	}
	if len(keys) == 0 {
		ents, _ := h["entitiesByKey"].(map[string]any)
		for k := range ents {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	return keys
}

func desktopEntities(state map[string]any) map[string]any {
	th, _ := state["turnHistory"].(map[string]any)
	h, _ := th["history"].(map[string]any)
	ents, _ := h["entitiesByKey"].(map[string]any)
	return ents
}

func desktopMeta(state map[string]any, title string) Meta {
	m := Meta{Mode: "live", Title: str(state, "title"), Cwd: str(state, "cwd"), Model: str(state, "latestModel"), Effort: str(state, "latestReasoningEffort")}
	if m.Title == "" {
		m.Title = title
	}
	if ts, ok := state["latestThreadSettings"].(map[string]any); ok {
		if v := str(ts, "model"); v != "" {
			m.Model = v
		}
		if v := str(ts, "effort"); v != "" {
			m.Effort = v
		}
		m.Approval = str(ts, "approvalPolicy")
		m.Reviewer = str(ts, "approvalsReviewer")
		if sp, ok := ts["sandboxPolicy"].(map[string]any); ok {
			m.Sandbox = str(sp, "type")
		}
	}
	if u, ok := state["latestTokenUsageInfo"].(map[string]any); ok {
		m.CtxMax, _ = num(u, "modelContextWindow")
		if last, ok := u["last"].(map[string]any); ok {
			m.CtxUsed, _ = num(last, "inputTokens")
		}
	}
	if g, ok := state["gitInfo"].(map[string]any); ok {
		m.Branch = str(g, "branch")
	}
	if tp, ok := state["turnsPagination"].(map[string]any); ok {
		if done, ok := tp["hasLoadedOldest"].(bool); ok && !done {
			m.HasOlder = true
		}
	}
	if rs, ok := state["threadRuntimeStatus"].(map[string]any); ok {
		m.Runtime = str(rs, "type")
		if m.Runtime == "active" {
			m.Running = true
		}
	}
	keys := desktopTurnOrder(state)
	if len(keys) > 0 {
		if t, ok := desktopEntities(state)[keys[len(keys)-1]].(map[string]any); ok {
			if str(t, "status") == "inProgress" {
				m.Running = true
			}
			m.TurnAt, _ = num(t, "turnStartedAtMs")
		}
	}
	reqs, _ := state["requests"].([]any)
	for _, r := range reqs {
		rm, _ := r.(map[string]any)
		if rm == nil {
			continue
		}
		req := Request{Method: str(rm, "method")}
		switch id := rm["id"].(type) {
		case string:
			req.ID = id
		case float64:
			req.ID = fmt.Sprintf("%d", int64(id))
		}
		p, _ := rm["params"].(map[string]any)
		switch {
		case strings.Contains(req.Method, "commandExecution"):
			req.Kind = "command"
			req.Title = "Run Command?"
			req.Body = shellScript(str(p, "command"))
			if r := str(p, "reason"); r != "" {
				req.Body += "\n\n" + r
			}
		case strings.Contains(req.Method, "fileChange"):
			req.Kind = "file"
			req.Title = "Apply File Changes?"
			req.Body = str(p, "reason")
		case strings.Contains(req.Method, "requestUserInput"):
			req.Kind = "question"
			req.Title = "Codex Has A Question"
			qs, _ := p["questions"].([]any)
			for _, q := range qs {
				qm, _ := q.(map[string]any)
				if qm == nil {
					continue
				}
				qq := Question{ID: str(qm, "id"), Header: str(qm, "header"), Question: str(qm, "question")}
				qq.Other, _ = qm["isOther"].(bool)
				qq.Secret, _ = qm["isSecret"].(bool)
				opts, _ := qm["options"].([]any)
				for _, o := range opts {
					if om, ok := o.(map[string]any); ok {
						qq.Options = append(qq.Options, str(om, "label"))
						qq.Descriptions = append(qq.Descriptions, str(om, "description"))
					}
				}
				req.Questions = append(req.Questions, qq)
			}
		default:
			req.Kind = "other"
			req.Title = req.Method
			req.Body = compactJSON(p, 1200)
		}
		m.Requests = append(m.Requests, req)
	}
	return m
}

func lastTurnID(state map[string]any) string {
	keys := desktopTurnOrder(state)
	if len(keys) == 0 {
		return ""
	}
	t, _ := desktopEntities(state)[keys[len(keys)-1]].(map[string]any)
	return str(t, "turnId")
}

// ---- rollout JSONL (cold threads) ----

// rolloutItem converts the `item` of an `item_completed` event (PascalCase, snake_case fields).
func rolloutItem(m map[string]any) (Item, bool) {
	t := str(m, "type")
	it := Item{ID: str(m, "id"), Status: str(m, "status"), Images: collectImages(m), Audio: collectAudio(m)}
	switch t {
	case "UserMessage":
		it.K = "user"
		c, _ := m["content"].([]any)
		it.Text = joinTexts(c, "text")
	case "AgentMessage":
		it.K = "assistant"
		c, _ := m["content"].([]any)
		it.Text = joinTexts(c, "text")
		it.Phase = str(m, "phase")
		it.Asks = parseAsks(m["questions"])
	case "Reasoning":
		it.K = "reasoning"
		s, _ := m["summary_text"].([]any)
		it.Text = joinTexts(s, "text")
		if it.Text == "" {
			return it, false
		}
	case "CommandExecution":
		it.K = "cmd"
		if c, ok := m["command"].([]any); ok && len(c) > 0 {
			if s, ok := c[len(c)-1].(string); ok {
				it.Title = clipHead(s, 800)
			}
		}
		it.Body = clipTail(str(m, "aggregated_output"), maxOutput)
		it.Exit = intPtr(num(m, "exit_code"))
	case "FileChange":
		it.K = "file"
		ch, _ := m["changes"].(map[string]any)
		var paths []string
		var diffs strings.Builder
		for p, c := range ch {
			cm, _ := c.(map[string]any)
			paths = append(paths, strings.ToUpper(firstChar(str(cm, "type")))+" "+p)
			if d := str(cm, "unified_diff"); d != "" {
				diffs.WriteString(d)
			} else {
				diffs.WriteString(str(cm, "content"))
			}
			diffs.WriteString("\n")
		}
		sort.Strings(paths)
		it.Title = strings.Join(paths, "\n")
		it.Body = clipHead(diffs.String(), maxDiff)
	case "McpToolCall":
		it.K = "tool"
		it.Title = str(m, "server") + "." + str(m, "tool")
		it.Body = compactJSON(m["arguments"], maxArgs)
	case "WebSearch":
		it.K = "search"
		it.Title = str(m, "query")
	case "SubAgentActivity":
		it.K = "agent"
		it.Title = str(m, "kind") + " " + str(m, "agent_path")
		it.AgentPath = str(m, "agent_path")
		it.AgentID = str(m, "agent_thread_id")
	case "ImageView", "ImageGeneration":
		it.K = "image"
		it.Title = map[string]string{"ImageView": "Viewed Image", "ImageGeneration": "Generated Image"}[t]
	case "FunctionCallOutput", "Extension":
		if len(it.Audio) > 0 {
			it.K = "tool"
			it.Title = "Audio"
			return it, true
		}
		if len(it.Images) > 0 {
			it.K = "image"
			it.Title = "Tool Image"
			return it, true
		}
		return it, false
	default:
		it.K = "info"
		it.Text = humanize(t)
	}
	return it, true
}

// desktopTurnDiff returns the unified diff the desktop computed for a turn.
func desktopTurnDiff(state map[string]any, key string) string {
	t, _ := desktopEntities(state)[key].(map[string]any)
	if d := str(t, "diff"); d != "" {
		return d
	}
	// Fall back to the per-file diffs of the turn's file changes.
	var b strings.Builder
	items, _ := t["items"].([]any)
	for _, it := range items {
		im, _ := it.(map[string]any)
		if str(im, "type") != "fileChange" {
			continue
		}
		ch, _ := im["changes"].([]any)
		for _, c := range ch {
			cm, _ := c.(map[string]any)
			d, p := str(cm, "diff"), str(cm, "path")
			kind := ""
			if km, ok := cm["kind"].(map[string]any); ok {
				kind = str(km, "type")
			}
			if !strings.HasPrefix(d, "---") && !strings.HasPrefix(d, "diff ") {
				fmt.Fprintf(&b, "--- %s\n+++ %s\n", p, p)
			}
			if (kind == "add" || kind == "delete") && !strings.HasPrefix(d, "@@") && !strings.HasPrefix(d, "---") {
				d = prefixLines(d, map[string]string{"add": "+", "delete": "-"}[kind])
			}
			b.WriteString(d)
			if !strings.HasSuffix(d, "\n") {
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// rolloutDiff renders a rollout FileChange item as a unified diff.
func rolloutDiff(m map[string]any) string {
	ch, _ := m["changes"].(map[string]any)
	paths := make([]string, 0, len(ch))
	for p := range ch {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		cm, _ := ch[p].(map[string]any)
		if d := str(cm, "unified_diff"); d != "" {
			if !strings.HasPrefix(d, "---") && !strings.HasPrefix(d, "diff ") {
				fmt.Fprintf(&b, "--- %s\n+++ %s\n", p, p)
			}
			b.WriteString(d)
		} else {
			kind := str(cm, "type")
			fmt.Fprintf(&b, "--- %s\n+++ %s\n", p, p)
			pre := "+"
			if kind == "delete" {
				pre = "-"
			}
			b.WriteString(prefixLines(str(cm, "content"), pre))
		}
		if !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func prefixLines(s, pre string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(pre + l + "\n")
	}
	return b.String()
}
