// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.CapturedURL = "https://example.com/x-001.tgz?i=0"
	s.JobID = "job-1"
	s.TotalParts = 2
	s.Headers = map[string]string{"User-Agent": "test"}
	s.VerifyMode = "full"
	s.Parts = []*Part{
		{Num: 1, Filename: "x-001.tgz", Index: 0, Status: Done, ActualSize: 100, Attempts: 1, VerifiedAt: time.Now().UTC()},
		{Num: 2, Filename: "x-002.tgz", Index: 1, Status: Pending},
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalParts != 2 || got.JobID != "job-1" || len(got.Parts) != 2 {
		t.Fatalf("loaded state mismatch: %+v", got)
	}
	if got.Parts[0].Status != Done || got.Parts[0].ActualSize != 100 {
		t.Errorf("part 1 = %+v", got.Parts[0])
	}
	if got.Part(2).Filename != "x-002.tgz" || got.Part(2).Index != 1 {
		t.Errorf("Part(2) = %+v", got.Part(2))
	}
	if got.Part(0) != nil || got.Part(3) != nil {
		t.Error("out-of-range Part() should be nil")
	}
}

func TestUpdatePersistsAndKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.TotalParts = 1
	s.Parts = []*Part{{Num: 1, Filename: "x-001.tgz", Status: Pending}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func() { s.Parts[0].Status = Done }); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Parts[0].Status != Done {
		t.Errorf("status = %s, want done", got.Parts[0].Status)
	}
	// previous version preserved for disaster recovery
	b, err := os.ReadFile(backupPath(dir))
	if err != nil {
		t.Fatalf("no backup written: %v", err)
	}
	if !strings.Contains(string(b), `"pending"`) {
		t.Error("backup doesn't hold the previous version")
	}
}

func TestLoadRejectsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(Path(dir), []byte(`{"version":99,"parts":[]}`), 0644)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("err = %v, want version mismatch", err)
	}
}

func TestLoadRejectsUnknownStatus(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(Path(dir), []byte(`{"version":2,"parts":[{"num":1,"filename":"x-001.tgz","index":0,"status":"quarantined"}]}`), 0644)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "unknown status") {
		t.Errorf("err = %v, want unknown-status rejection", err)
	}
}

func TestLoadRejectsUnsafeFilename(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(Path(dir), []byte(`{"version":2,"parts":[{"num":1,"filename":"..\\evil.tgz","index":0,"status":"pending"}]}`), 0644)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "unsafe filename") {
		t.Errorf("err = %v, want unsafe-filename rejection", err)
	}
}

func TestLoadCorruptHintsAtBackup(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.Parts = []*Part{{Num: 1, Filename: "x-001.tgz", Status: Pending}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil { // second save creates the .bak
		t.Fatal(err)
	}
	os.WriteFile(Path(dir), []byte("{truncated"), 0644)
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), ".bak") {
		t.Errorf("err = %v, want backup hint", err)
	}
}

func TestCounts(t *testing.T) {
	s := New(t.TempDir())
	s.Parts = []*Part{
		{Num: 1, Status: Done, ActualSize: 10},
		{Num: 2, Status: Downloaded, ActualSize: 20},
		{Num: 3, Status: Pending},
		{Num: 4, Status: Attention},
		{Num: 5, Status: Corrupt},
	}
	pending, downloaded, done, attention, corrupt, bytes := s.Counts()
	if pending != 1 || downloaded != 1 || done != 1 || attention != 1 || corrupt != 1 || bytes != 30 {
		t.Errorf("Counts = %d %d %d %d %d %d", pending, downloaded, done, attention, corrupt, bytes)
	}
}

func TestRemainingEstimate(t *testing.T) {
	s := New(t.TempDir())
	s.Parts = []*Part{
		{Num: 1, Status: Done, ActualSize: 100},
		{Num: 2, Status: Pending, ExpectedSize: 80},
		{Num: 3, Status: Pending}, // unknown → average of done (100)
	}
	remaining, ok := s.RemainingEstimate()
	if !ok || remaining != 180 {
		t.Errorf("RemainingEstimate = %d,%v, want 180,true", remaining, ok)
	}

	s2 := New(t.TempDir())
	s2.Parts = []*Part{{Num: 1, Status: Pending}}
	if _, ok := s2.RemainingEstimate(); ok {
		t.Error("estimate with no data should report !ok")
	}
}

func TestCookieFilePermissionsAndAtomicity(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCookie(dir, "SID=secret"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(CookiePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("cookie file mode = %o, want 0600", fi.Mode().Perm())
	}
	// overwrite must go through rename, leaving no window without a file
	if err := SaveCookie(dir, "SID=rotated"); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCookie(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c != "SID=rotated" {
		t.Errorf("cookie = %q", c)
	}
	if _, err := os.Stat(CookiePath(dir) + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp cookie file left behind")
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("expected error for missing state")
	}
	if _, err := LoadCookie(t.TempDir()); err == nil {
		t.Error("expected error for missing cookie file")
	}
}
