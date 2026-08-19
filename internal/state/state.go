// SPDX-License-Identifier: GPL-3.0-or-later

// Package state persists carryout's per-export progress: which parts are
// downloaded, verified, or need attention, plus the audit trail of expected
// vs. actual sizes and attempt counts. Session cookies live in a separate
// 0600 file so the state file itself contains no secrets.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	FileName       = "carryout.json"
	CookieFileName = "cookies.txt"
)

type PartStatus string

const (
	// Pending: not downloaded yet.
	Pending PartStatus = "pending"
	// Downloaded: bytes on disk and quick checks passed; full verification pending.
	Downloaded PartStatus = "downloaded"
	// Done: downloaded and verified (fully, or quick-only if verify_mode is quick).
	Done PartStatus = "done"
	// Corrupt: on disk but failed integrity verification. Re-downloading costs
	// another attempt, so it takes an explicit -redo to requeue.
	Corrupt PartStatus = "corrupt"
	// Attention: gave up after the per-run retry cap; a fresh `carryout get`
	// will requeue it.
	Attention PartStatus = "attention"
)

type Part struct {
	Num          int        `json:"num"`
	Filename     string     `json:"filename"`
	Status       PartStatus `json:"status"`
	ExpectedSize int64      `json:"expected_size,omitempty"`
	ActualSize   int64      `json:"actual_size,omitempty"`
	// Attempts counts downloads Google actually started serving — the number
	// that plausibly counts against Takeout's per-part download limit.
	Attempts    int       `json:"attempts,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
	VerifiedAt  time.Time `json:"verified_at,omitzero"`
}

type State struct {
	Version     int               `json:"version"`
	CreatedAt   time.Time         `json:"created_at"`
	CapturedURL string            `json:"captured_url"`
	JobID       string            `json:"job_id,omitempty"`
	TotalParts  int               `json:"total_parts"`
	Headers     map[string]string `json:"headers"`
	VerifyMode  string            `json:"verify_mode"`
	Parts       []*Part           `json:"parts"`

	mu  sync.Mutex
	dir string
}

// New creates a fresh state rooted at dir. Filenames must be filled in by the
// caller.
func New(dir string) *State {
	return &State{Version: 1, CreatedAt: time.Now().UTC(), dir: dir}
}

func Path(dir string) string { return filepath.Join(dir, FileName) }

func Exists(dir string) bool {
	_, err := os.Stat(Path(dir))
	return err == nil
}

func Load(dir string) (*State, error) {
	b, err := os.ReadFile(Path(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no %s in %s — run `carryout init` there first", FileName, dir)
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", Path(dir), err)
	}
	s.dir = dir
	return &s, nil
}

// Save writes the state atomically (temp file + rename).
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *State) saveLocked() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(s.dir) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, Path(s.dir))
}

// Update applies fn under the state lock and saves the result.
func (s *State) Update(fn func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
	return s.saveLocked()
}

// View runs fn under the state lock without saving.
func (s *State) View(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

func (s *State) Part(num int) *Part {
	if num < 1 || num > len(s.Parts) {
		return nil
	}
	return s.Parts[num-1]
}

// Counts summarizes part statuses; doneBytes sums ActualSize of completed parts.
func (s *State) Counts() (pending, downloaded, done, attention, corrupt int, doneBytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.Parts {
		switch p.Status {
		case Pending:
			pending++
		case Downloaded:
			downloaded++
			doneBytes += p.ActualSize
		case Done:
			done++
			doneBytes += p.ActualSize
		case Attention:
			attention++
		case Corrupt:
			corrupt++
		}
	}
	return
}

func CookiePath(dir string) string { return filepath.Join(dir, CookieFileName) }

// SaveCookie stores the Cookie header value with owner-only permissions.
func SaveCookie(dir, cookie string) error {
	p := CookiePath(dir)
	// O_TRUNC via WriteFile keeps existing perms, so remove first to be sure
	// a previously loose file doesn't survive.
	_ = os.Remove(p)
	return os.WriteFile(p, []byte(strings.TrimSpace(cookie)+"\n"), 0600)
}

func LoadCookie(dir string) (string, error) {
	b, err := os.ReadFile(CookiePath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no %s in %s — run `carryout init` or `carryout auth` first", CookieFileName, dir)
		}
		return "", err
	}
	c := strings.TrimSpace(string(b))
	if c == "" {
		return "", fmt.Errorf("%s is empty — run `carryout auth`", CookiePath(dir))
	}
	return c, nil
}
