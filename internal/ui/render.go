// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jm2/carryout/internal/fetch"
)

const (
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
	wrapOff    = "\x1b[?7l" // autowrap off: the belt on the never-wrap guarantee
	wrapOn     = "\x1b[?7h"
	eraseTail  = "\x1b[K" // to end of line
	eraseBelow = "\x1b[J" // to end of screen
)

// Renderer is the live console: scrolling event lines above a pinned block of
// per-download progress rows plus an aggregate footer, repainted by polling a
// fetch.Snapshot. A single mutex owns the terminal; every operation composes
// its full byte sequence and issues exactly one Write (no flicker, no
// tearing, no buffering of os.Stdout).
type Renderer struct {
	mu       sync.Mutex
	out      interface{ Write([]byte) (int, error) }
	width    func() (int, bool)
	interval time.Duration
	now      func() time.Time

	k         int // block lines currently painted (0 = none)
	held      bool
	heldBuf   []string
	lastFrame string
	lastW     int

	snap      func() fetch.Snapshot
	started   bool
	stop      chan struct{}
	done      chan struct{}
	closed    sync.Once
	restoreVT func() // undo enableVT's console-mode change (Windows)

	// speed tracking (EMA, τ ≈ 4s)
	lastSample time.Time
	lastCur    map[int]int64
	partSpeed  map[int]float64
	partSeen   map[int]bool
	lastRun    int64
	aggSpeed   float64
	aggSeen    bool
}

func newRenderer(out interface{ Write([]byte) (int, error) }, width func() (int, bool)) *Renderer {
	return &Renderer{
		out:       out,
		width:     width,
		interval:  300 * time.Millisecond,
		now:       time.Now,
		lastCur:   make(map[int]int64),
		partSpeed: make(map[int]float64),
		partSeen:  make(map[int]bool),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// NewLive returns a live renderer when out is a usable ANSI terminal; ok is
// false when the caller should use NewPlain instead (not a TTY, TERM=dumb,
// VT unavailable, width unknown).
func NewLive(out *os.File, tty bool) (*Renderer, bool) {
	if !tty || os.Getenv("TERM") == "dumb" {
		return nil, false
	}
	restore, ok := enableVT(out)
	if !ok {
		return nil, false
	}
	if _, ok := termWidth(out); !ok {
		restore() // don't leave the console mode changed on the plain path
		return nil, false
	}
	r := newRenderer(out, func() (int, bool) { return termWidth(out) })
	r.restoreVT = restore
	return r, true
}

// Start begins polling snap and painting the pinned block.
func (r *Renderer) Start(snap func() fetch.Snapshot) {
	r.mu.Lock()
	r.snap = snap
	r.started = true
	r.lastSample = r.now()
	r.out.Write([]byte(hideCursor + wrapOff))
	r.mu.Unlock()

	go func() {
		defer close(r.done)
		t := time.NewTicker(r.interval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.tick()
			}
		}
	}()
}

func (r *Renderer) tick() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held || r.snap == nil {
		return
	}
	s := r.snap()
	r.updateSpeeds(s)
	r.repaintLocked(s)
}

// updateSpeeds folds a snapshot into the per-part and aggregate EMAs.
func (r *Renderer) updateSpeeds(s fetch.Snapshot) {
	now := r.now()
	dt := now.Sub(r.lastSample).Seconds()
	if dt <= 0 {
		return
	}
	alpha := 1 - math.Exp(-dt/4.0)

	seen := make(map[int]bool, len(s.Active))
	for _, a := range s.Active {
		seen[a.Num] = true
		last, ok := r.lastCur[a.Num]
		if !ok || a.Cur < last {
			// New part, or a retry restarted from a lower offset: reset
			// rather than folding a negative delta into the average.
			r.lastCur[a.Num] = a.Cur
			r.partSpeed[a.Num] = 0
			r.partSeen[a.Num] = false
			continue
		}
		inst := float64(a.Cur-last) / dt
		r.partSpeed[a.Num] += alpha * (inst - r.partSpeed[a.Num])
		if r.partSpeed[a.Num] < 0.5 {
			r.partSpeed[a.Num] = 0 // settle stalls instead of decaying forever
		}
		r.partSeen[a.Num] = true
		r.lastCur[a.Num] = a.Cur
	}
	for num := range r.lastCur {
		if !seen[num] {
			delete(r.lastCur, num)
			delete(r.partSpeed, num)
			delete(r.partSeen, num)
		}
	}

	if s.RunBytes >= r.lastRun {
		inst := float64(s.RunBytes-r.lastRun) / dt
		r.aggSpeed += alpha * (inst - r.aggSpeed)
		if r.aggSpeed < 0.5 {
			r.aggSpeed = 0
		}
		r.aggSeen = true
	}
	r.lastRun = s.RunBytes
	r.lastSample = now
}

