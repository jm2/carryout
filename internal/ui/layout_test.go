// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"strings"
	"testing"

	"github.com/jm2/carryout/internal/fetch"
)

var layoutParts = []fetch.ActivePart{
	{Num: 34, Filename: "takeout-20260818T182058Z-2-015.tgz", Cur: 5_900_000_000, Expected: 53_687_091_200},
	{Num: 7, Filename: strings.Repeat("very-long-name-", 4) + "-001.tgz", Cur: 0, Expected: 0}, // unknown length
	{Num: 142, Filename: "x-001.tgz", Cur: 53_687_091_200, Expected: 53_687_091_200},           // 100%
	{Num: 1, Filename: "takeout-20260818T182058Z-8-010.tgz", Cur: 1, Expected: 7_000_000_000_000},
}

var layoutSnap = fetch.Snapshot{
	Active:     layoutParts,
	RunBytes:   1_200_000_000_000,
	Done:       35,
	Downloaded: 1,
	Total:      142,
	DoneBytes:  1_800_000_000_000,
	Remaining:  5_100_000_000_000,
}

func TestRowsNeverExceedWidth(t *testing.T) {
	for _, w := range []int{200, 120, 80, 60, 40, 28, 20, 12} {
		usable := w - 1
		for _, a := range layoutParts {
			for _, speed := range []struct {
				v    float64
				have bool
			}{{0, false}, {103.4 * 1024 * 1024, true}} {
				row := partRow(a, speed.v, speed.have, usable)
				if cells(row) > usable {
					t.Errorf("w=%d part %d: row is %d cells (> %d): %q", w, a.Num, cells(row), usable, row)
				}
			}
		}
		agg := aggRow(layoutSnap, 600*1024*1024, true, usable)
		if cells(agg) > usable {
			t.Errorf("w=%d agg: %d cells (> %d): %q", w, cells(agg), usable, agg)
		}
		if cells(separator(usable)) > usable {
			t.Errorf("w=%d separator too wide", w)
		}
	}
}

func TestFlexOrderBarThenNameThenExtras(t *testing.T) {
	a := layoutParts[0] // long filename, known size
	wide := partRow(a, 100e6, true, 199)
	if !strings.Contains(wide, a.Filename) {
		t.Errorf("wide row should keep the full filename: %q", wide)
	}
	if got := strings.Count(wide, "█") + strings.Count(wide, "░"); got != barMax {
		t.Errorf("wide row bar = %d cells, want %d", got, barMax)
	}

	medium := partRow(a, 100e6, true, 79)
	if gotBar := strings.Count(medium, "█") + strings.Count(medium, "░"); gotBar > 0 && gotBar < barMin {
		t.Errorf("bar shrank below min: %d", gotBar)
	}
	if !strings.Contains(medium, "MiB/s") || !strings.Contains(medium, "%") {
		t.Errorf("medium row lost %%/speed before bar/name: %q", medium)
	}

	narrow := partRow(a, 100e6, true, 39)
	if strings.Contains(narrow, "█") {
		t.Errorf("narrow row should have dropped the bar: %q", narrow)
	}

	tiny := partRow(a, 100e6, true, 11)
	if !strings.HasPrefix(tiny, "part 034") {
		t.Errorf("tiny row lost its identity: %q", tiny)
	}
}

func TestEllipsizeKeepsDistinguishingTail(t *testing.T) {
	name := "takeout-20260818T182058Z-2-014.tgz"
	got := ellipsize(name, 20)
	if cells(got) > 20 {
		t.Errorf("ellipsize overran: %q (%d cells)", got, cells(got))
	}
	if !strings.Contains(got, "…") || !strings.HasSuffix(got, "-2-014.tgz") {
		t.Errorf("tail not preserved: %q", got)
	}
	if ellipsize(name, 4) != "" {
		t.Error("hopelessly small budget should yield empty string")
	}
	if ellipsize("short.tgz", 20) != "short.tgz" {
		t.Error("short names must pass through untouched")
	}
}

func TestBar(t *testing.T) {
	if got := bar(0.5, 10); got != "█████░░░░░" {
		t.Errorf("bar(0.5,10) = %q", got)
	}
	if got := bar(0, 10); got != strings.Repeat("░", 10) {
		t.Errorf("bar(0,10) = %q", got)
	}
	if got := bar(1, 10); got != strings.Repeat("█", 10) {
		t.Errorf("bar(1,10) = %q", got)
	}
	if got := bar(2.5, 10); got != strings.Repeat("█", 10) {
		t.Errorf("bar clamps above 1: %q", got)
	}
}

func TestFitHardClamp(t *testing.T) {
	got := fit(strings.Repeat("x", 100), 20)
	if cells(got) != 20 || !strings.HasSuffix(got, "…") {
		t.Errorf("fit = %q (%d cells)", got, cells(got))
	}
	if fit("ok", 20) != "ok" {
		t.Error("fit must not touch fitting strings")
	}
}

func TestAggRowUnknownRemaining(t *testing.T) {
	s := layoutSnap
	s.Remaining = -1
	row := aggRow(s, 0, false, 119)
	if strings.Contains(row, "%") || strings.Contains(row, "ETA") || strings.Contains(row, "█") {
		t.Errorf("unknown remaining must drop bar/%%/ETA: %q", row)
	}
	if !strings.Contains(row, "parts 36/142") {
		t.Errorf("counts missing: %q", row)
	}
}

func TestAggRowComplete(t *testing.T) {
	row := aggRow(layoutSnap, 600*1024*1024, true, 159)
	for _, want := range []string{"parts 36/142", "%", "MiB/s", "ETA"} {
		if !strings.Contains(row, want) {
			t.Errorf("agg row missing %q: %q", want, row)
		}
	}
}
