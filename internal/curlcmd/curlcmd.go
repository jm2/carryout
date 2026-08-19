// SPDX-License-Identifier: GPL-3.0-or-later

// Package curlcmd parses a browser "Copy as cURL" command into the URL and
// headers needed to replay that exact request.
package curlcmd

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Capture is the result of parsing a pasted cURL command.
type Capture struct {
	URL     string
	Headers http.Header // includes Cookie, merged from -H and -b/--cookie
}

// flags that consume a following argument but carry nothing we need.
var skipArgFlags = map[string]bool{
	"-X": true, "--request": true,
	"-d": true, "--data": true, "--data-raw": true, "--data-binary": true, "--data-urlencode": true,
	"-o": true, "--output": true,
	"-u": true, "--user": true,
	"-m": true, "--max-time": true,
	"--connect-timeout": true, "--retry": true,
	"-C": true, "--continue-at": true,
	"-x": true, "--proxy": true,
	"-T": true, "--upload-file": true,
	"--range": true, "-r": true,
}

// Parse parses a bash-style cURL command (as produced by Chrome/Firefox
// "Copy as cURL" or the CurlWget extension) into its URL and headers.
func Parse(s string) (*Capture, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty input")
	}
	if looksLikeCmdFormat(s) {
		return nil, errors.New(`this looks like Windows "Copy as cURL (cmd)" output — copy the bash variant instead`)
	}

	toks, err := tokenize(s)
	if err != nil {
		return nil, err
	}

	cap := &Capture{Headers: http.Header{}}
	var cookieParts []string

	i := 0
	if len(toks) > 0 && isCurlWord(toks[0]) {
		i = 1
	}

	for ; i < len(toks); i++ {
		t := toks[i]

		flagName, inlineVal, hasInline := t, "", false
		if strings.HasPrefix(t, "--") {
			if eq := strings.Index(t, "="); eq >= 0 {
				flagName, inlineVal, hasInline = t[:eq], t[eq+1:], true
			}
		}
		nextVal := func() (string, error) {
			if hasInline {
				return inlineVal, nil
			}
			i++
			if i >= len(toks) {
				return "", fmt.Errorf("flag %s is missing its value", flagName)
			}
			return toks[i], nil
		}

		switch flagName {
		case "-H", "--header":
			v, err := nextVal()
			if err != nil {
				return nil, err
			}
			name, val, ok := strings.Cut(v, ":")
			if !ok {
				continue
			}
			name, val = strings.TrimSpace(name), strings.TrimSpace(val)
			if strings.EqualFold(name, "cookie") {
				if val != "" {
					cookieParts = append(cookieParts, val)
				}
			} else if name != "" {
				cap.Headers.Add(name, val)
			}
		case "-b", "--cookie":
			v, err := nextVal()
			if err != nil {
				return nil, err
			}
			// -b with no '=' means a cookie *file* path; we only take literal cookies
			if strings.Contains(v, "=") {
				cookieParts = append(cookieParts, v)
			}
		case "-A", "--user-agent":
			v, err := nextVal()
			if err != nil {
				return nil, err
			}
			cap.Headers.Set("User-Agent", v)
		case "-e", "--referer":
			v, err := nextVal()
			if err != nil {
				return nil, err
			}
			cap.Headers.Set("Referer", v)
		default:
			if strings.HasPrefix(flagName, "-") && flagName != "-" {
				if skipArgFlags[flagName] {
					if _, err := nextVal(); err != nil {
						return nil, err
					}
				}
				continue // unknown or boolean flag: ignore
			}
			if cap.URL == "" {
				cap.URL = t
			}
		}
	}

	if len(cookieParts) > 0 {
		ck, err := SanitizeCookie(strings.Join(cookieParts, "; "))
		if err != nil {
			return nil, fmt.Errorf("cookie in capture: %w", err)
		}
		cap.Headers.Set("Cookie", ck)
	}
	if !strings.HasPrefix(cap.URL, "http://") && !strings.HasPrefix(cap.URL, "https://") {
		return nil, errors.New(`no http(s) URL found — paste the bash "Copy as cURL" output (not the cmd or PowerShell variant)`)
	}
	return cap, nil
}

func isCurlWord(tok string) bool {
	return tok == "curl" || tok == "curl.exe" || strings.HasSuffix(tok, "/curl")
}

// SanitizeCookie normalizes a Cookie header value from a paste: hard-wrap
// newlines and stray control characters are collapsed (an embedded newline
// would make every subsequent request fail with an invalid header), and the
// shape is validated so garbage can't be silently saved as the session.
func SanitizeCookie(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || !strings.Contains(s, "=") {
		return "", errors.New("that doesn't look like a cookie string")
	}
	for _, chunk := range strings.Split(s, ";") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		if !strings.Contains(chunk, "=") {
			if len(chunk) > 40 {
				chunk = chunk[:40] + "…"
			}
			return "", fmt.Errorf("cookie fragment %q has no '=' — paste the full Cookie header value", chunk)
		}
	}
	return s, nil
}

