// SPDX-License-Identifier: GPL-3.0-or-later

package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jm2/carryout/internal/sniff"
	"github.com/jm2/carryout/internal/state"
	"github.com/jm2/carryout/internal/takeout"
)

func gzipPayload(t *testing.T, seed string, repeat int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(bytes.Repeat([]byte(seed), repeat)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeTakeout mimics the usercontent download host: cookie-gated, Range-aware,
// with per-part contents and injectable failures.
type fakeTakeout struct {
	t     *testing.T
	mu    sync.Mutex
	parts map[int][]byte // 1-based part number -> archive bytes

	validCookie string
	flipAfter   int // after this many served parts, rotate validCookie to flipTo
	flipTo      string
	servedOK    int
	fail500     map[int]int    // part -> how many 500s to serve before succeeding
	htmlPage    map[int]string // part -> serve this HTML instead (200 text/html)
	rawPage     map[int][]byte // part -> serve this as application/octet-stream
	cdName      map[int]string // part -> Content-Disposition filename to claim
	fullBody206 map[int]bool   // part -> answer Range requests with a start-0 206
	served      map[int]int    // part -> times body was served
	requests    map[int]int    // part -> total requests seen
}

var testPathRe = regexp.MustCompile(`/download/x-(\d+)\.tgz$`)

func (s *fakeTakeout) handler(w http.ResponseWriter, r *http.Request) {
	m := testPathRe.FindStringSubmatch(r.URL.Path)
	if m == nil {
		s.t.Errorf("unexpected path %s", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	num, _ := strconv.Atoi(m[1])

	s.mu.Lock()
	s.requests[num]++
	// the i= query index must be filename number minus one (as in the capture)
	if got := r.URL.Query().Get("i"); got != strconv.Itoa(num-1) {
		s.mu.Unlock()
		s.t.Errorf("part %d: i=%s, want %d", num, got, num-1)
		http.NotFound(w, r)
		return
	}
	if ua := r.Header.Get("User-Agent"); ua != "test-agent" {
		s.t.Errorf("part %d: User-Agent = %q, want test-agent", num, ua)
	}
	if r.Header.Get("Cookie") != s.validCookie {
		s.mu.Unlock()
		http.Redirect(w, r, "https://accounts.google.com/ServiceLogin?continue=x", http.StatusFound)
		return
	}
	if html, ok := s.htmlPage[num]; ok {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, html)
		return
	}
	if raw, ok := s.rawPage[num]; ok {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(raw)
		return
	}
	if s.fail500[num] > 0 {
		s.fail500[num]--
		s.mu.Unlock()
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body, ok := s.parts[num]
	if !ok {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if cd, ok := s.cdName[num]; ok {
		w.Header().Set("Content-Disposition", `attachment; filename="`+cd+`"`)
	}
	s.served[num]++
	s.servedOK++
	if s.flipAfter > 0 && s.servedOK >= s.flipAfter {
		s.validCookie = s.flipTo
		s.flipAfter = 0
	}
	full206 := s.fullBody206[num]
	s.mu.Unlock()

	if full206 && r.Header.Get("Range") != "" {
		// a server that "resumes" by restarting from byte zero
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body)
		return
	}
	http.ServeContent(w, r, fmt.Sprintf("x-%03d.tgz", num), time.Unix(1700000000, 0), bytes.NewReader(body))
}

type testEnv struct {
	dir    string
	srv    *httptest.Server
	fake   *fakeTakeout
	st     *state.State
	tmpl   *takeout.Template
	cookie string
}

func newTestEnv(t *testing.T, parts map[int][]byte, total int) *testEnv {
	t.Helper()
	fake := &fakeTakeout{
		t:           t,
		parts:       parts,
		validCookie: "SID=good",
		fail500:     map[int]int{},
		htmlPage:    map[int]string{},
		rawPage:     map[int][]byte{},
		cdName:      map[int]string{},
		fullBody206: map[int]bool{},
		served:      map[int]int{},
		requests:    map[int]int{},
	}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)

	captured := srv.URL + "/download/x-001.tgz?j=test-job&user=1&i=0"
	tmpl, err := takeout.Derive(captured)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	st := state.New(dir)
	st.CapturedURL = captured
	st.JobID = "test-job"
	st.TotalParts = total
	st.VerifyMode = "full"
	st.Headers = map[string]string{"User-Agent": "test-agent"}
	for n := 1; n <= total; n++ {
		st.Parts = append(st.Parts, &state.Part{
			Num:      n,
			Filename: fmt.Sprintf("x-%03d.tgz", n),
			Index:    n - 1,
			Status:   state.Pending,
		})
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveCookie(dir, "SID=good"); err != nil {
		t.Fatal(err)
	}
	return &testEnv{dir: dir, srv: srv, fake: fake, st: st, tmpl: tmpl, cookie: "SID=good"}
}

func (e *testEnv) options(t *testing.T) Options {
	return Options{
		Dir:          e.dir,
		Workers:      2,
		MaxTries:     2,
		FullVerify:   true,
		StallTimeout: 30 * time.Second,
		RetryDelay:   10 * time.Millisecond,
		Cooldown:     50 * time.Millisecond,
		Logf:         t.Logf,
	}
}

func TestHappyPathInlineVerified(t *testing.T) {
	parts := map[int][]byte{
		1: gzipPayload(t, "part one ", 5000),
		2: gzipPayload(t, "part two ", 6000),
		3: gzipPayload(t, "part three ", 7000),
	}
	env := newTestEnv(t, parts, 3)
	f := New(env.tmpl, env.st, env.cookie, env.options(t))

	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Complete() || sum.Done != 3 {
		t.Fatalf("summary = %+v, want 3 done", sum)
	}
	for n, want := range parts {
		p := env.st.Part(n)
		if p.Status != state.Done || p.VerifiedAt.IsZero() {
			t.Errorf("part %d not done+verified: %+v", n, p)
		}
		if p.Attempts != 1 {
			t.Errorf("part %d served %d times, want 1", n, p.Attempts)
		}
		got, err := os.ReadFile(filepath.Join(env.dir, p.Filename))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("part %d content mismatch: %d vs %d bytes", n, len(got), len(want))
		}
		if p.ExpectedSize != int64(len(want)) || p.ActualSize != int64(len(want)) {
			t.Errorf("part %d sizes: expected=%d actual=%d want %d", n, p.ExpectedSize, p.ActualSize, len(want))
		}
	}
	st2, err := state.Load(env.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Part(2).Status != state.Done {
		t.Error("persisted state lost part 2 completion")
	}
}

func TestAuthExpiryPausesAndRefreshes(t *testing.T) {
	parts := map[int][]byte{
		1: gzipPayload(t, "one ", 3000),
		2: gzipPayload(t, "two ", 3000),
		3: gzipPayload(t, "three ", 3000),
	}
	env := newTestEnv(t, parts, 3)
	env.fake.flipAfter = 1 // session dies after the first served part
	env.fake.flipTo = "SID=fresh"

	var refreshes int
	opts := env.options(t)
	opts.Workers = 1 // deterministic ordering
	opts.RefreshAuth = func(ctx context.Context, reason string) (string, error) {
		refreshes++
		return "SID=fresh", nil
	}
	f := New(env.tmpl, env.st, env.cookie, opts)

	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Complete() {
		t.Fatalf("summary = %+v", sum)
	}
	if refreshes != 1 {
		t.Errorf("RefreshAuth called %d times, want 1", refreshes)
	}
	c, err := state.LoadCookie(env.dir)
	if err != nil {
		t.Fatal(err)
	}
	if c != "SID=fresh" {
		t.Errorf("persisted cookie = %q", c)
	}
	if got := env.st.Part(2).Attempts + env.st.Part(3).Attempts; got != 2 {
		t.Errorf("parts 2+3 served %d times total, want 2 (auth failures must not count)", got)
	}
}

func TestAuthExpiryWithoutPromptStopsRun(t *testing.T) {
	parts := map[int][]byte{1: gzipPayload(t, "one ", 1000)}
	env := newTestEnv(t, parts, 1)
	env.fake.validCookie = "SID=other" // our cookie is dead from the start

	opts := env.options(t)
	opts.RefreshAuth = nil
	f := New(env.tmpl, env.st, env.cookie, opts)

	_, err := f.Run(context.Background())
	if err == nil || !AuthNeeded(err) {
		t.Fatalf("err = %v, want auth-needed fatal", err)
	}
	if env.st.Part(1).Attempts != 0 {
		t.Errorf("dead session burned %d served downloads, want 0", env.st.Part(1).Attempts)
	}
}

func TestFreshCookiesThatDontHelpFlagThePart(t *testing.T) {
	// The part is rejected even with brand-new cookies (per-part cap, expired
	// link): must NOT loop the prompt forever.
	parts := map[int][]byte{1: gzipPayload(t, "one ", 1000)}
	env := newTestEnv(t, parts, 1)
	env.fake.validCookie = "SID=never-matches"

	var refreshes int
	opts := env.options(t)
	opts.RefreshAuth = func(ctx context.Context, reason string) (string, error) {
		refreshes++
		return fmt.Sprintf("SID=fresh-%d", refreshes), nil
	}
	f := New(env.tmpl, env.st, env.cookie, opts)

	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatalf("run should continue past a capped part, got %v", err)
	}
	if refreshes != 1 {
		t.Errorf("RefreshAuth called %d times, want exactly 1 (no prompt loop)", refreshes)
	}
	if sum.Attention != 1 || env.st.Part(1).Status != state.Attention {
		t.Errorf("part not flagged: %+v", env.st.Part(1))
	}
	if env.st.Part(1).Attempts != 0 {
		t.Errorf("rejected part burned %d serves", env.st.Part(1).Attempts)
	}
}

func TestTransientErrorRetriesThenSucceeds(t *testing.T) {
	parts := map[int][]byte{1: gzipPayload(t, "one ", 2000)}
	env := newTestEnv(t, parts, 1)
	env.fake.fail500[1] = 1

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Done != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if env.st.Part(1).Attempts != 1 {
		t.Errorf("500s counted as served downloads: %d", env.st.Part(1).Attempts)
	}
}

func TestPersistentErrorFlagsForAttention(t *testing.T) {
	parts := map[int][]byte{
		1: gzipPayload(t, "one ", 2000),
		2: gzipPayload(t, "two ", 2000),
	}
	env := newTestEnv(t, parts, 2)
	env.fake.fail500[2] = 99 // never recovers

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err) // attention is non-fatal; other parts finish
	}
	if sum.Done != 1 || sum.Attention != 1 {
		t.Fatalf("summary = %+v, want 1 done 1 attention", sum)
	}
	if env.fake.requests[2] != 2 {
		t.Errorf("part 2 got %d requests, want exactly MaxTries=2 (no hammering)", env.fake.requests[2])
	}
}

func TestResumeFromPartialFile(t *testing.T) {
	full := gzipPayload(t, "resumable data ", 8000)
	env := newTestEnv(t, map[int][]byte{1: full}, 1)

	half := len(full) / 2
	partPath := filepath.Join(env.dir, env.st.Part(1).Filename+".part")
	if err := os.WriteFile(partPath, full[:half], 0644); err != nil {
		t.Fatal(err)
	}

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Done != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	p := env.st.Part(1)
	if p.VerifiedAt.IsZero() {
		t.Error("resumed part must be fully verified, not trusted on size")
	}
	got, err := os.ReadFile(filepath.Join(env.dir, p.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("resumed file mismatch: %d vs %d bytes", len(got), len(full))
	}
	if p.ExpectedSize != int64(len(full)) {
		t.Errorf("ExpectedSize = %d, want %d (from Content-Range)", p.ExpectedSize, len(full))
	}
}

func TestCompletePartFileFinalizedVia416(t *testing.T) {
	// Crash after the last byte but before the rename leaves a byte-complete
	// .part; the next run's Range request gets 416 and must finalize, not
	// wedge or re-download.
	full := gzipPayload(t, "complete ", 6000)
	env := newTestEnv(t, map[int][]byte{1: full}, 1)
	partPath := filepath.Join(env.dir, env.st.Part(1).Filename+".part")
	if err := os.WriteFile(partPath, full, 0644); err != nil {
		t.Fatal(err)
	}

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Done != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	p := env.st.Part(1)
	if p.Attempts != 0 {
		t.Errorf("finalizing a complete file burned %d serves, want 0", p.Attempts)
	}
	if p.VerifiedAt.IsZero() {
		t.Error("finalized part must be fully verified")
	}
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Error(".part not renamed into place")
	}
}

func TestServerRestartFrom206StartZero(t *testing.T) {
	full := gzipPayload(t, "restart me ", 8000)
	env := newTestEnv(t, map[int][]byte{1: full}, 1)
	env.fake.fullBody206[1] = true

	half := len(full) / 2
	partPath := filepath.Join(env.dir, env.st.Part(1).Filename+".part")
	if err := os.WriteFile(partPath, full[:half], 0644); err != nil {
		t.Fatal(err)
	}

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Done != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	got, err := os.ReadFile(filepath.Join(env.dir, env.st.Part(1).Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("start-0 206 was appended instead of restarted: got %d bytes, want %d", len(got), len(full))
	}
}

func TestCorruptArchiveCaughtInline(t *testing.T) {
	// valid gzip magic, garbage stream: passes quick sniff, must fail inline
	corrupt := append([]byte{0x1f, 0x8b, 0x08, 0x00}, bytes.Repeat([]byte("junk"), 5000)...)
	env := newTestEnv(t, map[int][]byte{1: corrupt}, 1)

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Corrupt != 1 {
		t.Fatalf("summary = %+v, want 1 corrupt", sum)
	}
	p := env.st.Part(1)
	if p.Status != state.Corrupt || p.LastError == "" {
		t.Errorf("part 1 = %+v", p)
	}
	if _, err := os.Stat(filepath.Join(env.dir, p.Filename)); err != nil {
		t.Error("corrupt file was deleted; it should be kept for inspection")
	}
}

func TestUnknownHTMLPageFlagsPart(t *testing.T) {
	parts := map[int][]byte{
		1: gzipPayload(t, "one ", 1000),
		2: gzipPayload(t, "two ", 1000),
	}
	env := newTestEnv(t, parts, 2)
	env.fake.htmlPage[1] = "<html><body>Download quota exceeded for this file</body></html>"

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatalf("one quota page should not kill the run: %v", err)
	}
	if sum.Attention != 1 || sum.Done != 1 {
		t.Fatalf("summary = %+v, want part 1 attention and part 2 done", sum)
	}
	if env.st.Part(1).Attempts != 0 {
		t.Errorf("HTML page counted as %d served download(s), want 0", env.st.Part(1).Attempts)
	}
	diag := filepath.Join(env.dir, env.st.Part(1).Filename+".error.html")
	if _, serr := os.Stat(diag); serr != nil {
		t.Errorf("diagnostic page not saved at %s", diag)
	}
}

func TestSystemicHTMLPagesAreFatal(t *testing.T) {
	env := newTestEnv(t, map[int][]byte{}, 3)
	page := "<html><body>Something broke</body></html>"
	env.fake.htmlPage[1], env.fake.htmlPage[2], env.fake.htmlPage[3] = page, page, page

	opts := env.options(t)
	opts.Workers = 1
	f := New(env.tmpl, env.st, env.cookie, opts)
	_, err := f.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "systemic") {
		t.Fatalf("err = %v, want systemic fatal after repeated HTML pages", err)
	}
}

