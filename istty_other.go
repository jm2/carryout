// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !darwin

package main

import "os"

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
