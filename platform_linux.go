package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"syscall"
)

func prepareProcess(cmd *exec.Cmd)                 { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func interruptProcess(p *os.Process)               { _ = syscall.Kill(-p.Pid, syscall.SIGINT) }
func terminateProcess(p *os.Process)               { _ = syscall.Kill(-p.Pid, syscall.SIGTERM) }
func killProcess(p *os.Process)                    { _ = syscall.Kill(-p.Pid, syscall.SIGKILL) }
func dialDesktopIPC(path string) (net.Conn, error) { return net.Dial("unix", path) }
func platformIPCPath() string                      { return "" }
func desktopBundlePath() string                    { return "" }
func openDesktopThread(id string) error            { return errors.New("Use The Codex CLI On Linux") }
func audioDecodeCommand(ctx context.Context, input, output string) *exec.Cmd {
	return exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", input, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", output)
}
func compressedVoiceAvailable() bool { _, err := exec.LookPath("ffmpeg"); return err == nil }
func speechRuntimeName() string      { return "whisper-cli" }
func sqliteBinary() string           { return "sqlite3" }
func defaultRouteIP() string {
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
func hostName() string                 { name, _ := os.Hostname(); return name }
func windowsCodexBin() (string, error) { return "", errors.New("Windows Only") }
