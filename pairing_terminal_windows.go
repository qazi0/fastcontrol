package main

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

func pairingQRColors(output io.Writer) func() {
	file, ok := output.(*os.File)
	if !ok {
		return func() {}
	}
	handle := windows.Handle(file.Fd())
	var info windows.ConsoleScreenBufferInfo
	if windows.GetConsoleScreenBufferInfo(handle, &info) != nil {
		return func() {}
	}
	setColor := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleTextAttribute")
	if setColor.Find() != nil {
		return func() {}
	}
	status, _, _ := setColor.Call(uintptr(handle), 15)
	if status == 0 {
		return func() {}
	}
	return func() { setColor.Call(uintptr(handle), uintptr(info.Attributes)) }
}
