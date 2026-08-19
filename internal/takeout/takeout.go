// SPDX-License-Identifier: GPL-3.0-or-later

// Package takeout derives every per-part download URL from a single captured
// Takeout download URL. It makes no assumptions about Google's numbering
// scheme beyond what the capture itself shows: the offset between the
// filename part number and the i= query index is measured from the capture,
// and only those two values are substituted when building other part URLs.
// Everything else (host, signed query parameters, ordering, encoding) is
// preserved byte for byte.
package takeout

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
)

// partNameRe matches an archive filename ending in a part number, e.g.
// takeout-20260815T123456Z-001.tgz or takeout-20260815T123456Z-3-042.zip.
var partNameRe = regexp.MustCompile(`^(.*-)(\d+)(\.[A-Za-z0-9.]+)$`)

var indexParamRe = regexp.MustCompile(`(^|&)i=\d+`)

// ErrRedirectURL is returned when the capture is of the takeout.google.com
// redirect rather than the resolved usercontent download URL.
var ErrRedirectURL = errors.New(`this is the takeout.google.com redirect URL — session cookies are bound to the final download host, so captures of it don't replay.
Capture the request to takeout-download…usercontent.google.com instead: in DevTools > Network, follow the redirect chain to the request whose path ends in the archive filename (e.g. …-001.tgz) and use "Copy as cURL" on that one`)

// Template builds per-part URLs from one captured download URL.
type Template struct {
	CapturedURL string
	JobID       string // j= query parameter, informational
	NumWidth    int    // zero-padding width of the part number in the filename
	CapturedNum int    // part number seen in the captured filename
	HasIndex    bool   // captured URL has an i= query parameter
	Offset      int    // filename part number minus the i= index

	parsed     *url.URL
	pathPrefix string // URL path up to and including the '-' before the number
	pathSuffix string // extension, including the leading dot
}

// Derive builds a Template from a captured download URL.
func Derive(raw string) (*Template, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("unexpected URL scheme %q", u.Scheme)
	}

	base := path.Base(u.Path)
	m := partNameRe.FindStringSubmatch(base)
	if m == nil {
		if u.Host == "takeout.google.com" {
			return nil, ErrRedirectURL
		}
		return nil, fmt.Errorf("URL path %q doesn't end in a numbered archive filename like takeout-…-001.tgz", u.Path)
	}

	num, err := strconv.Atoi(m[2])
	if err != nil {
		return nil, fmt.Errorf("part number %q: %w", m[2], err)
	}

	t := &Template{
		CapturedURL: raw,
		NumWidth:    len(m[2]),
		CapturedNum: num,
		parsed:      u,
		pathPrefix:  u.Path[:len(u.Path)-len(base)] + m[1],
		pathSuffix:  m[3],
	}

	q := u.Query()
	t.JobID = q.Get("j")
	if iv := q.Get("i"); iv != "" {
		idx, err := strconv.Atoi(iv)
		if err == nil {
			t.HasIndex = true
			t.Offset = num - idx
		}
	}
	return t, nil
}

// Filename returns the archive filename for a given part number (1-based,
// matching the numbers on the Takeout page).
func (t *Template) Filename(partNum int) string {
	base := path.Base(t.pathPrefix)
	return fmt.Sprintf("%s%0*d%s", base, t.NumWidth, partNum, t.pathSuffix)
}

// PartURL returns the download URL for a given part number. Only the filename
// number and the i= index are substituted; the rest of the captured URL is
// left untouched.
func (t *Template) PartURL(partNum int) string {
	u := *t.parsed
	u.Path = fmt.Sprintf("%s%0*d%s", t.pathPrefix, t.NumWidth, partNum, t.pathSuffix)
	u.RawPath = ""
	if t.HasIndex {
		u.RawQuery = indexParamRe.ReplaceAllString(u.RawQuery, "${1}i="+strconv.Itoa(partNum-t.Offset))
	}
	return u.String()
}
