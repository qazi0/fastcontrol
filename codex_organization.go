package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Codex reads native project, section and spawn data without changing Codex's
// database. localIDs is the relay's existing thread index, not remote group IDs.
func (r *OrganizationReader) Codex(db string, localIDs []string) (OrganizationSnapshot, error) {
	if len(localIDs) > organizationRowLimit {
		return emptyOrganization(), errors.New("too many indexed Codex sessions")
	}
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	known, ids := organizationIDs(localIDs)
	legacy := filepath.Join(r.home, ".codex", ".codex-global-state.json")
	key := "codex:" + db
	paths := []string{legacy}
	if db != "" {
		paths = append(paths, db, db+"-wal")
	}
	before, err := organizationStamps(paths)
	if err != nil {
		return r.previous(key, ids, err)
	}
	if s, ok := r.cached(key, ids, before); ok {
		return s, nil
	}
	s, err := loadCodexOrganization(db, legacy, known)
	if err != nil {
		return r.previous(key, ids, err)
	}
	after, err := organizationStamps(paths)
	if err != nil || !sameOrganizationStamps(before, after) {
		return r.previous(key, ids, errors.New("Codex organization changed during refresh"))
	}
	normalizeOrganization(&s)
	return r.publish(key, ids, after, s), nil
}

type codexOrganizationThread struct {
	ID       string `json:"id"`
	Title    string `json:"display_title"`
	Project  string `json:"project_id"`
	Section  string `json:"section_id"`
	Position int    `json:"position"`
	Archived int    `json:"archived"`
	Source   string `json:"source"`
	Kind     string `json:"thread_source"`
}

func organizationSQL(db, query string, out any) error {
	b, err := sqliteJSON(db, query)
	if err != nil {
		return errors.New("could not read native Codex organization")
	}
	if len(b) > organizationStoreLimit {
		return errors.New("Codex organization exceeds its size limit")
	}
	if len(b) == 0 {
		b = []byte("[]")
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("invalid native Codex organization result")
	}
	return nil
}

