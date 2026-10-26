package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/journal"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

const organizationTestScope = "11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222"
const organizationTestParent = "33333333-3333-4333-8333-333333333333"
const organizationTestChild = "44444444-4444-4444-8444-444444444444"

func organizationJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func organizationClaudeFixture(t *testing.T) (string, *leveldb.DB) {
	t.Helper()
	home := t.TempDir()
	root, store := organizationSourcePaths(home)
	if err := os.MkdirAll(store, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := leveldb.OpenFile(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, id := range []string{organizationTestParent, organizationTestChild} {
		v := map[string]any{"sessionId": "local_" + id, "cliSessionId": id, "bridgeSessionIds": []string{"session_" + id}, "title": "Native " + id[:4], "unknown_future_field": map[string]any{"ignored": true}}
		if id == organizationTestChild {
			v["spawnedFrom"] = map[string]any{"sessionId": "local_" + organizationTestParent, "taskId": "task-test", "sideSession": true}
		}
		organizationJSON(t, filepath.Join(root, filepath.FromSlash(organizationTestScope), "local_"+id+".json"), v)
	}
	return home, db
}

func organizationSidebar(t *testing.T, db *leveldb.DB, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Put([]byte("_https://claude.ai\x00\x01dframe-store"), append([]byte{1}, b...), &opt.WriteOptions{Sync: true}); err != nil {
		t.Fatal(err)
	}
}

func TestOrganizationClaudeRetainedOldJournalBatch(t *testing.T) {
	home, db := organizationClaudeFixture(t)
	organizationSidebar(t, db, map[string]any{"state": map[string]any{"codeSidebarByScope": map[string]any{organizationTestScope: map[string]any{"sections": []any{map[string]any{"id": "current", "name": "Current group", "kind": "manual", "members": []string{"code:session_" + organizationTestChild}}}}}}})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, store := organizationSourcePaths(home)
	logs, _ := filepath.Glob(filepath.Join(store, "*.log"))
	if len(logs) == 0 {
		t.Fatal("fixture has no journal")
	}
	f, err := os.OpenFile(logs[len(logs)-1], os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	w := journal.NewWriter(f)
	record, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	// A valid empty batch with an older sequence is retained by Chromium.
	if _, err = record.Write(make([]byte, 12)); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := NewOrganizationReader(home).Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil || len(s.Groups) != 1 || s.Groups[0].Name != "Current group" {
		t.Fatal("retained batch blocked current preferences", err)
	}
	data, err := os.ReadFile(logs[len(logs)-1])
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 1
	if validateClaudeJournal(data) == nil {
		t.Fatal("corrupt journal checksum accepted")
	}
}

func TestOrganizationClaudeDesktopGroupsAndFullChildren(t *testing.T) {
	home, db := organizationClaudeFixture(t)
	organizationSidebar(t, db, map[string]any{"state": map[string]any{
		"codeSidebarByScope": map[string]any{organizationTestScope: map[string]any{"sections": []any{
			map[string]any{"id": "manual-1", "kind": "manual", "name": "Release work", "order": 2, "members": []string{"code:session_" + organizationTestChild, "code:session_cloud-only"}},
			map[string]any{"id": "manual-2", "kind": "manual", "name": "Next", "order": 1, "members": []string{}},
			map[string]any{"id": "pinned", "kind": "pinned", "name": "Pinned", "members": []string{"code:session_" + organizationTestParent}},
		}}},
		"customGroupsByScope": map[string]any{organizationTestScope: map[string]any{"groups": []any{map[string]any{"id": "stale", "name": "Old group"}}, "assignments": map[string]string{}}},
	}})
	reader := NewOrganizationReader(home)
	s, err := reader.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Groups) != 2 || s.Groups[0].Name != "Next" || len(s.Placements) != 2 {
		t.Fatalf("unexpected organization: %+v", s)
	}
	p := s.Placements[organizationTestChild]
	if p.ParentID != organizationTestParent || p.Relation != "delegated" || len(p.GroupIDs) != 1 || p.DisplayTitle != "Native 4444" {
		t.Fatalf("lost desktop joins: %+v", p)
	}
	if _, ok := s.Placements["session_cloud-only"]; ok {
		t.Fatal("cloud member became a local session")
	}
	if err = db.Put([]byte("after-reader"), []byte("still writable"), &opt.WriteOptions{Sync: true}); err != nil {
		t.Fatal("reader interfered with the native writer", err)
	}
	// Cached callers may mutate their result without changing another caller.
	s.Groups[0].Name = "caller mutation"
	p.GroupIDs[0] = "caller mutation"
	s.Placements[organizationTestChild] = p
	next, err := reader.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil {
		t.Fatal(err)
	}
	if next.Groups[0].Name != "Next" || next.Placements[organizationTestChild].GroupIDs[0] == "caller mutation" {
		t.Fatal("cached snapshot leaked mutable state")
	}
}

func TestOrganizationClaudeScopeDeletionAndFreshEmptyState(t *testing.T) {
	home, db := organizationClaudeFixture(t)
	reader := NewOrganizationReader(home)
	put := func(sections []any) {
		organizationSidebar(t, db, map[string]any{"state": map[string]any{
			"codeSidebarByScope":  map[string]any{organizationTestScope: map[string]any{"sections": sections}, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb": map[string]any{"sections": []any{map[string]any{"id": "other-account", "name": "Wrong scope", "kind": "manual", "members": []string{"code:session_" + organizationTestChild}}}}},
			"customGroupsByScope": map[string]any{organizationTestScope: map[string]any{"groups": []any{map[string]any{"id": "legacy", "name": "Deleted legacy"}}}},
		}})
	}
	put([]any{map[string]any{"id": "current", "name": "Current", "kind": "manual", "members": []string{"code:session_" + organizationTestChild}}})
	s, err := reader.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil || len(s.Groups) != 1 {
		t.Fatalf("scope filtering: %+v %v", s, err)
	}
	put([]any{})
	s, err = reader.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil || len(s.Groups) != 0 || len(s.Placements[organizationTestChild].GroupIDs) != 0 {
		t.Fatalf("deleted groups survived refresh: %+v %v", s, err)
	}
	root, _ := organizationSourcePaths(home)
	if err = os.WriteFile(filepath.Join(root, filepath.FromSlash(organizationTestScope), "deleted_"+organizationTestParent), []byte("deleted"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err = reader.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Placements[organizationTestParent]; ok || s.Placements[organizationTestChild].ParentID != "" {
		t.Fatal("deleted parent retained a relation")
	}
}

func TestOrganizationClaudeForkDetachedAndCycles(t *testing.T) {
	home, _ := organizationClaudeFixture(t)
	root, _ := organizationSourcePaths(home)
	write := func(id, parent string, detached bool, spawn bool) {
		v := map[string]any{"sessionId": "local_" + id, "cliSessionId": id, "title": "Title", "lineageDetached": detached}
		if spawn {
			v["spawnedFrom"] = map[string]any{"sessionId": "local_" + parent, "taskId": "task", "sideSession": true}
		} else {
			v["forkedFromSessionId"] = "local_" + parent
		}
		organizationJSON(t, filepath.Join(root, filepath.FromSlash(organizationTestScope), "local_"+id+".json"), v)
	}
	write(organizationTestChild, organizationTestParent, false, false)
	r := NewOrganizationReader(home)
	s, err := r.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil {
		t.Fatal(err)
	}
	if p := s.Placements[organizationTestChild]; p.ParentID != organizationTestParent || p.Relation != "fork" {
		t.Fatalf("fork: %+v", p)
	}
	write(organizationTestChild, organizationTestParent, true, true)
	s, err = r.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil || s.Placements[organizationTestChild].ParentID != "" {
		t.Fatal("detached child remained attached", err)
	}
	write(organizationTestParent, organizationTestChild, false, true)
	write(organizationTestChild, organizationTestParent, false, true)
	s, err = r.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil {
		t.Fatal(err)
	}
	if s.Placements[organizationTestParent].ParentID != "" || s.Placements[organizationTestChild].ParentID != "" {
		t.Fatal("parent cycle survived normalization")
	}
}

func TestOrganizationClaudeLegacyAndInputLimits(t *testing.T) {
	home, db := organizationClaudeFixture(t)
	organizationSidebar(t, db, map[string]any{"state": map[string]any{"customGroupsByScope": map[string]any{organizationTestScope: map[string]any{
		"groups": []any{map[string]any{"id": "g1", "name": "Legacy"}}, "assignments": map[string]string{"code:session_" + organizationTestParent: "g1", "code:session_cloud-only": "g1"},
	}}}})
	r := NewOrganizationReader(home)
	s, err := r.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil || len(s.Groups) != 1 || len(s.Placements[organizationTestParent].GroupIDs) != 1 {
		t.Fatalf("legacy join: %+v %v", s, err)
	}
	root, _ := organizationSourcePaths(home)
	path := filepath.Join(root, filepath.FromSlash(organizationTestScope), "local_"+organizationTestChild+".json")
	if err = os.WriteFile(path, []byte(strings.Repeat("x", organizationFileLimit+1)), 0600); err != nil {
		t.Fatal(err)
	}
	// A changed transcript index must never receive stale cached placements.
	s, err = r.Claude([]string{organizationTestChild})
	if err == nil || len(s.Placements) != 0 {
		t.Fatal("unavailable IDs leaked from stale cache")
	}
}

func TestOrganizationSnapshotConcurrentCallers(t *testing.T) {
	home, _ := organizationClaudeFixture(t)
	r := NewOrganizationReader(home)
	if _, err := r.Claude([]string{organizationTestParent, organizationTestChild}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 4; j++ {
				s, err := r.Claude([]string{organizationTestParent, organizationTestChild})
				if err != nil {
					t.Error(err)
					return
				}
				if s.Placements[organizationTestChild].ParentID != organizationTestParent {
					t.Error("partially published relation")
				}
				delete(s.Placements, organizationTestParent)
			}
		}()
	}
	close(start)
	wg.Wait()
	s, err := r.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil || len(s.Placements) != 2 {
		t.Fatal("caller mutation changed the shared snapshot", err)
	}
}
