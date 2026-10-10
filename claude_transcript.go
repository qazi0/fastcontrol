package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type claudeTranscript struct {
	Meta  Meta
	Turns []Turn
	First string
	More  bool
	Diffs map[string]string
	cur   int
	tools map[string][2]int
}

func newClaudeTranscript() *claudeTranscript {
	return &claudeTranscript{Meta: Meta{Provider: "claude", Mode: "tail", PermissionMode: "default"}, Turns: []Turn{}, Diffs: map[string]string{}, cur: -1, tools: map[string][2]int{}}
}

func mapValue(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func arrayValue(v any) []any { a, _ := v.([]any); return a }
func claudeTime(s string) int64 {
	t, _ := time.Parse(time.RFC3339Nano, s)
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
func claudeText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var out []string
	for _, b := range arrayValue(v) {
		m := mapValue(b)
		if str(m, "type") == "text" {
			out = append(out, str(m, "text"))
		}
	}
	return strings.Join(out, "\n")
}

func (v *claudeTranscript) ensureTurn(key string, at int64) *Turn {
	for i := range v.Turns {
		if v.Turns[i].Key == key {
			v.cur = i
			return &v.Turns[i]
		}
	}
	if key == "" {
		key = fmt.Sprintf("history-%d", len(v.Turns))
	}
	v.Turns = append(v.Turns, Turn{Key: key, TurnID: key, StartedAt: at, Status: "completed", Items: []Item{}})
	v.cur = len(v.Turns) - 1
	return &v.Turns[v.cur]
}
func (v *claudeTranscript) current() *Turn {
	if v.cur < 0 {
		return v.ensureTurn("earlier", 0)
	}
	return &v.Turns[v.cur]
}
func (v *claudeTranscript) upsert(it Item) {
	t := v.current()
	for i := range t.Items {
		if t.Items[i].ID == it.ID {
			t.Items[i] = it
			return
		}
	}
	t.Items = append(t.Items, it)
	if it.K == "cmd" || it.K == "file" || it.K == "tool" || it.K == "agent" {
		v.tools[it.ID] = [2]int{v.cur, len(t.Items) - 1}
	}
}

func (v *claudeTranscript) apply(r map[string]any) {
	if side, _ := r["isSidechain"].(bool); side {
		return
	}
	if cwd := str(r, "cwd"); cwd != "" {
		v.Meta.Cwd = cwd
	}
	if branch := str(r, "gitBranch"); branch != "" {
		v.Meta.Branch = branch
	}
	if effort := str(r, "effort"); effort != "" {
		v.Meta.Effort = effort
	}
	switch str(r, "type") {
	case "custom-title":
		v.Meta.Title = str(r, "customTitle")
	case "summary":
		if v.Meta.Title == "" {
			v.Meta.Title = str(r, "summary")
		}
	case "permission-mode":
		v.Meta.PermissionMode = str(r, "permissionMode")
	case "cost-state":
		for _, usage := range mapValue(r["modelUsage"]) {
			u := mapValue(usage)
			if n, ok := num(u, "contextWindow"); ok {
				v.Meta.CtxMax = n
			}
		}
	case "system":
		if str(r, "subtype") == "turn_duration" {
			t := v.current()
			t.DurationMs, _ = num(r, "durationMs")
			t.Status = "completed"
		}
		if str(r, "subtype") == "compact_boundary" {
			v.upsert(Item{ID: str(r, "uuid"), K: "info", Text: "Context Compacted"})
		}
	case "user":
		if meta, _ := r["isMeta"].(bool); meta {
			return
		}
		message := mapValue(r["message"])
		content := message["content"]
		text := claudeText(content)
		images := collectClaudeImages(content)
		if text != "" || len(images) > 0 {
			key := str(r, "uuid")
			if key == "" {
				key = str(r, "promptId")
			}
			v.ensureTurn(key, claudeTime(str(r, "timestamp")))
			v.upsert(Item{ID: key, K: "user", Text: text, Images: images})
			if v.First == "" {
				v.First = clipPlain(text, 160)
			}
		}
		for _, block := range arrayValue(content) {
			b := mapValue(block)
			if str(b, "type") != "tool_result" {
				continue
			}
			id := str(b, "tool_use_id")
			position, ok := v.tools[id]
			if !ok {
				continue
			}
			item := &v.Turns[position[0]].Items[position[1]]
			item.Status = "completed"
			if item.K != "file" {
				item.Body = clipTail(claudeText(b["content"]), maxOutput)
			}
			item.Images = collectClaudeImages(b["content"])
			if bad, _ := b["is_error"].(bool); bad {
				item.Status = "failed"
				exit := 1
				item.Exit = &exit
			}
			if item.K == "cmd" {
				if code, ok := num(mapValue(r["toolUseResult"]), "exitCode"); ok {
					value := int(code)
					item.Exit = &value
				}
				if item.Exit == nil {
					code := 0
					item.Exit = &code
				}
			}
		}
	case "assistant":
		message := mapValue(r["message"])
		if model := str(message, "model"); model != "" {
			v.Meta.Model = model
		}
		usage := mapValue(message["usage"])
		input, _ := num(usage, "input_tokens")
		read, _ := num(usage, "cache_read_input_tokens")
		created, _ := num(usage, "cache_creation_input_tokens")
		if input+read+created > 0 {
			v.Meta.CtxUsed = input + read + created
		}
		base := str(message, "id")
		if base == "" {
			base = str(r, "uuid")
		}
		for i, block := range arrayValue(message["content"]) {
			index := i
			if n, ok := num(r, "apiBlockIndex"); ok {
				index = int(n) + i
			}
			it := claudeBlock(mapValue(block), fmt.Sprintf("%s:%d", base, index))
			if it.K == "" {
				continue
			}
			v.upsert(it)
			if it.K == "file" {
				v.current().HasDiff = true
				v.Diffs[v.current().Key] += it.Body + "\n"
			}
		}
	}
	if v.Meta.Title == "" {
		v.Meta.Title = v.First
	}
}

func claudeBlock(b map[string]any, id string) Item {
	it := Item{ID: id}
	switch str(b, "type") {
	case "text":
		it.K = "assistant"
		it.Text = str(b, "text")
	case "thinking":
		it.K = "reasoning"
		it.Text = str(b, "thinking")
	case "tool_use":
		it.ID = str(b, "id")
		it.Status = "inProgress"
		input := mapValue(b["input"])
		name := str(b, "name")
		switch name {
		case "Bash":
			it.K = "cmd"
			it.Title = str(input, "command")
		case "Edit", "Write", "MultiEdit":
			it.K = "file"
			path := str(input, "file_path")
			if path == "" {
				path = str(input, "path")
			}
			it.Title = "M " + path
			old := str(input, "old_string")
			next := str(input, "new_string")
			if name == "Write" {
				next = str(input, "content")
				it.Title = "A " + path
			}
			it.Body = "--- " + path + "\n+++ " + path + "\n" + prefixLines(old, "-") + prefixLines(next, "+")
		case "Agent", "Task":
			it.K = "agent"
			it.Title = str(input, "description")
		case "WebSearch":
			it.K = "search"
			it.Title = str(input, "query")
		default:
			it.K = "tool"
			it.Title = name
			it.Body = compactJSON(input, maxArgs)
		}
	}
	return it
}

func collectClaudeImages(v any) []string {
	out := collectImages(v)
	for _, block := range arrayValue(v) {
		b := mapValue(block)
		if str(b, "type") != "image" {
			continue
		}
		source := mapValue(b["source"])
		if str(source, "type") == "base64" {
			if path := cacheImage(str(source, "data")); path != "" {
				out = append(out, path)
			}
		}
		if str(source, "type") == "url" {
			out = append(out, str(source, "url"))
		}
	}
	return out
}

func readClaudeTranscript(path string, window int, tailBytes int64) (*claudeTranscript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := st.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	v := newClaudeTranscript()
	v.More = start > 0
	if start > 0 {
		head := make([]byte, 64<<10)
		n, _ := f.ReadAt(head, 0)
		for _, line := range bytes.Split(head[:n], []byte{'\n'}) {
			var r map[string]any
			if json.Unmarshal(line, &r) == nil {
				v.apply(r)
			}
		}
		v.Turns = []Turn{}
		v.cur = -1
		v.tools = map[string][2]int{}
	}
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(f, 64<<10)
	if start > 0 {
		_, _ = reader.ReadBytes('\n')
	}
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			break
		}
		var r map[string]any
		if json.Unmarshal(line, &r) == nil {
			v.apply(r)
		}
	}
	if len(v.Turns) > window {
		v.Turns = v.Turns[len(v.Turns)-window:]
		v.More = true
	}
	v.reindex()
	v.Meta.HasOlder = v.More
	return v, nil
}

func (v *claudeTranscript) reindex() {
	v.cur = len(v.Turns) - 1
	v.tools = map[string][2]int{}
	for ti, turn := range v.Turns {
		for ii, item := range turn.Items {
			if item.K == "cmd" || item.K == "file" || item.K == "tool" || item.K == "agent" {
				v.tools[item.ID] = [2]int{ti, ii}
			}
		}
	}
}
