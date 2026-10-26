package main

// Read-only view of a thread from its rollout JSONL file. Used for threads the
// desktop app has not loaded. Reads only the tail of the file (transcripts can
// be 60+ MB) and then follows appended lines.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

const (
	tailBytes    = 2 << 20
	maxTailBytes = 64 << 20
	maxTurnDiff  = 1 << 20
)

type rolloutView struct {
	path     string
	tail     int64 // bytes read from the end of the file on load
	maxTurns int   // turns kept in memory
	offset   int64
	clipped  bool // the load started past the beginning of the file, or turns were dropped
	turns    []*Turn
	byKey    map[string]*Turn
	diffs    map[string]*strings.Builder
	cur      *Turn
	ctxUsed  int64
	ctxMax   int64
}

func newRolloutView(path string) *rolloutView {
	return &rolloutView{path: path, tail: tailBytes, maxTurns: 40}
}

type rolloutChange struct {
	newTurns []*Turn         // turns created by these lines
	items    map[*Turn][]int // appended item indexes for existing turns
	status   map[*Turn]bool  // turns whose status changed
	ctx      bool            // context usage changed
}

// load reads the tail of the file and builds the initial view.
func (v *rolloutView) load() error {
	v.turns, v.byKey, v.diffs, v.cur, v.clipped = nil, map[string]*Turn{}, map[string]*strings.Builder{}, nil, false
	f, err := os.Open(v.path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	start := st.Size() - v.tail
	skipFirst := start > 0
	if start < 0 {
		start = 0
	}
	v.clipped = skipFirst
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	v.offset = start
	// A long running turn can begin before the transcript tail. Recover its
	// lifecycle marker so opening the chat reports activity immediately.
	if skipFirst {
		if marker := previousTurnMarker(f, start); marker != nil {
			ch := &rolloutChange{items: map[*Turn][]int{}, status: map[*Turn]bool{}}
			var record rolloutLine
			if json.Unmarshal(marker, &record) == nil {
				var payload map[string]any
				if json.Unmarshal(record.Payload, &payload) == nil && str(payload, "type") == "task_started" {
					v.apply(marker, ch)
				}
			}
		}
	}
	_, err = v.consume(f, skipFirst)
	return err
}

// previousTurnMarker searches complete records before a tail window. It uses
// fixed chunks and skips oversized records, keeping long tool output bounded.
func previousTurnMarker(f *os.File, before int64) []byte {
	const chunk = 256 << 10
	const maxScan = 64 << 20
	floor := before - maxScan
	if floor < 0 {
		floor = 0
	}
	end := before
	var suffix []byte
	for end > floor {
		start := end - chunk
		if start < floor {
			start = floor
		}
		buf := make([]byte, end-start)
		n, err := f.ReadAt(buf, start)
		if err != nil && err != io.EOF {
			return nil
		}
		buf = append(buf[:n], suffix...)
		lines := bytes.Split(buf, []byte("\n"))
		// The rightmost fragment is incomplete on the first chunk. Later
		// chunks contain the saved prefix of the following complete line.
		limit := len(lines) - 1
		for i := limit - 1; i >= 0; i-- {
			if i == 0 && start > 0 {
				break
			}
			line := lines[i]
			if !bytes.Contains(line, []byte(`"event_msg"`)) {
				continue
			}
			var record rolloutLine
			if json.Unmarshal(line, &record) != nil || record.Type != "event_msg" {
				continue
			}
			var payload struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(record.Payload, &payload) == nil {
				switch payload.Type {
				case "task_started", "task_complete", "turn_aborted":
					return append([]byte(nil), line...)
				}
			}
		}
		if len(lines[0]) < 1<<20 {
			suffix = append(append([]byte(nil), lines[0]...), '\n')
		} else {
			suffix = nil
		}
		end = start
	}
	return nil
}

// grow widens the window so that about `turns` turns are visible, reading
// more of the file when needed. It reports whether anything changed.
func (v *rolloutView) grow(turns int) bool {
	if turns+10 > v.maxTurns {
		v.maxTurns = turns + 10
	}
	if len(v.turns) >= turns || !v.clipped || v.tail >= maxTailBytes {
		return len(v.turns) >= turns
	}
	for len(v.turns) < turns && v.clipped && v.tail < maxTailBytes {
		v.tail *= 4
		if v.tail > maxTailBytes {
			v.tail = maxTailBytes
		}
		_ = v.load()
	}
	return true
}

// hasOlder reports whether turns before the window exist.
func (v *rolloutView) hasOlder(window int) bool {
	return len(v.turns) > window || (v.clipped && v.tail < maxTailBytes)
}

// poll reads lines appended since the last call.
func (v *rolloutView) poll() (*rolloutChange, error) {
	f, err := os.Open(v.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= v.offset {
		return nil, err
	}
	if _, err := f.Seek(v.offset, io.SeekStart); err != nil {
		return nil, err
	}
	return v.consume(f, false)
}

func (v *rolloutView) consume(r io.Reader, skipFirst bool) (*rolloutChange, error) {
	ch := &rolloutChange{items: map[*Turn][]int{}, status: map[*Turn]bool{}}
	br := bufio.NewReaderSize(r, 1<<20)
	first := true
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			// Partial trailing line: leave it for the next poll.
			return ch, nil
		}
		v.offset += int64(len(line))
		if first && skipFirst {
			first = false
			continue
		}
		first = false
		v.apply(bytes.TrimSpace(line), ch)
	}
}

