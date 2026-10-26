package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

type claudeWorkRef struct {
	ID, ToolID, Title, Kind, State, Output, Directory string
	At                                                int64
}
type claudeWorkSummary struct {
	File            os.FileInfo
	Activity, Model string
	Finished        bool
}
type claudeWorkIndex struct {
	mu          sync.Mutex
	file        os.FileInfo
	offset      int64
	refs, tools map[string]claudeWorkRef
}

var claudeTaskNotice = regexp.MustCompile(`(?s)<task-notification>(.*?)</task-notification>`)
var claudeTaskTag = regexp.MustCompile(`(?s)<(task-id|tool-use-id|output-file|status|summary)>(.*?)</(?:task-id|tool-use-id|output-file|status|summary)>`)

func claudeWorkState(s string) string {
	switch s {
	case "completed", "success":
		return "completed"
	case "failed", "error":
		return "failed"
	case "killed", "stopped", "cancelled", "interrupted":
		return "stopped"
	case "running", "async_launched", "in_progress":
		return "running"
	case "pending", "queued":
		return "queued"
	}
	return "saved"
}
func (idx *claudeWorkIndex) apply(r map[string]any) {
	message := mapValue(r["message"])
	for _, block := range arrayValue(message["content"]) {
		b := mapValue(block)
		input := mapValue(b["input"])
		if str(b, "type") == "tool_use" {
			ref := claudeWorkRef{ToolID: str(b, "id"), Title: str(input, "description")}
			switch str(b, "name") {
			case "Agent", "Task":
				ref.Kind = "agent"
			case "Bash":
				ref.Kind = "command"
				ref.Title = str(input, "command")
			default:
				continue
			}
			ref.Title = clipPlain(ref.Title, 240)
			idx.tools[ref.ToolID] = ref
		}
		if str(b, "type") != "tool_result" {
			continue
		}
		tool := str(b, "tool_use_id")
		ref := idx.tools[tool]
		delete(idx.tools, tool)
		u := mapValue(r["toolUseResult"])
		if id := str(u, "agentId"); id != "" {
			ref.ID = id
			ref.Kind = "agent"
		}
		if id := str(u, "backgroundTaskId"); id != "" {
			ref.ID = id
			ref.Kind = "command"
		}
		if id := str(u, "taskId"); id != "" && (str(u, "taskType") == "workflow" || str(u, "taskType") == "local_workflow") {
			ref.ID = id
			ref.Kind = "workflow"
			ref.Directory = str(u, "transcriptDir")
		}
		if ref.ID != "" {
			ref.At = claudeTime(str(r, "timestamp"))
			ref.ToolID = tool
			ref.Output = str(u, "outputFile")
			ref.State = claudeWorkState(str(u, "status"))
			if ref.State == "saved" {
				ref.State = "running"
			}
			if s := str(u, "description"); s != "" {
				ref.Title = clipPlain(s, 240)
			}
			if s := str(u, "summary"); s != "" && ref.Title == "" {
				ref.Title = clipPlain(s, 240)
			}
			if bad, _ := b["is_error"].(bool); bad {
				ref.State = "failed"
			}
			idx.refs[ref.ID] = ref
		}
		// TaskOutput returns the task's authoritative state as a nested object.
		task := mapValue(u["task"])
		id := str(task, "task_id")
		if old, ok := idx.refs[id]; ok && str(task, "status") != "" {
			old.State = claudeWorkState(str(task, "status"))
			idx.refs[id] = old
		}
	}
	if str(r, "type") != "user" && str(r, "type") != "attachment" && str(r, "type") != "system" {
		return
	}
	attachment := mapValue(r["attachment"])
	text := claudeText(message["content"]) + "\n" + claudeText(attachment["prompt"]) + "\n" + claudeText(attachment["content"]) + "\n" + claudeText(r["rendered"])
	for _, notice := range claudeTaskNotice.FindAllStringSubmatch(text, -1) {
		fields := map[string]string{}
		for _, tag := range claudeTaskTag.FindAllStringSubmatch(notice[1], -1) {
			fields[tag[1]] = tag[2]
		}
		id := fields["task-id"]
		if !organizationID(id) {
			continue
		}
		ref := idx.refs[id]
		ref.ID = id
		if ref.Kind == "" {
			ref.Kind = "agent"
		}
		if ref.ToolID == "" {
			ref.ToolID = fields["tool-use-id"]
		}
		if fields["status"] != "" {
			ref.State = claudeWorkState(fields["status"])
		}
		if fields["output-file"] != "" {
			ref.Output = fields["output-file"]
		}
		if ref.Title == "" {
			ref.Title = clipPlain(fields["summary"], 240)
		}
		idx.refs[id] = ref
	}
}

// Each append cursor is serialized independently. Callers only receive copies.
// Closed/replaced files restart the cursor; incomplete final records are retried.
func (m *ClaudeManager) workIndex(path string) (map[string]claudeWorkRef, map[string]claudeWorkRef, error) {
	m.workMu.Lock()
	if m.workIndexes == nil {
		m.workIndexes = map[string]*claudeWorkIndex{}
	}
	idx := m.workIndexes[path]
	if idx == nil {
		idx = &claudeWorkIndex{}
		m.workIndexes[path] = idx
	}
	m.workMu.Unlock()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if idx.file == nil || !os.SameFile(idx.file, st) || st.Size() < idx.file.Size() || (st.Size() == idx.file.Size() && !st.ModTime().Equal(idx.file.ModTime())) {
		idx.offset = 0
		idx.refs = map[string]claudeWorkRef{}
		idx.tools = map[string]claudeWorkRef{}
	}
	if st.Size() != idx.offset {
		reader := bufio.NewReaderSize(io.NewSectionReader(f, idx.offset, st.Size()-idx.offset), 64<<10)
		for {
			line, e := reader.ReadBytes('\n')
			if e != nil {
				if e != io.EOF {
					return nil, nil, e
				}
				break
			}
			idx.offset += int64(len(line))
			var r map[string]any
			if json.Unmarshal(line, &r) == nil {
				if side, _ := r["isSidechain"].(bool); !side || strings.HasPrefix(filepath.Base(path), "agent-") {
					idx.apply(r)
				}
			}
		}
	}
	idx.file = st
	refs, tools := map[string]claudeWorkRef{}, map[string]claudeWorkRef{}
	for id, ref := range idx.refs {
		refs[id] = ref
	}
	for id, ref := range idx.tools {
		tools[id] = ref
	}
	return refs, tools, nil
}
