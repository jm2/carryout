// SPDX-License-Identifier: GPL-3.0-or-later

// Package sniff classifies HTTP response bodies (archive vs. HTML error page)
// and fully verifies downloaded archives — the equivalent of gzip -t.
package sniff

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"
)

// IsHTML reports whether a response looks like an HTML page rather than an
// archive, from its Content-Type and leading bytes.
func IsHTML(contentType string, head []byte) bool {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml") {
		return true
	}
	h := bytes.TrimLeft(head, " \t\r\n")
	h = bytes.TrimPrefix(h, []byte{0xef, 0xbb, 0xbf}) // UTF-8 BOM
	return len(h) > 0 && h[0] == '<'
}

// LooksLikeSignIn reports whether HTML content appears to be a Google sign-in
// or session-expired page (as opposed to some other error page).
func LooksLikeSignIn(body []byte) bool {
	b := bytes.ToLower(body)
	for _, marker := range []string{
		"accounts.google.com",
		"servicelogin",
		"sign in",
		"signin",
		"session expired",
	} {
		if bytes.Contains(b, []byte(marker)) {
			return true
		}
	}
	return false
}

// Kind identifies an archive from its magic bytes: "gzip", "zip", or "".
func Kind(head []byte) string {
	if len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b {
		return "gzip"
	}
	if len(head) >= 4 && head[0] == 'P' && head[1] == 'K' {
		if (head[2] == 3 && head[3] == 4) || (head[2] == 5 && head[3] == 6) || (head[2] == 7 && head[3] == 8) {
			return "zip"
		}
	}
	return ""
}

// FileKind reads the magic bytes of a file on disk.
func FileKind(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 4)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", err
	}
	return Kind(head[:n]), nil
}

// VerifyFile reads the whole archive to confirm its integrity: for gzip this
// decompresses the full stream and checks the trailing CRC (what gzip -t
// does); for zip it reads every entry so each CRC is checked.
func VerifyFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	head := make([]byte, 4)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	switch Kind(head[:n]) {
	case "gzip":
		br := bufio.NewReaderSize(f, 1<<20)
		zr, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("gzip header: %w", err)
		}
		if _, err := io.Copy(io.Discard, zr); err != nil {
			return fmt.Errorf("gzip stream: %w", err)
		}
		return zr.Close()
	case "zip":
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		zr, err := zip.NewReader(f, fi.Size())
		if err != nil {
			return fmt.Errorf("zip directory: %w", err)
		}
		for _, zf := range zr.File {
			rc, err := zf.Open()
			if err != nil {
				return fmt.Errorf("zip entry %s: %w", zf.Name, err)
			}
			_, err = io.Copy(io.Discard, rc)
			cerr := rc.Close()
			if err != nil {
				return fmt.Errorf("zip entry %s: %w", zf.Name, err)
			}
			if cerr != nil {
				return fmt.Errorf("zip entry %s: %w", zf.Name, cerr)
			}
		}
		return nil
	default:
		return fmt.Errorf("unrecognized archive magic bytes % x", head[:n])
	}
}
