// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package ui

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll") // known DLL: always resolved from System32
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
)

type coord struct{ X, Y int16 }
type smallRect struct{ Left, Top, Right, Bottom int16 }
type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// termWidth returns the console WINDOW width (not the scrollback buffer
// width, which can be far larger).
func termWidth(f *os.File) (int, bool) {
	var info consoleScreenBufferInfo
	r, _, _ := procGetConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0, false
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	return w, w > 0
}

// enableVT turns on ANSI escape processing. Fails on pre-Win10 conhost, in
// which case the caller falls back to plain output.
func enableVT(f *os.File) bool {
	const enableVirtualTerminalProcessing = 0x0004
	var mode uint32
	if err := syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode); err != nil {
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	r, _, _ := procSetConsoleMode.Call(f.Fd(), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
