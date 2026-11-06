//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
)

func pairingQRColors(output io.Writer) func() {
	if file, ok := output.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprint(output, "\x1b[97;40m")
			return func() { fmt.Fprint(output, "\x1b[0m") }
		}
	}
	return func() {}
}