func TestMislabeledHTMLPageFlagsPartAndCounts(t *testing.T) {
	parts := map[int][]byte{1: gzipPayload(t, "one ", 1000)}
	env := newTestEnv(t, parts, 1)
	// Google claims binary but sends an error page: flagged, and counted as
	// served since Google may have counted it too.
	env.fake.rawPage[1] = []byte("<html><body>Something went wrong</body></html>")

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Attention != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if got := env.st.Part(1).Attempts; got != 1 {
		t.Errorf("Attempts = %d, want 1 (count conservatively when Google claims binary)", got)
	}
}

func TestMissing404FlagsPart(t *testing.T) {
	parts := map[int][]byte{1: gzipPayload(t, "one ", 1000)} // part 2 doesn't exist
	env := newTestEnv(t, parts, 2)

	opts := env.options(t)
	opts.Workers = 1
	f := New(env.tmpl, env.st, env.cookie, opts)
	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatalf("a single 404 should flag the part, not kill the run: %v", err)
	}
	if sum.Done != 1 || sum.Attention != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

func Test404StreakIsFatal(t *testing.T) {
	env := newTestEnv(t, map[int][]byte{}, 3) // nothing exists

	opts := env.options(t)
	opts.Workers = 1
	f := New(env.tmpl, env.st, env.cookie, opts)
	_, err := f.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want fatal after a 404 streak", err)
	}
}

