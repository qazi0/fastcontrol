package main

import (
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
)

// Paired clients can save any regular file linked in a chat, without executing it.
func serveFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Use GET Or HEAD", 405)
		return
	}
	path := r.URL.Query().Get("path")
	if !filepath.IsAbs(path) {
		http.Error(w, "Use An Absolute File Path", 400)
		return
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		http.Error(w, "File Is Unavailable", 404)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "File Is Unavailable", 404)
		return
	}
	defer f.Close()
	st, err = f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.Error(w, "File Is Unavailable", 404)
		return
	}
	name := filepath.Base(path)
	contentType := mime.TypeByExtension(filepath.Ext(name))
	if contentType == "" {
		head := make([]byte, 512)
		n, _ := f.Read(head)
		contentType = audioMIME(head[:n])
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "File Is Unavailable", 500)
			return
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(w, r, name, st.ModTime(), f)
}
