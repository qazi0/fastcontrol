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
	"runtime"
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
	for _, b := range [][]byte{nil, []byte("RIFF"), make([]byte, 200)} {
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

func TestWhisperTranscriptFileAndCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the native test executable in TestWindowsSpeechCleanup")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "whisper-cli")
	script := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-file" ]; then shift; output="$1"; fi
  shift
done
printf '%s' "$output" > "$(dirname "$0")/path"
printf 'Spoken Draft.\n' > "$output.txt"
printf 'Runtime Log Should Not Be The Draft\n'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s := &Speech{bin: bin, model: "whisper.cpp:base-q5_1", engine: "whispercpp", modelPath: "/model with spaces.bin", busy: make(chan struct{}, 1)}
	recording := make([]byte, 44+32000*185)
	copy(recording, testVoiceWAV())
	binary.LittleEndian.PutUint32(recording[4:], uint32(len(recording)-8))
	binary.LittleEndian.PutUint32(recording[40:], uint32(len(recording)-44))
	r := httptest.NewRequest("POST", "/transcribe", bytes.NewReader(recording))
	r.Header.Set("Content-Type", "audio/wav")
	w := httptest.NewRecorder()
	s.serve(w, r)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("Spoken Draft.")) || bytes.Contains(w.Body.Bytes(), []byte("Runtime Log")) {
		t.Fatalf("transcript response: %d %s", w.Code, w.Body.String())
	}
	path, err := os.ReadFile(filepath.Join(dir, "path"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{string(path), string(path) + ".txt"} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("temporary recording or transcript remained: %s", file)
		}
	}
	if len(s.busy) != 0 {
		t.Fatal("transcription slot leaked")
	}
}

func TestSpeechDiscoveryRejectsMissingOrCorruptModel(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".fastcontrol", "speech")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, speechRuntimeName()), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if discoverSpeech(home, false).model != "" {
		t.Fatal("missing model reported available")
	}
	models := filepath.Join(home, ".fastcontrol", "models")
	if err := os.MkdirAll(models, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(models, bundledSpeechModel), []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	if discoverSpeech(home, false).model != "" {
		t.Fatal("partial model reported available")
	}
}

func TestConvertedVoiceWAVPreservesPCM(t *testing.T) {
	original := testVoiceWAV()
	decoded, err := canonicalVoiceWAV(original)
	if err != nil || !bytes.Equal(original, decoded) {
		t.Fatalf("PCM conversion changed samples: %v", err)
	}
	// A malformed chunk size must be rejected before indexing its contents.
	bad := append([]byte(nil), original...)
	binary.LittleEndian.PutUint32(bad[16:], 0xffffffff)
	if _, err := canonicalVoiceWAV(bad); err == nil {
		t.Fatal("accepted overflowing chunk")
	}
	longRecording := make([]byte, 44+32000*185)
	copy(longRecording, original)
	binary.LittleEndian.PutUint32(longRecording[4:], uint32(len(longRecording)-8))
	binary.LittleEndian.PutUint32(longRecording[40:], uint32(len(longRecording)-44))
	if _, err := canonicalVoiceWAV(longRecording); err != nil {
		t.Fatalf("rejected a valid recording longer than two minutes: %v", err)
	}
	path := filepath.Join(t.TempDir(), "long.wav")
	if err := os.WriteFile(path, longRecording, 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := canonicalVoiceFile(path); err != nil || n != int64(len(longRecording)) {
		t.Fatalf("disk normalization: %d %v", n, err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, longRecording) {
		t.Fatalf("disk normalization changed PCM: %v", err)
	}

}
