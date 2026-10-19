package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Codex accepts local images directly. Other attachments use its local file tools.
func withUploadedFiles(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	filesJSON, ok := fields["files"]
	if !ok {
		return raw, nil
	}
	var files []string
	if json.Unmarshal(filesJSON, &files) != nil || len(files) > 12 {
		return nil, errors.New("Choose Up To 12 Files")
	}
	if err := checkUploads(files); err != nil {
		return nil, err
	}
	var text string
	if v, ok := fields["text"]; ok && json.Unmarshal(v, &text) != nil {
		return nil, errors.New("Invalid Message Text")
	}
	if len(files) > 0 {
		text += "\n\nAttached files on this computer (read these files as needed):"
		for _, path := range files {
			name := filepath.Base(path)
			if len(name) > 37 && name[36] == '-' {
				name = name[37:]
			}
			text += fmt.Sprintf("\n[%s](<%s>)", strings.ReplaceAll(name, "]", "\\]"), strings.ReplaceAll(path, ">", "%3E"))
		}
	}
	fields["text"], _ = json.Marshal(strings.TrimSpace(text))
	delete(fields, "files")
	return json.Marshal(fields)
}