func TestWrongFileServedIsFatal(t *testing.T) {
	parts := map[int][]byte{1: gzipPayload(t, "one ", 1000)}
	env := newTestEnv(t, parts, 1)
	env.fake.cdName[1] = "some-other-file.tgz"

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	_, err := f.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mapping") {
		t.Fatalf("err = %v, want wrong-file fatal", err)
	}
	if _, serr := os.Stat(filepath.Join(env.dir, env.st.Part(1).Filename)); serr == nil {
		t.Error("wrong file was written under the expected name")
	}
}

func TestAdoptExistingCompleteFile(t *testing.T) {
	body := gzipPayload(t, "already here ", 3000)
	env := newTestEnv(t, map[int][]byte{1: body}, 1)
	if err := os.WriteFile(filepath.Join(env.dir, env.st.Part(1).Filename), body, 0644); err != nil {
		t.Fatal(err)
	}

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Done != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if env.fake.requests[1] != 0 {
		t.Errorf("adopted part still hit the network %d times", env.fake.requests[1])
	}
	if env.st.Part(1).VerifiedAt.IsZero() {
		t.Error("adopted file must be fully verified")
	}
	if err := sniff.VerifyFile(context.Background(), filepath.Join(env.dir, env.st.Part(1).Filename)); err != nil {
		t.Error(err)
	}
}

