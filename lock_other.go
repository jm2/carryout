// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !darwin && !windows

package main

import "os"

func processAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}
