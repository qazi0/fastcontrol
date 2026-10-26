package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"
)

const (
	organizationFileLimit  = 8 << 20
	organizationStoreLimit = 64 << 20
	organizationRowLimit   = 20000
)

// OrganizationSnapshot contains only sessions already present on this host.
// Readers receive their own copy; a caller cannot mutate the cached snapshot.
type OrganizationSnapshot struct {
	Groups     []OrganizationGroup         `json:"groups"`
	Placements map[string]SessionPlacement `json:"placements"`
}

type OrganizationGroup struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Kind  string   `json:"kind"`
	Order int      `json:"order"`
	Roots []string `json:"roots,omitempty"`
}

type SessionPlacement struct {
	DisplayTitle string   `json:"displayTitle,omitempty"`
	ProjectID    string   `json:"projectId,omitempty"`
	GroupIDs     []string `json:"groupIds,omitempty"`
	ParentID     string   `json:"parentId,omitempty"`
	Relation     string   `json:"relation,omitempty"`
	Position     int      `json:"position,omitempty"`
	Archived     bool     `json:"archived,omitempty"`
	Detached     bool     `json:"-"`
}

// Apply optional native presentation metadata to fresh thread values. Source
// subagents remain read-only; full child sessions keep their normal controls.
func applyThreadOrganization(threads []ThreadInfo, s OrganizationSnapshot) []ThreadInfo {
	out := make([]ThreadInfo, len(threads))
	for i, t := range threads {
		t.DisplayTitle, t.ProjectID, t.Relation = "", "", ""
		t.GroupIDs = nil
		if !t.Subagent {
			t.ParentID = ""
		}
		if p, ok := s.Placements[t.ID]; ok {
			t.DisplayTitle, t.ProjectID, t.Relation = p.DisplayTitle, p.ProjectID, p.Relation
			t.GroupIDs = append([]string(nil), p.GroupIDs...)
			if p.ParentID != "" {
				t.ParentID = p.ParentID
			}
			if p.Relation == "subagent" {
				t.Subagent = true
			}
		}
		out[i] = t
	}
	return out
}

type organizationStamp struct {
	path string
	info os.FileInfo
}

type organizationCache struct {
	stamps   []organizationStamp
	ids      string
	snapshot OrganizationSnapshot
}

// OrganizationReader serializes refresh work separately from cache access.
// Source readers build complete values before publishing under mu. No map or
// slice reachable from a published value is subsequently mutated.
type OrganizationReader struct {
	home      string
	mu        sync.Mutex
	refreshMu sync.Mutex
	cache     map[string]organizationCache
}

func NewOrganizationReader(home string) *OrganizationReader {
	return &OrganizationReader{home: home, cache: make(map[string]organizationCache)}
}

func emptyOrganization() OrganizationSnapshot {
	return OrganizationSnapshot{Groups: []OrganizationGroup{}, Placements: map[string]SessionPlacement{}}
}

func cloneOrganization(s OrganizationSnapshot) OrganizationSnapshot {
	out := emptyOrganization()
	out.Groups = append(out.Groups, s.Groups...)
	for i := range out.Groups {
		out.Groups[i].Roots = append([]string(nil), out.Groups[i].Roots...)
	}
	for id, p := range s.Placements {
		p.GroupIDs = append([]string(nil), p.GroupIDs...)
		out.Placements[id] = p
	}
	return out
}

func sameOrganizationStamps(a, b []organizationStamp) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].path != b[i].path {
			return false
		}
		x, y := a[i].info, b[i].info
		if x == nil || y == nil {
			if x != nil || y != nil {
				return false
			}
			continue
		}
		if x.Size() != y.Size() || !x.ModTime().Equal(y.ModTime()) || !os.SameFile(x, y) {
			return false
		}
	}
	return true
}

func organizationStamps(paths []string) ([]organizationStamp, error) {
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	stamps := make([]organizationStamp, 0, len(paths))
	for _, path := range paths {
		info, err := organizationSourceInfo(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("organization source is a symbolic link")
		}
		stamps = append(stamps, organizationStamp{path, info})
	}
	return stamps, nil
}

// Windows path metadata may defer loading its file ID until SameFile is called.
// Capture it from an open handle before caching, while this path still names it.
func organizationSourceInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || runtime.GOOS != "windows" || info.Mode()&os.ModeSymlink != 0 {
		return info, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	captured, err := f.Stat()
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if info.Size() != captured.Size() || !info.ModTime().Equal(captured.ModTime()) || !os.SameFile(info, captured) {
		return nil, errors.New("organization source changed while capturing its identity")
	}
	return captured, nil
}

func (r *OrganizationReader) cached(key, ids string, stamps []organizationStamp) (OrganizationSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.cache[key]
	if !ok || c.ids != ids || !sameOrganizationStamps(c.stamps, stamps) {
		return OrganizationSnapshot{}, false
	}
	return cloneOrganization(c.snapshot), true
}