func TestAdoptionRejectsWrongSize(t *testing.T) {
	body := gzipPayload(t, "real content ", 3000)
	truncated := body[:len(body)/2]
	env := newTestEnv(t, map[int][]byte{1: body}, 1)
	env.st.Part(1).ExpectedSize = int64(len(body))
	if err := os.WriteFile(filepath.Join(env.dir, env.st.Part(1).Filename), truncated, 0644); err != nil {
		t.Fatal(err)
	}

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Done != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if env.fake.requests[1] == 0 {
		t.Error("truncated file was adopted instead of re-downloaded")
	}
	got, _ := os.ReadFile(filepath.Join(env.dir, env.st.Part(1).Filename))
	if !bytes.Equal(got, body) {
		t.Error("final file is not the full content")
	}
}

func TestOnlyFilter(t *testing.T) {
	parts := map[int][]byte{
		1: gzipPayload(t, "one ", 1000),
		2: gzipPayload(t, "two ", 1000),
	}
	env := newTestEnv(t, parts, 2)
	opts := env.options(t)
	opts.Only = map[int]bool{2: true}
	f := New(env.tmpl, env.st, env.cookie, opts)

	sum, err := f.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Done != 1 || sum.Pending != 1 {
		t.Fatalf("summary = %+v, want part 2 done and part 1 untouched", sum)
	}
	if env.fake.requests[1] != 0 {
		t.Errorf("part 1 was requested despite -only 2")
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	body := gzipPayload(t, "one ", 1000)
	env := newTestEnv(t, map[int][]byte{1: body}, 2)
	// an adoptable stray must NOT be adopted (state mutated) during dry-run
	if err := os.WriteFile(filepath.Join(env.dir, env.st.Part(1).Filename), body, 0644); err != nil {
		t.Fatal(err)
	}
	env.st.Part(2).Status = state.Attention
	if err := env.st.Save(); err != nil {
		t.Fatal(err)
	}

	opts := env.options(t)
	opts.DryRun = true
	f := New(env.tmpl, env.st, env.cookie, opts)

	if _, err := f.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.fake.requests[1]+env.fake.requests[2] != 0 {
		t.Error("dry run hit the network")
	}
	st2, err := state.Load(env.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Part(1).Status != state.Pending || st2.Part(2).Status != state.Attention {
		t.Errorf("dry run mutated persisted state: %+v %+v", st2.Part(1), st2.Part(2))
	}
}

func TestSnapshot(t *testing.T) {
	env := newTestEnv(t, map[int][]byte{}, 3)
	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	env.st.Update(func() {
		env.st.Part(3).Status = state.Done
		env.st.Part(3).ActualSize = 1000
		env.st.Part(1).ExpectedSize = 4000
		env.st.Part(2).ExpectedSize = 2000
	})

	p1 := f.trackPart(env.st.Part(1), 500, 4000)
	f.trackPart(env.st.Part(2), 0, 2000)
	p1.cur.Add(100)
	f.runBytes.Add(600)

	s := f.Snapshot()
	if len(s.Active) != 2 || s.Active[0].Num != 1 || s.Active[1].Num != 2 {
		t.Fatalf("Active = %+v, want parts 1,2 sorted", s.Active)
	}
	if s.Active[0].Filename != "x-001.tgz" || s.Active[0].Cur != 600 || s.Active[0].Expected != 4000 {
		t.Errorf("Active[0] = %+v", s.Active[0])
	}
	if s.Done != 1 || s.Total != 3 || s.DoneBytes != 1000 || s.RunBytes != 600 {
		t.Errorf("counts = %+v", s)
	}
	// estimate: 4000 + 2000 pending, minus 600 already on disk in actives
	if s.Remaining != 5400 {
		t.Errorf("Remaining = %d, want 5400", s.Remaining)
	}

	// concurrent tracking must be race-free (run with -race)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			f.trackPart(env.st.Part(2), 0, 2000)
			f.untrackPart(2)
		}
	}()
	for range 200 {
		_ = f.Snapshot()
	}
	<-done
}

