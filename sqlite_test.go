package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func openTestSQLiteWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteFileURI(path, "_pragma=busy_timeout(2000)&_pragma=wal_autocheckpoint(0)"))
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		t.Fatalf("enable WAL: %v", err)
	}
	return db
}

func TestSQLiteWALConcurrentReadOnlyWithActiveWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state_5.sqlite")
	writer := openTestSQLiteWriter(t, path)
	defer writer.Close()

	if _, err := writer.Exec(`
		CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT);
		INSERT INTO items VALUES (1, 'checkpointed');
		PRAGMA wal_checkpoint(FULL);
		INSERT INTO items VALUES (2, 'wal-committed');
	`); err != nil {
		t.Fatalf("seed WAL db: %v", err)
	}

	// Keep an open write transaction active while readers query concurrently.
	tx, err := writer.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO items VALUES (3, 'uncommitted')`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert in tx: %v", err)
	}

	old := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	_ = os.Chtimes(path, old, old)
	_ = os.Chtimes(path+"-wal", old, old)
	dbBefore, err := os.Stat(path)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("stat db: %v", err)
	}
	walBefore, err := os.Stat(path + "-wal")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("stat wal: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				data, err := sqliteJSON(path, "SELECT id, label FROM items ORDER BY id")
				if err != nil {
					errCh <- err
					return
				}
				var rows []struct {
					ID    int    `json:"id"`
					Label string `json:"label"`
				}
				if err := json.Unmarshal(data, &rows); err != nil {
					errCh <- err
					return
				}
				if len(rows) != 2 || rows[0].Label != "checkpointed" || rows[1].Label != "wal-committed" {
					errCh <- errors.New("unexpected WAL snapshot: " + string(data))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		_ = tx.Rollback()
		t.Fatalf("concurrent reader failed: %v", err)
	}

	// Verify read-only enforcement rejects writes even while WAL is active.
	if _, err := sqliteJSON(path, "INSERT INTO items VALUES (99, 'forbidden')"); err == nil {
		_ = tx.Rollback()
		t.Fatal("read-only sqliteJSON permitted INSERT")
	}

	// Verify read-only access did not touch database or WAL modification times.
	dbAfter, err := os.Stat(path)
	if err != nil || !dbAfter.ModTime().Equal(dbBefore.ModTime()) {
		_ = tx.Rollback()
		t.Fatalf("database mtime changed: before=%v after=%v err=%v", dbBefore.ModTime(), dbAfter.ModTime(), err)
	}
	walAfter, err := os.Stat(path + "-wal")
	if err != nil || !walAfter.ModTime().Equal(walBefore.ModTime()) {
		_ = tx.Rollback()
		t.Fatalf("wal mtime changed: before=%v after=%v err=%v", walBefore.ModTime(), walAfter.ModTime(), err)
	}

	// Readers must not leave locks that block the writer's commit.
	if err := tx.Commit(); err != nil {
		t.Fatalf("writer commit blocked: %v", err)
	}

	data, err := sqliteJSON(path, "SELECT id, label FROM items ORDER BY id")
	if err != nil {
		t.Fatalf("post-commit read: %v", err)
	}
	var rows []struct {
		ID    int    `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("unmarshal post-commit: %v", err)
	}
	if len(rows) != 3 || rows[2].Label != "uncommitted" {
		t.Fatalf("post-commit WAL row missing: %s", data)
	}
}

func TestSQLiteWALCleanlyClosedWithoutSHM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clean_wal.sqlite")
	writer := openTestSQLiteWriter(t, path)
	if _, err := writer.Exec(`CREATE TABLE t (v INTEGER); INSERT INTO t VALUES (42);`); err != nil {
		writer.Close()
		t.Fatalf("seed db: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	_ = os.Remove(path + "-shm")
	_ = os.Remove(path + "-wal")

	data, err := sqliteJSON(path, "SELECT v FROM t")
	if err != nil {
		t.Fatalf("WAL read without -shm failed: %v", err)
	}
	var rows []struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(data, &rows); err != nil || len(rows) != 1 || rows[0].V != 42 {
		t.Fatalf("unexpected result: %s err=%v", data, err)
	}
}

func TestSQLiteWALReadOnlyDirectoryFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory chmod semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read-only permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox_wal.sqlite")
	writer := openTestSQLiteWriter(t, path)
	if _, err := writer.Exec(`CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('ok');`); err != nil {
		writer.Close()
		t.Fatalf("seed db: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	_ = os.Remove(path + "-shm")
	_ = os.Remove(path + "-wal")

	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0755) })

	data, err := sqliteJSON(path, "SELECT v FROM t")
	if err != nil {
		t.Fatalf("read-only dir immutable fallback failed: %v", err)
	}
	var rows []struct {
		V string `json:"v"`
	}
	if err := json.Unmarshal(data, &rows); err != nil || len(rows) != 1 || rows[0].V != "ok" {
		t.Fatalf("unexpected fallback output: %s err=%v", data, err)
	}
	if _, err := sqliteJSON(path, "INSERT INTO t VALUES ('no')"); err == nil {
		t.Fatal("immutable fallback permitted write")
	}
}

func TestSQLiteMissingFileAndSpecialPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.sqlite")
	if _, err := sqliteJSON(missing, "SELECT 1"); err == nil {
		t.Fatal("expected error for missing sqlite database")
	}

	special := filepath.Join(dir, "state #1 ? with spaces.sqlite")
	writer := openTestSQLiteWriter(t, special)
	defer writer.Close()
	if _, err := writer.Exec(`CREATE TABLE s (k TEXT); INSERT INTO s VALUES ('hello');`); err != nil {
		t.Fatalf("seed special db: %v", err)
	}
	data, err := sqliteJSON(special, "SELECT k FROM s")
	if err != nil {
		t.Fatalf("special path read failed: %v", err)
	}
	var rows []struct {
		K string `json:"k"`
	}
	if err := json.Unmarshal(data, &rows); err != nil || len(rows) != 1 || rows[0].K != "hello" {
		t.Fatalf("unexpected special path output: %s err=%v", data, err)
	}
}

func TestSQLiteThreadsProjectsAndArbitraryWALDB(t *testing.T) {
	dir := t.TempDir()
	codexDB := filepath.Join(dir, "state_5.sqlite")
	writer := openTestSQLiteWriter(t, codexDB)
	defer writer.Close()
	if _, err := writer.Exec(`
		CREATE TABLE threads (
			id TEXT PRIMARY KEY,
			name TEXT,
			title TEXT,
			first_user_message TEXT,
			preview TEXT,
			cwd TEXT,
			source TEXT,
			thread_source TEXT,
			recency_at_ms INTEGER,
			rollout_path TEXT,
			is_pinned INTEGER,
			git_branch TEXT,
			model TEXT,
			reasoning_effort TEXT,
			approval_mode TEXT,
			sandbox_policy TEXT,
			archived INTEGER
		);
		INSERT INTO threads VALUES (
			't1', NULL, 'First Thread', 'Prompt', 'Preview text', '/work/app',
			'desktop', NULL, 1700000000000, '', 1, NULL, NULL, NULL, NULL, '{"type":"disabled"}', 0
		);
		INSERT INTO threads VALUES (
			't2', 'Renamed', 'Old', 'Prompt 2', 'Second preview', '/work/app',
			'cli', 'subagent', 1700000001000, '', 0, 'main', 'gpt-5', 'high', 'on-request', NULL, 0
		);
	`); err != nil {
		t.Fatalf("seed codex db: %v", err)
	}

	threads, err := listThreads(codexDB)
	if err != nil {
		t.Fatalf("listThreads: %v", err)
	}
	if len(threads) != 2 {
		t.Fatalf("expected 2 threads, got %d", len(threads))
	}
	if threads[0].ID != "t1" || !threads[0].Pinned || threads[0].Sandbox != "dangerFullAccess" || threads[0].Title != "First Thread" {
		t.Fatalf("unexpected first thread: %+v", threads[0])
	}
	if threads[1].ID != "t2" || !threads[1].Subagent || threads[1].Title != "Renamed" || threads[1].Branch != "main" {
		t.Fatalf("unexpected second thread: %+v", threads[1])
	}

	projects, err := listProjects(codexDB)
	if err != nil {
		t.Fatalf("listProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].Cwd != "/work/app" || projects[0].Threads != 1 {
		t.Fatalf("unexpected projects: %+v", projects)
	}

	// Verify arbitrary WAL-mode database like Antigravity conversation_summaries.db.
	agyDB := filepath.Join(dir, "conversation_summaries.db")
	agyWriter := openTestSQLiteWriter(t, agyDB)
	defer agyWriter.Close()
	if _, err := agyWriter.Exec(`
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			step_count INTEGER NOT NULL DEFAULT 0,
			last_modified_time DATETIME NOT NULL,
			not_fully_idle NUMERIC NOT NULL DEFAULT false
		);
		INSERT INTO conversation_summaries VALUES (
			'conv-1', 'Test Summary', 'CASCADE_RUN_STATUS_RUNNING', 12, '2026-11-27T13:00:00Z', 1
		);
	`); err != nil {
		t.Fatalf("seed agy db: %v", err)
	}
	data, err := sqliteJSON(agyDB, "SELECT conversation_id, title, status, step_count FROM conversation_summaries")
	if err != nil {
		t.Fatalf("agy sqliteJSON: %v", err)
	}
	var agyRows []struct {
		ID        string `json:"conversation_id"`
		Title     string `json:"title"`
		Status    string `json:"status"`
		StepCount int    `json:"step_count"`
	}
	if err := json.Unmarshal(data, &agyRows); err != nil || len(agyRows) != 1 || agyRows[0].ID != "conv-1" || agyRows[0].Status != "CASCADE_RUN_STATUS_RUNNING" || agyRows[0].StepCount != 12 {
		t.Fatalf("unexpected agy rows: %s err=%v", data, err)
	}
}
