package main

import (
	"bytes"
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

const maxAudioBytes = 128 << 20

func audioMIME(head []byte) string {
	switch mime := http.DetectContentType(head); mime {
	case "audio/mpeg", "audio/aiff":
		return mime
	case "audio/wave":
		return "audio/wav"
	case "application/ogg":
		if bytes.Contains(head, []byte("OpusHead")) || bytes.Contains(head, []byte("\x01vorbis")) {
			return "audio/ogg"
		}
	}
	if bytes.HasPrefix(head, []byte("fLaC")) {
		return "audio/flac"
	}
	if len(head) >= 12 && string(head[4:8]) == "ftyp" {
		return "audio/mp4"
	}
	if len(head) >= 4 && head[0] == 0xff {
		if head[1]&0xf6 == 0xf0 {
			return "audio/aac"
		}
		if head[1]&0xe0 == 0xe0 && head[1]&0x06 != 0 && head[2]&0xf0 != 0xf0 && head[2]&0xf0 != 0 && head[2]&0x0c != 0x0c {
			return "audio/mpeg"
		}
	}
	return ""
}

func cacheAudio(encoded string) string {
	if len(encoded) > maxAudioBytes*4/3+1024 {
		return ""
	}
	if strings.HasPrefix(encoded, "data:") {
		prefix, data, ok := strings.Cut(encoded, ",")
		if !ok || !strings.HasPrefix(prefix, "data:audio/") || !strings.HasSuffix(prefix, ";base64") {
			return ""
		}
		encoded = data
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) > maxAudioBytes || audioMIME(data[:min(len(data), 512)]) == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".fastcontrol", "audio")
	if os.MkdirAll(dir, 0700) != nil {
		return ""
	}
	hash := sha256.Sum256(data)
	path := filepath.Join(dir, hex.EncodeToString(hash[:]))
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Size() == int64(len(data)) {
		return path
	}
	f, err := os.CreateTemp(dir, ".audio-")
	if err != nil {
		return ""
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ""
	}
	if os.Rename(f.Name(), path) != nil {
		// Another transcript reader may have published the same content first.
		if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() || st.Size() != int64(len(data)) {
			return ""
		}
	}
	return path
}

// Audio payloads follow the same lazy local-file transport as rendered images.
func collectAudio(v any) []string {
	var out []string
	seen := map[string]bool{}
	add := func(path string, encoded bool) {
		if encoded || strings.HasPrefix(path, "data:") {
			path = cacheAudio(path)
		}
		if path == "" || seen[path] || len(out) >= 12 {
			return
		}
		if !filepath.IsAbs(path) && !strings.HasPrefix(path, "https://") && !strings.HasPrefix(path, "http://") {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	var walk func(any, int)
	walk = func(v any, depth int) {
		if depth > 8 {
			return
		}
		switch m := v.(type) {
		case []any:
			for _, child := range m {
				walk(child, depth+1)
			}
		case string:
			if strings.HasPrefix(m, "data:audio/") {
				add(m, false)
			} else if len(m) < 16<<20 && (strings.HasPrefix(m, "{") || strings.HasPrefix(m, "[")) {
				var child any
				if json.Unmarshal([]byte(m), &child) == nil {
					walk(child, depth+1)
				}
			}
		case map[string]any:
			if strings.Contains(strings.ToLower(str(m, "type")), "audio") {
				for _, key := range []string{"path", "savedPath", "saved_path", "audioUrl", "audio_url", "url"} {
					if nested, ok := m[key].(map[string]any); ok {
						add(str(nested, "url"), false)
					} else {
						add(str(m, key), false)
					}
				}
				if data := str(m, "data"); data != "" {
					add(data, true)
				}
			}
			for _, key := range []string{"content", "contentItems", "content_items", "result", "output"} {
				if child, ok := m[key]; ok {
					walk(child, depth+1)
				}
			}
		}
	}
	walk(v, 0)
	return out
}

func serveAudio(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Use GET Or HEAD", 405)
		return
	}
	path := r.URL.Query().Get("path")
	if !filepath.IsAbs(path) {
		http.Error(w, "Use An Absolute Audio Path", 400)
		return
	}
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		http.Error(w, "Audio Is Unavailable", 404)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "Audio Is Unavailable", 404)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxAudioBytes {
		http.Error(w, "Audio Is Too Large Or Unavailable", 413)
		return
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	mime := audioMIME(head[:n])
	if mime == "" {
		http.Error(w, "Unsupported Audio Format", 415)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "Audio Is Unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}
