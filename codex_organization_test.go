package main

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func organizationExecSQL(t *testing.T, path, query string) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command(sqliteBinary(), path, query).CombinedOutput(); err != nil {
			t.Fatalf("organization fixture: %s %v", out, err)
		}
		return
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func organizationCodexFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "state.sqlite")
	organizationExecSQL(t, path, `CREATE TABLE threads(id TEXT PRIMARY KEY,name TEXT,title TEXT,project_id TEXT,thread_section_id TEXT,section_position INTEGER,archived INTEGER,source TEXT,thread_source TEXT);
CREATE TABLE projects(id TEXT PRIMARY KEY,name TEXT,metadata TEXT,position INTEGER,created_at_ms INTEGER,updated_at_ms INTEGER);
CREATE TABLE project_roots(project_id TEXT,position INTEGER,path TEXT);
CREATE TABLE thread_sections(id TEXT PRIMARY KEY,name TEXT,appearance TEXT);
CREATE TABLE thread_spawn_edges(parent_thread_id TEXT,child_thread_id TEXT,status TEXT);
INSERT INTO projects VALUES('project-1','Named native project','{}',2,0,0);
INSERT INTO project_roots VALUES('project-1',0,'/work/first'),('project-1',1,'/work/second');
INSERT INTO thread_sections VALUES('section-1','Review work','{}');
INSERT INTO threads VALUES('parent','Full native title','Old title','project-1','section-1',2,0,'desktop','');
INSERT INTO threads VALUES('agent','','Agent','project-1','section-1',3,0,'','subagent');
INSERT INTO threads VALUES('delegated','','Delegated session','project-1','section-1',4,0,'desktop','');
INSERT INTO threads VALUES('fork','','Forked session','project-1','',5,0,'{"forked_from_id":"parent"}','');
INSERT INTO threads VALUES('archived','','Archived','project-1','section-1',0,1,'','');
INSERT INTO threads VALUES('not-indexed','','Hidden from the relay','project-1','section-1',0,0,'','');
INSERT INTO thread_spawn_edges VALUES('parent','agent','open'),('parent','delegated','closed'),('not-indexed','archived','open');`)
	return home, path
}

