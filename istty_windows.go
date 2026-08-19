// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package main

import (
	"os"
	"syscall"
)

// isTTY reports whether f is an interactive console. A char-device check is
// not enough on Windows: NUL and hidden service consoles are char devices
// too, and an unattended `carryout get` must exit for re-auth instead of
// prompting a console nobody sees.
func isTTY(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}
