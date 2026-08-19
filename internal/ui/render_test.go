// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jm2/carryout/internal/fetch"
)

// testRenderer builds a renderer with an injected buffer, fixed width, and a
// manual clock; ticks are driven by hand for determinism.
func testRenderer(width int) (*Renderer, *bytes.Buffer, func(time.Duration)) {
	var buf bytes.Buffer
	r := newRenderer(&buf, func() (int, bool) { return width, true })
	cur := time.Unix(1700000000, 0)
	r.now = func() time.Time { return cur }
	r.lastSample = cur
	advance := func(d time.Duration) { cur = cur.Add(d) }
	return r, &buf, advance
}

func snapWith(parts ...fetch.ActivePart) func() fetch.Snapshot {
	return func() fetch.Snapshot {
		return fetch.Snapshot{
			Active: parts, RunBytes: 1000, Done: 1, Total: 5,
			DoneBytes: 5000, Remaining: 20000,
		}
	}
}

func TestEventLineScrollsAboveBlock(t *testing.T) {
	r, buf, advance := testRenderer(100)
	r.snap = snapWith(
		fetch.ActivePart{Num: 1, Filename: "x-001.tgz", Cur: 100, Expected: 1000},
		fetch.ActivePart{Num: 2, Filename: "x-002.tgz", Cur: 300, Expected: 1000},
	)
	r.tick()
	advance(300 * time.Millisecond)
	r.tick()
	r.Logf("part 003: download started")

	screen := playVT(buf.String()).rendered()
	if len(screen) < 4 {
		t.Fatalf("screen too short: %q", screen)
	}
	if !strings.Contains(screen[0], "part 003: download started") {
		t.Errorf("event line not above the block: %q", screen[0])
	}
	joined := strings.Join(screen[1:], "\n")
	for _, want := range []string{"part 001", "part 002", "parts 1/5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("block missing %q after event:\n%s", want, joined)
		}
	}
}

func TestHoldReleaseSemantics(t *testing.T) {
	r, buf, advance := testRenderer(100)
	r.snap = snapWith(fetch.ActivePart{Num: 1, Filename: "x-001.tgz", Cur: 100, Expected: 1000})
	r.tick()

	r.Hold()
	held := playVT(buf.String())
	if len(held.rendered()) != 0 {
		t.Errorf("block not erased on Hold: %q", held.rendered())
	}
	if held.cursorHidden || held.wrapDisabled {
		t.Error("Hold must restore cursor and autowrap for the prompt")
	}

	before := buf.Len()
	r.Logf("part 001: downloaded")
	r.Progressf("tick tick")
	advance(300 * time.Millisecond)
	r.tick()
	if buf.Len() != before {
		t.Errorf("output written while held: %q", buf.String()[before:])
	}

	r.Release()
	screen := playVT(buf.String())
	joined := strings.Join(screen.rendered(), "\n")
	if !strings.Contains(joined, "1 log line(s) arrived while the prompt was up") {
		t.Errorf("replay header missing:\n%s", joined)
	}
	if !strings.Contains(joined, "part 001: downloaded") {
		t.Errorf("held event not replayed:\n%s", joined)
	}
	if strings.Contains(joined, "tick tick") {
		t.Errorf("progress tick must be dropped, not replayed:\n%s", joined)
	}
	if !screen.cursorHidden || !screen.wrapDisabled {
		t.Error("Release must re-hide the cursor and disable autowrap")
	}
}

func TestBlockShrinkLeavesNoStaleRows(t *testing.T) {
	r, buf, advance := testRenderer(100)
	r.snap = snapWith(
		fetch.ActivePart{Num: 1, Filename: "x-001.tgz", Cur: 1, Expected: 10},
		fetch.ActivePart{Num: 2, Filename: "x-002.tgz", Cur: 1, Expected: 10},
		fetch.ActivePart{Num: 3, Filename: "x-003.tgz", Cur: 1, Expected: 10},
		fetch.ActivePart{Num: 4, Filename: "x-004.tgz", Cur: 1, Expected: 10},
	)
	r.tick()
	r.snap = snapWith(
		fetch.ActivePart{Num: 1, Filename: "x-001.tgz", Cur: 2, Expected: 10},
		fetch.ActivePart{Num: 2, Filename: "x-002.tgz", Cur: 2, Expected: 10},
	)
	advance(300 * time.Millisecond)
	r.tick()

	screen := playVT(buf.String()).rendered()
	rows := 0
	for _, ln := range screen {
		if strings.HasPrefix(ln, "part ") {
			rows++
		}
		if strings.Contains(ln, "part 003") || strings.Contains(ln, "part 004") {
			t.Errorf("stale row survived shrink: %q", ln)
		}
	}
	if rows != 2 {
		t.Errorf("expected 2 part rows after shrink, got %d:\n%s", rows, strings.Join(screen, "\n"))
	}
}

func TestIdenticalFrameSkipsWrite(t *testing.T) {
	r, buf, advance := testRenderer(100)
	// a fully stalled run: no bytes moving anywhere
	r.snap = func() fetch.Snapshot {
		return fetch.Snapshot{
			Active: []fetch.ActivePart{{Num: 1, Filename: "x-001.tgz", Cur: 100, Expected: 1000}},
			Done:   1, Total: 5, DoneBytes: 5000, Remaining: 20000,
		}
	}
	r.tick() // paint (speed —)
	advance(300 * time.Millisecond)
	r.tick() // speed settles to 0 B/s
	advance(300 * time.Millisecond)
	r.tick() // identical from here on
	n := buf.Len()
	for range 5 {
		advance(300 * time.Millisecond)
		r.tick()
	}
	if buf.Len() != n {
		t.Errorf("identical frames wrote %d bytes", buf.Len()-n)
	}
}

func TestCloseRestoresTerminal(t *testing.T) {
	r, buf, _ := testRenderer(100)
	r.snap = snapWith(fetch.ActivePart{Num: 1, Filename: "x-001.tgz", Cur: 100, Expected: 1000})
	r.tick()
	r.Close()
	r.Close() // idempotent

	screen := playVT(buf.String())
	if screen.cursorHidden || screen.wrapDisabled {
		t.Error("Close must restore cursor and autowrap")
	}
	if got := screen.rendered(); len(got) != 0 {
		t.Errorf("block not erased on Close: %q", got)
	}
}

func TestSpeedEMAResetsOnLowerOffset(t *testing.T) {
	r, _, advance := testRenderer(100)

	advance(time.Second)
	r.updateSpeeds(fetch.Snapshot{Active: []fetch.ActivePart{{Num: 1, Cur: 1000}}})
	if r.partSeen[1] {
		t.Error("first sample must not claim a speed yet")
	}

	advance(time.Second)
	r.updateSpeeds(fetch.Snapshot{Active: []fetch.ActivePart{{Num: 1, Cur: 5000}}})
	if !r.partSeen[1] || r.partSpeed[1] <= 0 {
		t.Errorf("speed not established: seen=%v speed=%f", r.partSeen[1], r.partSpeed[1])
	}

	// retry restarted the part from a lower offset
	advance(time.Second)
	r.updateSpeeds(fetch.Snapshot{Active: []fetch.ActivePart{{Num: 1, Cur: 200}}})
	if r.partSeen[1] || r.partSpeed[1] != 0 {
		t.Errorf("EMA not reset on lower offset: seen=%v speed=%f", r.partSeen[1], r.partSpeed[1])
	}

	// vanished part must be forgotten
	advance(time.Second)
	r.updateSpeeds(fetch.Snapshot{})
	if _, ok := r.lastCur[1]; ok {
		t.Error("vanished part not cleaned up")
	}
}
