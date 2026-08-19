// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !darwin && !windows

package main

func diskFree(dir string) (uint64, bool) { return 0, false }
