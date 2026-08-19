// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package main

import "syscall"

// processAlive reports whether pid refers to a live process. Signal(0) is not
// implemented on Windows (it always errors, which would make every lock look
// stale), so ask the kernel directly.
func processAlive(pid int) bool {
	const stillActive = 259 // STILL_ACTIVE
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied means the process exists but isn't ours: alive.
		return err == syscall.ERROR_ACCESS_DENIED
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true // it opened; assume alive
	}
	return code == stillActive
}
