package main

import (
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var restartManager = windows.NewLazySystemDLL("rstrtmgr.dll")
var rmStart = restartManager.NewProc("RmStartSession")
var rmRegister = restartManager.NewProc("RmRegisterResources")
var rmList = restartManager.NewProc("RmGetList")
var rmEnd = restartManager.NewProc("RmEndSession")

type rmProcessInfo struct {
	PID         uint32
	Started     windows.Filetime
	Name        [256]uint16
	Service     [64]uint16
	Type        uint32
	Status      uint32
	Session     uint32
	Restartable int32
}

// This only asks which processes use a resource. It never invokes Restart
// Manager's shutdown/restart functions and never locks the transcript file.
func rmHasOwner(paths []string) (alive, known bool) {
	for _, proc := range []*windows.LazyProc{rmStart, rmRegister, rmList, rmEnd} {
		if proc.Find() != nil {
			return false, false
		}
	}
	var handle uint32
	var key [33]uint16
	status, _, _ := rmStart.Call(uintptr(unsafe.Pointer(&handle)), 0, uintptr(unsafe.Pointer(&key[0])))
	if status != 0 {
		return false, false
	}
	defer rmEnd.Call(uintptr(handle))
	names := make([]*uint16, 0, len(paths))
	for _, path := range paths {
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return false, false
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return false, true
	}
	status, _, _ = rmRegister.Call(uintptr(handle), uintptr(len(names)), uintptr(unsafe.Pointer(&names[0])), 0, 0, 0, 0)
	runtime.KeepAlive(names)
	if status != 0 {
		return false, false
	}
	var needed, count, reasons uint32
	status, _, _ = rmList.Call(uintptr(handle), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)), 0, uintptr(unsafe.Pointer(&reasons)))
	for attempt := 0; attempt < 3; attempt++ {
		if status == 0 && count == 0 {
			// Reboot reasons concern shutdown privileges, not whether the list
			// was read successfully. Our own read handle can set DetectedSelf.
			return false, true
		}
		if status != 234 || needed == 0 || needed > 1024 {
			return false, false
		}
		items := make([]rmProcessInfo, needed)
		count = needed
		status, _, _ = rmList.Call(uintptr(handle), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&items[0])), uintptr(unsafe.Pointer(&reasons)))
		if status == 0 {
			for _, item := range items[:count] {
				if item.PID != uint32(os.Getpid()) {
					return true, true
				}
			}
			// Reboot reasons concern shutdown privileges, not whether the list
			// was read successfully. Our own read handle can set DetectedSelf.
			return false, true
		}
	}
	return false, false
}

func platformRolloutOwners(paths []string) map[string]rolloutOwnerEvidence {
	result := map[string]rolloutOwnerEvidence{}
	alive, known := rmHasOwner(paths)
	if !known {
		return result
	}
	if !alive || len(paths) == 1 {
		for _, path := range paths {
			result[path] = rolloutOwnerEvidence{known: true, alive: alive}
		}
		return result
	}
	// Usually all stale transcripts are abandoned, handled by one batch above.
	// Only split when a real owner exists and needs attribution to its file.
	for _, path := range paths {
		alive, known := rmHasOwner([]string{path})
		result[path] = rolloutOwnerEvidence{known: known, alive: alive}
	}
	return result
}
