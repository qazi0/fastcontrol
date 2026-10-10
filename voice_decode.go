package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"time"
)

var errVoiceTooLarge = errors.New("recording too long")

// macOS supplies the AAC decoder, so the relay needs no codec package.
func decodeVoice(parent context.Context, input, output string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	done, observed := make(chan struct{}), make(chan struct{})
	tooLarge := false
	go func() {
		defer close(observed)
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if st, err := os.Stat(output); err == nil && st.Size() > maxVoiceBytes+65536 {
					tooLarge = true
					cancel()
					return
				}
			}
		}
	}()
	err := exec.CommandContext(ctx, "/usr/bin/afconvert", "-f", "WAVE", "-d", "LEI16@16000", "-c", "1", input, output).Run()
	close(done)
	<-observed
	if tooLarge {
		return nil, errVoiceTooLarge
	}
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(output)
	if err != nil {
		return nil, err
	}
	if st.Size() > maxVoiceBytes+65536 {
		return nil, errVoiceTooLarge
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return nil, err
	}
	return canonicalVoiceWAV(data)
}

// CoreAudio emits an extensible WAV header; strip optional chunks for the
// existing strict mono PCM contract and enforce the duration before inference.
func canonicalVoiceWAV(b []byte) ([]byte, error) {
	invalid := errors.New("invalid decoded audio")
	if len(b) < 44 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" || int64(binary.LittleEndian.Uint32(b[4:8])) != int64(len(b)-8) {
		return nil, invalid
	}
	var format, data []byte
	for at := 12; at < len(b); {
		if at+8 > len(b) {
			return nil, invalid
		}
		size := int64(binary.LittleEndian.Uint32(b[at+4 : at+8]))
		end := int64(at) + 8 + size
		if end > int64(len(b)) {
			return nil, invalid
		}
		chunk := b[at+8 : int(end)]
		switch string(b[at : at+4]) {
		case "fmt ":
			if format != nil {
				return nil, invalid
			}
			format = chunk
		case "data":
			if data != nil {
				return nil, invalid
			}
			data = chunk
		}
		at = int(end) + (int(size) & 1)
		if at > len(b) {
			return nil, invalid
		}
	}
	if len(format) < 16 || data == nil || len(data) == 0 || len(data)%2 != 0 {
		return nil, invalid
	}
	if len(data) > maxVoiceBytes-44 {
		return nil, errVoiceTooLarge
	}
	typ := binary.LittleEndian.Uint16(format[:2])
	if typ == 0xfffe {
		guid := []byte{1, 0, 0, 0, 0, 0, 16, 0, 128, 0, 0, 170, 0, 56, 155, 113}
		if len(format) < 40 || !bytes.Equal(format[24:40], guid) {
			return nil, invalid
		}
	} else if typ != 1 {
		return nil, invalid
	}
	if binary.LittleEndian.Uint16(format[2:4]) != 1 || binary.LittleEndian.Uint32(format[4:8]) != 16000 || binary.LittleEndian.Uint32(format[8:12]) != 32000 || binary.LittleEndian.Uint16(format[12:14]) != 2 || binary.LittleEndian.Uint16(format[14:16]) != 16 {
		return nil, invalid
	}
	out := make([]byte, 44+len(data))
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	copy(out[20:36], format[:16])
	binary.LittleEndian.PutUint16(out[20:], 1)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(data)))
	copy(out[44:], data)
	return out, validateVoice(out)
}