// CookieFromPaste extracts a Cookie header value from user input that may be
// a full cURL command, a "Cookie: ..." header line, or a raw cookie string.
func CookieFromPaste(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty paste")
	}
	if low := strings.ToLower(s); strings.Contains(low, "invoke-webrequest") || strings.Contains(low, "invoke-restmethod") || strings.Contains(s, "@{") {
		return "", errors.New(`this looks like PowerShell output — use "Copy as cURL (bash)", or paste just the cookie string`)
	}
	if isCurlWord(strings.Fields(s)[0]) {
		c, err := Parse(s)
		if err != nil {
			return "", err
		}
		ck := c.Headers.Get("Cookie")
		if ck == "" {
			return "", errors.New("that capture has no Cookie header — copy the request that actually carries your session cookies")
		}
		return ck, nil // already sanitized by Parse
	}
	if len(s) >= 7 && strings.EqualFold(s[:7], "cookie:") {
		s = strings.TrimSpace(s[7:])
	}
	return SanitizeCookie(s)
}

func looksLikeCmdFormat(s string) bool {
	return strings.Contains(s, "^\r\n") || strings.Contains(s, "^\n") ||
		strings.Contains(s, ` ^"`) || strings.HasSuffix(s, "^")
}

// tokenize splits a bash command line into words, honoring backslash escapes,
// single quotes, double quotes, and ANSI-C $'...' quoting.
func tokenize(s string) ([]string, error) {
	var toks []string
	var cur strings.Builder
	has := false

	flush := func() {
		if has {
			toks = append(toks, cur.String())
			cur.Reset()
			has = false
		}
	}

	rs := []rune(s)
	i := 0
	for i < len(rs) {
		c := rs[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
			i++
		case c == '\\':
			if i+1 >= len(rs) {
				i++
				continue
			}
			n := rs[i+1]
			if n == '\n' { // line continuation
				i += 2
			} else if n == '\r' && i+2 < len(rs) && rs[i+2] == '\n' {
				i += 3
			} else {
				cur.WriteRune(n)
				has = true
				i += 2
			}
		case c == '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				cur.WriteRune(rs[j])
				j++
			}
			if j >= len(rs) {
				return nil, errors.New("unterminated single quote")
			}
			has = true
			i = j + 1
		case c == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' && j+1 < len(rs) {
					switch n := rs[j+1]; n {
					case '"', '\\', '$', '`':
						cur.WriteRune(n)
					case '\n': // continuation inside quotes
					default:
						cur.WriteRune('\\')
						cur.WriteRune(n)
					}
					j += 2
					continue
				}
				cur.WriteRune(rs[j])
				j++
			}
			if j >= len(rs) {
				return nil, errors.New("unterminated double quote")
			}
			has = true
			i = j + 1
		case c == '$' && i+1 < len(rs) && rs[i+1] == '\'':
			j := i + 2
			for j < len(rs) && rs[j] != '\'' {
				if rs[j] == '\\' && j+1 < len(rs) {
					adv, err := ansiEscape(rs[j:], &cur)
					if err != nil {
						return nil, err
					}
					j += adv
					continue
				}
				cur.WriteRune(rs[j])
				j++
			}
			if j >= len(rs) {
				return nil, errors.New("unterminated $'...' quote")
			}
			has = true
			i = j + 1
		default:
			cur.WriteRune(c)
			has = true
			i++
		}
	}
	flush()
	return toks, nil
}

// ansiEscape decodes one backslash escape inside $'...' quoting, writing the
// result to out and returning the number of runes consumed.
func ansiEscape(rs []rune, out *strings.Builder) (int, error) {
	if len(rs) < 2 {
		return 1, nil
	}
	switch rs[1] {
	case 'n':
		out.WriteByte('\n')
	case 't':
		out.WriteByte('\t')
	case 'r':
		out.WriteByte('\r')
	case 'a':
		out.WriteByte('\a')
	case 'b':
		out.WriteByte('\b')
	case 'f':
		out.WriteByte('\f')
	case 'v':
		out.WriteByte('\v')
	case '\'', '"', '\\':
		out.WriteRune(rs[1])
	case 'x':
		if len(rs) < 4 {
			return 2, errors.New(`truncated \x escape in $'...' quote`)
		}
		hi, ok1 := hexVal(rs[2])
		lo, ok2 := hexVal(rs[3])
		if !ok1 || !ok2 {
			return 2, errors.New(`bad \x escape in $'...' quote`)
		}
		out.WriteByte(byte(hi<<4 | lo))
		return 4, nil
	case 'u':
		if len(rs) < 6 {
			return 2, errors.New(`truncated \u escape in $'...' quote`)
		}
		v := 0
		for k := 2; k < 6; k++ {
			d, ok := hexVal(rs[k])
			if !ok {
				return 2, errors.New(`bad \u escape in $'...' quote`)
			}
			v = v<<4 | d
		}
		out.WriteRune(rune(v))
		return 6, nil
	default:
		out.WriteRune('\\')
		out.WriteRune(rs[1])
	}
	return 2, nil
}

func hexVal(r rune) (int, bool) {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0'), true
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10, true
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10, true
	}
	return 0, false
}
