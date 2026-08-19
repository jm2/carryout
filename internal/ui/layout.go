// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jm2/carryout/internal/fetch"
)

// cells returns the display width of s in terminal cells. Inputs are either
// validated-ASCII Takeout filenames (takeout.ValidFilename forbids anything
// else) or strings assembled here from single-cell glyphs ('█', '░', '─',
// '…'), so rune count equals cell count. No combining or double-width
// handling — if the filename charset ever widens, this is the tripwire.
func cells(s string) int { return utf8.RuneCountInString(s) }

const (
	barMax  = 30
	barMin  = 10
	nameMin = 12
	gap     = "  "
)

// bar renders a fraction as a block bar of exactly width cells (no brackets).
func bar(frac float64, width int) string {
	if width <= 0 {
		return ""
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// ellipsize shortens a filename to at most max cells with a middle ellipsis,
// keeping the distinguishing "-G-NNN.ext" tail. Returns "" when max is too
// small to be useful.
func ellipsize(name string, max int) string {
	if cells(name) <= max {
		return name
	}
	if max < 5 {
		return ""
	}
	r := []rune(name)
	tail := (max - 1) * 2 / 3
	head := max - 1 - tail
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// fit is the single place the never-wrap guarantee is enforced: every
// rendered line passes through it and is truncated if a layout bug upstream
// ever overruns the terminal width.
func fit(s string, usable int) string {
	if cells(s) <= usable {
		return s
	}
	if usable < 1 {
		return ""
	}
	r := []rune(s)
	return string(r[:usable-1]) + "…"
}

// flex shrinks the variable elements of a row (bar, then filename) in the
// agreed order until prefix+name+bar+right fits within usable cells. It
// returns the chosen name width and bar width (either may reach 0).
func flex(usable, fixed, nameW, barW int) (int, int) {
	over := func() int {
		total := fixed
		if nameW > 0 {
			total += len(gap) + nameW
		}
		if barW > 0 {
			total += len(gap) + barW + 2 // brackets
		}
		return total - usable
	}
	for over() > 0 && barW > barMin {
		barW--
	}
	for over() > 0 && nameW > nameMin {
		nameW--
	}
	if over() > 0 && barW > 0 {
		barW = 0
	}
	for over() > 0 && nameW > 0 {
		nameW--
	}
	return nameW, barW
}

// partRow renders one active download line within usable cells:
//
//	part 034  …Z-2-015.tgz  [██░░░░░░░░]   11%  104.1 MiB/s  5.6/50.0 GiB
func partRow(a fetch.ActivePart, speed float64, haveSpeed bool, usable int) string {
	prefix := fmt.Sprintf("part %03d", a.Num)

	spd := "—"
	if haveSpeed {
		spd = fetch.HumanBytes(int64(speed)) + "/s"
	}
	// The current-bytes side is right-aligned in a fixed 10-cell column (the
	// widest HumanBytes output): a variable-width field here would feed the
	// flex math a different budget every tick and make the BAR resize as
	// bytes flow — visible jitter on a static terminal.
	var frac float64
	pct := ""
	sizes := fmt.Sprintf("%10s", fetch.HumanBytes(a.Cur))
	if a.Expected > 0 {
		frac = float64(a.Cur) / float64(a.Expected)
		pct = fmt.Sprintf("%4.0f%%", frac*100)
		sizes = fmt.Sprintf("%10s/%s", fetch.HumanBytes(a.Cur), fetch.HumanBytes(a.Expected))
	}

	build := func(nameW, barW int, withSpeed, withSizes bool) string {
		var b strings.Builder
		b.WriteString(prefix)
		if nameW > 0 {
			b.WriteString(gap)
			b.WriteString(ellipsize(a.Filename, nameW))
		}
		if barW > 0 {
			b.WriteString(gap)
			b.WriteString("[")
			b.WriteString(bar(frac, barW))
			b.WriteString("]")
		}
		if pct != "" {
			b.WriteString(gap)
			b.WriteString(pct)
		}
		if withSpeed {
			b.WriteString(gap)
			b.WriteString(fmt.Sprintf("%12s", spd))
		}
		if withSizes {
			b.WriteString(gap)
			b.WriteString(sizes)
		}
		return b.String()
	}

	rightWidth := func(withSpeed, withSizes bool) int {
		w := 0
		if pct != "" {
			w += len(gap) + cells(pct)
		}
		if withSpeed {
			w += len(gap) + 12
		}
		if withSizes {
			w += len(gap) + cells(sizes)
		}
		return w
	}

	wantBar := 0
	if a.Expected > 0 {
		wantBar = barMax
	}
	// Shrink order: bar 30→10, filename ellipsized to 12, bar dropped, name
	// dropped, then sizes dropped, then speed dropped, then hard truncate.
	for _, right := range []struct{ speed, sizes bool }{{true, true}, {true, false}, {false, false}} {
		fixed := cells(prefix) + rightWidth(right.speed, right.sizes)
		nameW, barW := flex(usable, fixed, cells(a.Filename), wantBar)
		row := build(nameW, barW, right.speed, right.sizes)
		if cells(row) <= usable {
			return row
		}
	}
	return fit(build(0, 0, false, false), usable)
}

// aggRow renders the pinned aggregate footer within usable cells:
//
//	parts 36/142  [█████░░░░░]  25%  602.0 MiB/s  1.7/6.8 TiB  ETA 2h28m
func aggRow(s fetch.Snapshot, speed float64, haveSpeed bool, usable int) string {
	completed := s.Done + s.Downloaded
	prefix := fmt.Sprintf("parts %d/%d", completed, s.Total)

	var cum int64 = s.DoneBytes
	for _, a := range s.Active {
		cum += a.Cur
	}

	spd := "—"
	if haveSpeed {
		spd = fetch.HumanBytes(int64(speed)) + "/s"
	}

	// Same fixed-width treatment as partRow: the cumulative side must not
	// re-flex the footer bar as bytes arrive.
	var frac float64
	pct, sizes, eta := "", fmt.Sprintf("%10s", fetch.HumanBytes(cum)), ""
	wantBar := 0
	if s.Remaining >= 0 {
		total := cum + s.Remaining
		if total > 0 {
			frac = float64(cum) / float64(total)
			pct = fmt.Sprintf("%4.0f%%", frac*100)
			wantBar = barMax
		}
		sizes = fmt.Sprintf("%10s/%s", fetch.HumanBytes(cum), fetch.HumanBytes(total))
		if haveSpeed {
			if e, ok := fetch.ETAString(s.Remaining, speed); ok {
				// Fixed 9-cell column: padded when short, clamped when a
				// future ETAString change ever overruns its width contract.
				eta = fmt.Sprintf("ETA %9s", fit(e, 9))
			}
		}
	}

	build := func(barW int, withSpeed, withSizes, withETA bool) string {
		var b strings.Builder
		b.WriteString(prefix)
		if barW > 0 {
			b.WriteString(gap)
			b.WriteString("[")
			b.WriteString(bar(frac, barW))
			b.WriteString("]")
		}
		if pct != "" {
			b.WriteString(gap)
			b.WriteString(pct)
		}
		if withSpeed {
			b.WriteString(gap)
			b.WriteString(fmt.Sprintf("%12s", spd))
		}
		if withSizes {
			b.WriteString(gap)
			b.WriteString(sizes)
		}
		if withETA && eta != "" {
			b.WriteString(gap)
			b.WriteString(eta)
		}
		return b.String()
	}

	for _, v := range []struct{ speed, sizes, eta bool }{
		{true, true, true}, {true, true, false}, {true, false, false}, {false, false, false},
	} {
		fixed := cells(build(0, v.speed, v.sizes, v.eta))
		_, barW := flex(usable, fixed, 0, wantBar)
		row := build(barW, v.speed, v.sizes, v.eta)
		if cells(row) <= usable {
			return row
		}
	}
	return fit(build(0, false, false, false), usable)
}

func separator(usable int) string {
	if usable < 1 {
		return ""
	}
	return strings.Repeat("─", usable)
}
