package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxVoiceBytes = 44 + 16000*2*120
const bundledSpeechModel = "ggml-base-q5_1.bin"
const bundledSpeechModelSHA256 = "422f1ae452ade6f30a004d7e5c6a43195e4433bc370bf23fac9cc591f01a8898"

type Speech struct {
	bin              string
	model            string
	engine           string
	modelPath        string
	busy             chan struct{}
	mu               sync.Mutex
	home             string
	preferMacWhisper bool
	installing       bool
	progress         int64
	installError     string
}

func newSpeech() *Speech {
	home, _ := os.UserHomeDir()
	return discoverSpeech(home, os.Getenv("FASTCONTROL_SPEECH_ENGINE") != "whispercpp")
}
func discoverSpeech(home string, preferMacWhisper bool) *Speech {
	s := &Speech{busy: make(chan struct{}, 1), home: home, preferMacWhisper: preferMacWhisper}
	if preferMacWhisper {
		for _, path := range []string{"/Applications/MacWhisper.app/Contents/MacOS/mw", "/usr/local/bin/mw"} {
			if !speechExecutable(path) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			out, err := exec.CommandContext(ctx, path, "models", "list").Output()
			cancel()
			if err != nil {
				continue
			}
			for _, model := range []string{"parakeet-pro:nvidia_parakeet-v3", "parakeet-pro:nvidia_parakeet-v3_494MB", "whisper-cpp:ggml-model-whisper-turbo", "whisper-cpp:ggml-model-whisper-tiny"} {
				if strings.Contains(string(out), model+" ") {
					s.bin = path
					s.model = model
					s.engine = "macwhisper"
					return s
				}
			}
		}
	}
	path := filepath.Join(home, ".fastcontrol", "speech", "whisper-cli")
	model := filepath.Join(home, ".fastcontrol", "models", bundledSpeechModel)
	if speechExecutable(path) && validSpeechModel(model) {
		s.bin = path
		s.model = "whisper.cpp:base-q5_1"
		s.engine = "whispercpp"
		s.modelPath = model
	}
	return s
}
func speechExecutable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0
}
func validSpeechModel(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != 59707625 {
		return false
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return false
	}
	return hex.EncodeToString(hash.Sum(nil)) == bundledSpeechModelSHA256
}
func (s *Speech) capability() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"available": s.availableLocked(), "model": s.model, "engine": s.engine, "installable": speechExecutable(filepath.Join(s.home, ".fastcontrol/speech/whisper-cli")), "installing": s.installing, "progress": s.progress, "error": s.installError}
}
func (s *Speech) availableLocked() bool {
	if s.bin == "" || s.model == "" {
		return false
	}
	if s.engine == "whispercpp" {
		return speechExecutable(s.bin) && validSpeechModel(s.modelPath)
	}
	return true
}
func (s *Speech) config() (string, string, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bin, s.model, s.engine, s.modelPath
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
		_, _ = w.Write(mustJSON(s.capability()))
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		fail(405, "Use GET or POST")
		return
	}
	bin, model, engine, modelPath := s.config()
	if bin == "" || model == "" {
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
	cmd := exec.CommandContext(ctx, bin, "transcribe", f.Name(), "--model", model, "--language", "auto", "--no-speakers", "--format", "txt", "--no-timestamps")
	transcriptPath := f.Name() + ".txt"
	if engine == "whispercpp" {
		defer os.Remove(transcriptPath)
		cmd = exec.CommandContext(ctx, bin, "--model", modelPath, "--file", f.Name(), "--language", "auto", "--threads", "4", "--beam-size", "1", "--best-of", "1", "--no-timestamps", "--no-prints", "--output-txt", "--output-file", f.Name())
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			fail(504, "Transcription Timed Out. Try a Shorter Recording")
		} else {
			fail(502, "Transcription Failed. Check The Speech Runtime On The Mac")
		}
		return
	}
	text := strings.TrimSpace(out.String())
	if engine == "whispercpp" {
		file, err := os.Open(transcriptPath)
		if err != nil {
			fail(502, "Could Not Read The Transcript")
			return
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 1<<20))
		_ = file.Close()
		if readErr != nil {
			fail(502, "Could Not Read The Transcript")
			return
		}
		text = strings.TrimSpace(string(data))
		if text == "[BLANK_AUDIO]" {
			text = ""
		}
	}
	if text == "" {
		fail(422, "No Speech Detected")
		return
	}
	_, _ = w.Write(mustJSON(map[string]any{"text": text, "model": model, "durationMs": time.Since(start).Milliseconds()}))
}
