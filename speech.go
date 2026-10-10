package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxVoiceBytes = 44 + 16000*2*120

type Speech struct {
	bin   string
	model string
	busy  chan struct{}
}

func newSpeech() *Speech {
	s := &Speech{busy: make(chan struct{}, 1)}
	for _, p := range []string{"/Applications/MacWhisper.app/Contents/MacOS/mw", "/usr/local/bin/mw"} {
		if st, err := os.Stat(p); err == nil && st.Mode()&0111 != 0 {
			s.bin = p
			break
		}
	}
	if s.bin == "" {
		return s
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.bin, "models", "list").Output()
	if err != nil {
		s.bin = ""
		return s
	}
	models := string(out)
	for _, m := range []string{"parakeet-pro:nvidia_parakeet-v3", "parakeet-pro:nvidia_parakeet-v3_494MB", "whisper-cpp:ggml-model-whisper-turbo", "whisper-cpp:ggml-model-whisper-tiny"} {
		if strings.Contains(models, m+" ") {
			s.model = m
			break
		}
	}
	return s
}

// Voice accepts a bounded PCM WAV, rather than arbitrary paths or commands.
func validateVoice(b []byte) error {
	if len(b) < 46 || len(b) > maxVoiceBytes || len(b)%2 != 0 ||
		string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" ||
		string(b[12:16]) != "fmt " || binary.LittleEndian.Uint32(b[16:20]) != 16 ||
		binary.LittleEndian.Uint16(b[20:22]) != 1 || binary.LittleEndian.Uint16(b[22:24]) != 1 ||
		binary.LittleEndian.Uint32(b[24:28]) != 16000 || binary.LittleEndian.Uint32(b[28:32]) != 32000 ||
		binary.LittleEndian.Uint16(b[32:34]) != 2 || binary.LittleEndian.Uint16(b[34:36]) != 16 ||
		string(b[36:40]) != "data" || int(binary.LittleEndian.Uint32(b[40:44])) != len(b)-44 ||
		int(binary.LittleEndian.Uint32(b[4:8])) != len(b)-8 {
		return errors.New("Record Mono PCM Audio at 16 kHz, Up To Two Minutes")
	}
	return nil
}

func (s *Speech) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, msg string) {
		w.WriteHeader(status)
		_, _ = w.Write(mustJSON(map[string]string{"error": msg}))
	}
	if r.Method == http.MethodGet {
		_, _ = w.Write(mustJSON(map[string]any{"available": s.bin != "" && s.model != "", "model": s.model}))
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		fail(405, "Use GET or POST")
		return
	}
	if s.bin == "" || s.model == "" {
		fail(503, "No Local Speech Model On This Mac")
		return
	}
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "audio/wav" {
		fail(415, "Send WAV Audio")
		return
	}
	select {
	case s.busy <- struct{}{}:
		defer func() { <-s.busy }()
	default:
		w.Header().Set("Retry-After", "2")
		fail(429, "This Mac Is Transcribing Another Recording")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVoiceBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		fail(413, "Recording Is Too Large")
		return
	}
	if err := validateVoice(b); err != nil {
		fail(400, err.Error())
		return
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".fastcontrol", "voice")
	if err := os.MkdirAll(dir, 0700); err != nil {
		fail(500, "Could Not Save Recording")
		return
	}
	f, err := os.CreateTemp(dir, "recording-*.wav")
	if err != nil {
		fail(500, "Could Not Save Recording")
		return
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		fail(500, "Could Not Save Recording")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, s.bin, "transcribe", f.Name(), "--model", s.model, "--language", "auto", "--no-speakers", "--format", "txt", "--no-timestamps")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			fail(504, "Transcription Timed Out. Try a Shorter Recording")
		} else {
			fail(502, "Transcription Failed. Check MacWhisper On The Mac")
		}
		return
	}
	text := strings.TrimSpace(out.String())
	if text == "" {
		fail(422, "No Speech Detected")
		return
	}
	_, _ = w.Write(mustJSON(map[string]any{"text": text, "model": s.model, "durationMs": time.Since(start).Milliseconds()}))
}
