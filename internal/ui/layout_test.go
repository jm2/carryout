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

func TestBarWidthStableAsBytesFlow(t *testing.T) {
	// Regression (user-reported): on a static terminal, the bar visibly
	// resized between 0-2% — dropping to nothing and reappearing as a sliver
	// — because the variable-width downloaded/total field re-flexed the bar
	// as byte counts churned through different HumanBytes widths. The bar
	// width and the total row width must be pure functions of the terminal
	// width, never of the byte count.
	exp := int64(50) << 30
	curs := []int64{0, 1, 500 << 10, 1 << 20, 999 << 20, 10 << 30, exp}
	for _, w := range []int{160, 120, 90, 80, 72, 60, 50} {
		usable := w - 1
		var bars, lens []int
		for _, cur := range curs {
			a := fetch.ActivePart{Num: 7, Filename: "takeout-20260818T182058Z-2-014.tgz", Cur: cur, Expected: exp}
			row := partRow(a, 100e6, true, usable)
			bars = append(bars, strings.Count(row, "█")+strings.Count(row, "░"))
			lens = append(lens, cells(row))
		}
		for i := 1; i < len(curs); i++ {
			if bars[i] != bars[0] {
				t.Errorf("w=%d: bar resized as bytes flowed: cur=%d → %d cells (was %d)", w, curs[i], bars[i], bars[0])
			}
			if lens[i] != lens[0] {
				t.Errorf("w=%d: row width changed as bytes flowed: cur=%d → %d cells (was %d)", w, curs[i], lens[i], lens[0])
			}
		}
	}

	// footer: cumulative bytes churn with a fixed grand total
	total := int64(7) << 40
	var bars, lens []int
	for _, cum := range []int64{0, 1 << 20, 500 << 30, total / 2, total - 1} {
		s := fetch.Snapshot{Done: 3, Total: 142, DoneBytes: cum, Remaining: total - cum}
		row := aggRow(s, 600e6, true, 119)
		bars = append(bars, strings.Count(row, "█")+strings.Count(row, "░"))
		lens = append(lens, cells(row))
	}
	for i := 1; i < len(bars); i++ {
		if bars[i] != bars[0] || lens[i] != lens[0] {
			t.Errorf("footer jitter: bars=%v lens=%v", bars, lens)
		}
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
