package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLinuxSQLiteWALAndReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state # with spaces.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE sample (value INTEGER); INSERT INTO sample VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	data, err := sqliteJSON(path, "SELECT value FROM sample")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if json.Unmarshal(data, &rows) != nil || len(rows) != 1 || rows[0]["value"] != float64(7) {
		t.Fatalf("WAL: %s", data)
	}
	if _, err = sqliteJSON(path, "INSERT INTO sample VALUES (8)"); err == nil {
		t.Fatal("read-only connection wrote data")
	}
}
func TestLinuxUnixSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ipc.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connection, err := dialDesktopIPC(path)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
}
func TestLinuxProcessGroupStop(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	prepareProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer killProcess(cmd.Process)
	interruptProcess(cmd.Process)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("process group ignored interrupt")
	}
}
func TestLinuxCompressedVoiceDecode(t *testing.T) {
	if !compressedVoiceAvailable() {
		t.Skip("FFmpeg is not installed")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "voice.m4a")
	output := filepath.Join(dir, "voice.wav")
	cmd := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=310", "-ar", "16000", "-ac", "1", "-c:a", "aac", "-b:a", "32k", input)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("AAC fixture: %v %s", err, data)
	}
	decodedBytes, err := decodeVoice(context.Background(), input, output)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil || int64(len(data)) != decodedBytes {
		t.Fatal("decoded byte count", err)
	}
	if err = validateVoice(data); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(input); err != nil || st.Size() >= int64(len(data)) {
		t.Fatal("fixture was not compressed")
	}
}