func (r *OrganizationReader) publish(key, ids string, stamps []organizationStamp, s OrganizationSnapshot) OrganizationSnapshot {
	r.mu.Lock()
	r.cache[key] = organizationCache{stamps: stamps, ids: ids, snapshot: s}
	r.mu.Unlock()
	return cloneOrganization(s)
}

func (r *OrganizationReader) previous(key, ids string, err error) (OrganizationSnapshot, error) {
	r.mu.Lock()
	c, ok := r.cache[key]
	r.mu.Unlock()
	if ok && c.ids == ids {
		return cloneOrganization(c.snapshot), err
	}
	return emptyOrganization(), err
}

func organizationIDs(ids []string) (map[string]bool, string) {
	known := make(map[string]bool, len(ids))
	for _, id := range ids {
		if organizationID(id) {
			known[id] = true
		}
	}
	ordered := make([]string, 0, len(known))
	for id := range known {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	sum := sha256.Sum256([]byte(strings.Join(ordered, "\x00")))
	return known, hex.EncodeToString(sum[:])
}

func organizationID(id string) bool {
	if len(id) == 0 || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if c < 33 || c == '/' || c == '\\' || c == 127 {
			return false
		}
	}
	return true
}

func organizationText(value string) string {
	value = strings.TrimSpace(strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return ' '
		}
		return c
	}, value))
	r := []rune(value)
	if len(r) > 1024 {
		value = string(r[:1024])
	}
	return value
}

func organizationReadFile(path string, limit int64) ([]byte, error) {
	before, err := organizationSourceInfo(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return nil, errors.New("organization source exceeds its size limit or is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("organization source changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := organizationSourceInfo(path)
	if err != nil || len(b) > int(limit) || !sameOrganizationStamps([]organizationStamp{{path, before}}, []organizationStamp{{path, after}}) {
		return nil, errors.New("organization source changed while reading")
	}
	return b, nil
}

func organizationScopedID(provider, scope, id string) string {
	sum := sha256.Sum256([]byte(scope))
	return fmt.Sprintf("%s:%x:%s", provider, sum[:8], id)
}

// Normalize drops invalid edges and removes every edge participating in a cycle.
// Missing parents stay at the root; they never become invented sessions.
func normalizeOrganization(s *OrganizationSnapshot) {
	groups := make(map[string]bool, len(s.Groups))
	filtered := s.Groups[:0]
	for _, g := range s.Groups {
		g.Name = organizationText(g.Name)
		if g.ID == "" || len(g.ID) > 512 || g.Name == "" || groups[g.ID] {
			continue
		}
		groups[g.ID] = true
		filtered = append(filtered, g)
	}
	s.Groups = filtered
	for id, p := range s.Placements {
		p.DisplayTitle = organizationText(p.DisplayTitle)
		seen := map[string]bool{}
		p.GroupIDs = append([]string(nil), p.GroupIDs...)
		out := p.GroupIDs[:0]
		for _, g := range p.GroupIDs {
			if groups[g] && !seen[g] {
				out = append(out, g)
				seen[g] = true
			}
		}
		p.GroupIDs = out
		if !groups[p.ProjectID] {
			p.ProjectID = ""
		}
		parent, exists := s.Placements[p.ParentID]
		if p.ParentID == id || !exists || parent.Archived || p.Archived {
			p.ParentID = ""
			p.Relation = ""
		}
		s.Placements[id] = p
	}
	bad := map[string]bool{}
	done := map[string]bool{}
	for start := range s.Placements {
		positions := map[string]int{}
		chain := []string{}
		id := start
		for id != "" && !done[id] {
			if at, ok := positions[id]; ok {
				for _, x := range chain[at:] {
					bad[x] = true
				}
				break
			}
			if len(chain) >= 128 {
				for _, x := range chain {
					bad[x] = true
				}
				break
			}
			positions[id] = len(chain)
			chain = append(chain, id)
			id = s.Placements[id].ParentID
		}
		for _, x := range chain {
			done[x] = true
		}
	}
	for id := range bad {
		p := s.Placements[id]
		p.ParentID = ""
		p.Relation = ""
		s.Placements[id] = p
	}
	sort.SliceStable(s.Groups, func(i, j int) bool {
		if s.Groups[i].Order != s.Groups[j].Order {
			return s.Groups[i].Order < s.Groups[j].Order
		}
		return s.Groups[i].ID < s.Groups[j].ID
	})
}

func organizationSourcePaths(home string) (string, string) {
	base := filepath.Join(home, "Library", "Application Support", "Claude")
	if runtime.GOOS == "windows" {
		base = filepath.Join(home, "AppData", "Roaming", "Claude")
	} else if runtime.GOOS == "linux" {
		base = filepath.Join(home, ".config", "Claude")
	}
	if current, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == filepath.Clean(current) {
		if runtime.GOOS == "windows" || runtime.GOOS == "linux" {
			if config, err := os.UserConfigDir(); err == nil {
				base = filepath.Join(config, "Claude")
			}
		}
	}
	return filepath.Join(base, "claude-code-sessions"), filepath.Join(base, "Local Storage", "leveldb")
}