func TestOrganizationCodexNativeNamesGroupsAndRelations(t *testing.T) {
	home, path := organizationCodexFixture(t)
	r := NewOrganizationReader(home)
	s, err := r.Codex(path, []string{"parent", "agent", "delegated", "fork", "archived"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Groups) != 2 || len(s.Placements) != 5 {
		t.Fatalf("native organization: %+v", s)
	}
	p := s.Placements["parent"]
	if p.DisplayTitle != "Full native title" || p.ProjectID != "codex:project:project-1" || len(p.GroupIDs) != 2 || p.Position != 2 {
		t.Fatalf("native placement: %+v", p)
	}
	if s.Placements["agent"].Relation != "subagent" || s.Placements["delegated"].Relation != "delegated" || s.Placements["fork"].Relation != "fork" {
		t.Fatalf("different session relationships were conflated: %+v", s.Placements)
	}
	for _, id := range []string{"agent", "delegated", "fork"} {
		if s.Placements[id].ParentID != "parent" {
			t.Fatalf("missing parent of %s", id)
		}
	}
	if !s.Placements["archived"].Archived || len(s.Placements["archived"].GroupIDs) != 0 {
		t.Fatal("archived membership leaked")
	}
	if _, ok := s.Placements["not-indexed"]; ok {
		t.Fatal("native index expanded the authorized session list")
	}
	for _, g := range s.Groups {
		if g.Kind == "project" && (len(g.Roots) != 2 || g.Roots[0] != "/work/first") {
			t.Fatalf("project roots: %+v", g)
		}
	}
	s.Placements["parent"] = SessionPlacement{DisplayTitle: "caller mutation"}
	s.Groups[1].Roots = append(s.Groups[1].Roots, "caller mutation")
	next, err := r.Codex(path, []string{"parent", "agent", "delegated", "fork", "archived"})
	if err != nil || next.Placements["parent"].DisplayTitle != "Full native title" {
		t.Fatal("Codex cache was mutable", err)
	}
}

func TestOrganizationCodexPendingMembershipMigration(t *testing.T) {
	home, path := organizationCodexFixture(t)
	organizationExecSQL(t, path, `UPDATE threads SET project_id='' WHERE id='delegated';`)
	legacy := filepath.Join(home, ".codex/.codex-global-state.json")
	host := "local:" + filepath.Dir(path)
	state := map[string]any{
		"app-server-project-id-by-legacy-project-id-by-host": map[string]any{host: map[string]string{"old-project": "project-1"}},
		"app-server-projects-migration-by-host":              map[string]any{host: map[string]bool{"threadAssignmentsMigrated": false}},
		"thread-project-assignments":                         map[string]any{"delegated": map[string]string{"projectKind": "local", "projectId": "old-project"}},
		"thread-project-membership-host-ids":                 map[string]string{"delegated": "local"},
	}
	organizationJSON(t, legacy, state)
	r := NewOrganizationReader(home)
	s, err := r.Codex(path, []string{"parent", "delegated"})
	if err != nil || s.Placements["delegated"].ProjectID != "codex:project:project-1" {
		t.Fatal("pending assignment was dropped", err)
	}
	state["app-server-projects-migration-by-host"] = map[string]any{host: map[string]bool{"threadAssignmentsMigrated": true}}
	organizationJSON(t, legacy, state)
	s, err = r.Codex(path, []string{"parent", "delegated"})
	if err != nil || s.Placements["delegated"].ProjectID != "" {
		t.Fatal("finished migration resurrected stale membership", err)
	}
}

func TestOrganizationCodexSchemaFallbackAndDeletion(t *testing.T) {
	home, path := organizationCodexFixture(t)
	legacy := filepath.Join(home, ".codex", ".codex-global-state.json")
	organizationJSON(t, legacy, map[string]any{
		"local-projects":             map[string]any{"legacy": map[string]any{"id": "legacy", "name": "Old project", "rootPaths": []string{"/old"}}},
		"thread-project-assignments": map[string]any{"parent": map[string]any{"projectKind": "local", "projectId": "legacy"}, "missing": map[string]any{"projectKind": "local", "projectId": "legacy"}},
	})
	r := NewOrganizationReader(home)
	if _, err := r.Codex(path, []string{"parent"}); err != nil {
		t.Fatal(err)
	}
	organizationExecSQL(t, path, `DELETE FROM projects;DELETE FROM thread_sections;DELETE FROM threads WHERE id='parent';`)
	s, err := r.Codex(path, []string{"parent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Groups) != 0 || len(s.Placements) != 0 {
		t.Fatalf("deleted native metadata was resurrected from legacy state: %+v", s)
	}
	// An old native schema can still use the verified legacy project schema.
	old := filepath.Join(home, "old.sqlite")
	organizationExecSQL(t, old, `CREATE TABLE threads(id TEXT PRIMARY KEY,title TEXT);INSERT INTO threads VALUES('parent','Existing title');`)
	s, err = r.Codex(old, []string{"parent", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Groups) != 1 || s.Placements["parent"].DisplayTitle != "Existing title" || s.Placements["parent"].ProjectID != "codex:project:legacy" {
		t.Fatalf("old schema fallback: %+v", s)
	}
	if _, ok := s.Placements["missing"]; ok {
		t.Fatal("missing native session became a placeholder")
	}
}

func TestOrganizationCodexLegacyRequiresKnownLocalMembership(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, ".codex", ".codex-global-state.json")
	organizationJSON(t, legacy, map[string]any{
		"local-projects": map[string]any{"one": map[string]any{"id": "one", "name": "One", "rootPaths": []string{"/one"}}},
		"thread-project-assignments": map[string]any{
			"known":   map[string]any{"projectKind": "local", "projectId": "one"},
			"cloud":   map[string]any{"projectKind": "cloud", "projectId": "one"},
			"missing": map[string]any{"projectKind": "local", "projectId": "one"},
		},
	})
	r := NewOrganizationReader(home)
	s, err := r.Codex("", []string{"known", "cloud"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Placements) != 1 || s.Placements["known"].ProjectID != "codex:project:one" {
		t.Fatalf("legacy membership: %+v", s)
	}
}

func TestOrganizationCodexFileReplacementAndParentCycles(t *testing.T) {
	home, path := organizationCodexFixture(t)
	r := NewOrganizationReader(home)
	if _, err := r.Codex(path, []string{"parent", "agent"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(home, "next.sqlite")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(replacement, b, 0600); err != nil {
		t.Fatal(err)
	}
	organizationExecSQL(t, replacement, `UPDATE threads SET name='Different native' WHERE id='parent';INSERT INTO thread_spawn_edges VALUES('agent','parent','open');`)
	if err = os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	s, err := r.Codex(path, []string{"parent", "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Placements["parent"].DisplayTitle != "Different native" {
		t.Fatal("replaced native database did not invalidate the cache")
	}
	if s.Placements["parent"].ParentID != "" || s.Placements["agent"].ParentID != "" {
		t.Fatal("native parent cycle remained")
	}
}
