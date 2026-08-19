// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"strconv"
	"strings"
)

// vtScreen is a minimal test-only VT interpreter: it plays back the escape
// sequences the renderer emits and produces the final screen content, so
// tests assert "what's on screen" instead of brittle escape-sequence goldens.
// Supported: \r, \n, CSI n A, CSI K, CSI 2K, CSI J, CSI ?25h/l, CSI ?7h/l.
type vtScreen struct {
	lines        [][]rune
	row, col     int
	cursorHidden bool
	wrapDisabled bool
}

func playVT(input string) *vtScreen {
	s := &vtScreen{lines: [][]rune{{}}}
	rs := []rune(input)
	i := 0
	for i < len(rs) {
		c := rs[i]
		switch {
		case c == '\r':
			s.col = 0
			i++
		case c == '\n':
			s.row++
			s.col = 0
			for len(s.lines) <= s.row {
				s.lines = append(s.lines, []rune{})
			}
			i++
		case c == 0x1b && i+1 < len(rs) && rs[i+1] == '[':
			j := i + 2
			for j < len(rs) && !(rs[j] >= '@' && rs[j] <= '~') {
				j++
			}
			if j >= len(rs) {
				return s
			}
			s.csi(string(rs[i+2:j]), rs[j])
			i = j + 1
		default:
			s.put(c)
			i++
		}
	}
	return s
}

func (s *vtScreen) csi(params string, final rune) {
	switch final {
	case 'A':
		n := 1
		if params != "" {
			n, _ = strconv.Atoi(params)
		}
		s.row -= n
		if s.row < 0 {
			s.row = 0
		}
	case 'K':
		switch params {
		case "", "0":
			if s.col < len(s.lines[s.row]) {
				s.lines[s.row] = s.lines[s.row][:s.col]
			}
		case "2":
			s.lines[s.row] = []rune{}
		}
	case 'J':
		if params == "" || params == "0" {
			if s.col < len(s.lines[s.row]) {
				s.lines[s.row] = s.lines[s.row][:s.col]
			}
			s.lines = s.lines[:s.row+1]
		}
	case 'h', 'l':
		switch params {
		case "?25":
			s.cursorHidden = final == 'l'
		case "?7":
			s.wrapDisabled = final == 'l'
		}
	}
}

func (s *vtScreen) put(c rune) {
	line := s.lines[s.row]
	for len(line) < s.col {
		line = append(line, ' ')
	}
	if s.col < len(line) {
		line[s.col] = c
	} else {
		line = append(line, c)
	}
	s.lines[s.row] = line
	s.col++
}

// rendered returns the screen lines with trailing blank lines trimmed.
func (s *vtScreen) rendered() []string {
	out := make([]string, len(s.lines))
	for i, l := range s.lines {
		out[i] = string(l)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}