func (r *Renderer) buildLines(s fetch.Snapshot, usable int) []string {
	lines := make([]string, 0, len(s.Active)+2)
	for _, a := range s.Active {
		lines = append(lines, fit(partRow(a, r.partSpeed[a.Num], r.partSeen[a.Num], usable), usable))
	}
	lines = append(lines, separator(usable))
	lines = append(lines, fit(aggRow(s, r.aggSpeed, r.aggSeen, usable), usable))
	return lines
}

// repaintLocked repaints the pinned block in place. Caller holds r.mu.
func (r *Renderer) repaintLocked(s fetch.Snapshot) {
	w, ok := r.width()
	if !ok || w < 2 {
		w = 80
	}
	usable := w - 1
	lines := r.buildLines(s, usable)
	frame := strings.Join(lines, "\n")
	if frame == r.lastFrame && w == r.lastW && r.k == len(lines) {
		return // nothing changed: write zero bytes (SSH thrift, stall calm)
	}

	var b bytes.Buffer
	b.WriteString("\r")
	if r.k > 1 {
		fmt.Fprintf(&b, "\x1b[%dA", r.k-1)
	}
	for i, ln := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(ln)
		b.WriteString(eraseTail)
	}
	b.WriteString(eraseBelow)
	r.out.Write(b.Bytes())
	r.k = len(lines)
	r.lastFrame = frame
	r.lastW = w
}

// eraseBlockLocked removes the pinned block, leaving the cursor at column 0
// of what was its first line. Caller holds r.mu.
func (r *Renderer) eraseBlockLocked(b *bytes.Buffer) {
	b.WriteString("\r")
	if r.k > 1 {
		fmt.Fprintf(b, "\x1b[%dA", r.k-1)
	}
	b.WriteString(eraseBelow)
	r.k = 0
	r.lastFrame = ""
}

// Logf prints an event line into the scrolling history above the block.
func (r *Renderer) Logf(format string, args ...any) {
	line := stampLine(format, args...)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held {
		r.heldBuf = append(r.heldBuf, line)
		return
	}
	var b bytes.Buffer
	r.eraseBlockLocked(&b)
	b.WriteString(line)
	b.WriteString("\n")
	r.out.Write(b.Bytes())
	// Repaint immediately so the block doesn't vanish until the next tick.
	if r.snap != nil {
		r.repaintLocked(r.snap())
	}
}

// Progressf drops the plain-mode progress ticks: they are redundant on a
// live display. Deliberately lock-free and empty.
func (r *Renderer) Progressf(string, ...any) {}

// Hold hands the terminal to the interactive prompt: the block is erased and
// the cursor and autowrap are restored (a paste needs both).
func (r *Renderer) Hold() {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b bytes.Buffer
	r.eraseBlockLocked(&b)
	b.WriteString(showCursor + wrapOn)
	r.out.Write(b.Bytes())
	r.held = true
}

// Release replays event lines withheld during the prompt and resumes painting.
func (r *Renderer) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.held = false
	var b bytes.Buffer
	if len(r.heldBuf) > 0 {
		fmt.Fprintf(&b, "\n(%d log line(s) arrived while the prompt was up)\n", len(r.heldBuf))
		for _, l := range r.heldBuf {
			b.WriteString(l)
			b.WriteString("\n")
		}
		r.heldBuf = nil
	}
	b.WriteString(hideCursor + wrapOff)
	r.out.Write(b.Bytes())
	// next tick repaints
}

// Close stops the poller, erases the block, and restores the terminal.
// Idempotent: cmdGet closes explicitly before the summary and again via defer.
func (r *Renderer) Close() {
	r.closed.Do(func() {
		r.mu.Lock()
		started := r.started
		r.mu.Unlock()
		if started {
			close(r.stop)
			<-r.done
		}
		r.mu.Lock()
		var b bytes.Buffer
		r.eraseBlockLocked(&b)
		b.WriteString(showCursor + wrapOn)
		r.out.Write(b.Bytes())
		if r.restoreVT != nil {
			r.restoreVT()
		}
		r.mu.Unlock()
	})
}
