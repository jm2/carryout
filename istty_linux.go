// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTTY reports whether f is an interactive terminal. A char-device check is
// not enough: /dev/null is a char device too, and `carryout get </dev/null`
// must not try to prompt.
func isTTY(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
