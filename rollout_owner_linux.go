package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func platformRolloutOwners(paths []string) map[string]rolloutOwnerEvidence {
	result := map[string]rolloutOwnerEvidence{}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return result
	}
	aliases := map[string][]string{}
	for _, path := range paths {
		result[path] = rolloutOwnerEvidence{known: true}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			canonical = path
		}
		aliases[canonical] = append(aliases[canonical], path)
	}
	for _, process := range processes {
		if _, err := strconv.Atoi(process.Name()); err != nil {
			continue
		}
		root := filepath.Join("/proc", process.Name())
		fds, err := os.ReadDir(filepath.Join(root, "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			path, err := os.Readlink(filepath.Join(root, "fd", fd.Name()))
			if err != nil {
				continue
			}
			if len(aliases[path]) == 0 {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, "fdinfo", fd.Name()))
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 2 && fields[0] == "flags:" {
					flags, err := strconv.ParseUint(fields[1], 8, 64)
					if err == nil && flags&3 != 0 {
						for _, original := range aliases[path] {
							result[original] = rolloutOwnerEvidence{known: true, alive: true}
						}
					}
				}
			}
		}
	}
	return result
}
