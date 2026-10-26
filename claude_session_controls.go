package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type claudePreferenceGate struct {
	mu    sync.Mutex
	users int
}

func (m *ClaudeManager) lockPreference(id string) (func(), error) {
	m.mu.Lock()
	if _, ok := m.entries[id]; !ok && m.runs[id] == nil {
		m.mu.Unlock()
		return nil, errors.New("Claude session not found")
	}
	if m.prefGates == nil {
		m.prefGates = map[string]*claudePreferenceGate{}
	}
	gate := m.prefGates[id]
	if gate == nil {
		gate = &claudePreferenceGate{}
		m.prefGates[id] = gate
	}
	gate.users++
	m.mu.Unlock()
	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		m.mu.Lock()
		gate.users--
		if gate.users == 0 {
			delete(m.prefGates, id)
		}
		m.mu.Unlock()
	}, nil
}

func openClaudeSessionFile(path string, flags int) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() == 0 {
		return nil, errors.New("Chat transcript is unavailable")
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		f.Close()
		return nil, errors.New("Chat transcript changed. Try again")
	}
	return f, nil
}

func (m *ClaudeManager) renameSession(id, title string) error {
	title, err := sessionTitle(title)
	if err != nil {
		return err
	}
	unlock, err := m.lockPreference(id)
	if err != nil {
		return err
	}
	defer unlock()
	m.mu.Lock()
	entry, ok := m.entries[id]
	run := m.runs[id]
	m.mu.Unlock()
	if !ok {
		return errors.New("Claude session not found")
	}
	f, err := openClaudeSessionFile(entry.Path, os.O_RDWR|os.O_APPEND)
	if err != nil {
		return err
	}
	// One append follows the CLI's typed session-metadata format. It does not
	// rewrite history or interrupt a concurrent CLI writer.
	data := append(mustJSON(map[string]any{"type": "custom-title", "customTitle": title, "sessionId": id}), '\n')
	if info, e := f.Stat(); e == nil && info.Size() > 0 {
		var tail [1]byte
		if _, e = f.ReadAt(tail[:], info.Size()-1); e == nil && tail[0] != '\n' {
			data = append([]byte{'\n'}, data...)
		}
	}
	n, err := f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if closeErr != nil {
		return closeErr
	}
	p := m.preference(id)
	p.Title = title
	if err = m.setPreference(id, p); err != nil {
		return err
	}
	if run != nil {
		run.mu.Lock()
		run.view.Meta.Title = title
		run.mu.Unlock()
		run.publish()
	}
	m.requestRefresh()
	return nil
}

type claudeForkHeader struct {
	Type         string            `json:"type"`
	UUID         string            `json:"uuid"`
	Parent       string            `json:"parentUuid"`
	Side         bool              `json:"isSidechain"`
	Session      string            `json:"sessionId"`
	Replacements []json.RawMessage `json:"replacements"`
}

