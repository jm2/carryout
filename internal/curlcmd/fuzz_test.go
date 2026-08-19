// SPDX-License-Identifier: GPL-3.0-or-later

package curlcmd

import (
	"strings"
	"testing"
)

// FuzzParse hardens the capture parser against arbitrary pasted text. The
// invariants: never panic, never accept a non-http(s) URL, and never emit a
// Cookie header containing control characters (which would poison every
// subsequent request).
func FuzzParse(f *testing.F) {
	f.Add(chromeSample)
	f.Add(`curl 'https://h/x-001.tgz' -b 'a=b' --compressed`)
	f.Add("curl $'https://h/x-001.tgz' -H $'cookie: a=\\x41\\u0042'")
	f.Add(`curl --user-agent="A" --header="X: y" "https://h/x-001.tgz"`)
	f.Add("curl 'unterminated")
	f.Fuzz(func(t *testing.T, s string) {
		c, err := Parse(s)
		if err != nil {
			return
		}
		if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
			t.Fatalf("accepted non-http URL %q", c.URL)
		}
		if ck := c.Headers.Get("Cookie"); strings.ContainsAny(ck, "\r\n") {
			t.Fatalf("cookie with control characters survived: %q", ck)
		}
	})
}

// FuzzCookieFromPaste guards the auth-refresh path: whatever the user pastes,
// an accepted cookie must be a sane single-line header value.
func FuzzCookieFromPaste(f *testing.F) {
	f.Add("SID=a; HSID=b")
	f.Add("Cookie: SID=a;\nHSID=b")
	f.Add(chromeSample)
	f.Add("not a cookie at all")
	f.Fuzz(func(t *testing.T, s string) {
		ck, err := CookieFromPaste(s)
		if err != nil {
			return
		}
		if strings.ContainsAny(ck, "\r\n\t") {
			t.Fatalf("accepted cookie with control characters: %q", ck)
		}
		if !strings.Contains(ck, "=") {
			t.Fatalf("accepted cookie without any pair: %q", ck)
		}
	})
}
