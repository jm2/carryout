// SPDX-License-Identifier: GPL-3.0-or-later

// Package takeout turns one captured Takeout download URL plus the export's
// file inventory into per-part download URLs.
//
// Real exports are not a single numbered sequence: a large export ships as
// several groups (takeout-<stamp>-<G>-<NNN>.tgz) whose NNN restarts at 001
// within each group, while the i= query index runs across the whole export.
// So the mapping filename↔index is data, not arithmetic: the inventory
// (pasted from the Takeout page) supplies the ordered filenames, the capture
// supplies the URL shape and one known filename↔index pair to calibrate
// against, and BuildURL only splices those two values into the captured URL —
// everything else (host, signed query parameters, ordering) is preserved.
package takeout

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// ErrRedirectURL is returned when the capture is of the takeout.google.com
// redirect rather than the resolved usercontent download URL.
var ErrRedirectURL = errors.New(`this is the takeout.google.com redirect URL — session cookies are bound to the final download host, so captures of it don't replay.
Capture the request to takeout-download…usercontent.google.com instead: in DevTools > Network, follow the redirect chain to the request whose path ends in the archive filename (e.g. …-001.tgz) and use "Copy as cURL" on that one`)

var indexParamRe = regexp.MustCompile(`(^|&)i=\d+`)

