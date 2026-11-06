package main

import (
	"os"
	"runtime"
	"sort"
	"sync"
	"time"
)

type rolloutOwnerEvidence struct {
	file    os.FileInfo
	checked time.Time
	alive   bool
	known   bool
}
type rolloutOwnerCache struct {
	mu      sync.Mutex
	probeMu sync.Mutex
	entries map[string]rolloutOwnerEvidence
	pending map[string]os.FileInfo
	working bool
	probe   func([]string) map[string]rolloutOwnerEvidence
}

var rolloutOwners = &rolloutOwnerCache{entries: map[string]rolloutOwnerEvidence{}, pending: map[string]os.FileInfo{}, probe: platformRolloutOwners}

func ownerEvidenceTTL() time.Duration {
	if runtime.GOOS == "windows" {
		return 15 * time.Second
	}
	return 3 * time.Second
}

func sameRolloutFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// Only list reads wait for a cold owner probe. Session goroutines consume the
// last immutable evidence and queue refreshes, so controls never wait on lsof.
func (c *rolloutOwnerCache) refresh(files map[string]os.FileInfo) {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	needed := map[string]os.FileInfo{}
	c.mu.Lock()
	for path, info := range files {
		old := c.entries[path]
		if !sameRolloutFile(old.file, info) || time.Since(old.checked) > ownerEvidenceTTL() {
			needed[path] = info
		}
	}
	c.mu.Unlock()
	if len(needed) == 0 {
		return
	}
	paths := make([]string, 0, len(needed))
	for path := range needed {
		paths = append(paths, path)
	}
	results := c.probe(paths)
	checked := time.Now()
	updates := map[string]rolloutOwnerEvidence{}
	for path, info := range needed {
		after, err := os.Stat(path)
		if err != nil || !sameRolloutFile(info, after) {
			continue
		}
		result := results[path]
		result.file = info
		result.checked = checked
		updates[path] = result
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for path, result := range updates {
		if old := c.entries[path]; !result.known && old.known && sameRolloutFile(old.file, result.file) {
			result.known = true
			result.alive = old.alive
		}
		c.entries[path] = result
	}
	if len(c.entries) > 2048 {
		keys := make([]string, 0, len(c.entries))
		for path := range c.entries {
			keys = append(keys, path)
		}
		sort.Slice(keys, func(i, j int) bool { return c.entries[keys[i]].checked.Before(c.entries[keys[j]].checked) })
		for _, path := range keys[:len(keys)-2048] {
			delete(c.entries, path)
		}
	}
}

func (c *rolloutOwnerCache) running(path string, info os.FileInfo) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[path]
	valid := sameRolloutFile(entry.file, info)
	if !valid || time.Since(entry.checked) > ownerEvidenceTTL() {
		c.pending[path] = info
		if !c.working {
			c.working = true
			go c.work()
		}
	}
	// A failed/unsupported probe is not evidence that a live process exited.
	return !valid || !entry.known || entry.alive
}

func (c *rolloutOwnerCache) work() {
	for {
		// Coalesce session timers into one native owner query.
		time.Sleep(20 * time.Millisecond)
		c.mu.Lock()
		if len(c.pending) == 0 {
			c.working = false
			c.mu.Unlock()
			return
		}
		batch := c.pending
		c.pending = map[string]os.FileInfo{}
		c.mu.Unlock()
		c.refresh(batch)
	}
}

func confirmThreadActivity(threads []ThreadInfo) {
	files := map[string]os.FileInfo{}
	for i := range threads {
		thread := &threads[i]
		if !thread.Running {
			continue
		}
		info, err := os.Stat(thread.Rollout)
		if err != nil {
			thread.Running = false
			thread.Active = false
			continue
		}
		if time.Since(info.ModTime()) > time.Minute {
			files[thread.Rollout] = info
		}
	}
	rolloutOwners.refresh(files)
	for i := range threads {
		thread := &threads[i]
		if info := files[thread.Rollout]; thread.Running && info != nil && !rolloutOwners.running(thread.Rollout, info) {
			thread.Running = false
			thread.Active = false
			thread.LastErr = "Process exited"
		}
	}
}

func liveRolloutRunning(path string, started bool) bool {
	if !started {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && (time.Since(info.ModTime()) <= time.Minute || rolloutOwners.running(path, info))
}
