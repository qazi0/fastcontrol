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

const grokStateMaxBytes = 4 << 20

type grokThreadPref struct {
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
}

type grokStateData struct {
	Pinned   map[string]bool           `json:"pinned,omitempty"`
	Archived map[string]bool           `json:"archived,omitempty"`
	Titles   map[string]string         `json:"titles,omitempty"`
	Prefs    map[string]grokThreadPref `json:"prefs,omitempty"`
}

// grokState persists fastcontrol-local Grok organization and preference state
// under ~/.fastcontrol/grok-state.json using atomic temp+rename writes with
// mode 0600 on POSIX platforms.
type grokState struct {
	mu   sync.RWMutex
	path string
	data grokStateData
}

func defaultGrokStatePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fastcontrol", "grok-state.json")
}

func emptyGrokStateData() grokStateData {
	return grokStateData{
		Pinned:   map[string]bool{},
		Archived: map[string]bool{},
		Titles:   map[string]string{},
		Prefs:    map[string]grokThreadPref{},
	}
}

func cloneGrokStateData(src grokStateData) grokStateData {
	dst := emptyGrokStateData()
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
		if v != (grokThreadPref{}) {
			dst.Prefs[k] = v
		}
	}
	return dst
}

func newGrokState(path string) (*grokState, error) {
	if strings.TrimSpace(path) == "" {
		path = defaultGrokStatePath()
	}
	s := &grokState{
		path: path,
		data: emptyGrokStateData(),
	}
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > grokStateMaxBytes {
		return nil, errors.New("Grok State File Is Invalid Or Too Large")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return s, nil
	}
	var loaded grokStateData
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, err
	}
	s.data = cloneGrokStateData(loaded)
	return s, nil
}

func (s *grokState) saveLocked() error {
	if s.path == "" {
		return errors.New("Grok State Path Is Empty")
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

	f, err := os.CreateTemp(dir, ".grok-state-*")
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

func (s *grokState) Snapshot() grokStateData {
	if s == nil {
		return emptyGrokStateData()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneGrokStateData(s.data)
}

func (s *grokState) IsPinned(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Pinned[id]
}

func (s *grokState) SetPinned(id string, pinned bool) error {
	if s == nil {
		return errors.New("Grok State Is Unavailable")
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

func (s *grokState) IsArchived(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Archived[id]
}

func (s *grokState) SetArchived(id string, archived bool) error {
	if s == nil {
		return errors.New("Grok State Is Unavailable")
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

func (s *grokState) Title(id string) string {
	if s == nil || id == "" {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Titles[id]
}

func (s *grokState) SetTitle(id, title string) (string, error) {
	if s == nil {
		return "", errors.New("Grok State Is Unavailable")
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

func (s *grokState) ClearTitle(id string) error {
	if s == nil {
		return errors.New("Grok State Is Unavailable")
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

func (s *grokState) Pref(id string) grokThreadPref {
	if s == nil || id == "" {
		return grokThreadPref{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Prefs[id]
}

func (s *grokState) SetPref(id string, pref grokThreadPref) error {
	if s == nil {
		return errors.New("Grok State Is Unavailable")
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
	if pref == (grokThreadPref{}) {
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

// Grok organizes top-level Grok conversations with full parity for pinning,
// archiving, custom titles, and workspace project grouping. Subagents are
// excluded from the top-level list.
func (r *OrganizationReader) Grok(archived bool, state *grokState, records []grokRecord) ([]ThreadInfo, []OrganizationGroup) {
	threads, snapshot := grokBuildOrganization(archived, state, records)
	return threads, snapshot.Groups
}

// grokBuildOrganization returns both the organized ThreadInfo slice and the
// normalized OrganizationSnapshot (so callers can also serve op:"group").
func grokBuildOrganization(archived bool, state *grokState, records []grokRecord) ([]ThreadInfo, OrganizationSnapshot) {
	st := emptyGrokStateData()
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

	// Count basenames across workspaces so duplicate folder names get disambiguated.
	baseCounts := map[string]int{}
	seenCwds := map[string]bool{}
	for _, rec := range records {
		if grokRecordIsSubagent(rec) {
			continue
		}
		if st.Archived[rec.ID] != archived {
			continue
		}
		if rec.Cwd != "" && !seenCwds[rec.Cwd] {
			seenCwds[rec.Cwd] = true
			baseCounts[grokWorkspaceBase(rec.Cwd)]++
		}
	}

	threads := make([]ThreadInfo, 0, len(records))
	for _, rec := range records {
		if grokRecordIsSubagent(rec) {
			continue
		}
		isArchived := st.Archived[rec.ID]
		if isArchived != archived {
			continue
		}

		pref := st.Prefs[rec.ID]
		title := grokRecordTitle(rec, "")
		preview := grokRecordPreview(rec)
		info := ThreadInfo{
			Provider: "grok",
			ID:       rec.ID,
			Title:    clipPlain(title, 240),
			Preview:  clipPlain(preview, 160),
			Cwd:      rec.Cwd,
			Source:   "grok",
			Recency:  rec.Recency,
			Rollout:  rec.UpdatesPath,
			Active:   rec.Running || rec.External,
			Running:  rec.Running,
			TurnAt:   rec.TurnAt,
			Pinned:   st.Pinned[rec.ID],
			Branch:   rec.HeadBranch,
			Model:    grokStorageFirstNonEmpty(pref.Model, rec.Model),
			Effort:   grokStorageFirstNonEmpty(pref.Effort, rec.ReasoningEffort),
		}
		threads = append(threads, info)

		placement := SessionPlacement{
			DisplayTitle: st.Titles[rec.ID],
			Archived:     isArchived,
		}

		if rec.Cwd != "" {
			gid := organizationScopedID("grok", rec.Cwd, "project")
			acc, ok := projectGroups[gid]
			if !ok {
				acc = &groupAccum{
					group: OrganizationGroup{
						ID:    gid,
						Name:  grokWorkspaceDisplayName(rec.Cwd, baseCounts),
						Kind:  "project",
						Roots: []string{rec.Cwd},
					},
					rootSet: map[string]bool{rec.Cwd: true},
				}
				projectGroups[gid] = acc
			}
			if rec.Recency > acc.recency {
				acc.recency = rec.Recency
			}
			placement.ProjectID = gid
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

func grokWorkspaceBase(cwd string) string {
	base := filepath.Base(cwd)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "Home"
	}
	return base
}

func grokWorkspaceDisplayName(cwd string, baseCounts map[string]int) string {
	base := grokWorkspaceBase(cwd)
	if baseCounts[base] > 1 {
		parent := filepath.Base(filepath.Dir(cwd))
		if parent != "" && parent != "." && parent != string(filepath.Separator) {
			return parent + "/" + base
		}
	}
	return base
}

// grokProjects builds the folder-based Project summary list for non-archived,
// top-level Grok conversations.
func grokProjects(records []grokRecord, state *grokState) []Project {
	st := emptyGrokStateData()
	if state != nil {
		st = state.Snapshot()
	}
	byCwd := map[string]*Project{}
	for _, rec := range records {
		if grokRecordIsSubagent(rec) || st.Archived[rec.ID] || rec.Cwd == "" {
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
