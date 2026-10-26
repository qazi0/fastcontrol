package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/journal"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

type claudeDesktopOrganization struct {
	SessionID string   `json:"sessionId"`
	CLI       string   `json:"cliSessionId"`
	BridgeIDs []string `json:"bridgeSessionIds"`
	Title     string   `json:"title"`
	Archived  bool     `json:"isArchived"`
	Detached  bool     `json:"lineageDetached"`
	Fork      string   `json:"forkedFromSessionId"`
	Spawn     *struct {
		SessionID     string          `json:"sessionId"`
		Side          bool            `json:"sideSession"`
		UserFork      bool            `json:"userFork"`
		DeletedParent json.RawMessage `json:"deletedParent"`
	} `json:"spawnedFrom"`
	Lineage *struct {
		SessionID string `json:"sessionId"`
		Kind      string `json:"kind"`
	} `json:"lineage"`
	scope string
	path  string
}

type claudeSidebarStore struct {
	State struct {
		Current map[string]json.RawMessage `json:"codeSidebarByScope"`
		Legacy  map[string]json.RawMessage `json:"customGroupsByScope"`
	} `json:"state"`
}

type claudeSidebarSection struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Order   int      `json:"order"`
	Members []string `json:"members"`
}

// Claude enriches only CLI IDs supplied by the relay's native transcript index.
// The metadata and sidebar databases are never used to create controllable chats.
func (r *OrganizationReader) Claude(localIDs []string) (OrganizationSnapshot, error) {
	if len(localIDs) > organizationRowLimit {
		return emptyOrganization(), errors.New("too many indexed Claude sessions")
	}
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	known, ids := organizationIDs(localIDs)
	root, store := organizationSourcePaths(r.home)
	paths, err := claudeOrganizationPaths(root, store)
	if err != nil {
		return r.previous("claude", ids, err)
	}
	stamps, err := organizationStamps(paths)
	if err != nil {
		return r.previous("claude", ids, err)
	}
	if s, ok := r.cached("claude", ids, stamps); ok {
		return s, nil
	}
	s, err := loadClaudeOrganization(root, store, known)
	if err != nil {
		return r.previous("claude", ids, err)
	}
	afterPaths, err := claudeOrganizationPaths(root, store)
	if err != nil {
		return r.previous("claude", ids, err)
	}
	after, err := organizationStamps(afterPaths)
	if err != nil || !sameOrganizationStamps(stamps, after) {
		return r.previous("claude", ids, errors.New("Claude organization changed during refresh"))
	}
	normalizeOrganization(&s)
	return r.publish("claude", ids, after, s), nil
}

func claudeOrganizationPaths(root, store string) ([]string, error) {
	paths := []string{root}
	for _, pattern := range []string{"local_*.json", "deleted_*"} {
		matched, err := filepath.Glob(filepath.Join(root, "*", "*", pattern))
		if err != nil {
			return nil, err
		}
		paths = append(paths, matched...)
	}
	storePaths, err := claudeLevelPaths(store)
	if err != nil {
		return nil, err
	}
	paths = append(paths, storePaths...)
	if len(paths) > organizationRowLimit {
		return nil, errors.New("Claude organization has too many source files")
	}
	return paths, nil
}

func claudeLevelPaths(store string) ([]string, error) {
	paths := []string{store}
	entries, err := os.ReadDir(store)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "CURRENT" || strings.HasPrefix(name, "MANIFEST-") || strings.HasSuffix(name, ".ldb") || strings.HasSuffix(name, ".sst") || strings.HasSuffix(name, ".log") {
			paths = append(paths, filepath.Join(store, name))
		}
	}
	if len(paths) > 2048 {
		return nil, errors.New("Claude sidebar has too many source files")
	}
	return paths, nil
}

func claudeScope(root, path string) string {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || !organizationUUID(parts[0]) || !organizationUUID(parts[1]) {
		return ""
	}
	return strings.Join(parts, "/")
}

func organizationUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func loadClaudeOrganization(root, store string, known map[string]bool) (OrganizationSnapshot, error) {
	s := emptyOrganization()
	paths, _ := filepath.Glob(filepath.Join(root, "*", "*", "local_*.json"))
	if len(paths) > organizationRowLimit {
		return s, errors.New("too many Claude desktop sessions")
	}
	byDesktop := map[string]claudeDesktopOrganization{}
	byCLI := map[string]claudeDesktopOrganization{}
	ambiguous := map[string]bool{}
	var bytesRead int64
	for _, path := range paths {
		scope := claudeScope(root, path)
		if scope == "" {
			continue
		}
		b, err := organizationReadFile(path, organizationFileLimit)
		if err != nil {
			return s, fmt.Errorf("read Claude session organization: %w", err)
		}
		bytesRead += int64(len(b))
		if bytesRead > organizationStoreLimit {
			return s, errors.New("Claude metadata exceeds its total size limit")
		}
		var record claudeDesktopOrganization
		if json.Unmarshal(b, &record) != nil || !known[record.CLI] || !strings.HasPrefix(record.SessionID, "local_") || !organizationUUID(strings.TrimPrefix(record.SessionID, "local_")) {
			continue
		}
		if strings.TrimSuffix(filepath.Base(path), ".json") != record.SessionID {
			continue
		}
		tombstone := filepath.Join(filepath.Dir(path), "deleted_"+strings.TrimPrefix(record.SessionID, "local_"))
		if _, err := os.Lstat(tombstone); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return s, err
		}
		record.scope, record.path = scope, path
		if _, ok := byCLI[record.CLI]; ok {
			ambiguous[record.CLI] = true
			continue
		}
		byCLI[record.CLI] = record
		byDesktop[scope+"/"+record.SessionID] = record
	}
	for id := range ambiguous {
		delete(byCLI, id)
	}
	for id, record := range byCLI {
		p := SessionPlacement{DisplayTitle: record.Title, Archived: record.Archived, Detached: record.Detached}
		parent, relation := "", ""
		if !record.Detached {
			if record.Lineage != nil && record.Lineage.SessionID != "" {
				parent = record.Lineage.SessionID
				relation = "delegated"
				if record.Lineage.Kind == "fork" || record.Lineage.Kind == "user_fork" {
					relation = "fork"
				}
			} else if record.Spawn != nil && (len(record.Spawn.DeletedParent) == 0 || string(record.Spawn.DeletedParent) == "null") {
				parent = record.Spawn.SessionID
				relation = "delegated"
				if record.Spawn.UserFork || record.Fork == parent {
					relation = "fork"
				}
			} else if record.Fork != "" {
				parent = record.Fork
				relation = "fork"
			}
		}
		if target, ok := byDesktop[record.scope+"/"+parent]; ok && !ambiguous[target.CLI] && !target.Archived {
			p.ParentID, p.Relation = target.CLI, relation
		}
		s.Placements[id] = p
	}
	data, err := readClaudeSidebarStore(store)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if len(data) == 0 {
		return s, nil
	}
	var sidebar claudeSidebarStore
	if err = json.Unmarshal(data, &sidebar); err != nil {
		return s, errors.New("Claude sidebar organization is not valid JSON")
	}
	if len(sidebar.State.Current) > 1000 || len(sidebar.State.Legacy) > 1000 {
		return s, errors.New("too many Claude sidebar scopes")
	}
	byMember := map[string]string{}
	for id, record := range byCLI {
		if record.Archived || len(record.BridgeIDs) > 1000 {
			continue
		}
		byMember[record.scope+"/code:"+record.SessionID] = id
		for _, bridge := range record.BridgeIDs {
			if !organizationID(bridge) {
				continue
			}
			key := record.scope + "/code:" + bridge
			if existing := byMember[key]; existing != "" && existing != id {
				byMember[key] = "!ambiguous"
			} else {
				byMember[key] = id
			}
		}
	}
	apply := func(scope string, sections []claudeSidebarSection) {
		if len(sections) > 1000 {
			return
		}
		for _, section := range sections {
			if section.Kind != "manual" || !organizationID(section.ID) || organizationText(section.Name) == "" || len(section.Members) > organizationRowLimit {
				continue
			}
			gid := organizationScopedID("claude", scope, section.ID)
			s.Groups = append(s.Groups, OrganizationGroup{ID: gid, Name: section.Name, Kind: "group", Order: section.Order})
			for pos, member := range section.Members {
				id := byMember[scope+"/"+member]
				p, ok := s.Placements[id]
				if !ok || p.Archived {
					continue
				}
				p.GroupIDs = append(p.GroupIDs, gid)
				p.Position = pos
				s.Placements[id] = p
			}
		}
	}
	for scope, raw := range sidebar.State.Current {
		if !claudeKnownScope(scope, byCLI) {
			continue
		}
		var current struct {
			Sections []claudeSidebarSection `json:"sections"`
		}
		if json.Unmarshal(raw, &current) != nil {
			return s, errors.New("invalid Claude sidebar sections")
		}
		apply(scope, current.Sections)
	}
	for scope, raw := range sidebar.State.Legacy {
		if _, current := sidebar.State.Current[scope]; current || !claudeKnownScope(scope, byCLI) {
			continue
		}
		var old struct {
			Groups      []struct{ ID, Name string } `json:"groups"`
			Assignments map[string]string           `json:"assignments"`
		}
		if json.Unmarshal(raw, &old) != nil || len(old.Groups) > 1000 || len(old.Assignments) > organizationRowLimit {
			return s, errors.New("invalid legacy Claude groups")
		}
		sections := make([]claudeSidebarSection, 0, len(old.Groups))
		for i, g := range old.Groups {
			sections = append(sections, claudeSidebarSection{ID: g.ID, Name: g.Name, Kind: "manual", Order: i})
		}
		for member, gid := range old.Assignments {
			for i := range sections {
				if sections[i].ID == gid {
					sections[i].Members = append(sections[i].Members, member)
					break
				}
			}
		}
		for i := range sections {
			sort.Strings(sections[i].Members)
		}
		apply(scope, sections)
	}
	return s, nil
}

