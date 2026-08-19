// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"bytes"
	"strings"
	"testing"
)

func TestPlainHoldsEventsAndDropsProgressDuringPrompt(t *testing.T) {
	var out bytes.Buffer
	g := NewPlain(&out)

	g.Logf("before prompt")
	g.Hold()
	g.Logf("part 042: downloaded 50.0 GiB")  // event: must survive the prompt
	g.Progressf("part 043 12%% · 100 MiB/s") // ephemeral: must be dropped
	if strings.Contains(out.String(), "042") || strings.Contains(out.String(), "043") {
		t.Fatalf("output leaked while held:\n%s", out.String())
	}
	g.Release()
	g.Logf("after prompt")

	got := out.String()
	if !strings.Contains(got, "before prompt") || !strings.Contains(got, "after prompt") {
		t.Errorf("open-gate lines missing:\n%s", got)
	}
	if !strings.Contains(got, "part 042: downloaded 50.0 GiB") {
		t.Errorf("held event line was not replayed:\n%s", got)
	}
	if !strings.Contains(got, "1 log line(s) arrived while the prompt was up") {
		t.Errorf("replay header missing:\n%s", got)
	}
	if strings.Contains(got, "part 043") {
		t.Errorf("progress tick should have been dropped:\n%s", got)
	}
	if strings.Index(got, "part 042") < strings.Index(got, "before prompt") {
		t.Errorf("replay ordering wrong:\n%s", got)
	}
}

func TestPlainReleaseWithoutBufferIsSilent(t *testing.T) {
	var out bytes.Buffer
	g := NewPlain(&out)
	g.Hold()
	g.Release()
	if out.Len() != 0 {
		t.Errorf("empty release produced output: %q", out.String())
	}
}
