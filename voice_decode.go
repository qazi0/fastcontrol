package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Decode on disk so a long recording does not require an audio-sized heap buffer.
func decodeVoice(ctx context.Context, input, output string) (int64, error) {
	if err := audioDecodeCommand(ctx, input, output).Run(); err != nil {
		return 0, err
	}
	return canonicalVoiceFile(output)
}

// Normalize optional WAV chunks while copying PCM between private files.
func canonicalVoiceFile(path string) (int64, error) {
	in, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return 0, err
	}
	var header [12]byte
	if _, err = in.ReadAt(header[:], 0); err != nil {
		return 0, err
	}
	invalid := errors.New("invalid decoded audio")
	if string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" || int64(binary.LittleEndian.Uint32(header[4:8])) != st.Size()-8 {
		return 0, invalid
	}
	var format []byte
	var dataAt, dataSize int64
	for at := int64(12); at < st.Size(); {
		var chunk [8]byte
		if _, err = in.ReadAt(chunk[:], at); err != nil {
			return 0, invalid
		}
		size := int64(binary.LittleEndian.Uint32(chunk[4:]))
		end := at + 8 + size
		if end > st.Size() {
			return 0, invalid
		}
		switch string(chunk[:4]) {
		case "fmt ":
			if format != nil || size < 16 {
				return 0, invalid
			}
			format = make([]byte, min(size, 40))
			if _, err = in.ReadAt(format, at+8); err != nil {
				return 0, err
			}
		case "data":
			if dataAt != 0 {
				return 0, invalid
			}
			dataAt = at + 8
			dataSize = size
		}
		at = end + (size & 1)
		if at > st.Size() {
			return 0, invalid
		}
	}
	if format == nil || dataAt == 0 || dataSize == 0 || dataSize%2 != 0 {
		return 0, invalid
	}
	if err = validateVoiceFormat(format); err != nil {
		return 0, err
	}
	canonical := make([]byte, 44)
	copy(canonical, "RIFF")
	binary.LittleEndian.PutUint32(canonical[4:], uint32(dataSize+36))
	copy(canonical[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(canonical[16:], 16)
	copy(canonical[20:36], format[:16])
	binary.LittleEndian.PutUint16(canonical[20:], 1)
	copy(canonical[36:], "data")
	binary.LittleEndian.PutUint32(canonical[40:], uint32(dataSize))
	if err = validateVoiceHeader(canonical, dataSize+44); err != nil {
		return 0, err
	}
	out, err := os.CreateTemp(filepath.Dir(path), ".pcm-*.wav")
	if err != nil {
		return 0, err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	if _, err = out.Write(canonical); err != nil {
		return 0, err
	}
	if _, err = in.Seek(dataAt, io.SeekStart); err != nil {
		return 0, err
	}
	if _, err = io.CopyN(out, in, dataSize); err != nil {
		return 0, err
	}
	if err = out.Close(); err != nil {
		return 0, err
	}
	if err = in.Close(); err != nil {
		return 0, err
	}
	if err = os.Rename(out.Name(), path); err != nil {
		return 0, err
	}
	return dataSize + 44, nil
}

func validateVoiceFormat(format []byte) error {
	invalid := errors.New("invalid decoded audio")
	if len(format) < 16 {
		return invalid
	}
	typ := binary.LittleEndian.Uint16(format[:2])
	if typ == 0xfffe {
		guid := []byte{1, 0, 0, 0, 0, 0, 16, 0, 128, 0, 0, 170, 0, 56, 155, 113}
		if len(format) < 40 || !bytes.Equal(format[24:40], guid) {
			return invalid
		}
	} else if typ != 1 {
		return invalid
	}
	if binary.LittleEndian.Uint16(format[2:4]) != 1 || binary.LittleEndian.Uint32(format[4:8]) != 16000 || binary.LittleEndian.Uint32(format[8:12]) != 32000 || binary.LittleEndian.Uint16(format[12:14]) != 2 || binary.LittleEndian.Uint16(format[14:16]) != 16 {
		return invalid
	}
	return nil
}

// CoreAudio emits an extensible WAV header; strip optional chunks for the
// mono PCM contract.
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
	if err := validateVoiceFormat(format); err != nil {
		return nil, err
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
