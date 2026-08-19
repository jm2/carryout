// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !darwin && !windows

package ui

import "os"

func termWidth(f *os.File) (int, bool)   { return 0, false }
func enableVT(f *os.File) (func(), bool) { return nil, false }