// archiveNameRe matches a Takeout archive filename. The character set is
// deliberately narrow ([A-Za-z0-9._-]) so an inventory or capture can never
// smuggle a path separator, drive colon, or ".." into an on-disk filename.
var archiveNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*\.(?:tgz|tar\.gz|zip|tbz|mbox|gz)$`)

// inventoryNameRe finds Takeout filenames inside arbitrary pasted text
// (export summaries, saved page HTML, plain lists).
var inventoryNameRe = regexp.MustCompile(`takeout-[A-Za-z0-9]+[A-Za-z0-9_-]*\.(?:tgz|tar\.gz|zip|tbz|mbox)`)

// downloadedCountRe captures Takeout's per-file counter when the paste is the
// page's export summary ("... (Number of times already downloaded: 3)").
var downloadedCountRe = regexp.MustCompile(`^[^A-Za-z0-9]{0,10}\(?Number of times already downloaded:\s*(\d+)`)

// trailingNumRe splits a single-sequence filename for legacy -parts synthesis.
var trailingNumRe = regexp.MustCompile(`^(.*-)(\d+)(\.[A-Za-z0-9.]+)$`)

// multiGroupRe recognizes the grouped naming (…-<G>-<NNN>.ext) for which
// numeric synthesis would generate URLs that don't exist.
var multiGroupRe = regexp.MustCompile(`-\d+-\d+\.[A-Za-z0-9.]+$`)

// ValidFilename reports whether name is a plausible Takeout archive filename
// that is safe to use as an on-disk name.
func ValidFilename(name string) bool {
	return archiveNameRe.MatchString(name)
}

// Template carries the URL shape of one captured part download.
type Template struct {
	CapturedURL   string
	JobID         string // j= query parameter, informational
	CapturedName  string // basename of the captured path
	CapturedIndex int    // i= query value in the capture; -1 if absent
	HasIndex      bool

	parsed  *url.URL
	pathDir string // path up to and including the final '/'
}

// Derive builds a Template from a captured download URL.
func Derive(raw string) (*Template, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("unexpected URL scheme %q — paste the bash \"Copy as cURL\" output", u.Scheme)
	}

	base := path.Base(u.Path)
	if u.Host == "takeout.google.com" {
		return nil, ErrRedirectURL
	}
	if !ValidFilename(base) {
		return nil, fmt.Errorf("URL path %q doesn't end in a Takeout archive filename like takeout-…-001.tgz", u.Path)
	}

	t := &Template{
		CapturedURL:   raw,
		CapturedName:  base,
		CapturedIndex: -1,
		parsed:        u,
		pathDir:       u.Path[:len(u.Path)-len(base)],
	}
	q := u.Query()
	t.JobID = q.Get("j")
	if iv := q.Get("i"); iv != "" {
		if idx, err := strconv.Atoi(iv); err == nil {
			t.HasIndex = true
			t.CapturedIndex = idx
		}
	}
	return t, nil
}

// BuildURL returns the download URL for one inventory entry. Only the
// filename and the i= value are substituted; the captured URL's query is
// otherwise left byte-for-byte intact. index < 0 leaves i= untouched.
func (t *Template) BuildURL(filename string, index int) string {
	u := *t.parsed
	u.Path = t.pathDir + filename
	u.RawPath = ""
	if t.HasIndex && index >= 0 {
		u.RawQuery = indexParamRe.ReplaceAllString(u.RawQuery, "${1}i="+strconv.Itoa(index))
	}
	return u.String()
}

// SelfCheck verifies that rebuilding the captured part reproduces the
// captured URL exactly. A mismatch means the URL contains encoding this
// package would alter — refuse early rather than send Google URLs it never
// signed.
func (t *Template) SelfCheck() error {
	rebuilt := t.BuildURL(t.CapturedName, t.CapturedIndex)
	if rebuilt != t.CapturedURL {
		return fmt.Errorf("rebuilding the captured URL alters it (got %q) — the capture contains encoding carryout can't preserve; please report this with the URL shape", rebuilt)
	}
	return nil
}

// Entry is one file of the export inventory.
type Entry struct {
	Filename   string
	Index      int // i= value; -1 when unknown
	Downloaded int // Google's "Number of times already downloaded"; -1 when unknown
}

// ParseInventory extracts the ordered file list from text pasted off the
// Takeout page (the export summary, a saved page, or a plain list of names).
// Duplicate mentions keep the first occurrence's position; a per-file
// download counter following the name is captured when present.
func ParseInventory(text string) ([]Entry, error) {
	locs := inventoryNameRe.FindAllStringIndex(text, -1)
	var entries []Entry
	seen := make(map[string]int)
	for _, loc := range locs {
		name := text[loc[0]:loc[1]]
		if !ValidFilename(name) {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		e := Entry{Filename: name, Index: -1, Downloaded: -1}
		tail := text[loc[1]:min(loc[1]+80, len(text))]
		if m := downloadedCountRe.FindStringSubmatch(tail); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				e.Downloaded = n
			}
		}
		seen[name] = len(entries)
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, errors.New("no Takeout filenames found in the pasted text — paste the export summary from the Takeout page (the list of takeout-…tgz files)")
	}
	return entries, nil
}

// Calibrate assigns i= indexes to the inventory by position, anchored on the
// capture's known filename↔index pair. It requires the captured file to be in
// the inventory and the resulting indexes to be non-negative.
func Calibrate(entries []Entry, t *Template) error {
	pos := -1
	for i, e := range entries {
		if e.Filename == t.CapturedName {
			pos = i
			break
		}
	}
	if pos < 0 {
		return fmt.Errorf("the captured file %s is not in the pasted file list — capture one of the listed parts, or re-paste the full summary", t.CapturedName)
	}
	if !t.HasIndex {
		for i := range entries {
			entries[i].Index = -1
		}
		return nil
	}
	base := t.CapturedIndex - pos
	if base+0 < 0 { // first entry would get a negative index
		return fmt.Errorf("capture (i=%d for list position %d) implies negative indexes — the list order doesn't match the export's index order; capture the FIRST file in the list and try again", t.CapturedIndex, pos+1)
	}
	for i := range entries {
		entries[i].Index = base + i
	}
	return nil
}

// Synthesize builds an inventory for a simple single-sequence export from the
// captured filename and a total count (legacy -parts mode). It refuses
// grouped naming, where synthesis would produce files that don't exist.
func Synthesize(t *Template, total int) ([]Entry, error) {
	if multiGroupRe.MatchString(t.CapturedName) {
		return nil, fmt.Errorf("%s looks like a multi-group export (…-<group>-<number>.tgz) — a plain part count can't describe it; paste the file list from the Takeout page instead", t.CapturedName)
	}
	m := trailingNumRe.FindStringSubmatch(t.CapturedName)
	if m == nil {
		return nil, fmt.Errorf("can't derive a numbering pattern from %q — paste the file list from the Takeout page instead", t.CapturedName)
	}
	num, err := strconv.Atoi(m[2])
	if err != nil || num < 1 || num > total {
		return nil, fmt.Errorf("captured part number %q is outside 1-%d — check the part count", m[2], total)
	}
	width := len(m[2])
	base := -1
	if t.HasIndex {
		base = t.CapturedIndex - (num - 1)
		if base < 0 {
			return nil, fmt.Errorf("capture (i=%d for part %d) implies negative indexes — paste the file list from the Takeout page instead", t.CapturedIndex, num)
		}
	}
	entries := make([]Entry, total)
	for i := range entries {
		idx := -1
		if t.HasIndex {
			idx = base + i
		}
		entries[i] = Entry{
			Filename:   fmt.Sprintf("%s%0*d%s", m[1], width, i+1, m[3]),
			Index:      idx,
			Downloaded: -1,
		}
	}
	return entries, nil
}

// GroupSummaryIsGrouped reports whether the inventory uses grouped naming.
func GroupSummaryIsGrouped(entries []Entry) bool {
	for _, e := range entries {
		if multiGroupRe.MatchString(e.Filename) {
			return true
		}
	}
	return false
}

// GroupSummary renders a compact description of the inventory's shape, e.g.
// "8 groups: -1- ×19, -2- ×18, …" for grouped exports, for the user to eyeball
// against the Takeout page.
func GroupSummary(entries []Entry) string {
	groups := make(map[string]int)
	var order []string
	for _, e := range entries {
		key := "ungrouped"
		if m := regexp.MustCompile(`-(\d+)-\d+\.[A-Za-z0-9.]+$`).FindStringSubmatch(e.Filename); m != nil {
			key = m[1]
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key]++
	}
	if len(order) == 1 && order[0] == "ungrouped" {
		return fmt.Sprintf("%d files, single sequence", len(entries))
	}
	var parts []string
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("group %s ×%d", k, groups[k]))
	}
	return fmt.Sprintf("%d files in %d groups: %s", len(entries), len(order), strings.Join(parts, ", "))
}
