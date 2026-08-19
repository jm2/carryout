// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"os"
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
		{Num: 1, Filename: "x-001.tgz", Status: Done, ActualSize: 100, Attempts: 1, VerifiedAt: time.Now().UTC()},
		{Num: 2, Filename: "x-002.tgz", Status: Pending},
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
	if got.Part(2).Filename != "x-002.tgz" {
		t.Errorf("Part(2) = %+v", got.Part(2))
	}
	if got.Part(0) != nil || got.Part(3) != nil {
		t.Error("out-of-range Part() should be nil")
	}
}

func TestUpdatePersists(t *testing.T) {
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

func TestCookieFilePermissions(t *testing.T) {
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
	c, err := LoadCookie(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c != "SID=secret" {
		t.Errorf("cookie = %q", c)
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
