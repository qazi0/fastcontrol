package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const antigravityStateMaxBytes = 4 << 20

type antigravityThreadPref struct {
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
}

type antigravityStateData struct {
	Pinned   map[string]bool                  `json:"pinned,omitempty"`
	Archived map[string]bool                  `json:"archived,omitempty"`
	Titles   map[string]string                `json:"titles,omitempty"`
	Prefs    map[string]antigravityThreadPref `json:"prefs,omitempty"`
}

// antigravityState persists fastcontrol-local Antigravity organization and
// preference state under ~/.fastcontrol/antigravity-state.json using atomic
// temp+rename writes with mode 0600.
type antigravityState struct {
	mu   sync.RWMutex
	path string
	data antigravityStateData
}

func defaultAntigravityStatePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fastcontrol", "antigravity-state.json")
}

func emptyAntigravityStateData() antigravityStateData {
	return antigravityStateData{
		Pinned:   map[string]bool{},
		Archived: map[string]bool{},
		Titles:   map[string]string{},
		Prefs:    map[string]antigravityThreadPref{},
	}
}

func cloneAntigravityStateData(src antigravityStateData) antigravityStateData {
	dst := emptyAntigravityStateData()
	for k, v := range src.Pinned {
		if v {
			dst.Pinned[k] = true
		}
	}
	for k, v := range src.Archived {
		if v {
			dst.Archived[k] = true
		}
	}
	for k, v := range src.Titles {
		if v != "" {
			dst.Titles[k] = v
		}
	}
	for k, v := range src.Prefs {
		if v != (antigravityThreadPref{}) {
			dst.Prefs[k] = v
		}
	}
	return dst
}

func newAntigravityState(path string) (*antigravityState, error) {
	if strings.TrimSpace(path) == "" {
		path = defaultAntigravityStatePath()
	}
	s := &antigravityState{
		path: path,
		data: emptyAntigravityStateData(),
	}
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > antigravityStateMaxBytes {
		return nil, errors.New("Antigravity State File Is Invalid Or Too Large")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return s, nil
	}
	var loaded antigravityStateData
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, err
	}
	s.data = cloneAntigravityStateData(loaded)
	return s, nil
}

