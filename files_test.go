package main

import (
	"bytes"
	"mime"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestFileDownloadsPreserveAnyTypeAndRange(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"audio.mp3", "archive.zip", "document.pdf", "source.html", "unicode résumé.custom", "empty.bin"} {
		data := []byte{0, 0xff, 1, 2, 3, 4}
		if name == "empty.bin" {
			data = nil
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		endpoint := "/file?path=" + url.QueryEscape(path)
		for _, method := range []string{"GET", "HEAD"} {
			w := httptest.NewRecorder()
			serveFile(w, httptest.NewRequest(method, endpoint, nil))
			disposition, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
			if w.Code != 200 || err != nil || disposition != "attachment" || params["filename"] != name || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("%s %s: %d %v", method, name, w.Code, w.Header())
			}
			if method == "GET" && !bytes.Equal(w.Body.Bytes(), data) || method == "HEAD" && w.Body.Len() != 0 {
				t.Fatalf("changed downloaded bytes: %s", name)
			}
		}
		if len(data) > 0 {
			r := httptest.NewRequest("GET", endpoint, nil)
			r.Header.Set("Range", "bytes=1-3")
			w := httptest.NewRecorder()
			serveFile(w, r)
			if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), data[1:4]) {
				t.Fatalf("range failed: %s", name)
			}
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"POST", filepath.Join(dir, "audio.mp3"), 405},
		{"GET", "relative.zip", 400},
		{"GET", filepath.Join(dir, "missing.zip"), 404},
		{"GET", dir, 404},
	} {
		w := httptest.NewRecorder()
		serveFile(w, httptest.NewRequest(tc.method, "/file?path="+url.QueryEscape(tc.path), nil))
		if w.Code != tc.status {
			t.Fatalf("invalid download: %s %s: %d", tc.method, tc.path, w.Code)
		}
	}
}
