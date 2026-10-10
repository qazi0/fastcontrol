package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func prepareProcess(cmd *exec.Cmd)                 { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func interruptProcess(p *os.Process)               { _ = syscall.Kill(-p.Pid, syscall.SIGINT) }
func terminateProcess(p *os.Process)               { _ = syscall.Kill(-p.Pid, syscall.SIGTERM) }
func killProcess(p *os.Process)                    { _ = syscall.Kill(-p.Pid, syscall.SIGKILL) }
func dialDesktopIPC(path string) (net.Conn, error) { return net.Dial("unix", path) }
func platformIPCPath() string                      { return "" }
func desktopBundlePath() string                    { return "/Applications/ChatGPT.app/Contents/Resources/app.asar" }
func openDesktopThread(id string) error {
	return exec.Command("/usr/bin/open", "-g", "codex://threads/"+id).Run()
}
func audioDecodeCommand(ctx context.Context, input, output string) *exec.Cmd {
	return exec.CommandContext(ctx, "/usr/bin/afconvert", "-f", "WAVE", "-d", "LEI16@16000", "-c", "1", input, output)
}
func compressedVoiceAvailable() bool { return true }
func speechRuntimeName() string      { return "whisper-cli" }
func sqliteBinary() string           { return "/usr/bin/sqlite3" }

// defaultRouteIP returns the IPv4 address of the interface carrying the default route (usually Wi-Fi).
func defaultRouteIP() string {
	out, err := exec.Command("/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "interface:" {
			ifc, err := net.InterfaceByName(f[1])
			if err != nil {
				return ""
			}
			as, _ := ifc.Addrs()
			for _, a := range as {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
					return ipn.IP.String()
				}
			}
		}
	}
	return ""
}

func hostName() string {
	if out, err := exec.Command("/usr/sbin/scutil", "--get", "ComputerName").Output(); err == nil {
		if n := strings.TrimSpace(string(out)); n != "" {
			return n
		}
	}
	h, _ := os.Hostname()
	return strings.TrimSuffix(h, ".local")
}

func windowsCodexBin() (string, error) { return "", errors.New("Windows Only") }