func loadCodexOrganization(db, legacy string, known map[string]bool) (OrganizationSnapshot, error) {
	s := emptyOrganization()
	nativeProjects := false
	nativeThreads := false
	nativeGroups := map[string]bool{}
	if db != "" {
		if _, err := os.Stat(db); err == nil {
			var tables []struct {
				Name string `json:"name"`
			}
			if err = organizationSQL(db, `select name from sqlite_master where type='table' and name in ('threads','projects','project_roots','thread_sections','thread_spawn_edges')`, &tables); err != nil {
				return s, err
			}
			has := map[string]bool{}
			for _, t := range tables {
				has[t.Name] = true
			}
			nativeProjects = has["projects"]
			if has["projects"] {
				var projects []struct {
					ID, Name string
					Position int
				}
				if err = organizationSQL(db, `select id,name,position from projects order by position,id limit 20001`, &projects); err != nil {
					return s, err
				}
				if len(projects) > organizationRowLimit {
					return s, errors.New("too many native Codex projects")
				}
				for _, p := range projects {
					if !organizationID(p.ID) {
						continue
					}
					id := "codex:project:" + p.ID
					nativeGroups[id] = true
					s.Groups = append(s.Groups, OrganizationGroup{ID: id, Name: p.Name, Kind: "project", Order: p.Position})
				}
			}
			if has["project_roots"] {
				var roots []struct {
					ProjectID string `json:"project_id"`
					Path      string `json:"path"`
				}
				if err = organizationSQL(db, `select project_id,path from project_roots order by project_id,position limit 20001`, &roots); err != nil {
					return s, err
				}
				if len(roots) > organizationRowLimit {
					return s, errors.New("too many native Codex project roots")
				}
				byGroup := map[string][]string{}
				for _, root := range roots {
					if root.Path != "" && len(root.Path) < 32768 {
						byGroup["codex:project:"+root.ProjectID] = append(byGroup["codex:project:"+root.ProjectID], root.Path)
					}
				}
				for i := range s.Groups {
					s.Groups[i].Roots = byGroup[s.Groups[i].ID]
				}
			}
			if has["thread_sections"] {
				var sections []struct{ ID, Name string }
				if err = organizationSQL(db, `select id,name from thread_sections order by id limit 20001`, &sections); err != nil {
					return s, err
				}
				if len(sections) > organizationRowLimit {
					return s, errors.New("too many native Codex sections")
				}
				for i, g := range sections {
					if organizationID(g.ID) {
						id := "codex:section:" + g.ID
						nativeGroups[id] = true
						s.Groups = append(s.Groups, OrganizationGroup{ID: id, Name: g.Name, Kind: "group", Order: i})
					}
				}
			}
			if has["threads"] && len(known) > 0 {
				nativeThreads = true
				var columns []struct {
					Name string `json:"name"`
				}
				if err = organizationSQL(db, `select name from pragma_table_info('threads')`, &columns); err != nil {
					return s, err
				}
				cols := map[string]bool{}
				for _, c := range columns {
					cols[c.Name] = true
				}
				col := func(name, fallback string) string {
					if cols[name] {
						return name
					}
					return fallback
				}
				title := []string{}
				for _, c := range []string{"name", "title"} {
					if cols[c] {
						title = append(title, "nullif("+c+",'')")
					}
				}
				title = append(title, "''")
				selects := []string{"id", "coalesce(" + strings.Join(title, ",") + ") as display_title", col("project_id", "''") + " as project_id", col("thread_section_id", "''") + " as section_id", col("section_position", "0") + " as position", col("archived", "0") + " as archived", col("source", "''") + " as source", col("thread_source", "''") + " as thread_source"}
				// SQLite coalesce requires at least two arguments on older schemas.
				if len(title) == 1 {
					selects[1] = "'' as display_title"
				}
				ordered := make([]string, 0, len(known))
				for id := range known {
					ordered = append(ordered, id)
				}
				sort.Strings(ordered)
				quoted := make([]string, 0, len(ordered))
				for _, id := range ordered {
					quoted = append(quoted, sqlQuote(id))
				}
				if len(quoted) > organizationRowLimit {
					return s, errors.New("too many indexed Codex sessions")
				}
				query := "select " + strings.Join(selects, ",") + " from threads where id in (" + strings.Join(quoted, ",") + ") limit 20001"
				var threads []codexOrganizationThread
				if err = organizationSQL(db, query, &threads); err != nil {
					return s, err
				}
				for _, t := range threads {
					if !known[t.ID] {
						continue
					}
					p := SessionPlacement{DisplayTitle: t.Title, Archived: t.Archived != 0, Position: t.Position}
					project, section := "codex:project:"+t.Project, "codex:section:"+t.Section
					if !p.Archived && nativeGroups[project] {
						p.ProjectID = project
						p.GroupIDs = append(p.GroupIDs, project)
					}
					if !p.Archived && nativeGroups[section] {
						p.GroupIDs = append(p.GroupIDs, section)
					}
					var source struct {
						Fork     string `json:"forked_from_id"`
						Subagent struct {
							Spawn struct {
								Parent string `json:"parent_thread_id"`
							} `json:"thread_spawn"`
						} `json:"subagent"`
					}
					if json.Unmarshal([]byte(t.Source), &source) == nil {
						if source.Fork != "" {
							p.ParentID, p.Relation = source.Fork, "fork"
						} else if source.Subagent.Spawn.Parent != "" {
							p.ParentID, p.Relation = source.Subagent.Spawn.Parent, "subagent"
						}
					}
					s.Placements[t.ID] = p
				}
				if has["thread_spawn_edges"] {
					var edges []struct {
						Parent string `json:"parent_thread_id"`
						Child  string `json:"child_thread_id"`
					}
					query = `select parent_thread_id,child_thread_id from thread_spawn_edges where child_thread_id in (` + strings.Join(quoted, ",") + `) limit 20001`
					if err = organizationSQL(db, query, &edges); err != nil {
						return s, err
					}
					if len(edges) > organizationRowLimit {
						return s, errors.New("too many Codex parent relations")
					}
					parents := map[string]string{}
					ambiguous := map[string]bool{}
					for _, edge := range edges {
						if old := parents[edge.Child]; old != "" && old != edge.Parent {
							ambiguous[edge.Child] = true
						}
						parents[edge.Child] = edge.Parent
					}
					kinds := map[string]string{}
					sources := map[string]string{}
					for _, t := range threads {
						kinds[t.ID] = t.Kind
						sources[t.ID] = t.Source
					}
					for id, parent := range parents {
						p, ok := s.Placements[id]
						if !ok || p.Relation == "fork" {
							continue
						}
						if ambiguous[id] {
							p.ParentID, p.Relation = "", ""
						} else {
							isSubagent := p.Relation == "subagent"
							p.ParentID, p.Relation = parent, "delegated"
							if isSubagent || kinds[id] == "subagent" || kinds[id] == "guardian_review" || !codexFullChild(ThreadInfo{Kind: kinds[id], Source: sources[id]}) {
								p.Relation = "subagent"
							}
						}
						s.Placements[id] = p
					}
				}
			}
		} else if !os.IsNotExist(err) {
			return s, fmt.Errorf("inspect Codex organization: %w", err)
		}
	}
	if !nativeProjects {
		eligible := known
		if nativeThreads {
			eligible = make(map[string]bool, len(s.Placements))
			for id := range s.Placements {
				eligible[id] = true
			}
		}
		if err := loadLegacyCodexProjects(legacy, eligible, &s); err != nil {
			return s, err
		}
	} else if err := loadCodexMigratingAssignments(legacy, db, known, &s); err != nil {
		return s, err
	}
	return s, nil
}

