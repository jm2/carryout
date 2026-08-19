// SPDX-License-Identifier: GPL-3.0-or-later

package takeout

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDeriveTypicalCapture(t *testing.T) {
	raw := "https://takeout-download.usercontent.google.com/download/takeout-20260818T182058Z-3-001.tgz?j=abc-uuid&user=12345&i=38&authuser=0"
	tmpl, err := Derive(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.JobID != "abc-uuid" {
		t.Errorf("JobID = %q", tmpl.JobID)
	}
	if tmpl.CapturedName != "takeout-20260818T182058Z-3-001.tgz" {
		t.Errorf("CapturedName = %q", tmpl.CapturedName)
	}
	if !tmpl.HasIndex || tmpl.CapturedIndex != 38 {
		t.Errorf("HasIndex=%v CapturedIndex=%d", tmpl.HasIndex, tmpl.CapturedIndex)
	}
	if err := tmpl.SelfCheck(); err != nil {
		t.Errorf("SelfCheck: %v", err)
	}

	got := tmpl.BuildURL("takeout-20260818T182058Z-7-014.tgz", 90)
	want := "https://takeout-download.usercontent.google.com/download/takeout-20260818T182058Z-7-014.tgz?j=abc-uuid&user=12345&i=90&authuser=0"
	if got != want {
		t.Errorf("BuildURL\n got %s\nwant %s", got, want)
	}
}

func TestBuildURLPreservesQueryOrderAndEncoding(t *testing.T) {
	raw := "https://host.example/download/x-001.tgz?zz=first&j=a%2Bb&i=0&aa=last"
	tmpl, err := Derive(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := tmpl.BuildURL("x-005.tgz", 4)
	want := "https://host.example/download/x-005.tgz?zz=first&j=a%2Bb&i=4&aa=last"
	if got != want {
		t.Errorf("BuildURL\n got %s\nwant %s", got, want)
	}
	if err := tmpl.SelfCheck(); err != nil {
		t.Errorf("SelfCheck: %v", err)
	}
}

func TestBuildURLWithoutIndexParam(t *testing.T) {
	tmpl, err := Derive("https://host.example/download/x-001.tgz?j=abc")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.HasIndex {
		t.Error("HasIndex should be false")
	}
	if got, want := tmpl.BuildURL("x-003.tgz", -1), "https://host.example/download/x-003.tgz?j=abc"; got != want {
		t.Errorf("BuildURL = %s, want %s", got, want)
	}
}

func TestDeriveRedirectURL(t *testing.T) {
	_, err := Derive("https://takeout.google.com/settings/takeout/download?j=abc&i=0")
	if !errors.Is(err, ErrRedirectURL) {
		t.Errorf("err = %v, want ErrRedirectURL", err)
	}
}

func TestDeriveRejectsNonArchiveURL(t *testing.T) {
	for _, raw := range []string{
		"https://host.example/download/readme.txt",
		"https://host.example/download/",
		"ftp://host.example/x-001.tgz",
	} {
		if _, err := Derive(raw); err == nil {
			t.Errorf("Derive(%q) succeeded, want error", raw)
		}
	}
}

func TestValidFilenameRejectsTraversal(t *testing.T) {
	bad := []string{
		`evil\..\..\x-001.tgz`, // encoded backslashes decoded into the path
		"../x-001.tgz",
		"a/b-001.tgz",
		"x:alt-001.tgz",
		".hidden-001.tgz",
		"x-001.exe",
	}
	for _, name := range bad {
		if ValidFilename(name) {
			t.Errorf("ValidFilename(%q) = true, want false", name)
		}
	}
	good := []string{"takeout-20260818T182058Z-3-001.tgz", "takeout-20260818T182058Z-001.zip", "x-001.tar.gz"}
	for _, name := range good {
		if !ValidFilename(name) {
			t.Errorf("ValidFilename(%q) = false, want true", name)
		}
	}
}

func TestDeriveRejectsTraversalCapture(t *testing.T) {
	// %5C decodes to backslash in u.Path; the filename check must refuse it.
	if _, err := Derive(`https://host.example/download/evil%5C..%5C..%5Cx-001.tgz?i=0`); err == nil {
		t.Error("traversal capture accepted, want error")
	}
}

// realExportSummary mirrors the user-facing Takeout page text: groups listed
// out of order, NNN restarting per group.
func realExportSummary() string {
	var b strings.Builder
	b.WriteString("Export summary\nOverall status: Completed\nDownloadable Zips: ")
	groups := []struct{ g, n int }{{3, 19}, {2, 18}, {4, 19}, {1, 19}, {7, 19}, {6, 19}, {8, 10}, {5, 19}}
	for _, gr := range groups {
		for i := 1; i <= gr.n; i++ {
			fmt.Fprintf(&b, "takeout-20260818T182058Z-%d-%03d.tgz (Number of times already downloaded: %d), ", gr.g, i, (gr.g+i)%3)
		}
	}
	b.WriteString("\nTotal size of your export: 6926.33 GB\n")
	return b.String()
}

func TestParseInventoryRealSummary(t *testing.T) {
	entries, err := ParseInventory(realExportSummary())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 142 {
		t.Fatalf("parsed %d entries, want 142", len(entries))
	}
	if entries[0].Filename != "takeout-20260818T182058Z-3-001.tgz" {
		t.Errorf("first = %q", entries[0].Filename)
	}
	if entries[141].Filename != "takeout-20260818T182058Z-5-019.tgz" {
		t.Errorf("last = %q", entries[141].Filename)
	}
	// per-file download counters captured
	if entries[0].Downloaded != (3+1)%3 {
		t.Errorf("entries[0].Downloaded = %d", entries[0].Downloaded)
	}
	if !GroupSummaryIsGrouped(entries) {
		t.Error("grouped export not detected")
	}
	sum := GroupSummary(entries)
	if !strings.Contains(sum, "142 files") || !strings.Contains(sum, "8 groups") {
		t.Errorf("GroupSummary = %q", sum)
	}
}

func TestParseInventoryDedupsAndPlainLists(t *testing.T) {
	entries, err := ParseInventory("takeout-20260818T182058Z-1-001.tgz\ntakeout-20260818T182058Z-1-002.tgz\ntakeout-20260818T182058Z-1-001.tgz\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2 (dedup)", len(entries))
	}
	if entries[0].Downloaded != -1 {
		t.Errorf("Downloaded = %d, want -1 (unknown)", entries[0].Downloaded)
	}
	if _, err := ParseInventory("no filenames here"); err == nil {
		t.Error("expected error for empty inventory")
	}
}

func TestCalibrateAssignsGlobalIndexes(t *testing.T) {
	entries, err := ParseInventory(realExportSummary())
	if err != nil {
		t.Fatal(err)
	}
	// groups are listed 3(19), 2(18), …: group 2's -2-003 is the 22nd file,
	// list position 21 (0-based), so a capture of it carries i=21
	tmpl, err := Derive("https://host.example/download/takeout-20260818T182058Z-2-003.tgz?j=x&i=21")
	if err != nil {
		t.Fatal(err)
	}
	if err := Calibrate(entries, tmpl); err != nil {
		t.Fatal(err)
	}
	if entries[0].Index != 0 || entries[141].Index != 141 {
		t.Errorf("indexes = %d..%d, want 0..141", entries[0].Index, entries[141].Index)
	}
	if entries[21].Filename != "takeout-20260818T182058Z-2-003.tgz" || entries[21].Index != 21 {
		t.Errorf("captured entry = %+v", entries[21])
	}
}

func TestCalibrateRequiresCapturedFileInList(t *testing.T) {
	entries, _ := ParseInventory("takeout-20260818T182058Z-1-001.tgz")
	tmpl, _ := Derive("https://host.example/download/takeout-20260818T182058Z-9-001.tgz?i=0")
	if err := Calibrate(entries, tmpl); err == nil {
		t.Error("expected error for capture not in inventory")
	}
}

func TestCalibrateRejectsNegativeIndexes(t *testing.T) {
	entries, _ := ParseInventory("takeout-x-1-001.tgz takeout-x-1-002.tgz")
	// captured file is at position 1 but claims i=0 → position 0 would get -1
	tmpl, _ := Derive("https://host.example/download/takeout-x-1-002.tgz?i=0")
	if err := Calibrate(entries, tmpl); err == nil {
		t.Error("expected error for negative base index")
	}
}

func TestCalibrateWithoutIndexParam(t *testing.T) {
	entries, _ := ParseInventory("takeout-x-1-001.tgz takeout-x-1-002.tgz")
	tmpl, _ := Derive("https://host.example/download/takeout-x-1-001.tgz")
	if err := Calibrate(entries, tmpl); err != nil {
		t.Fatal(err)
	}
	if entries[0].Index != -1 || entries[1].Index != -1 {
		t.Errorf("indexes = %d,%d, want -1,-1", entries[0].Index, entries[1].Index)
	}
}

func TestSynthesizeSingleSequence(t *testing.T) {
	tmpl, err := Derive("https://host.example/download/takeout-20240101T000000Z-001.tgz?j=x&i=0")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Synthesize(tmpl, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("len = %d", len(entries))
	}
	if entries[2].Filename != "takeout-20240101T000000Z-003.tgz" || entries[2].Index != 2 {
		t.Errorf("entries[2] = %+v", entries[2])
	}
}

func TestSynthesizeRefusesMultiGroup(t *testing.T) {
	tmpl, err := Derive("https://host.example/download/takeout-20260818T182058Z-3-001.tgz?j=x&i=0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Synthesize(tmpl, 142); err == nil {
		t.Error("Synthesize accepted a multi-group filename; a part count cannot describe that export")
	}
}