func claudeKnownScope(scope string, records map[string]claudeDesktopOrganization) bool {
	for _, r := range records {
		if r.scope == scope {
			return true
		}
	}
	return false
}

// Copy complete source files to an isolated in-memory storage. Checking the
// complete inventory before and after rejects concurrent compaction or appends.
// The LevelDB reader locks only that copy, never Electron's live database.
func readClaudeSidebarStore(path string) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		b, err := readClaudeSidebarSnapshot(path)
		if err == nil || os.IsNotExist(err) {
			return b, err
		}
		if attempt == 2 {
			return nil, err
		}
	}
	return nil, errors.New("Claude sidebar snapshot is unavailable")
}

func readClaudeSidebarSnapshot(path string) ([]byte, error) {
	paths, err := claudeLevelPaths(path)
	if err != nil {
		return nil, err
	}
	before, err := organizationStamps(paths)
	if err != nil {
		return nil, err
	}
	current, err := organizationReadFile(filepath.Join(path, "CURRENT"), 4096)
	if err != nil {
		return nil, err
	}
	manifest, ok := claudeLevelFile(strings.TrimSpace(string(current)))
	if !ok || manifest.Type != storage.TypeManifest {
		return nil, errors.New("invalid Claude sidebar manifest")
	}
	mem := storage.NewMemStorage()
	defer mem.Close()
	var total int64
	descriptors := map[storage.FileDesc]bool{}
	for _, p := range paths {
		fd, ok := claudeLevelFile(filepath.Base(p))
		if !ok {
			continue
		}
		if descriptors[fd] {
			return nil, errors.New("duplicate Claude sidebar storage file")
		}
		descriptors[fd] = true
		data, err := organizationReadFile(p, organizationStoreLimit)
		if err != nil {
			return nil, err
		}
		if fd.Type == storage.TypeJournal {
			if err = validateClaudeJournal(data); err != nil {
				return nil, err
			}
		}
		total += int64(len(data))
		if total > organizationStoreLimit {
			return nil, errors.New("Claude sidebar exceeds its snapshot size limit")
		}
		w, err := mem.Create(fd)
		if err != nil {
			return nil, err
		}
		_, writeErr := w.Write(data)
		closeErr := w.Close()
		if writeErr != nil {
			return nil, writeErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	afterPaths, err := claudeLevelPaths(path)
	if err != nil {
		return nil, err
	}
	after, err := organizationStamps(afterPaths)
	if err != nil || !sameOrganizationStamps(before, after) {
		return nil, errors.New("Claude sidebar changed during snapshot")
	}
	if err = mem.SetMeta(manifest); err != nil {
		return nil, err
	}
	// Chromium may retain valid batches older than its manifest checkpoint.
	// Validate their checksums and structure above, then let normal recovery
	// skip stale sequence numbers while keeping strict table/manifest checks.
	db, err := leveldb.Open(mem, &opt.Options{ReadOnly: true, ErrorIfMissing: true, Strict: opt.StrictAll &^ opt.StrictJournal, BlockCacheCapacity: 1 << 20, OpenFilesCacheCapacity: 32})
	if err != nil {
		return nil, fmt.Errorf("could not read the Claude sidebar snapshot: %w", err)
	}
	defer db.Close()
	value, err := db.Get([]byte("_https://claude.ai\x00\x01dframe-store"), nil)
	if errors.Is(err, leveldb.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("could not read Claude sidebar preferences")
	}
	return decodeClaudeStorageValue(value)
}

func validateClaudeJournal(data []byte) error {
	r := journal.NewReader(bytes.NewReader(data), nil, true, true)
	for {
		reader, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New("invalid Claude sidebar journal checksum")
		}
		record, err := io.ReadAll(io.LimitReader(reader, organizationStoreLimit+1))
		if err != nil || len(record) < 12 || len(record) > organizationStoreLimit {
			return errors.New("invalid Claude sidebar journal record")
		}
		var batch leveldb.Batch
		seq, count := binary.LittleEndian.Uint64(record), binary.LittleEndian.Uint32(record[8:])
		if seq > (1<<56)-1 || uint64(count) > (1<<56)-1-seq || batch.Load(record[12:]) != nil || uint32(batch.Len()) != count {
			return errors.New("invalid Claude sidebar journal batch")
		}
	}
}

func claudeLevelFile(name string) (storage.FileDesc, bool) {
	var fd storage.FileDesc
	var number string
	if strings.HasPrefix(name, "MANIFEST-") {
		fd.Type = storage.TypeManifest
		number = strings.TrimPrefix(name, "MANIFEST-")
	} else {
		switch filepath.Ext(name) {
		case ".log":
			fd.Type = storage.TypeJournal
		case ".ldb", ".sst":
			fd.Type = storage.TypeTable
		default:
			return fd, false
		}
		number = strings.TrimSuffix(name, filepath.Ext(name))
	}
	if len(number) == 0 || len(number) > 20 {
		return fd, false
	}
	for _, c := range number {
		if c < '0' || c > '9' {
			return fd, false
		}
	}
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil || n < 0 {
		return fd, false
	}
	fd.Num = n
	return fd, true
}

func decodeClaudeStorageValue(value []byte) ([]byte, error) {
	if len(value) < 1 || len(value) > organizationFileLimit {
		return nil, errors.New("Claude sidebar preference exceeds its size limit")
	}
	switch value[0] {
	case 1:
		runes := make([]rune, len(value)-1)
		for i, b := range value[1:] {
			runes[i] = rune(b)
		}
		return []byte(string(runes)), nil
	case 0:
		if (len(value)-1)%2 != 0 {
			return nil, errors.New("invalid Claude UTF-16 preference")
		}
		units := make([]uint16, (len(value)-1)/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(value[1+2*i:])
		}
		return []byte(string(utf16.Decode(units))), nil
	case '{':
		if utf8.Valid(value) {
			return value, nil
		}
	}
	return nil, errors.New("unsupported Claude preference encoding")
}