type rolloutLine struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func (v *rolloutView) turnFor(id string, ch *rolloutChange) *Turn {
	if id == "" {
		if v.cur != nil {
			return v.cur
		}
		id = "earlier"
	}
	if t, ok := v.byKey[id]; ok {
		return t
	}
	t := &Turn{Key: id, TurnID: id, Items: []Item{}}
	v.byKey[id] = t
	v.turns = append(v.turns, t)
	ch.newTurns = append(ch.newTurns, t)
	if len(v.turns) > v.maxTurns {
		old := v.turns[0]
		delete(v.byKey, old.Key)
		delete(v.diffs, old.Key)
		v.turns = v.turns[1:]
		v.clipped = true
	}
	return t
}

func (v *rolloutView) apply(line []byte, ch *rolloutChange) {
	if len(line) == 0 {
		return
	}
	var l rolloutLine
	if json.Unmarshal(line, &l) != nil {
		return
	}
	switch l.Type {
	case "event_msg":
		var p map[string]any
		if json.Unmarshal(l.Payload, &p) != nil {
			return
		}
		switch str(p, "type") {
		case "task_started":
			t := v.turnFor(str(p, "turn_id"), ch)
			t.Status = "inProgress"
			if s, ok := num(p, "started_at"); ok {
				t.StartedAt = s * 1000
			}
			if w, ok := num(p, "model_context_window"); ok {
				v.ctxMax = w
			}
			v.cur = t
			ch.status[t] = true
		case "task_complete":
			t := v.turnFor(str(p, "turn_id"), ch)
			t.Status = "completed"
			t.DurationMs, _ = num(p, "duration_ms")
			if e, ok := p["error"].(map[string]any); ok {
				t.Status = "failed"
				t.Error = errorText(str(e, "message"))
			}
			ch.status[t] = true
		case "turn_aborted":
			t := v.turnFor(str(p, "turn_id"), ch)
			t.Status = "interrupted"
			ch.status[t] = true
		case "token_count":
			info, _ := p["info"].(map[string]any)
			last, _ := info["last_token_usage"].(map[string]any)
			if n, ok := num(last, "input_tokens"); ok {
				v.ctxUsed = n
				ch.ctx = true
			}
			if w, ok := num(info, "model_context_window"); ok {
				v.ctxMax = w
			}
		case "item_completed":
			im, _ := p["item"].(map[string]any)
			if im == nil {
				return
			}
			it, ok := rolloutItem(im)
			if !ok {
				return
			}
			t := v.turnFor(str(p, "turn_id"), ch)
			t.Items = append(t.Items, it)
			ch.items[t] = append(ch.items[t], len(t.Items)-1)
			if it.K == "file" {
				t.HasDiff = true
				b := v.diffs[t.Key]
				if b == nil {
					b = &strings.Builder{}
					v.diffs[t.Key] = b
				}
				if b.Len() < maxTurnDiff {
					b.WriteString(rolloutDiff(im))
				}
			}
		}
	case "compacted":
		if v.cur != nil {
			v.cur.Items = append(v.cur.Items, Item{ID: "compact", K: "info", Text: "Context compacted"})
			ch.items[v.cur] = append(ch.items[v.cur], len(v.cur.Items)-1)
		}
	}
}

// diff returns the full diff recorded for a turn's file changes.
func (v *rolloutView) diff(key string) string {
	if b := v.diffs[key]; b != nil {
		return b.String()
	}
	return ""
}

func (v *rolloutView) snapshot(maxTurns int) []Turn {
	ts := v.turns
	if len(ts) > maxTurns {
		ts = ts[len(ts)-maxTurns:]
	}
	out := make([]Turn, 0, len(ts))
	for _, t := range ts {
		out = append(out, *t)
	}
	return out
}

func (v *rolloutView) running() bool {
	return len(v.turns) > 0 && v.turns[len(v.turns)-1].Status == "inProgress"
}

func (v *rolloutView) lastTurn() *Turn {
	if len(v.turns) == 0 {
		return nil
	}
	return v.turns[len(v.turns)-1]
}
