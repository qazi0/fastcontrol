package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
)

var (
	kernel32DLL                 = syscall.NewLazyDLL("kernel32.dll")
	procExpandEnvironmentString = kernel32DLL.NewProc("ExpandEnvironmentStringsW")
)

func init() {
	platformWindowsLivePathDirs = windowsLivePathDirs
	platformWindowsEnvVar = windowsRegistryEnvVar
	platformBuildCLICmd = windowsBuildCLICmd
}

func prepareProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x08000000 | 0x00000200
	cmd.SysProcAttr.HideWindow = true
}

func windowsExpandEnvString(raw string) string {
	if raw == "" || !strings.Contains(raw, "%") {
		return raw
	}
	src, err := syscall.UTF16PtrFromString(raw)
	if err != nil {
		return expandWindowsEnvVars(raw, os.Getenv)
	}
	n, _, _ := procExpandEnvironmentString.Call(uintptr(unsafe.Pointer(src)), 0, 0)
	if n == 0 || n > 1<<18 {
		return expandWindowsEnvVars(raw, os.Getenv)
	}
	dst := make([]uint16, n)
	r, _, _ := procExpandEnvironmentString.Call(uintptr(unsafe.Pointer(src)), uintptr(unsafe.Pointer(&dst[0])), n)
	if r == 0 || r > n {
		return expandWindowsEnvVars(raw, os.Getenv)
	}
	expanded := syscall.UTF16ToString(dst[:r])
	if strings.Contains(expanded, "%") {
		expanded = expandWindowsEnvVars(expanded, os.Getenv)
	}
	return expanded
}

func windowsRegistryStringValue(root syscall.Handle, subkey, name string) string {
	subkeyPtr, err := syscall.UTF16PtrFromString(subkey)
	if err != nil {
		return ""
	}
	var handle syscall.Handle
	if err := syscall.RegOpenKeyEx(root, subkeyPtr, 0, syscall.KEY_READ, &handle); err != nil {
		return ""
	}
	defer syscall.RegCloseKey(handle)

	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return ""
	}
	var valType uint32
	var bufLen uint32
	if err := syscall.RegQueryValueEx(handle, namePtr, nil, &valType, nil, &bufLen); err != nil || bufLen == 0 {
		return ""
	}
	if valType != syscall.REG_SZ && valType != syscall.REG_EXPAND_SZ {
		return ""
	}
	// Bound allocation to protect against corrupt registry entries.
	if bufLen > 1<<20 {
		return ""
	}
	buf := make([]uint16, (bufLen+1)/2+1)
	size := uint32(len(buf) * 2)
	if err := syscall.RegQueryValueEx(handle, namePtr, nil, &valType, (*byte)(unsafe.Pointer(&buf[0])), &size); err != nil {
		return ""
	}
	n := int(size / 2)
	if n > len(buf) {
		n = len(buf)
	}
	raw := syscall.UTF16ToString(buf[:n])
	if valType == syscall.REG_EXPAND_SZ || strings.Contains(raw, "%") {
		raw = windowsExpandEnvString(raw)
	}
	return raw
}

func windowsRegistryEnvVar(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	if v := strings.TrimSpace(windowsRegistryStringValue(syscall.HKEY_CURRENT_USER, `Environment`, name)); v != "" {
		return v
	}
	return strings.TrimSpace(windowsRegistryStringValue(syscall.HKEY_LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, name))
}

func windowsLivePathDirs() []string {
	userPath := windowsRegistryStringValue(syscall.HKEY_CURRENT_USER, `Environment`, "Path")
	sysPath := windowsRegistryStringValue(syscall.HKEY_LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, "Path")
	combined := userPath
	if sysPath != "" {
		if combined != "" {
			combined += ";" + sysPath
		} else {
			combined = sysPath
		}
	}
	if combined == "" {
		return nil
	}
	return splitWindowsPathList(combined, os.Getenv)
}

func windowsBuildCLICmd(ctx context.Context, bin string, args ...string) (*exec.Cmd, error) {
	if !isWindowsBatchFile(bin) {
		if ctx != nil {
			return exec.CommandContext(ctx, bin, args...), nil
		}
		return exec.Command(bin, args...), nil
	}
	comspec := strings.TrimSpace(os.Getenv("ComSpec"))
	if comspec == "" {
		if sysRoot := strings.TrimSpace(os.Getenv("SystemRoot")); sysRoot != "" {
			comspec = filepath.Join(sysRoot, "System32", "cmd.exe")
		} else {
			comspec = "cmd.exe"
		}
	}
	cmdLine, err := buildWindowsBatchCmdLine(comspec, bin, args)
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	if ctx != nil {
		cmd = exec.CommandContext(ctx, comspec)
	} else {
		cmd = exec.Command(comspec)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: cmdLine}
	return cmd, nil
}
func interruptProcess(p *os.Process) { killProcess(p) }
func terminateProcess(p *os.Process) { killProcess(p) }
func killProcess(p *os.Process) {
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill.exe", "/PID", strconv.Itoa(p.Pid), "/T", "/F")
	prepareProcess(cmd)
	_ = cmd.Run()
	_ = p.Kill()
}
func platformProcessAlive(pid int) bool {
	const (
		processQueryLimitedInformation = 0x1000
		stillActive                    = 259
	)
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, os.ErrPermission)
	}
	defer syscall.CloseHandle(h)
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}
func dialDesktopIPC(path string) (net.Conn, error) {
	timeout := 5 * time.Second
	return winio.DialPipe(path, &timeout)
}
func platformIPCPath() string { return `\\.\pipe\codex-ipc` }
func desktopBundlePath() string {
	if base := codexPackagePath(); base != "" {
		return filepath.Join(base, "app", "resources", "app.asar")
	}
	return ""
}
func codexPackagePath() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "(Get-AppxPackage OpenAI.Codex | Select-Object -First 1).InstallLocation")
	prepareProcess(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func openDesktopThread(id string) error {
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", "codex://threads/"+id).Run()
}
func audioDecodeCommand(ctx context.Context, input, output string) *exec.Cmd {
	return exec.CommandContext(ctx, "ffmpeg.exe", "-nostdin", "-v", "error", "-y", "-i", input, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", output)
}
func compressedVoiceAvailable() bool { _, err := exec.LookPath("ffmpeg.exe"); return err == nil }
func speechRuntimeName() string      { return "whisper-cli.exe" }
func sqliteBinary() string           { return "sqlite3.exe" }
func defaultRouteIP() string {
	// A UDP dial selects the routing interface without sending a packet.
	conn, err := net.Dial("udp", "1.1.1.1:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return addr.IP.String()
}
func hostName() string { name, _ := os.Hostname(); return name }

func windowsCodexBin() (string, error) {
	if p, err := exec.LookPath("codex.exe"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	patterns := []string{filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenAI", "Codex", "bin", "*", "codex.exe"), filepath.Join(home, ".local", "bin", "codex.exe")}
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for i := len(matches) - 1; i >= 0; i-- {
			if speechExecutable(matches[i]) {
				return matches[i], nil
			}
		}
	}
	if base := codexPackagePath(); base != "" {
		for _, p := range []string{filepath.Join(base, "app", "resources", "codex.exe"), filepath.Join(base, "app", "resources", "codex-cli", "bin", "codex.exe")} {
			if speechExecutable(p) {
				return p, nil
			}
		}
	}
	return "", errors.New("Install The Native Codex CLI Or Codex Desktop")
}
