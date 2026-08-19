// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether pid refers to a live process. EPERM from
// kill(pid, 0) means the process exists but belongs to another user — that
// is ALIVE, not stale.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
