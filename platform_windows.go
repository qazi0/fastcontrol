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

	"github.com/Microsoft/go-winio"
)

func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000 | 0x00000200}
}
func interruptProcess(p *os.Process) { killProcess(p) }
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
