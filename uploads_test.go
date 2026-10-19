package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadDocumentsAndImageValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		name, mime, body string
		valid            bool
	}{
		{"report.pdf", "application/pdf", "%PDF-1.7\nreport", true},
		{"notes.txt", "text/plain", "notes", true},
		{"empty.bin", "application/octet-stream", "", true},
		{"bad.png", "image/png", "not an image", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/upload", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.mime)
			r.Header.Set("X-File-Name", url.QueryEscape("../"+tc.name))
			path, err := saveUpload(r)
			if !tc.valid {
				if err == nil {
					t.Fatal("invalid image accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(path)
			if err != nil || string(b) != tc.body || filepath.Dir(path) != uploadDir() || !strings.HasSuffix(path, "-"+tc.name) {
				t.Fatalf("upload mismatch: %s %v", path, err)
			}
			if err := checkUploads([]string{path}); err != nil {
				t.Fatal(err)
			}
		})
	}
	r := httptest.NewRequest("POST", "/upload", bytes.NewReader(make([]byte, (25<<20)+1)))
	if _, err := saveUpload(r); err == nil {
		t.Fatal("oversized upload accepted")
	}
	matches, _ := filepath.Glob(filepath.Join(uploadDir(), ".upload-*"))
	if len(matches) != 0 {
		t.Fatal("temporary upload leaked")
	}
	if err := checkUploads([]string{uploadDir()}); err == nil {
		t.Fatal("directory accepted")
	}
	outside := filepath.Join(t.TempDir(), "private.txt")
	os.WriteFile(outside, []byte("private"), 0600)
	link := filepath.Join(uploadDir(), "link.txt")
	if os.Symlink(outside, link) == nil && checkUploads([]string{link}) == nil {
		t.Fatal("symlink accepted")
	}
}

func TestUploadedFilesReachEveryProviderAsLocalReferences(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := httptest.NewRequest("POST", "/upload", strings.NewReader("%PDF-1.7\n"))
	r.Header.Set("Content-Type", "application/pdf")
	r.Header.Set("X-File-Name", "report.pdf")
	path, err := saveUpload(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"codex", "claude"} {
		for _, op := range []string{"new", "send"} {
			raw := mustJSON(map[string]any{"op": op, "provider": provider, "nonce": "n", "text": "Read the attached report", "files": []string{path}, "images": []string{"unchanged-image"}})
			normalized, err := withUploadedFiles(raw)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			json.Unmarshal(normalized, &m)
			if !strings.Contains(str(m, "text"), "Read the attached report") || !strings.Contains(str(m, "text"), path) || m["provider"] != provider || m["files"] != nil || m["nonce"] != "n" || len(arrayValue(m["images"])) != 1 {
				t.Fatalf("lost input: %s", normalized)
			}
		}
	}
	for _, files := range []any{[]string{"/etc/passwd"}, "bad", []int{1}} {
		if _, err := withUploadedFiles(mustJSON(map[string]any{"files": files})); err == nil {
			t.Fatalf("accepted %v", files)
		}
	}
}
