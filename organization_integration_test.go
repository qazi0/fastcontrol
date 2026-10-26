package main

import "testing"

func TestOrganizationGroupInheritsFullChildrenAndKeepsAgentBoundary(t *testing.T) {
	s := OrganizationSnapshot{Groups: []OrganizationGroup{{ID: "work"}}, Placements: map[string]SessionPlacement{
		"parent": {GroupIDs: []string{"work"}}, "full": {ParentID: "parent", Relation: "delegated"}, "agent": {ParentID: "parent", Relation: "subagent"}, "archived": {ParentID: "parent", Archived: true},
	}}
	ids, ok := organizationMembers(s, "work")
	if !ok || len(ids) != 2 || !ids["parent"] || !ids["full"] {
		t.Fatal("group did not preserve full child membership", ids)
	}
	if _, ok = organizationMembers(s, "unknown"); ok {
		t.Fatal("unknown group accepted")
	}
	input := []ThreadInfo{{ID: "full", Source: "desktop"}, {ID: "agent", Subagent: true}}
	out := applyThreadOrganization(input, s)
	if out[0].Subagent || out[0].ParentID != "parent" || !out[1].Subagent {
		t.Fatal("full sessions and subagents were conflated")
	}
	if input[0].ParentID != "" {
		t.Fatal("organization mutated a published source thread")
	}
}
