// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux || darwin

package main

import "syscall"

// diskFree returns the bytes available to this user on the filesystem
// containing dir.
func diskFree(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
