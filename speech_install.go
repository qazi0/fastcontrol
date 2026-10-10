package main

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const speechModelBytes = 59707625
const speechModelURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-base-q5_1.bin"

func (s *Speech) downloadModel(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	finished, observed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(observed)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-finished:
				return
			case <-tick.C:
				if st, err := os.Stat(path); err == nil {
					s.mu.Lock()
					p := st.Size() * 95 / speechModelBytes
					if p > s.progress {
						s.progress = p
					}
					s.mu.Unlock()
				}
			}
		}
	}()
	// Use macOS curl for the system's HTTPS certificates and network configuration.
	curl := "/usr/bin/curl"
	if runtime.GOOS == "windows" {
		curl = "curl.exe"
	}
	cmd := exec.CommandContext(ctx, curl, "-fL", "--retry", "3", "--max-time", "600", "--output", path, speechModelURL)
	err := cmd.Run()
	close(finished)
	<-observed
	if err != nil {
		return errors.New("Could Not Download The Speech Model. Check This Computer's Internet Connection")
	}
	return nil
}
func (s *Speech) install() (err error) {
	s.mu.Lock()
	if s.availableLocked() {
		s.mu.Unlock()
		return nil
	}
	if s.installing {
		s.mu.Unlock()
		return errors.New("Speech Model Installation Is Already Running")
	}
	s.installing = true
	s.progress = 0
	s.installError = ""
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.installing = false
		if err != nil {
			s.installError = err.Error()
		}
		s.mu.Unlock()
	}()
	return s.installModel()
}
func (s *Speech) installModel() error {
	runtime := filepath.Join(s.home, ".fastcontrol", "speech", speechRuntimeName())
	if !speechExecutable(runtime) {
		return errors.New("Speech Runtime Is Missing. Update fastcontrol On This Computer")
	}
	dir := filepath.Join(s.home, ".fastcontrol/models")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("Could Not Create The Speech Model Folder")
	}
	target := filepath.Join(dir, bundledSpeechModel)
	model := target
	if !validSpeechModel(target) {
		f, err := os.CreateTemp(dir, "model-download-*.part")
		if err != nil {
			return errors.New("Could Not Save The Speech Model")
		}
		defer os.Remove(f.Name())
		if err = f.Close(); err != nil {
			return errors.New("Could Not Save The Speech Model")
		}
		if err = s.downloadModel(f.Name()); err != nil {
			return err
		}
		st, err := os.Stat(f.Name())
		if err != nil || st.Size() != speechModelBytes {
			return errors.New("Speech Model Download Was Incomplete. Try Again")
		}
		if !validSpeechModel(f.Name()) {
			return errors.New("Speech Model Checksum Failed. Try Again")
		}
		model = f.Name()
	}
	s.mu.Lock()
	s.progress = 96
	s.mu.Unlock()
	if err := warmSpeech(runtime, model, dir); err != nil {
		return err
	}
	if model != target {
		if err := os.Rename(model, target); err != nil {
			return errors.New("Could Not Install The Speech Model")
		}
	}
	selected := discoverSpeech(s.home, s.preferMacWhisper)
	if selected.bin == "" || selected.model == "" {
		return errors.New("Speech Model Setup Failed. Try Again")
	}
	s.mu.Lock()
	s.bin = selected.bin
	s.model = selected.model
	s.engine = selected.engine
	s.modelPath = selected.modelPath
	s.progress = 100
	s.mu.Unlock()
	return nil
}
func warmSpeech(runtime, model, dir string) error {
	f, err := os.CreateTemp(dir, "speech-check-*.wav")
	if err != nil {
		return errors.New("Could Not Prepare The Speech Model")
	}
	path := f.Name()
	defer os.Remove(path)
	defer os.Remove(path + ".txt")
	b := make([]byte, 44+32000)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 32000)
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("Could Not Prepare The Speech Model")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtime, "--model", model, "--file", path, "--language", "auto", "--threads", "4", "--beam-size", "1", "--best-of", "1", "--no-timestamps", "--no-prints", "--output-txt", "--output-file", path)
	if err := cmd.Run(); err != nil {
		return errors.New("Speech Model Validation Failed. Try Again")
	}
	if _, err := os.Stat(path + ".txt"); err != nil {
		return errors.New("Speech Model Validation Failed. Try Again")
	}
	return nil
}
func (s *Speech) serveInstall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		_, _ = w.Write(mustJSON(s.capability()))
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(405)
		_, _ = w.Write(mustJSON(map[string]string{"error": "Use GET or POST"}))
		return
	}
	// Claim the job before starting its goroutine so concurrent taps share one download.
	s.mu.Lock()
	if s.availableLocked() {
		s.mu.Unlock()
		_, _ = w.Write(mustJSON(s.capability()))
		return
	}
	if !s.installing {
		if !speechExecutable(filepath.Join(s.home, ".fastcontrol", "speech", speechRuntimeName())) {
			s.mu.Unlock()
			w.WriteHeader(503)
			_, _ = w.Write(mustJSON(map[string]string{"error": "Speech Runtime Is Missing. Update fastcontrol On This Computer"}))
			return
		}
		s.installing = true
		s.progress = 0
		s.installError = ""
		go func() {
			err := s.installModel()
			s.mu.Lock()
			s.installing = false
			if err != nil {
				s.installError = err.Error()
			}
			s.mu.Unlock()
		}()
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(mustJSON(s.capability()))
}
