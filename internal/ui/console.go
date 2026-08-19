// SPDX-License-Identifier: GPL-3.0-or-later

// Package ui owns carryout's terminal output: a Plain timestamped logger
// (non-TTY runs, -plain, dry runs) and a live ANSI Renderer (per-download
// progress bars with a pinned aggregate footer).
//
// Design notes, recorded so they aren't relitigated:
//   - The terminal is never switched to raw mode: the interactive cookie
//     prompt reads a multi-line paste from stdin in canonical mode, and that
//     battle-tested flow must keep working untouched. The live renderer only
//     writes; Hold/Release hand the screen to the prompt.
//   - DECSTBM scroll regions were rejected (stateful, fragile across resizes
//     and tmux). SIGWINCH was rejected in favor of re-querying the terminal
//     width on every repaint — portable and race-free at ~3 Hz.
//   - os.Stdout is never wrapped in a buffered writer: the prompt prints to
//     it directly and must interleave correctly. Every renderer operation
//     composes one byte slice and issues exactly one Write instead.
package ui

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Console is what the CLI and the fetcher log through. Hold/Release bracket
// the interactive cookie prompt: implementations must silence background
// output while held and replay withheld event lines afterwards.
type Console interface {
	Logf(format string, args ...any)      // event lines (kept)
	Progressf(format string, args ...any) // ephemeral progress (droppable)
	Hold()
	Release()
	Close()
}

func stampLine(format string, args ...any) string {
	return time.Now().Format("15:04:05") + "  " + fmt.Sprintf(format, args...)
}

// Plain is the timestamped line logger: today's output, byte for byte. While
// held (cookie prompt on screen) event lines are buffered and replayed after
// the paste; progress ticks are dropped — they're stale within seconds.
type Plain struct {
	mu   sync.Mutex
	out  io.Writer
	held bool
	buf  []string
}

func NewPlain(out io.Writer) *Plain { return &Plain{out: out} }

func (g *Plain) Logf(format string, args ...any) {
	line := stampLine(format, args...)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held {
		g.buf = append(g.buf, line)
		return
	}
	fmt.Fprintln(g.out, line)
}

func (g *Plain) Progressf(format string, args ...any) {
	line := stampLine(format, args...)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held {
		return
	}
	fmt.Fprintln(g.out, line)
}

func (g *Plain) Hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = true
}

func (g *Plain) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = false
	if len(g.buf) == 0 {
		return
	}
	fmt.Fprintf(g.out, "\n(%d log line(s) arrived while the prompt was up)\n", len(g.buf))
	for _, l := range g.buf {
		fmt.Fprintln(g.out, l)
	}
	g.buf = nil
}

func (g *Plain) Close() {}
