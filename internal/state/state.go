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

	// Version is bumped whenever the schema changes incompatibly; Load
	// refuses other versions rather than silently misreading them.
	Version = 2
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
	// Attention: gave up after the per-run retry cap or a non-transient
	// per-part error; a fresh `carryout get` will requeue it.
	Attention PartStatus = "attention"
)

func validStatus(s PartStatus) bool {
	switch s {
	case Pending, Downloaded, Done, Corrupt, Attention:
		return true
	}
	return false
}

type Part struct {
	Num      int    `json:"num"`
	Filename string `json:"filename"`
	// Index is the part's i= query value from the export inventory; -1 when
	// the captured URL had no index parameter.
	Index        int        `json:"index"`
	Status       PartStatus `json:"status"`
	ExpectedSize int64      `json:"expected_size,omitempty"`
	ActualSize   int64      `json:"actual_size,omitempty"`
	// Attempts counts downloads Google actually started serving — the number
	// that plausibly counts against Takeout's per-part download limit. It is
	// seeded at init from the page's "Number of times already downloaded".
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

// New creates a fresh state rooted at dir. Parts must be filled in by the
// caller.
func New(dir string) *State {
	return &State{Version: Version, CreatedAt: time.Now().UTC(), dir: dir}
}

func Path(dir string) string       { return filepath.Join(dir, FileName) }
func backupPath(dir string) string { return Path(dir) + ".bak" }

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
		hint := ""
		if _, berr := os.Stat(backupPath(dir)); berr == nil {
			hint = fmt.Sprintf(" — a previous version exists at %s; inspect it and copy it over %s to recover", backupPath(dir), FileName)
		}
		return nil, fmt.Errorf("parsing %s: %w%s", Path(dir), err, hint)
	}
	if s.Version != Version {
		return nil, fmt.Errorf("%s has state version %d but this carryout speaks version %d — use a matching carryout build, or re-run `carryout init -force`", Path(dir), s.Version, Version)
	}
	for _, p := range s.Parts {
		if !validStatus(p.Status) {
			return nil, fmt.Errorf("%s: part %d has unknown status %q — refusing to guess; fix the file or re-init", Path(dir), p.Num, p.Status)
		}
		if p.Filename != filepath.Base(p.Filename) || strings.ContainsAny(p.Filename, `/\:`) || strings.Contains(p.Filename, "..") {
			return nil, fmt.Errorf("%s: part %d has unsafe filename %q", Path(dir), p.Num, p.Filename)
		}
	}
	s.dir = dir
	return &s, nil
}

// Save writes the state durably and atomically: temp file, fsync, keep the
// previous version as .bak, rename into place. The audit trail is the one
// file whose loss is unrecoverable, so it gets more care than the archives.
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
	final := Path(s.dir)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp)
		return werr
	}
	if _, err := os.Stat(final); err == nil {
		_ = os.Remove(backupPath(s.dir)) // Windows: rename won't clobber reliably
		_ = os.Rename(final, backupPath(s.dir))
	}
	return os.Rename(tmp, final)
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

// RemainingEstimate estimates bytes still to download: known expected sizes
// where recorded, the average completed-part size for the rest. Shared by the
// disk-space warning and the ETA display so the two can't drift.
func (s *State) RemainingEstimate() (remaining int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var doneBytes, doneCount int64
	unknown := 0
	for _, p := range s.Parts {
		switch p.Status {
		case Done, Downloaded:
			doneBytes += p.ActualSize
			doneCount++
		default:
			if p.ExpectedSize > 0 {
				remaining += p.ExpectedSize
			} else {
				unknown++
			}
		}
	}
	if unknown > 0 {
		if doneCount == 0 {
			return 0, false // nothing to extrapolate from
		}
		remaining += int64(unknown) * (doneBytes / doneCount)
	}
	return remaining, true
}

func CookiePath(dir string) string { return filepath.Join(dir, CookieFileName) }

// SaveCookie stores the Cookie header value with owner-only permissions,
// atomically (temp + rename) so an interrupt can't leave it missing or
// truncated.
func SaveCookie(dir, cookie string) error {
	final := CookiePath(dir)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimSpace(cookie)+"\n"), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
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
