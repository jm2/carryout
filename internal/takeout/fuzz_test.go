// SPDX-License-Identifier: GPL-3.0-or-later

package takeout

import "testing"

// FuzzParseInventory hardens the export-summary parser against arbitrary
// pasted page text. The invariants: never panic, never return success with an
// empty inventory, and — the security property — every accepted filename must
// be safe to use as an on-disk name (no separators, no traversal).
func FuzzParseInventory(f *testing.F) {
	f.Add(realExportSummary())
	f.Add("takeout-20260818T182058Z-1-001.tgz\ntakeout-20260818T182058Z-1-002.tgz\n")
	f.Add("Downloadable Zips: takeout-x-001.tgz (Number of times already downloaded: 3), takeout")
	f.Fuzz(func(t *testing.T, s string) {
		entries, err := ParseInventory(s)
		if err != nil {
			return
		}
		if len(entries) == 0 {
			t.Fatal("nil error with empty inventory")
		}
		seen := make(map[string]bool, len(entries))
		for _, e := range entries {
			if !ValidFilename(e.Filename) {
				t.Fatalf("unsafe filename accepted: %q", e.Filename)
			}
			if seen[e.Filename] {
				t.Fatalf("duplicate filename survived dedup: %q", e.Filename)
			}
			seen[e.Filename] = true
		}
	})
}

// FuzzDerive guards the capture path the same way: any URL Derive accepts
// must yield a filesystem-safe captured filename (the path-traversal guard).
func FuzzDerive(f *testing.F) {
	f.Add("https://takeout-download.usercontent.google.com/download/takeout-20260818T182058Z-3-001.tgz?j=a&user=1&i=0")
	f.Add("https://host/download/x-001.tgz")
	f.Add(`https://host/download/evil%5C..%5C..%5Cx-001.tgz?i=0`)
	f.Fuzz(func(t *testing.T, raw string) {
		tmpl, err := Derive(raw)
		if err != nil {
			return
		}
		if !ValidFilename(tmpl.CapturedName) {
			t.Fatalf("unsafe captured filename accepted: %q", tmpl.CapturedName)
		}
	})
}
