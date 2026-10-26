package main

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

func (m *ClaudeManager) requestRefresh() {
	if !m.refreshJob.CompareAndSwap(false, true) {
		return
	}
	go func() { defer m.refreshJob.Store(false); m.refresh() }()
}

func codexOrganizationIDs(db string, fallback []ThreadInfo) []string {
	var rows []struct {
		ID string `json:"id"`
	}
	if data, err := sqliteJSON(db, `select id from threads where archived=0 and preview<>'' limit 20001`); err == nil && json.Unmarshal(data, &rows) == nil && len(rows) <= organizationRowLimit {
		ids := make([]string, len(rows))
		for i := range rows {
			ids[i] = rows[i].ID
		}
		return ids
	}
	ids := make([]string, len(fallback))
	for i := range fallback {
		ids[i] = fallback[i].ID
	}
	return ids
}

func organizationMembers(s OrganizationSnapshot, group string) (map[string]bool, bool) {
	found := false
	for _, g := range s.Groups {
		if g.ID == group {
			found = true
			break
		}
	}
	if !found {
		return nil, false
	}
	members := map[string]bool{}
	for id, p := range s.Placements {
		if p.Archived || p.Relation == "subagent" {
			continue
		}
		for _, g := range p.GroupIDs {
			if g == group {
				members[id] = true
				break
			}
		}
	}
	// A full child inherits its parent's place in the tree even when it has no
	// explicit sidebar assignment. Subagents use the separate transcript viewer.
	for pass := 0; pass < 32; pass++ {
		changed := false
		for id, p := range s.Placements {
			if !members[id] && !p.Archived && p.Relation != "subagent" && members[p.ParentID] {
				members[id] = true
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return members, true
}

func addOrganizationParents(ids map[string]bool, s OrganizationSnapshot) {
	for pass := 0; pass < 32; pass++ {
		add := []string{}
		for id := range ids {
			parent := s.Placements[id].ParentID
			if p, ok := s.Placements[parent]; parent != "" && ok && !p.Archived && p.Relation != "subagent" && !ids[parent] {
				add = append(add, parent)
			}
		}
		if len(add) == 0 {
			return
		}
		for _, id := range add {
			ids[id] = true
		}
	}
}

func organizationIDQuery(ids map[string]bool) string {
	values := make([]string, 0, len(ids))
	for id := range ids {
		values = append(values, sqlQuote(id))
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

func (h *Hub) organizationGroup(id string) ([]ThreadInfo, error) {
	h.mu.Lock()
	s := h.orgState
	fallback := append([]ThreadInfo(nil), h.threads...)
	h.mu.Unlock()
	if h.organization != nil && h.db != "" {
		if current, err := h.organization.Codex(h.db, codexOrganizationIDs(h.db, fallback)); err == nil {
			s = current
		}
	}
	h.links.apply(&s)
	ids, ok := organizationMembers(s, id)
	if !ok {
		return nil, errors.New("This group is no longer available")
	}
	addOrganizationParents(ids, s)
	if len(ids) == 0 {
		return []ThreadInfo{}, nil
	}
	if len(ids) > organizationRowLimit {
		return nil, errors.New("This group has too many sessions")
	}
	threads, err := queryThreads(h.db, threadCols+` and id in (`+organizationIDQuery(ids)+`) order by recency_at_ms desc limit 1000`)
	if err != nil {
		threads = []ThreadInfo{}
		for _, t := range fallback {
			if ids[t.ID] {
				threads = append(threads, t)
			}
		}
	}
	return applyThreadOrganization(threads, s), nil
}

func (m *ClaudeManager) organizationGroup(id string) ([]ThreadInfo, error) {
	m.mu.Lock()
	s := m.orgState
	data := m.lastList
	m.mu.Unlock()
	ids, ok := organizationMembers(s, id)
	if !ok {
		return nil, errors.New("This group is no longer available")
	}
	addOrganizationParents(ids, s)
	var list struct {
		Threads []ThreadInfo `json:"threads"`
	}
	if json.Unmarshal(data, &list) != nil {
		return nil, errors.New("Sessions are still loading")
	}
	threads := []ThreadInfo{}
	for _, t := range list.Threads {
		if ids[t.ID] {
			threads = append(threads, t)
		}
	}
	return threads, nil
}
