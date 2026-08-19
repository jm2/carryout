// SPDX-License-Identifier: GPL-3.0-or-later

package curlcmd

import (
	"strings"
	"testing"
)

const chromeSample = `curl 'https://takeout-download.usercontent.google.com/download/takeout-20260815T123456Z-001.tgz?j=abc123&user=456&i=0' \
  -H 'accept: text/html,application/xhtml+xml' \
  -H 'accept-language: en-US,en;q=0.9' \
  -H 'cookie: SID=aaa; HSID=bbb; SSID=ccc' \
  -H 'user-agent: Mozilla/5.0 (X11; Linux x86_64) Chrome/126.0' \
  --compressed`

func TestParseChromeSample(t *testing.T) {
	c, err := Parse(chromeSample)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://takeout-download.usercontent.google.com/download/takeout-20260815T123456Z-001.tgz?j=abc123&user=456&i=0"; c.URL != want {
		t.Errorf("URL = %q, want %q", c.URL, want)
	}
	if got := c.Headers.Get("Cookie"); got != "SID=aaa; HSID=bbb; SSID=ccc" {
		t.Errorf("Cookie = %q", got)
	}
	if got := c.Headers.Get("User-Agent"); !strings.Contains(got, "Chrome/126.0") {
		t.Errorf("User-Agent = %q", got)
	}
	if got := c.Headers.Get("Accept-Language"); got != "en-US,en;q=0.9" {
		t.Errorf("Accept-Language = %q", got)
	}
}

func TestParseCookieFlagAndHeaderMerge(t *testing.T) {
	c, err := Parse(`curl 'https://example.com/x-001.tgz' -b 'A=1; B=2' -H 'cookie: C=3'`)
	if err != nil {
		t.Fatal(err)
	}
	got := c.Headers.Get("Cookie")
	if !strings.Contains(got, "A=1") || !strings.Contains(got, "C=3") {
		t.Errorf("merged cookie = %q", got)
	}
}

func TestParseAnsiCQuoting(t *testing.T) {
	c, err := Parse(`curl $'https://example.com/x-001.tgz' -H $'cookie: A=a\x21b; B=don\'t'`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Headers.Get("Cookie"); got != "A=a!b; B=don't" {
		t.Errorf("Cookie = %q", got)
	}
}

func TestParseInlineLongFlags(t *testing.T) {
	c, err := Parse(`curl --user-agent="Agent X" --header="X-Test: yes" "https://example.com/x-001.tgz"`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Headers.Get("User-Agent"); got != "Agent X" {
		t.Errorf("User-Agent = %q", got)
	}
	if got := c.Headers.Get("X-Test"); got != "yes" {
		t.Errorf("X-Test = %q", got)
	}
}

func TestParseSkipsFlagArguments(t *testing.T) {
	c, err := Parse(`curl -X GET -o out.tgz 'https://example.com/x-001.tgz'`)
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != "https://example.com/x-001.tgz" {
		t.Errorf("URL = %q (flag argument mistaken for URL?)", c.URL)
	}
}

func TestParseRejectsCmdFormat(t *testing.T) {
	_, err := Parse("curl ^\"https://example.com^\" ^\n -H ^\"cookie: a=b^\"")
	if err == nil || !strings.Contains(err.Error(), "bash") {
		t.Errorf("expected cmd-format rejection, got %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"", "curl -H 'cookie: a=b'", "curl 'unterminated"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", bad)
		}
	}
}

func TestCookieFromPaste(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"SID=a; HSID=b", "SID=a; HSID=b"},
		{"Cookie: SID=a; HSID=b", "SID=a; HSID=b"},
		{"cookie: SID=a", "SID=a"},
		{chromeSample, "SID=aaa; HSID=bbb; SSID=ccc"},
	}
	for _, c := range cases {
		got, err := CookieFromPaste(c.in)
		if err != nil {
			t.Errorf("CookieFromPaste(%.30q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("CookieFromPaste(%.30q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := CookieFromPaste("not a cookie at all"); err == nil {
		t.Error("expected error for junk input")
	}
}

func TestCookieFromPasteCollapsesHardWraps(t *testing.T) {
	// a cookie hard-wrapped across lines by an editor must not keep the
	// newline (it would make every request an invalid-header failure)
	got, err := CookieFromPaste("SID=aaa;\nHSID=bbb;\n\tSSID=ccc")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("control chars survived: %q", got)
	}
	if got != "SID=aaa; HSID=bbb; SSID=ccc" {
		t.Errorf("got %q", got)
	}
}

func TestCookieFromPasteRejectsPowerShell(t *testing.T) {
	ps := `Invoke-WebRequest -Uri "https://example.com/x-001.tgz" -Headers @{"cookie"="SID=a"}`
	if _, err := CookieFromPaste(ps); err == nil || !strings.Contains(err.Error(), "PowerShell") {
		t.Errorf("err = %v, want PowerShell rejection", err)
	}
}

func TestParseRejectsPowerShell(t *testing.T) {
	_, err := Parse(`Invoke-WebRequest -Uri "https://example.com/x-001.tgz"`)
	if err == nil || !strings.Contains(err.Error(), "http(s) URL") {
		t.Errorf("err = %v, want no-URL rejection", err)
	}
}

func TestSanitizeCookieValidatesShape(t *testing.T) {
	if _, err := SanitizeCookie("SID=a; ; HSID=b"); err != nil {
		t.Errorf("empty chunk should be tolerated: %v", err)
	}
	if _, err := SanitizeCookie("SID=a; brokenfragment"); err == nil {
		t.Error("fragment without '=' accepted")
	}
}