func (s *antigravityState) saveLocked() error {
	if s.path == "" {
		return errors.New("Antigravity State Path Is Empty")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	f, err := os.CreateTemp(dir, ".antigravity-state-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)

	if runtime.GOOS != "windows" {
		if err := f.Chmod(0600); err != nil {
			_ = f.Close()
			return err
		}
	}
	if _, err := f.Write(encoded); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	return nil
}

func (s *antigravityState) Snapshot() antigravityStateData {
	if s == nil {
		return emptyAntigravityStateData()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneAntigravityStateData(s.data)
}

func (s *antigravityState) IsPinned(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Pinned[id]
}

func (s *antigravityState) SetPinned(id string, pinned bool) error {
	if s == nil {
		return errors.New("Antigravity State Is Unavailable")
	}
	if !organizationID(id) {
		return errors.New("Invalid Conversation ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.data.Pinned[id]
	if pinned {
		s.data.Pinned[id] = true
	} else {
		delete(s.data.Pinned, id)
	}
	if err := s.saveLocked(); err != nil {
		if had {
			s.data.Pinned[id] = prev
		} else {
			delete(s.data.Pinned, id)
		}
		return err
	}
	return nil
}

func (s *antigravityState) IsArchived(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Archived[id]
}

func (s *antigravityState) SetArchived(id string, archived bool) error {
	if s == nil {
		return errors.New("Antigravity State Is Unavailable")
	}
	if !organizationID(id) {
		return errors.New("Invalid Conversation ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.data.Archived[id]
	if archived {
		s.data.Archived[id] = true
	} else {
		delete(s.data.Archived, id)
	}
	if err := s.saveLocked(); err != nil {
		if had {
			s.data.Archived[id] = prev
		} else {
			delete(s.data.Archived, id)
		}
		return err
	}
	return nil
}

func (s *antigravityState) Title(id string) string {
	if s == nil || id == "" {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Titles[id]
}

func (s *antigravityState) SetTitle(id, title string) (string, error) {
	if s == nil {
		return "", errors.New("Antigravity State Is Unavailable")
	}
	if !organizationID(id) {
		return "", errors.New("Invalid Conversation ID")
	}
	clean, err := sessionTitle(title)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.data.Titles[id]
	s.data.Titles[id] = clean
	if err := s.saveLocked(); err != nil {
		if had {
			s.data.Titles[id] = prev
		} else {
			delete(s.data.Titles, id)
		}
		return "", err
	}
	return clean, nil
}

func (s *antigravityState) ClearTitle(id string) error {
	if s == nil {
		return errors.New("Antigravity State Is Unavailable")
	}
	if !organizationID(id) {
		return errors.New("Invalid Conversation ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.data.Titles[id]
	delete(s.data.Titles, id)
	if err := s.saveLocked(); err != nil {
		if had {
			s.data.Titles[id] = prev
		}
		return err
	}
	return nil
}

func (s *antigravityState) Pref(id string) antigravityThreadPref {
	if s == nil || id == "" {
		return antigravityThreadPref{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Prefs[id]
}

func (s *antigravityState) SetPref(id string, pref antigravityThreadPref) error {
	if s == nil {
		return errors.New("Antigravity State Is Unavailable")
	}
	if !organizationID(id) {
		return errors.New("Invalid Conversation ID")
	}
	pref.Model = strings.TrimSpace(pref.Model)
	pref.Effort = strings.TrimSpace(pref.Effort)
	pref.PermissionMode = strings.TrimSpace(pref.PermissionMode)

	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.data.Prefs[id]
	if pref == (antigravityThreadPref{}) {
		delete(s.data.Prefs, id)
	} else {
		s.data.Prefs[id] = pref
	}
	if err := s.saveLocked(); err != nil {
		if had {
			s.data.Prefs[id] = prev
		} else {
			delete(s.data.Prefs, id)
		}
		return err
	}
	return nil
}

// Antigravity organizes top-level Antigravity conversations with full parity
// for pinning, archiving, custom titles, workspace project grouping, and
// native group IDs. Subagents are excluded from the top-level list.
func (r *OrganizationReader) Antigravity(archived bool, state *antigravityState, records []antigravityRecord) ([]ThreadInfo, []OrganizationGroup) {
	threads, snapshot := antigravityBuildOrganization(archived, state, records)
	return threads, snapshot.Groups
}

// antigravityBuildOrganization returns both the organized ThreadInfo slice and
// the normalized OrganizationSnapshot (so callers can also serve op:"group").
func antigravityBuildOrganization(archived bool, state *antigravityState, records []antigravityRecord) ([]ThreadInfo, OrganizationSnapshot) {
	st := emptyAntigravityStateData()
	if state != nil {
		st = state.Snapshot()
	}

	snap := emptyOrganization()

	type groupAccum struct {
		group   OrganizationGroup
		recency int64
		rootSet map[string]bool
	}
	projectGroups := map[string]*groupAccum{}
	manualGroups := map[string]*groupAccum{}

	// Count basenames across workspaces so duplicate folder names get disambiguated.
	baseCounts := map[string]int{}
	seenCwds := map[string]bool{}
	for _, rec := range records {
		if antigravityRecordIsSubagent(rec) {
			continue
		}
		if st.Archived[rec.ID] != archived {
			continue
		}
		if rec.Cwd != "" && !seenCwds[rec.Cwd] {
			seenCwds[rec.Cwd] = true
			baseCounts[antigravityWorkspaceBase(rec.Cwd)]++
		}
	}

	threads := make([]ThreadInfo, 0, len(records))
	for _, rec := range records {
		if antigravityRecordIsSubagent(rec) {
			continue
		}
		isArchived := st.Archived[rec.ID]
		if isArchived != archived {
			continue
		}

		pref := st.Prefs[rec.ID]
		source := rec.Source
		if source == "" {
			source = "antigravity"
		}
		title := antigravityFirstNonEmpty(rec.Title, rec.AnnotationTitle, clipPlain(rec.Preview, 120))
		info := ThreadInfo{
			Provider: "antigravity",
			ID:       rec.ID,
			Title:    clipPlain(title, 240),
			Preview:  clipPlain(rec.Preview, 160),
			Cwd:      rec.Cwd,
			Source:   source,
			Recency:  rec.Recency,
			Rollout:  rec.TranscriptPath,
			Active:   rec.Running,
			Running:  rec.Running,
			Pinned:   st.Pinned[rec.ID],
			Model:    pref.Model,
			Effort:   pref.Effort,
		}
		threads = append(threads, info)

		placement := SessionPlacement{
			DisplayTitle: st.Titles[rec.ID],
			Archived:     isArchived,
		}

		// Project group: group by workspace directory (Cwd), or fall back to
		// ProjectID only when a conversation has no workspace_uris.
		if rec.Cwd != "" {
			gid := organizationScopedID("antigravity", rec.Cwd, "project")
			acc, ok := projectGroups[gid]
			if !ok {
				acc = &groupAccum{
					group: OrganizationGroup{
						ID:    gid,
						Name:  antigravityWorkspaceDisplayName(rec.Cwd, baseCounts),
						Kind:  "project",
						Roots: []string{rec.Cwd},
					},
					rootSet: map[string]bool{rec.Cwd: true},
				}
				projectGroups[gid] = acc
			}
			for _, wp := range rec.WorkspacePaths {
				if wp != "" && !acc.rootSet[wp] {
					acc.rootSet[wp] = true
					acc.group.Roots = append(acc.group.Roots, wp)
				}
			}
			if rec.Recency > acc.recency {
				acc.recency = rec.Recency
			}
			placement.ProjectID = gid
			placement.GroupIDs = append(placement.GroupIDs, gid)
		} else if rec.ProjectID != "" && organizationID(rec.ProjectID) {
			gid := "antigravity:project:" + rec.ProjectID
			acc, ok := projectGroups[gid]
			if !ok {
				acc = &groupAccum{
					group: OrganizationGroup{
						ID:   gid,
						Name: rec.ProjectID,
						Kind: "project",
					},
					rootSet: map[string]bool{},
				}
				projectGroups[gid] = acc
			}
			if rec.Recency > acc.recency {
				acc.recency = rec.Recency
			}
			placement.ProjectID = gid
			placement.GroupIDs = append(placement.GroupIDs, gid)
		}

		if rec.GroupID != "" && organizationID(rec.GroupID) {
			gid := "antigravity:group:" + rec.GroupID
			acc, ok := manualGroups[gid]
			if !ok {
				acc = &groupAccum{
					group: OrganizationGroup{
						ID:   gid,
						Name: rec.GroupID,
						Kind: "group",
					},
					rootSet: map[string]bool{},
				}
				manualGroups[gid] = acc
			}
			if rec.Recency > acc.recency {
				acc.recency = rec.Recency
			}
			placement.GroupIDs = append(placement.GroupIDs, gid)
		}

		snap.Placements[rec.ID] = placement
	}

	sortedProjects := make([]*groupAccum, 0, len(projectGroups))
	for _, acc := range projectGroups {
		sortedProjects = append(sortedProjects, acc)
	}
	sort.Slice(sortedProjects, func(i, j int) bool {
		if sortedProjects[i].recency != sortedProjects[j].recency {
			return sortedProjects[i].recency > sortedProjects[j].recency
		}
		return sortedProjects[i].group.ID < sortedProjects[j].group.ID
	})
	for i, acc := range sortedProjects {
		acc.group.Order = i
		snap.Groups = append(snap.Groups, acc.group)
	}

	sortedManual := make([]*groupAccum, 0, len(manualGroups))
	for _, acc := range manualGroups {
		sortedManual = append(sortedManual, acc)
	}
	sort.Slice(sortedManual, func(i, j int) bool {
		if sortedManual[i].recency != sortedManual[j].recency {
			return sortedManual[i].recency > sortedManual[j].recency
		}
		return sortedManual[i].group.ID < sortedManual[j].group.ID
	})
	offset := len(sortedProjects)
	for i, acc := range sortedManual {
		acc.group.Order = offset + i
		snap.Groups = append(snap.Groups, acc.group)
	}

	normalizeOrganization(&snap)

	sort.Slice(threads, func(i, j int) bool {
		if threads[i].Pinned != threads[j].Pinned {
			return threads[i].Pinned
		}
		if threads[i].Recency != threads[j].Recency {
			return threads[i].Recency > threads[j].Recency
		}
		return threads[i].ID < threads[j].ID
	})

	threads = applyThreadOrganization(threads, snap)
	return threads, snap
}

func antigravityWorkspaceBase(cwd string) string {
	base := filepath.Base(cwd)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "Home"
	}
	return base
}

func antigravityWorkspaceDisplayName(cwd string, baseCounts map[string]int) string {
	base := antigravityWorkspaceBase(cwd)
	if baseCounts[base] > 1 {
		parent := filepath.Base(filepath.Dir(cwd))
		if parent != "" && parent != "." && parent != string(filepath.Separator) {
			return parent + "/" + base
		}
	}
	return base
}

// antigravityProjects builds the folder-based Project summary list for
// non-archived, top-level Antigravity conversations.
func antigravityProjects(records []antigravityRecord, state *antigravityState) []Project {
	st := emptyAntigravityStateData()
	if state != nil {
		st = state.Snapshot()
	}
	byCwd := map[string]*Project{}
	for _, rec := range records {
		if antigravityRecordIsSubagent(rec) || st.Archived[rec.ID] || rec.Cwd == "" {
			continue
		}
		p, ok := byCwd[rec.Cwd]
		if !ok {
			p = &Project{Cwd: rec.Cwd}
			byCwd[rec.Cwd] = p
		}
		p.Threads++
		if rec.Recency > p.Recency {
			p.Recency = rec.Recency
		}
	}
	out := make([]Project, 0, len(byCwd))
	for _, p := range byCwd {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Recency != out[j].Recency {
			return out[i].Recency > out[j].Recency
		}
		return out[i].Cwd < out[j].Cwd
	})
	return out
}