func TestETAString(t *testing.T) {
	if _, ok := ETAString(0, 100); ok {
		t.Error("zero remaining should have no ETA")
	}
	if _, ok := ETAString(100, 0); ok {
		t.Error("zero speed should have no ETA")
	}
	if got, ok := ETAString(360000, 100); !ok || got != "1h0m0s" {
		t.Errorf("ETAString = %q, %v", got, ok)
	}
	if got, ok := ETAString(1000, 100); !ok || got != "10s" {
		t.Errorf("short ETA = %q, %v (should round to seconds, not minutes)", got, ok)
	}
	if got, ok := ETAString(1<<40, 100); !ok || got != ">999 days" {
		t.Errorf("degenerate ETA = %q, %v (want the >999 days cap)", got, ok)
	}
	if got, ok := ETAString(160*86400, 1); !ok || got != "160 days" {
		t.Errorf("100+ day ETA = %q, %v (want whole days)", got, ok)
	}
	if got, ok := ETAString(55*86400, 10); !ok || got != "5.5 days" {
		t.Errorf("multi-day ETA = %q, %v", got, ok)
	}
	// The live footer reserves exactly 9 cells; ETAString must never exceed
	// it for any input (CodeRabbit finding on the stable-bar-width PR).
	for _, c := range []struct {
		remaining int64
		speed     float64
	}{
		{1, 1}, {90, 1}, {3600, 1}, {172799, 1}, {172801, 1},
		{86400 * 100, 1}, {86400 * 999, 1}, {86400 * 1000, 1}, {1 << 62, 0.001},
	} {
		if got, ok := ETAString(c.remaining, c.speed); ok && utf8.RuneCountInString(got) > 9 {
			t.Errorf("ETAString(%d, %g) = %q: %d cells, exceeds the 9-cell contract",
				c.remaining, c.speed, got, utf8.RuneCountInString(got))
		}
	}
}

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in           string
		start, total int64
		ok           bool
	}{
		{"bytes 100-299/300", 100, 300, true},
		{"bytes */300", -1, 300, true},
		{"bytes 0-99/*", 0, -1, true},
		{"", 0, 0, false},
		{"items 1-2/3", 0, 0, false},
		{"bytes garbage", 0, 0, false},
	}
	for _, c := range cases {
		start, total, ok := parseContentRange(c.in)
		if ok != c.ok || (ok && (start != c.start || total != c.total)) {
			t.Errorf("parseContentRange(%q) = %d,%d,%v want %d,%d,%v", c.in, start, total, ok, c.start, c.total, c.ok)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("120"); d != 2*time.Minute {
		t.Errorf("seconds form = %v", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Errorf("empty = %v", d)
	}
	future := time.Now().Add(10 * time.Minute).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(future); d < 9*time.Minute || d > 11*time.Minute {
		t.Errorf("http-date form = %v", d)
	}
}
