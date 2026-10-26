package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"sync"
)

type turnMarkerIndex struct {
	mu         sync.Mutex
	info       os.FileInfo
	before     int64
	checkpoint [32]byte
	marker     []byte
	markerEnd  int64
}

var turnMarkerIndexes = struct {
	sync.Mutex
	files map[string]*turnMarkerIndex
}{files: map[string]*turnMarkerIndex{}}

func markerCheckpoint(f *os.File, end int64) ([32]byte, bool) {
	start := end - 128
	if start < 0 {
		start = 0
	}
	data := make([]byte, end-start)
	n, err := f.ReadAt(data, start)
	return sha256.Sum256(data[:n]), n == len(data) && (err == nil || err == io.EOF)
}

// A turn can span hundreds of megabytes. Search backwards with bounded memory
// once, then inspect only appended records, including a boundary overlap.
// Each path has one index owner; source replacement invalidates the index.
func previousTurnMarker(f *os.File, before int64) []byte {
	info, err := f.Stat()
	if err != nil || before <= 0 {
		return nil
	}
	if before > info.Size() {
		before = info.Size()
	}
	turnMarkerIndexes.Lock()
	index := turnMarkerIndexes.files[f.Name()]
	if index == nil {
		index = &turnMarkerIndex{}
		turnMarkerIndexes.files[f.Name()] = index
	}
	turnMarkerIndexes.Unlock()
	index.mu.Lock()
	defer index.mu.Unlock()
	floor := int64(0)
	valid := index.info != nil && os.SameFile(info, index.info) && info.Size() >= index.info.Size()
	if valid {
		sum, ok := markerCheckpoint(f, index.before)
		valid = ok && sum == index.checkpoint
	}
	if valid && before <= index.before && index.markerEnd <= before {
		return append([]byte(nil), index.marker...)
	}
	if valid && before >= index.before {
		floor = index.before - (1 << 20)
		if floor < 0 {
			floor = 0
		}
	}
	marker, end := findTurnMarker(f, before, floor)
	if marker == nil && valid && before >= index.before {
		marker, end = index.marker, index.markerEnd
	}
	if !valid || before >= index.before {
		sum, ok := markerCheckpoint(f, before)
		if ok {
			index.info, index.before, index.checkpoint = info, before, sum
			index.marker, index.markerEnd = append([]byte(nil), marker...), end
		}
	}
	return append([]byte(nil), marker...)
}

func findTurnMarker(f *os.File, before, floor int64) ([]byte, int64) {
	const chunk = 256 << 10
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
			return nil, 0
		}
		buf = append(buf[:n], suffix...)
		lines := bytes.Split(buf, []byte("\n"))
		lineEnd := start + int64(len(buf))
		// The rightmost fragment has no terminating newline.
		lineEnd -= int64(len(lines[len(lines)-1]))
		for i := len(lines) - 2; i >= 0; i-- {
			line := lines[i]
			markerEnd := lineEnd
			lineEnd -= int64(len(line) + 1)
			if i == 0 && start > 0 {
				break
			}
			if !bytes.Contains(line, []byte(`"event_msg"`)) || len(line) > 1<<20 {
				continue
			}
			var record struct {
				Type    string `json:"type"`
				Payload struct {
					Type string `json:"type"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &record) == nil && record.Type == "event_msg" {
				switch record.Payload.Type {
				case "task_started", "task_complete", "turn_aborted":
					return append([]byte(nil), line...), markerEnd
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
	return nil, 0
}