func claudeForkType(kind string) bool {
	switch kind {
	case "user", "assistant", "attachment", "system", "progress":
		return true
	}
	return false
}
func eachClaudeLine(f *os.File, size int64, visit func([]byte) error) error {
	reader := bufio.NewReaderSize(io.NewSectionReader(f, 0, size), 64<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && json.Valid(line) {
			if e := visit(line); e != nil {
				return e
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Fork the captured complete prefix with fresh message IDs. Two streaming
// passes retain only the UUID graph, avoiding a full transcript copy in RAM.
func forkClaudeTranscript(path, source, title string) (string, error) {
	f, err := openClaudeSessionFile(path, os.O_RDONLY)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	headers := map[string]claudeForkHeader{}
	ids := map[string]string{}
	last := ""
	messages := 0
	replacements := []json.RawMessage{}
	err = eachClaudeLine(f, st.Size(), func(line []byte) error {
		var h claudeForkHeader
		if json.Unmarshal(line, &h) != nil {
			return nil
		}
		if h.Type == "content-replacement" && h.Session == source {
			replacements = append(replacements, h.Replacements...)
			return nil
		}
		if !claudeForkType(h.Type) || h.UUID == "" || h.Side {
			return nil
		}
		headers[h.UUID] = h
		if ids[h.UUID] == "" {
			ids[h.UUID] = uuid()
		}
		if h.Type != "progress" {
			last = h.UUID
			messages++
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if messages == 0 {
		return "", errors.New("This chat has no messages to fork")
	}
	child := uuid()
	directory := filepath.Dir(path)
	temp, err := os.CreateTemp(directory, ".fastcontrol-fork-")
	if err != nil {
		return "", err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	encoder := json.NewEncoder(temp)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err = eachClaudeLine(f, st.Size(), func(line []byte) error {
		var h claudeForkHeader
		if json.Unmarshal(line, &h) != nil || !claudeForkType(h.Type) || h.Type == "progress" || h.Side || ids[h.UUID] == "" {
			return nil
		}
		var row map[string]json.RawMessage
		if json.Unmarshal(line, &row) != nil {
			return nil
		}
		parent := ""
		seen := map[string]bool{}
		for current := h.Parent; current != ""; {
			if seen[current] {
				break
			}
			seen[current] = true
			p, ok := headers[current]
			if !ok {
				break
			}
			if p.Type != "progress" {
				parent = ids[current]
				break
			}
			current = p.Parent
		}
		row["uuid"] = mustJSON(ids[h.UUID])
		row["sessionId"] = mustJSON(child)
		row["isSidechain"] = []byte("false")
		if parent == "" {
			row["parentUuid"] = []byte("null")
		} else {
			row["parentUuid"] = mustJSON(parent)
		}
		if logical := ids[strRaw(row["logicalParentUuid"])]; logical != "" {
			row["logicalParentUuid"] = mustJSON(logical)
		} else {
			row["logicalParentUuid"] = []byte("null")
		}
		row["forkedFrom"] = mustJSON(map[string]string{"sessionId": source, "messageUuid": h.UUID})
		if h.UUID == last {
			row["timestamp"] = mustJSON(now)
		}
		for _, key := range []string{"teamName", "agentName", "slug", "sourceToolAssistantUUID"} {
			delete(row, key)
		}
		return encoder.Encode(row)
	})
	if err != nil {
		return "", err
	}
	if len(replacements) > 0 {
		if err = encoder.Encode(map[string]any{"type": "content-replacement", "sessionId": child, "uuid": uuid(), "timestamp": now, "replacements": replacements}); err != nil {
			return "", err
		}
	}
	if err = encoder.Encode(map[string]any{"type": "custom-title", "sessionId": child, "customTitle": title, "uuid": uuid(), "timestamp": now}); err != nil {
		return "", err
	}
	if err = temp.Sync(); err != nil {
		return "", err
	}
	if err = temp.Close(); err != nil {
		return "", err
	}
	// Linking a complete private temp file publishes exclusively and cannot
	// replace an existing session, including on UUID collision.
	if err = os.Link(temp.Name(), filepath.Join(directory, child+".jsonl")); err != nil {
		return "", err
	}
	return child, nil
}
func strRaw(value json.RawMessage) string {
	var text string
	_ = json.Unmarshal(value, &text)
	return text
}

func (m *ClaudeManager) forkSession(id string) (string, error) {
	m.mu.Lock()
	entry, ok := m.entries[id]
	m.mu.Unlock()
	if !ok {
		return "", errors.New("Claude session not found")
	}
	p := m.preference(id)
	title := firstNonEmpty(p.Title, entry.Info.Title, "Chat") + " (fork)"
	child, err := forkClaudeTranscript(entry.Path, id, title)
	if err != nil {
		return "", err
	}
	p.Title, p.ParentID, p.Archived = title, id, false
	if err = m.setPreference(child, p); err != nil {
		m.requestRefresh()
		return child, nil
	}
	m.refresh()
	return child, nil
}
