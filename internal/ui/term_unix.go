// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux || darwin

package ui

import (
	"os"
	"syscall"
	"unsafe"
)

// termWidth returns the terminal's column count. Re-queried on every repaint
// (one ioctl) instead of tracking SIGWINCH.
func termWidth(f *os.File) (int, bool) {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	return int(ws.Col), errno == 0 && ws.Col > 0
}

// enableVT is a no-op on unix terminals, which speak ANSI natively. The
// returned restore func is likewise a no-op.
func enableVT(f *os.File) (func(), bool) { return func() {}, true }
