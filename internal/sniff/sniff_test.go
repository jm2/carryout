// SPDX-License-Identifier: GPL-3.0-or-later

package sniff

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func gzipBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestIsHTML(t *testing.T) {
	if !IsHTML("text/html; charset=utf-8", []byte("anything")) {
		t.Error("content-type text/html not detected")
	}
	if !IsHTMLContentType("Text/HTML;charset=UTF-8") {
		t.Error("case/parameter variant not detected")
	}
	if !IsHTML("application/octet-stream", []byte("\n  <!DOCTYPE html><html>")) {
		t.Error("leading < not detected")
	}
	if IsHTML("application/octet-stream", []byte{0x1f, 0x8b, 0x08}) {
		t.Error("gzip misdetected as HTML")
	}
	if IsHTMLContentType("application/octet-stream") {
		t.Error("octet-stream misdetected as HTML content type")
	}
}

func TestLooksLikeSignIn(t *testing.T) {
	if !LooksLikeSignIn([]byte(`<a href="https://accounts.google.com/ServiceLogin">Sign in</a>`)) {
		t.Error("sign-in page not detected")
	}
	if LooksLikeSignIn([]byte(`<html><body>Quota exceeded</body></html>`)) {
		t.Error("generic error page misdetected as sign-in")
	}
}

func TestKind(t *testing.T) {
	if Kind([]byte{0x1f, 0x8b, 0x08, 0x00}) != "gzip" {
		t.Error("gzip magic not recognized")
	}
	if Kind([]byte{'P', 'K', 3, 4}) != "zip" {
		t.Error("zip magic not recognized")
	}
	if Kind([]byte("<html>")) != "" {
		t.Error("html recognized as archive")
	}
}

func TestVerifyGzipOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.tgz")

	// a real (tiny) tar.gz, like Takeout produces
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := bytes.Repeat([]byte("photo bytes "), 10000)
	if err := tw.WriteHeader(&tar.Header{Name: "photo.jpg", Mode: 0644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(content)
	tw.Close()

	if err := os.WriteFile(path, gzipBytes(t, tarBuf.Bytes()), 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(context.Background(), path); err != nil {
		t.Errorf("VerifyFile = %v, want nil", err)
	}
	if k, err := FileKind(path); err != nil || k != "gzip" {
		t.Errorf("FileKind = %q, %v", k, err)
	}
}

func TestVerifyTruncatedGzipFails(t *testing.T) {
	full := gzipBytes(t, bytes.Repeat([]byte("data"), 50000))
	path := filepath.Join(t.TempDir(), "trunc.tgz")
	if err := os.WriteFile(path, full[:len(full)-100], 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(context.Background(), path); err == nil {
		t.Error("truncated gzip verified OK, want error")
	}
}

func TestVerifyCorruptGzipFails(t *testing.T) {
	full := gzipBytes(t, bytes.Repeat([]byte("data"), 50000))
	full[len(full)/2] ^= 0xff // flip bits mid-stream
	path := filepath.Join(t.TempDir(), "corrupt.tgz")
	if err := os.WriteFile(path, full, 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(context.Background(), path); err == nil {
		t.Error("corrupted gzip verified OK, want error")
	}
}

func TestVerifyHonorsContext(t *testing.T) {
	full := gzipBytes(t, bytes.Repeat([]byte("data"), 50000))
	path := filepath.Join(t.TempDir(), "c.tgz")
	if err := os.WriteFile(path, full, 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := VerifyFile(ctx, path); err == nil {
		t.Error("cancelled verification returned nil")
	}
}

func TestVerifyZip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("hello"))
	zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(context.Background(), path); err != nil {
		t.Errorf("VerifyFile(zip) = %v", err)
	}
}

func TestVerifyGarbageFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.tgz")
	if err := os.WriteFile(path, []byte("<html>not an archive</html>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(context.Background(), path); err == nil {
		t.Error("garbage verified OK, want error")
	}
}

func TestStreamVerifierCleanStream(t *testing.T) {
	data := gzipBytes(t, bytes.Repeat([]byte("stream me "), 20000))
	v := NewGzipStreamVerifier()
	// write in odd-sized chunks like a network copy would
	for i := 0; i < len(data); i += 3391 {
		end := min(i+3391, len(data))
		if _, err := v.Write(data[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Finish(); err != nil {
		t.Errorf("Finish = %v, want nil", err)
	}
}

func TestStreamVerifierCatchesCorruption(t *testing.T) {
	data := gzipBytes(t, bytes.Repeat([]byte("stream me "), 20000))
	data[len(data)/2] ^= 0xff
	v := NewGzipStreamVerifier()
	if _, err := v.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := v.Finish(); err == nil {
		t.Error("corrupted stream verified OK, want error")
	}
}

func TestStreamVerifierCatchesTruncation(t *testing.T) {
	data := gzipBytes(t, bytes.Repeat([]byte("stream me "), 20000))
	v := NewGzipStreamVerifier()
	if _, err := v.Write(data[:len(data)-50]); err != nil {
		t.Fatal(err)
	}
	if err := v.Finish(); err == nil {
		t.Error("truncated stream verified OK, want error")
	}
}

func TestStreamVerifierAbortDoesNotBlock(t *testing.T) {
	v := NewGzipStreamVerifier()
	v.Write([]byte{0x1f, 0x8b})
	v.Abort() // must not deadlock
}
