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
	"sync"
	"testing"
	"time"

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
	s.served[num]++
	s.servedOK++
	if s.flipAfter > 0 && s.servedOK >= s.flipAfter {
		s.validCookie = s.flipTo
		s.flipAfter = 0
	}
	s.mu.Unlock()

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
		st.Parts = append(st.Parts, &state.Part{Num: n, Filename: tmpl.Filename(n), Status: state.Pending})
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

func TestHappyPath(t *testing.T) {
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
			t.Errorf("part %d: %+v", n, p)
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
	// reloaded state must agree (audit trail persisted)
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
	// session dies after the first served part
	env.fake.flipAfter = 1
	env.fake.flipTo = "SID=fresh"

	var refreshes int
	opts := env.options(t)
	opts.Workers = 1 // deterministic ordering
	opts.RefreshAuth = func(reason string) (string, error) {
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
	// the refreshed cookie must be persisted for future runs
	c, err := state.LoadCookie(env.dir)
	if err != nil {
		t.Fatal(err)
	}
	if c != "SID=fresh" {
		t.Errorf("persisted cookie = %q", c)
	}
	// auth failures must not count as served downloads
	if got := env.st.Part(2).Attempts + env.st.Part(3).Attempts; got != 2 {
		t.Errorf("parts 2+3 served %d times total, want 2", got)
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
	got, err := os.ReadFile(filepath.Join(env.dir, env.st.Part(1).Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("resumed file mismatch: %d vs %d bytes", len(got), len(full))
	}
	if env.st.Part(1).ExpectedSize != int64(len(full)) {
		t.Errorf("ExpectedSize = %d, want %d (from Content-Range)", env.st.Part(1).ExpectedSize, len(full))
	}
}

func TestCorruptArchiveCaughtByFullVerify(t *testing.T) {
	// valid gzip magic, garbage stream: passes quick sniff, must fail full verify
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
	// the file is kept for inspection
	if _, err := os.Stat(filepath.Join(env.dir, p.Filename)); err != nil {
		t.Error("corrupt file was deleted; it should be kept")
	}
}

func TestUnknownHTMLPageIsFatal(t *testing.T) {
	parts := map[int][]byte{1: gzipPayload(t, "one ", 1000)}
	env := newTestEnv(t, parts, 1)
	env.fake.htmlPage[1] = "<html><body>Download quota exceeded for this file</body></html>"

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	_, err := f.Run(context.Background())
	if err == nil {
		t.Fatal("expected fatal error for unknown HTML page")
	}
	if AuthNeeded(err) {
		t.Error("quota page misclassified as auth expiry")
	}
	diag := filepath.Join(env.dir, env.st.Part(1).Filename+".error.html")
	if _, serr := os.Stat(diag); serr != nil {
		t.Errorf("diagnostic page not saved at %s", diag)
	}
	// an HTML error page is not a served download
	if got := env.st.Part(1).Attempts; got != 0 {
		t.Errorf("HTML page counted as %d served download(s), want 0", got)
	}
}

func TestMislabeledHTMLPageIsFatalAndCounted(t *testing.T) {
	parts := map[int][]byte{1: gzipPayload(t, "one ", 1000)}
	env := newTestEnv(t, parts, 1)
	// Google claims binary but sends an error page: fatal, and counted as
	// served since Google may have counted it too.
	env.fake.rawPage[1] = []byte("<html><body>Something went wrong</body></html>")

	f := New(env.tmpl, env.st, env.cookie, env.options(t))
	_, err := f.Run(context.Background())
	if err == nil {
		t.Fatal("expected fatal error for mislabeled HTML page")
	}
	if AuthNeeded(err) {
		t.Error("misclassified as auth expiry")
	}
	if got := env.st.Part(1).Attempts; got != 1 {
		t.Errorf("Attempts = %d, want 1 (count conservatively when Google claims binary)", got)
	}
}

func TestMissingPartIsFatal(t *testing.T) {
	parts := map[int][]byte{1: gzipPayload(t, "one ", 1000)} // part 2 doesn't exist
	env := newTestEnv(t, parts, 2)

	opts := env.options(t)
	opts.Workers = 1
	f := New(env.tmpl, env.st, env.cookie, opts)
	_, err := f.Run(context.Background())
	if err == nil {
		t.Fatal("expected fatal error for 404")
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
	if err := sniff.VerifyFile(filepath.Join(env.dir, env.st.Part(1).Filename)); err != nil {
		t.Error(err)
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
	if env.st.Part(2).Status != state.Done {
		t.Errorf("part 2 = %+v", env.st.Part(2))
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	parts := map[int][]byte{1: gzipPayload(t, "one ", 1000)}
	env := newTestEnv(t, parts, 1)
	opts := env.options(t)
	opts.DryRun = true
	f := New(env.tmpl, env.st, env.cookie, opts)

	if _, err := f.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.fake.requests[1] != 0 {
		t.Error("dry run hit the network")
	}
	if env.st.Part(1).Status != state.Pending {
		t.Error("dry run changed part status")
	}
}