// Project definitions can migrate before membership. The native desktop keeps
// explicit pending assignments and their old-to-new ID map during that phase.
func loadCodexMigratingAssignments(path, db string, known map[string]bool, s *OrganizationSnapshot) error {
	data, err := organizationReadFile(path, organizationFileLimit)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state struct {
		Mappings  map[string]map[string]string `json:"app-server-project-id-by-legacy-project-id-by-host"`
		Migration map[string]struct {
			AssignmentsDone *bool `json:"threadAssignmentsMigrated"`
		} `json:"app-server-projects-migration-by-host"`
		Assignments map[string]struct {
			Kind string `json:"projectKind"`
			ID   string `json:"projectId"`
		} `json:"thread-project-assignments"`
		Hosts map[string]string `json:"thread-project-membership-host-ids"`
	}
	if json.Unmarshal(data, &state) != nil {
		return errors.New("invalid Codex project migration metadata")
	}
	host := "local:" + filepath.Clean(filepath.Dir(db))
	migration, ok := state.Migration[host]
	if !ok || migration.AssignmentsDone == nil || *migration.AssignmentsDone {
		return nil
	}
	groups := map[string]bool{}
	for _, group := range s.Groups {
		groups[group.ID] = true
	}
	for id, a := range state.Assignments {
		if !known[id] || a.Kind != "local" || (state.Hosts[id] != "" && state.Hosts[id] != "local") {
			continue
		}
		p, exists := s.Placements[id]
		if !exists || p.Archived || p.ProjectID != "" {
			continue
		}
		mapped := state.Mappings[host][a.ID]
		gid := "codex:project:" + mapped
		if mapped == "" || !groups[gid] {
			continue
		}
		p.ProjectID = gid
		p.GroupIDs = append(p.GroupIDs, gid)
		s.Placements[id] = p
	}
	return nil
}

func loadLegacyCodexProjects(path string, known map[string]bool, s *OrganizationSnapshot) error {
	b, err := organizationReadFile(path, organizationFileLimit)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state struct {
		Projects map[string]struct {
			ID, Name string
			Roots    []string `json:"rootPaths"`
		} `json:"local-projects"`
		Assignments map[string]struct {
			Kind string `json:"projectKind"`
			ID   string `json:"projectId"`
		} `json:"thread-project-assignments"`
		Order []string `json:"project-order"`
	}
	if json.Unmarshal(b, &state) != nil {
		return errors.New("invalid legacy Codex project metadata")
	}
	if len(state.Projects) > organizationRowLimit || len(state.Assignments) > organizationRowLimit || len(state.Order) > organizationRowLimit {
		return errors.New("legacy Codex project metadata exceeds its row limit")
	}
	positions := map[string]int{}
	for i, id := range state.Order {
		positions[id] = i
	}
	groups := map[string]bool{}
	for key, p := range state.Projects {
		if p.ID != key || !organizationID(p.ID) || len(p.Roots) > 1000 {
			continue
		}
		id := "codex:project:" + p.ID
		groups[id] = true
		s.Groups = append(s.Groups, OrganizationGroup{ID: id, Name: p.Name, Kind: "project", Order: positions[key], Roots: p.Roots})
	}
	for id, a := range state.Assignments {
		if !known[id] || a.Kind != "local" {
			continue
		}
		gid := "codex:project:" + a.ID
		if !groups[gid] {
			continue
		}
		p := s.Placements[id]
		if p.Archived {
			continue
		}
		p.ProjectID = gid
		p.GroupIDs = append(p.GroupIDs, gid)
		s.Placements[id] = p
	}
	return nil
}
