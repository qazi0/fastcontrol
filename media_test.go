package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func testVoiceWAV() []byte {
	b := make([]byte, 46)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 38)
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 2)
	return b
}

func TestVoiceRejectsInvalidPCMAndConcurrentRequest(t *testing.T) {
	if err := validateVoice(testVoiceWAV()); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{0, 4, 8, 12, 16, 20, 22, 24, 28, 32, 34, 36, 40} {
		b := testVoiceWAV()
		b[offset] ^= 1
		if validateVoice(b) == nil {
			t.Fatalf("accepted invalid WAV field at %d", offset)
		}
	}
	for _, b := range [][]byte{nil, []byte("RIFF"), make([]byte, maxVoiceBytes+1)} {
		if validateVoice(b) == nil {
			t.Fatal("accepted invalid recording")
		}
	}
	s := &Speech{bin: "/usr/bin/false", model: "local", busy: make(chan struct{}, 1)}
	s.busy <- struct{}{}
	r := httptest.NewRequest("POST", "/transcribe", bytes.NewReader(testVoiceWAV()))
	r.Header.Set("Content-Type", "audio/wav")
	w := httptest.NewRecorder()
	s.serve(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("busy response: %d", w.Code)
	}
	<-s.busy
	r = httptest.NewRequest("POST", "/transcribe", bytes.NewReader([]byte("invalid")))
	r.Header.Set("Content-Type", "audio/wav")
	w = httptest.NewRecorder()
	s.serve(w, r)
	if w.Code != 400 || len(s.busy) != 0 {
		t.Fatalf("invalid recording leaked slot: %d", w.Code)
	}
}

func TestImageServingAndDesktopRolloutParity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image with spaces.png")
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"imageView", "ImageView"} {
		raw := map[string]any{"id": "i", "type": typ, "path": path}
		var item Item
		if typ == "imageView" {
			item = desktopItem(raw)
		} else {
			var ok bool
			item, ok = rolloutItem(raw)
			if !ok {
				t.Fatal("rollout dropped image")
			}
		}
		if item.K != "image" || len(item.Images) != 1 || item.Images[0] != path {
			t.Fatalf("image lost: %+v", item)
		}
	}
	w := httptest.NewRecorder()
	serveImage(w, httptest.NewRequest("GET", "/image?path="+url.QueryEscape(path), nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), data.Bytes()) {
		t.Fatalf("image response: %d", w.Code)
	}
	if err := os.WriteFile(path, []byte("<svg onload=alert(1) />"), 0600); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	serveImage(w, httptest.NewRequest("GET", "/image?path="+url.QueryEscape(path), nil))
	if w.Code != 415 {
		t.Fatalf("served active non-image content: %d", w.Code)
	}
}
