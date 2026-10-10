package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func TestMain(m *testing.M) {
	if os.Getenv("FASTCONTROL_TEST_SPEECH_HELPER") == "1" {
		for i, arg := range os.Args {
			if arg == "--output-file" && i+1 < len(os.Args) {
				path := os.Args[i+1]
				_ = os.WriteFile(filepath.Join(filepath.Dir(os.Args[0]), "path"), []byte(path), 0600)
				_ = os.WriteFile(path+".txt", []byte("Spoken Draft.\n"), 0600)
				os.Exit(0)
			}
		}
		os.Exit(1)
	}
	os.Exit(m.Run())
}
func TestWindowsSpeechCleanup(t *testing.T) {
	t.Setenv("FASTCONTROL_TEST_SPEECH_HELPER", "1")
	dir := t.TempDir()
	bin := filepath.Join(dir, "whisper-cli.exe")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(bin, data, 0700); err != nil {
		t.Fatal(err)
	}
	s := &Speech{bin: bin, model: "whisper.cpp:base-q5_1", engine: "whispercpp", modelPath: "C:\\model with spaces.bin", busy: make(chan struct{}, 1)}
	request := httptest.NewRequest("POST", "/transcribe", bytes.NewReader(testVoiceWAV()))
	request.Header.Set("Content-Type", "audio/wav")
	response := httptest.NewRecorder()
	s.serve(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Spoken Draft.") {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	path, err := os.ReadFile(filepath.Join(dir, "path"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{string(path), string(path) + ".txt"} {
		if _, err = os.Stat(name); !os.IsNotExist(err) {
			t.Fatal("temporary voice file remained")
		}
	}
	if len(s.busy) != 0 {
		t.Fatal("voice slot leaked")
	}
}
func TestWindowsSQLiteWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state with spaces.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE sample (id TEXT, value INTEGER); INSERT INTO sample VALUES ('fresh', 7)")
	if err != nil {
		t.Fatal(err)
	}
	data, err := sqliteJSON(path, "SELECT id,value FROM sample")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err = json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["id"] != "fresh" || rows[0]["value"] != float64(7) {
		t.Fatalf("WAL row: %s", data)
	}
}
func TestWindowsNamedPipe(t *testing.T) {
	path := `\\.\pipe\fastcontrol-test-` + uuid()
	listener, err := winio.ListenPipe(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer connection.Close()
		_, err = connection.Write([]byte("native pipe"))
		done <- err
	}()
	connection, err := dialDesktopIPC(path)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	data := make([]byte, 11)
	if _, err = io.ReadFull(connection, data); err != nil {
		t.Fatal(err)
	}
	if string(data) != "native pipe" {
		t.Fatalf("pipe bytes: %q", data)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
