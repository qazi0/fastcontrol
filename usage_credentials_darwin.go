package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
)

func readClaudeUsageCredential(ctx context.Context) (map[string]any, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", "Claude Code-credentials", "-w")
	data, err := cmd.Output()
	if err != nil {
		return nil, errors.New("Claude Credentials Are Unavailable")
	}
	var value map[string]any
	err = json.Unmarshal(data, &value)
	return value, err
}
