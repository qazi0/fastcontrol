package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

func TestOrganizationStorageEncoding(t *testing.T) {
	latin := []byte{1, '{', '"', 'n', '"', ':', '"', 0xe9, '"', '}'}
	b, err := decodeClaudeStorageValue(latin)
	if err != nil || string(b) != `{"n":"é"}` {
		t.Fatalf("Latin-1 decoding: %q %v", b, err)
	}
	text := `{"n":"Test 🚀"}`
	value := []byte{0}
	for _, u := range utf16.Encode([]rune(text)) {
		value = binary.LittleEndian.AppendUint16(value, u)
	}
	b, err = decodeClaudeStorageValue(value)
	if err != nil || string(b) != text {
		t.Fatalf("UTF-16 decoding: %q %v", b, err)
	}
	for _, bad := range [][]byte{{}, {9, 1}, {0, 1}} {
		if _, err = decodeClaudeStorageValue(bad); err == nil {
			t.Fatal("unsupported storage value accepted")
		}
	}
}

func TestOrganizationSourceIdentityAndBounds(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "metadata.json")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := organizationStamps([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(root, "next.json")
	if err = os.WriteFile(next, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(next, a[0].info.ModTime(), a[0].info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	b, err := organizationStamps([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if sameOrganizationStamps(a, b) {
		t.Fatal("equal size and mtime concealed file replacement")
	}
	if _, err = organizationReadFile(path, 5); err == nil {
		t.Fatal("oversized source accepted")
	}
	r := NewOrganizationReader(root)
	ids := make([]string, organizationRowLimit+1)
	if _, err = r.Claude(ids); err == nil {
		t.Fatal("unbounded Claude index accepted")
	}
	if _, err = r.Codex("", ids); err == nil {
		t.Fatal("unbounded Codex index accepted")
	}
}

func TestOrganizationClaudeArchiveAndAmbiguousBridgeJoin(t *testing.T) {
	home, db := organizationClaudeFixture(t)
	root, _ := organizationSourcePaths(home)
	parentPath := filepath.Join(root, filepath.FromSlash(organizationTestScope), "local_"+organizationTestParent+".json")
	organizationJSON(t, parentPath, map[string]any{"sessionId": "local_" + organizationTestParent, "cliSessionId": organizationTestParent, "isArchived": true, "bridgeSessionIds": []string{"shared-bridge"}})
	childPath := filepath.Join(root, filepath.FromSlash(organizationTestScope), "local_"+organizationTestChild+".json")
	organizationJSON(t, childPath, map[string]any{"sessionId": "local_" + organizationTestChild, "cliSessionId": organizationTestChild, "bridgeSessionIds": []string{"shared-bridge"}, "spawnedFrom": map[string]any{"sessionId": "local_" + organizationTestParent, "taskId": "task", "sideSession": true, "deletedParent": nil}})
	organizationSidebar(t, db, map[string]any{"state": map[string]any{"codeSidebarByScope": map[string]any{organizationTestScope: map[string]any{"sections": []any{map[string]any{"id": "group", "kind": "manual", "name": "Group", "members": []string{"code:shared-bridge"}}}}}}})
	r := NewOrganizationReader(home)
	s, err := r.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Placements[organizationTestParent].Archived || s.Placements[organizationTestChild].ParentID != "" {
		t.Fatal("archived parent remained active")
	}
	// A duplicate bridge across two available sessions cannot choose an arbitrary child.
	organizationJSON(t, parentPath, map[string]any{"sessionId": "local_" + organizationTestParent, "cliSessionId": organizationTestParent, "bridgeSessionIds": []string{"shared-bridge"}})
	s, err = r.Claude([]string{organizationTestParent, organizationTestChild})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Placements[organizationTestParent].GroupIDs) != 0 || len(s.Placements[organizationTestChild].GroupIDs) != 0 {
		t.Fatal("ambiguous bridge membership chose a session")
	}
	if s.Placements[organizationTestChild].ParentID != organizationTestParent {
		t.Fatal("null deletedParent suppressed a valid parent")
	}
}
