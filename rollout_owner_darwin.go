package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func platformRolloutOwners(paths []string) map[string]rolloutOwnerEvidence {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	aliases := map[string][]string{}
	for _, path := range paths {
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			canonical = path
		}
		aliases[canonical] = append(aliases[canonical], path)
	}
	args := []string{"-nP", "-w", "-F0fain", "--"}
	for canonical := range aliases {
		args = append(args, canonical)
	}
	output, err := exec.CommandContext(ctx, "/usr/sbin/lsof", args...).Output()
	result := map[string]rolloutOwnerEvidence{}
	if ctx.Err() != nil {
		return result
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			return result
		}
	}
	for _, path := range paths {
		result[path] = rolloutOwnerEvidence{known: true}
	}
	access := byte(0)
	var inode uint64
	for _, field := range bytes.Split(output, []byte{0}) {
		field = bytes.TrimPrefix(field, []byte("\n"))
		if len(field) < 2 {
			continue
		}
		switch field[0] {
		case 'f':
			access = 0
			inode = 0
		case 'a':
			access = field[1]
		case 'i':
			inode, _ = strconv.ParseUint(string(field[1:]), 10, 64)
		case 'n':
			if access == 'w' || access == 'u' {
				canonical := string(field[1:])
				if originals := aliases[canonical]; len(originals) > 0 {
					info, err := os.Stat(canonical)
					if err == nil {
						if native, ok := info.Sys().(*syscall.Stat_t); ok && native.Ino == inode {
							for _, path := range originals {
								result[path] = rolloutOwnerEvidence{known: true, alive: true}
							}
						}
					}
				}
			}
		}
	}
	return result
}
