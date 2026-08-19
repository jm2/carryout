// SPDX-License-Identifier: GPL-3.0-or-later

//go:build darwin

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func isTTY(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGETA, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
