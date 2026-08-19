// SPDX-License-Identifier: GPL-3.0-or-later

package takeout

import (
	"errors"
	"strings"
	"testing"
)

func TestDeriveTypicalCapture(t *testing.T) {
	raw := "https://takeout-download.usercontent.google.com/download/takeout-20260815T123456Z-3-001.tgz?j=abc-uuid&user=12345&i=0&authuser=0"
	tmpl, err := Derive(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.JobID != "abc-uuid" {
		t.Errorf("JobID = %q", tmpl.JobID)
	}
	if tmpl.NumWidth != 3 || tmpl.CapturedNum != 1 {
		t.Errorf("NumWidth=%d CapturedNum=%d", tmpl.NumWidth, tmpl.CapturedNum)
	}
	if !tmpl.HasIndex || tmpl.Offset != 1 {
		t.Errorf("HasIndex=%v Offset=%d, want true/1", tmpl.HasIndex, tmpl.Offset)
	}

	if got, want := tmpl.PartURL(2), "https://takeout-download.usercontent.google.com/download/takeout-20260815T123456Z-3-002.tgz?j=abc-uuid&user=12345&i=1&authuser=0"; got != want {
		t.Errorf("PartURL(2)\n got %s\nwant %s", got, want)
	}
	if got := tmpl.PartURL(110); !strings.Contains(got, "-110.tgz") || !strings.Contains(got, "i=109") {
		t.Errorf("PartURL(110) = %s", got)
	}
	if got, want := tmpl.Filename(42), "takeout-20260815T123456Z-3-042.tgz"; got != want {
		t.Errorf("Filename(42) = %q, want %q", got, want)
	}
}

func TestDerivePreservesQueryOrderAndEncoding(t *testing.T) {
	raw := "https://host.example/download/x-001.tgz?zz=first&j=a%2Bb&i=0&aa=last"
	tmpl, err := Derive(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := tmpl.PartURL(5)
	want := "https://host.example/download/x-005.tgz?zz=first&j=a%2Bb&i=4&aa=last"
	if got != want {
		t.Errorf("PartURL(5)\n got %s\nwant %s", got, want)
	}
}

func TestDeriveZeroOffset(t *testing.T) {
	tmpl, err := Derive("https://host.example/download/x-005.tgz?i=5")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.Offset != 0 {
		t.Fatalf("Offset = %d, want 0", tmpl.Offset)
	}
	if got := tmpl.PartURL(7); !strings.Contains(got, "x-007.tgz?i=7") {
		t.Errorf("PartURL(7) = %s", got)
	}
}

func TestDeriveNoIndexParam(t *testing.T) {
	tmpl, err := Derive("https://host.example/download/x-001.tgz?j=abc")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.HasIndex {
		t.Error("HasIndex should be false")
	}
	if got, want := tmpl.PartURL(3), "https://host.example/download/x-003.tgz?j=abc"; got != want {
		t.Errorf("PartURL(3) = %s, want %s", got, want)
	}
}

func TestDeriveRedirectURL(t *testing.T) {
	_, err := Derive("https://takeout.google.com/settings/takeout/download?j=abc&i=0")
	if !errors.Is(err, ErrRedirectURL) {
		t.Errorf("err = %v, want ErrRedirectURL", err)
	}
}

func TestDeriveRejectsNonPartURL(t *testing.T) {
	if _, err := Derive("https://host.example/download/readme.txt"); err == nil {
		t.Error("expected error for filename without part number")
	}
}

func TestDeriveWidePadding(t *testing.T) {
	tmpl, err := Derive("https://host.example/download/x-0001.zip?i=0")
	if err != nil {
		t.Fatal(err)
	}
	if got := tmpl.Filename(12); got != "x-0012.zip" {
		t.Errorf("Filename(12) = %q", got)
	}
}
