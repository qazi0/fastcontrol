package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAudioServingRangesAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rendered clip.wav")
	data := testVoiceWAV()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	endpoint := "/audio?path=" + url.QueryEscape(path)
	for _, tc := range []struct {
		method, target, span string
		status               int
		body                 []byte
	}{
		{"GET", endpoint, "", 200, data},
		{"HEAD", endpoint, "", 200, nil},
		{"GET", endpoint, "bytes=12-19", 206, data[12:20]},
		{"GET", endpoint, "bytes=-4", 206, data[len(data)-4:]},
		{"GET", endpoint, "bytes=900-", 416, nil},
		{"POST", endpoint, "", 405, nil},
		{"GET", "/audio?path=relative.mp3", "", 400, nil},
		{"GET", "/audio?path=" + url.QueryEscape(path+".missing"), "", 404, nil},
	} {
		r := httptest.NewRequest(tc.method, tc.target, nil)
		if tc.span != "" {
			r.Header.Set("Range", tc.span)
		}
		w := httptest.NewRecorder()
		serveAudio(w, r)
		if w.Code != tc.status || (tc.status < 400 && !bytes.Equal(w.Body.Bytes(), tc.body)) {
			t.Fatalf("%s %s: status %d, body %q", tc.method, tc.span, w.Code, w.Body.String())
		}
		if tc.status < 400 && (w.Header().Get("Content-Type") != "audio/wav" || w.Header().Get("Accept-Ranges") != "bytes" || w.Header().Get("X-Content-Type-Options") != "nosniff") {
			t.Fatalf("missing audio/range headers: %v", w.Header())
		}
	}
	if err := os.WriteFile(path, []byte("<html>not audio</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	serveAudio(w, httptest.NewRequest("GET", endpoint, nil))
	if w.Code != 415 {
		t.Fatalf("served non-audio content: %d", w.Code)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(maxAudioBytes + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	serveAudio(w, httptest.NewRequest("GET", endpoint, nil))
	if w.Code != 413 {
		t.Fatalf("served oversized audio: %d", w.Code)
	}
}

func TestAudioToolPayloadParityAndConcurrentPublication(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	encoded := base64.StdEncoding.EncodeToString(testVoiceWAV())
	block := map[string]any{"type": "audio", "data": encoded, "mimeType": "audio/wav"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := cacheAudio(encoded)
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, testVoiceWAV()) {
				t.Errorf("partially published audio: %v", err)
			}
		}()
	}
	wg.Wait()
	for _, typ := range []string{"functionCallOutput", "FunctionCallOutput", "Extension"} {
		raw := map[string]any{"id": "audio", "type": typ, "output": []any{block, block}}
		var item Item
		if typ == "functionCallOutput" {
			item = desktopItem(raw)
		} else {
			var ok bool
			item, ok = rolloutItem(raw)
			if !ok {
				t.Fatal("rollout dropped tool audio")
			}
		}
		if len(item.Audio) != 1 || len(item.Images) != 0 {
			t.Fatalf("audio lost or duplicated: %+v", item)
		}
		wire, _ := json.Marshal(item)
		if bytes.Contains(wire, []byte(encoded)) {
			t.Fatal("wire update contains binary audio")
		}
		w := httptest.NewRecorder()
		serveAudio(w, httptest.NewRequest("GET", "/audio?path="+url.QueryEscape(item.Audio[0]), nil))
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), testVoiceWAV()) {
			t.Fatal("cached tool audio is not playable")
		}
	}
	if cacheAudio(base64.StdEncoding.EncodeToString([]byte("not audio"))) != "" || cacheAudio("data:image/png;base64,"+encoded) != "" {
		t.Fatal("accepted a non-audio payload")
	}
}
