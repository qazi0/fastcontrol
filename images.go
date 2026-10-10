package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const maxImageBytes = 10 << 20

func imageMIME(b []byte) string {
	m := http.DetectContentType(b)
	switch m {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return m
	}
	return ""
}

func cacheImage(data string) string {
	if len(data) > (maxImageBytes*4/3)+1024 {
		return ""
	}
	if strings.HasPrefix(data, "data:") {
		prefix, rest, ok := strings.Cut(data, ",")
		if !ok || !strings.HasPrefix(prefix, "data:image/") || !strings.HasSuffix(prefix, ";base64") {
			return ""
		}
		data = rest
	}
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(b) > maxImageBytes || imageMIME(b) == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".fastcontrol", "images")
	if os.MkdirAll(dir, 0700) != nil {
		return ""
	}
	hash := sha256.Sum256(b)
	path := filepath.Join(dir, hex.EncodeToString(hash[:]))
	if _, err := os.Stat(path); err == nil {
		return path
	}
	f, err := os.CreateTemp(dir, ".image-")
	if err != nil {
		return ""
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil || closeErr != nil || os.Rename(f.Name(), path) != nil {
		return ""
	}
	return path
}

// Keep binary payloads out of transcript updates. Images are fetched lazily.
func collectImages(v any) []string {
	var out []string
	seen := map[string]bool{}
	add := func(src string, encoded bool) {
		if encoded || strings.HasPrefix(src, "data:") {
			src = cacheImage(src)
		}
		if src == "" || seen[src] || len(out) >= 12 {
			return
		}
		if !filepath.IsAbs(src) && !strings.HasPrefix(src, "https://") && !strings.HasPrefix(src, "http://") {
			return
		}
		seen[src] = true
		out = append(out, src)
	}
	var walk func(any, int)
	walk = func(v any, depth int) {
		if depth > 8 {
			return
		}
		switch m := v.(type) {
		case []any:
			for _, x := range m {
				walk(x, depth+1)
			}
		case string:
			if strings.HasPrefix(m, "data:image/") {
				add(m, false)
			} else if len(m) < 16<<20 && (strings.HasPrefix(m, "{") || strings.HasPrefix(m, "[")) {
				var x any
				if json.Unmarshal([]byte(m), &x) == nil {
					walk(x, depth+1)
				}
			}
		case map[string]any:
			typ := strings.ToLower(str(m, "type"))
			if strings.Contains(typ, "image") {
				for _, key := range []string{"path", "savedPath", "saved_path", "imageUrl", "image_url", "url"} {
					add(str(m, key), false)
				}
				if data := str(m, "data"); data != "" {
					add(data, true)
				}
				if typ == "imagegeneration" && str(m, "savedPath") == "" && str(m, "saved_path") == "" {
					add(str(m, "result"), true)
				}
			}
			for _, key := range []string{"content", "contentItems", "content_items", "result", "output"} {
				if x, ok := m[key]; ok {
					walk(x, depth+1)
				}
			}
		}
	}
	walk(v, 0)
	return out
}

func serveImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Use GET", 405)
		return
	}
	path := r.URL.Query().Get("path")
	if !filepath.IsAbs(path) {
		http.Error(w, "Use An Absolute Image Path", 400)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "Image Is Unavailable", 404)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxImageBytes {
		http.Error(w, "Image Is Too Large Or Unavailable", 413)
		return
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	mime := imageMIME(head[:n])
	if mime == "" {
		http.Error(w, "Unsupported Image Format", 415)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "Image Is Unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}
