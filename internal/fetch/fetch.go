// SPDX-License-Identifier: GPL-3.0-or-later

// Package fetch downloads Takeout archive parts: a small pool of workers,
// resume via Range requests, a stall watchdog, content classification on
// every response, and a global halt the moment authentication looks dead — so
// a dead session never burns download attempts across the whole queue.
package fetch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jm2/carryout/internal/sniff"
	"github.com/jm2/carryout/internal/state"
	"github.com/jm2/carryout/internal/takeout"
)

// sentinel and classified errors
var (
	errAuthRedirect = errors.New("redirected to a Google sign-in page")
	errAuthExpired  = errors.New("session expired")
	errNotFound     = errors.New("HTTP 404")
	errStalled      = errors.New("transfer stalled")
	errGaveUp       = errors.New("gave up on part") // non-fatal: part flagged, run continues
)

// fatalError stops the whole run (no new attempts; in-flight ones finish).
type fatalError struct {
	msg        string
	authNeeded bool
}

func (e *fatalError) Error() string { return e.msg }

// AuthNeeded reports whether err means the run stopped because the session
// expired and no interactive refresh was possible.
func AuthNeeded(err error) bool {
	var fe *fatalError
	return errors.As(err, &fe) && fe.authNeeded
}

type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// partBrokenError flags one part for attention without stopping the run.
type partBrokenError struct{ msg string }

func (e *partBrokenError) Error() string { return e.msg }

// throttledError carries the server's Retry-After hint, if any.
type throttledError struct{ after time.Duration }

func (e *throttledError) Error() string { return "throttled (HTTP 429)" }

type Options struct {
	Dir          string
	Workers      int
	MaxTries     int // per part per run, including the first try
	FullVerify   bool
	Only         map[int]bool // nil = all parts
	StallTimeout time.Duration
	RetryDelay   time.Duration
	Cooldown     time.Duration // minimum wait after HTTP 429
	DryRun       bool
	// RefreshAuth is called (serialized; all new requests are held) when the
	// session looks expired. It returns a fresh Cookie header value. It must
	// honor ctx so Ctrl-C can abort a pending prompt.
	RefreshAuth func(ctx context.Context, reason string) (string, error)
	Logf        func(format string, args ...any)
	// ProgressLogf receives the periodic (ephemeral) progress ticks, so the
	// CLI can drop them while an interactive prompt owns the screen. Defaults
	// to Logf.
	ProgressLogf func(format string, args ...any)
	// AttemptWarnAt logs a warning when a part's served-download count
	// reaches this value (Takeout is believed to cap downloads per part).
	AttemptWarnAt int
}

type Summary struct {
	Pending, Downloaded, Done, Attention, Corrupt int
	BytesThisRun                                  int64
	Elapsed                                       time.Duration
}

func (s Summary) Complete() bool {
	return s.Pending == 0 && s.Downloaded == 0 && s.Attention == 0 && s.Corrupt == 0
}

type partProgress struct {
	cur      atomic.Int64
	expected int64
}

type Fetcher struct {
	opt    Options
	tmpl   *takeout.Template
	st     *state.State
	client *http.Client

	authMu  sync.Mutex
	authGen int
	cookie  string

	cdMu          sync.Mutex
	cooldownUntil time.Time

	notFoundStreak atomic.Int32
	htmlPages      atomic.Int32

	progMu   sync.Mutex
	progress map[int]*partProgress
	runBytes atomic.Int64
}

func New(tmpl *takeout.Template, st *state.State, cookie string, opt Options) *Fetcher {
	if opt.Workers <= 0 {
		opt.Workers = 3
	}
	if opt.MaxTries <= 0 {
		opt.MaxTries = 2
	}
	if opt.StallTimeout <= 0 {
		opt.StallTimeout = 2 * time.Minute
	}
	if opt.RetryDelay <= 0 {
		opt.RetryDelay = 30 * time.Second
	}
	if opt.Cooldown <= 0 {
		opt.Cooldown = 5 * time.Minute
	}
	if opt.AttemptWarnAt <= 0 {
		opt.AttemptWarnAt = 4
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	if opt.ProgressLogf == nil {
		opt.ProgressLogf = opt.Logf
	}
	f := &Fetcher{
		opt:      opt,
		tmpl:     tmpl,
		st:       st,
		cookie:   cookie,
		progress: make(map[int]*partProgress),
	}
	f.client = f.newClient()
	return f
}

// googleOwned reports whether a redirect target is a Google-operated host
// that should keep receiving our session cookies.
func googleOwned(host string) bool {
	for _, d := range []string{"google.com", "googleusercontent.com", "googleapis.com", "gstatic.com"} {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func (f *Fetcher) newClient() *http.Client {
	// HTTP/1.1 only: with HTTP/2 Go multiplexes every worker onto one TCP
	// connection, coupling all transfers to a single congestion window and
	// per-stream flow-control caps. Bulk parallel downloads want independent
	// connections.
	protos := new(http.Protocols)
	protos.SetHTTP1(true)
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DisableCompression:    true, // archives are already compressed; keep Content-Length honest
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		Protocols:             protos,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			host := strings.ToLower(req.URL.Hostname())
			if host == "accounts.google.com" || strings.HasSuffix(host, ".accounts.google.com") ||
				strings.Contains(strings.ToLower(req.URL.Path), "servicelogin") {
				return errAuthRedirect
			}
			// Go drops Cookie on cross-host redirects; keep it for Google hosts.
			if googleOwned(host) {
				_, cookie := f.snapshotAuth()
				req.Header.Set("Cookie", cookie)
			}
			return nil
		},
	}
}

func (f *Fetcher) logf(format string, args ...any) { f.opt.Logf(format, args...) }

func (f *Fetcher) snapshotAuth() (int, string) {
	f.authMu.Lock()
	defer f.authMu.Unlock()
	return f.authGen, f.cookie
}

func (f *Fetcher) authChanged(gen int) bool {
	f.authMu.Lock()
	defer f.authMu.Unlock()
	return gen != f.authGen
}

// refreshAuth swaps in fresh cookies. Serialized on authMu: while one worker
// prompts, every other worker's next attempt blocks in snapshotAuth, so no
// request is sent with cookies already known to be dead.
func (f *Fetcher) refreshAuth(ctx context.Context, seenGen int, reason string) error {
	f.authMu.Lock()
	defer f.authMu.Unlock()
	if seenGen != f.authGen {
		return nil // another worker already refreshed
	}
	// reason is often already "session expired (...)"; don't stutter.
	if !strings.HasPrefix(reason, "session expired") {
		reason = "session expired (" + reason + ")"
	}
	if f.opt.RefreshAuth == nil {
		return &fatalError{
			msg:        reason + " — run `carryout auth` with a fresh capture, then `carryout get` to resume",
			authNeeded: true,
		}
	}
	cookie, err := f.opt.RefreshAuth(ctx, reason)
	if err != nil {
		return &fatalError{msg: reason + ": " + err.Error(), authNeeded: true}
	}
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return &fatalError{msg: "session expired and no replacement cookies were provided", authNeeded: true}
	}
	if err := state.SaveCookie(f.opt.Dir, cookie); err != nil {
		return &fatalError{msg: "saving refreshed cookies: " + err.Error()}
	}
	f.cookie = cookie
	f.authGen++
	f.logf("session refreshed; resuming queue")
	return nil
}

func (f *Fetcher) waitCooldown(ctx context.Context) {
	f.cdMu.Lock()
	until := f.cooldownUntil
	f.cdMu.Unlock()
	if d := time.Until(until); d > 0 {
		sleepCtx(ctx, d)
	}
}

func (f *Fetcher) setCooldown(d time.Duration) {
	f.cdMu.Lock()
	defer f.cdMu.Unlock()
	if u := time.Now().Add(d); u.After(f.cooldownUntil) {
		f.cooldownUntil = u
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// update mutates state under its lock and persists it; persistence failures
// are loud but non-fatal (the in-memory state stays correct).
func (f *Fetcher) update(fn func()) {
	if err := f.st.Update(fn); err != nil {
		f.logf("WARNING: could not save %s: %v", state.FileName, err)
	}
}

// plan decides what this run will do. In dry-run mode it is strictly
// read-only: nothing is mutated and nothing is saved.
func (f *Fetcher) plan(dry bool) (download, verify []*state.Part, notes []string) {
	changed := false
	f.st.View(func() {
		for _, p := range f.st.Parts {
			if f.opt.Only != nil && !f.opt.Only[p.Num] {
				continue
			}
			final := filepath.Join(f.opt.Dir, p.Filename)
			switch p.Status {
			case state.Done:
				if _, err := os.Stat(final); err != nil {
					notes = append(notes, fmt.Sprintf("part %03d is marked done but %s is missing (moved elsewhere?) — leaving it done", p.Num, p.Filename))
				}
			case state.Downloaded:
				verify = append(verify, p)
			case state.Corrupt:
				notes = append(notes, fmt.Sprintf("part %03d failed verification earlier — re-download costs an attempt, requeue explicitly with `carryout get -redo %d`", p.Num, p.Num))
			case state.Pending, state.Attention:
				if fi, err := os.Stat(final); err == nil {
					kind, kerr := sniff.FileKind(final)
					sizeOK := p.ExpectedSize == 0 || fi.Size() == p.ExpectedSize
					if kerr == nil && kind != "" && sizeOK {
						// Adopt a complete-looking stray file (e.g. a part the
						// browser already downloaded) — but never trust it
						// without full verification.
						if dry {
							notes = append(notes, fmt.Sprintf("part %03d: would adopt existing file %s (%s) and verify it", p.Num, p.Filename, HumanBytes(fi.Size())))
							continue
						}
						p.Status = state.Downloaded
						p.ActualSize = fi.Size()
						p.LastError = ""
						changed = true
						notes = append(notes, fmt.Sprintf("part %03d: adopted existing file %s (%s); it will be fully verified", p.Num, p.Filename, HumanBytes(fi.Size())))
						verify = append(verify, p)
						continue
					}
					notes = append(notes, fmt.Sprintf("part %03d: existing file %s failed adoption checks (size or magic bytes); it will be re-downloaded", p.Num, p.Filename))
				}
				if p.Status == state.Attention && !dry {
					p.Status = state.Pending
					changed = true
				}
				download = append(download, p)
			}
		}
	})
	if changed {
		if err := f.st.Save(); err != nil {
			f.logf("WARNING: could not save %s: %v", state.FileName, err)
		}
	}
	sort.Slice(download, func(i, j int) bool { return download[i].Num < download[j].Num })
	return download, verify, notes
}

// Run executes the plan and blocks until done, interrupted, or a fatal error.
func (f *Fetcher) Run(ctx context.Context) (Summary, error) {
	start := time.Now()
	download, verify, notes := f.plan(f.opt.DryRun)
	for _, n := range notes {
		f.logf("%s", n)
	}

	if f.opt.DryRun {
		for _, p := range download {
			f.logf("would download part %03d: GET %s", p.Num, f.tmpl.BuildURL(p.Filename, p.Index))
		}
		for _, p := range verify {
			f.logf("would verify part %03d: %s", p.Num, filepath.Join(f.opt.Dir, p.Filename))
		}
		if len(download)+len(verify) == 0 {
			f.logf("nothing to do")
		}
		return f.summary(start), nil
	}

	if len(download)+len(verify) == 0 {
		f.logf("nothing to do — all selected parts are complete")
		return f.summary(start), nil
	}
	f.logf("this run: %d part(s) to download, %d to verify, %d worker(s)", len(download), len(verify), f.opt.Workers)

	// Two levels of cancellation: qctx stops the queue (fatal errors — no
	// NEW attempts start, but transfers Google is already serving stream to
	// completion, since those bytes are likely already counted against the
	// per-part limit). The parent ctx (Ctrl-C) hard-aborts everything.
	qctx, cancelQueue := context.WithCancelCause(ctx)
	defer cancelQueue(nil)

	verifyQ := make(chan *state.Part, 2*len(f.st.Parts)+1)
	for _, p := range verify {
		verifyQ <- p
	}

	downloadQ := make(chan *state.Part)
	go func() {
		defer close(downloadQ)
		for _, p := range download {
			select {
			case downloadQ <- p:
			case <-qctx.Done():
				return
			}
		}
	}()

	// progress reporter
	repCtx, repCancel := context.WithCancel(ctx)
	defer repCancel()
	repDone := make(chan struct{})
	go f.report(repCtx, repDone)

	var wg sync.WaitGroup
	for range f.opt.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range downloadQ {
				if qctx.Err() != nil {
					return
				}
				err := f.fetchPart(qctx, ctx, p)
				switch {
				case err == nil:
					needsVerify := false
					f.st.View(func() { needsVerify = p.Status == state.Downloaded })
					if needsVerify {
						verifyQ <- p
					}
				case errors.Is(err, errGaveUp):
					// flagged for attention; keep going with other parts
				case errors.Is(err, context.Canceled):
					return
				default:
					cancelQueue(err)
					return
				}
			}
		}()
	}

	// Two verifiers: with inline verification handling clean downloads, this
	// queue only sees resumed/adopted/zip parts, but a resumed-heavy run
	// shouldn't serialize behind one decompressor.
	var vwg sync.WaitGroup
	for range 2 {
		vwg.Add(1)
		go func() {
			defer vwg.Done()
			for p := range verifyQ {
				// verification is local and free; only a hard abort skips it
				if ctx.Err() != nil {
					continue // drain; carryout verify can finish later
				}
				VerifyPartFile(ctx, f.st, f.opt.Dir, p, f.opt.Logf)
			}
		}()
	}

	wg.Wait()
	close(verifyQ)
	vwg.Wait()
	repCancel()
	<-repDone

	sum := f.summary(start)
	if cause := context.Cause(qctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return sum, cause
	}
	if err := ctx.Err(); err != nil {
		return sum, fmt.Errorf("interrupted — run `carryout get` again to resume")
	}
	return sum, nil
}

func (f *Fetcher) summary(start time.Time) Summary {
	pending, downloaded, done, attention, corrupt, _ := f.st.Counts()
	return Summary{
		Pending: pending, Downloaded: downloaded, Done: done,
		Attention: attention, Corrupt: corrupt,
		BytesThisRun: f.runBytes.Load(),
		Elapsed:      time.Since(start).Round(time.Second),
	}
}

// fetchPart drives one part to completion: retries, auth refresh, throttle
// cooldowns. qctx gates starting new attempts (fatal errors elsewhere stop
// the queue); hctx aborts in-flight transfers (Ctrl-C only). Returns nil,
// errGaveUp, context.Canceled, or a fatal error.
func (f *Fetcher) fetchPart(qctx, hctx context.Context, p *state.Part) error {
	tries := 0
	throttleWaits := 0
	firstAuthGen := -1
	for {
		if qctx.Err() != nil {
			return context.Canceled
		}
		f.waitCooldown(qctx)
		if qctx.Err() != nil {
			return context.Canceled // a fatal halt can land mid-cooldown
		}

		gen, cookie := f.snapshotAuth()
		tries++
		err := f.attempt(hctx, p, cookie)
		if err == nil {
			return nil
		}

		var throttled *throttledError
		var broken *partBrokenError
		switch {
		case errors.Is(err, context.Canceled) || hctx.Err() != nil:
			return context.Canceled

		case errors.Is(err, errAuthExpired):
			tries-- // auth failures don't count against the part
			if firstAuthGen >= 0 && gen > firstAuthGen {
				// This attempt already ran with cookies newer than the first
				// failure and still got rejected: fresh auth doesn't help, so
				// don't loop the prompt — likely the per-part download cap or
				// an expired link.
				f.update(func() {
					p.Status = state.Attention
					p.LastError = "still rejected with fresh cookies — possible per-part download cap or expired link; check the Takeout page"
				})
				f.logf("part %03d: still rejected after a cookie refresh — flagged for attention; continuing with other parts", p.Num)
				return errGaveUp
			}
			if firstAuthGen < 0 {
				firstAuthGen = gen
			}
			f.logf("part %03d: %v — holding the queue for fresh cookies", p.Num, err)
			if rerr := f.refreshAuth(qctx, gen, err.Error()); rerr != nil {
				return rerr
			}

		case errors.As(err, &throttled):
			tries--
			throttleWaits++
			if throttleWaits > 4 {
				return &fatalError{msg: fmt.Sprintf("part %03d: still throttled after %d cooldowns — stopping so we don't hammer Google; try again later", p.Num, throttleWaits)}
			}
			cool := f.opt.Cooldown
			if throttled.after > cool {
				cool = throttled.after
			}
			if cool > 30*time.Minute {
				return &fatalError{msg: fmt.Sprintf("Google asked for a %s back-off (Retry-After) — stopping; try again later", throttled.after)}
			}
			f.logf("part %03d: HTTP 429 — cooling down for %s", p.Num, cool)
			f.setCooldown(cool)

		case errors.Is(err, errNotFound):
			streak := f.notFoundStreak.Add(1)
			f.update(func() {
				p.Status = state.Attention
				p.LastError = "HTTP 404 — no file at the constructed URL"
			})
			if streak >= 3 {
				return &fatalError{msg: "three parts in a row returned 404 — the export may have expired or the URL mapping is wrong; stopping before more attempts are spent. Re-check the Takeout page, then `carryout init -force` with a fresh capture and file list"}
			}
			f.logf("part %03d: HTTP 404 — flagged for attention; continuing (404 streak %d/3)", p.Num, streak)
			return errGaveUp

		case errors.As(err, &broken):
			f.update(func() {
				p.Status = state.Attention
				p.LastError = broken.msg
			})
			f.logf("part %03d: %s — flagged for attention; continuing", p.Num, broken.msg)
			return errGaveUp

		default:
			var re *retryableError
			if !errors.As(err, &re) {
				f.update(func() { p.LastError = err.Error() })
				return err // fatal
			}
			if f.authChanged(gen) {
				tries-- // collateral damage of a session death; retry free
				continue
			}
			f.update(func() { p.LastError = err.Error() })
			if tries >= f.opt.MaxTries {
				f.update(func() { p.Status = state.Attention })
				f.logf("part %03d: giving up after %d tries (%v) — flagged for attention; a later `carryout get` will retry it", p.Num, tries, err)
				return errGaveUp
			}
			f.logf("part %03d: try %d/%d failed (%v); retrying in %s", p.Num, tries, f.opt.MaxTries, err, f.opt.RetryDelay)
			sleepCtx(qctx, f.opt.RetryDelay)
		}
	}
}

// classifyHTMLPage decides what an HTML body means: session death pauses the
// queue; anything else flags the part and, if it keeps happening, stops the
// run as systemic.
func (f *Fetcher) classifyHTMLPage(p *state.Part, final string, body []byte) error {
	if sniff.LooksLikeSignIn(body) {
		return errAuthExpired
	}
	diag := final + ".error.html"
	_ = os.WriteFile(diag, body, 0644)
	if n := f.htmlPages.Add(1); n >= 3 {
		return &fatalError{msg: fmt.Sprintf("multiple parts are getting HTML pages instead of archives (latest saved to %s) — something systemic (expired export? blocked session?); stopping. Open the page in a browser to see what Google says", diag)}
	}
	return &partBrokenError{msg: fmt.Sprintf("Google sent an HTML page instead of the archive (saved to %s)", diag)}
}

// parseContentRange parses "bytes start-end/total". start or total are -1
// when given as "*". ok is false if the header is absent or malformed.
func parseContentRange(h string) (start, total int64, ok bool) {
	h = strings.TrimSpace(h)
	rest, found := strings.CutPrefix(h, "bytes ")
	if !found {
		return 0, 0, false
	}
	rangePart, totalStr, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	start, total = -1, -1
	if totalStr != "*" {
		t, err := strconv.ParseInt(strings.TrimSpace(totalStr), 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total = t
	}
	if rangePart != "*" {
		startStr, _, _ := strings.Cut(rangePart, "-")
		s, err := strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
		if err != nil {
			return 0, 0, false
		}
		start = s
	}
	return start, total, true
}

// attempt performs one HTTP download attempt for a part.
func (f *Fetcher) attempt(ctx context.Context, p *state.Part, cookie string) error {
	actx, acancel := context.WithCancelCause(ctx)
	defer acancel(nil)

	final := filepath.Join(f.opt.Dir, p.Filename)
	partPath := final + ".part"

	var offset int64
	if fi, err := os.Stat(partPath); err == nil {
		offset = fi.Size()
	}

	req, err := http.NewRequestWithContext(actx, http.MethodGet, f.tmpl.BuildURL(p.Filename, p.Index), nil)
	if err != nil {
		return &fatalError{msg: fmt.Sprintf("part %03d: building request: %v", p.Num, err)}
	}
	f.st.View(func() {
		for k, v := range f.st.Headers {
			req.Header.Set(k, v)
		}
	})
	req.Header.Set("Cookie", cookie)
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}

	resp, err := f.client.Do(req)
	if err != nil {
		if errors.Is(err, errAuthRedirect) {
			return errAuthExpired
		}
		return &retryableError{fmt.Errorf("request failed: %w", err)}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
		// proceed
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d)", errAuthExpired, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return &throttledError{after: parseRetryAfter(resp.Header.Get("Retry-After"))}
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		if offset > 0 {
			// Usually means the .part is already byte-complete (a crash hit
			// between the last byte and the rename). Confirm via the total
			// and finalize instead of re-downloading 50 GiB.
			if _, total, ok := parseContentRange(resp.Header.Get("Content-Range")); ok && total == offset {
				f.logf("part %03d: partial file is already complete (%s); finalizing without another download", p.Num, HumanBytes(offset))
				return f.finalizePart(p, partPath, final, offset, total, false, true)
			}
			_ = os.Remove(partPath)
			return &retryableError{errors.New("server rejected our resume offset (416); discarded the partial file to restart cleanly")}
		}
		return &retryableError{errors.New("HTTP 416 without a Range request; retrying")}
	case resp.StatusCode >= 500:
		return &retryableError{fmt.Errorf("HTTP %s", resp.Status)}
	default:
		return &fatalError{msg: fmt.Sprintf("part %03d: unexpected HTTP %s", p.Num, resp.Status)}
	}

	br := bufio.NewReaderSize(resp.Body, 512<<10)

	resuming := offset > 0 && resp.StatusCode == http.StatusPartialContent
	if offset > 0 && resp.StatusCode == http.StatusOK {
		f.logf("part %03d: server ignored the resume request; restarting from the beginning", p.Num)
		offset = 0
	}

	var expected int64 = -1
	if resuming {
		start, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		switch {
		case !ok:
			return &retryableError{errors.New("206 response without a parseable Content-Range")}
		case start == offset:
			expected = total
		case start == 0 && total > 0:
			// Server restarted from scratch despite saying 206: treat it as a
			// fresh full body rather than appending it after our offset.
			f.logf("part %03d: server restarted the transfer from byte 0; discarding the resume offset", p.Num)
			resuming = false
			offset = 0
			expected = total
		default:
			return &retryableError{fmt.Errorf("Content-Range starts at %d but we asked for %d — refusing to append", start, offset)}
		}
	} else if resp.ContentLength > 0 {
		expected = resp.ContentLength
	}

	// An error page usually announces itself in the Content-Type header.
	if sniff.IsHTMLContentType(resp.Header.Get("Content-Type")) {
		body, _ := io.ReadAll(io.LimitReader(br, 256<<10))
		return f.classifyHTMLPage(p, final, body)
	}

	// Google is serving binary content: count it now, before anything (a
	// network blip, another worker's fatal abort) can interrupt the transfer.
	// An interrupted serve plausibly still counts against Takeout's per-part
	// download limit, so the on-disk counter must never lag Google's.
	f.recordServed(p, resuming)
	f.notFoundStreak.Store(0)

	// The wrong-file tripwire: if Google names the file it is serving and
	// that isn't the file we asked for, the URL mapping is wrong — stop
	// before recording anything under a false name.
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if fn := params["filename"]; fn != "" && fn != p.Filename {
				return &fatalError{msg: fmt.Sprintf("part %03d: asked for %s but Google is serving %q — the filename↔index mapping is wrong; stopping. Re-run `carryout init -force` and paste the export file list", p.Num, p.Filename, fn)}
			}
		}
	}

	kind := ""
	if !resuming {
		head, perr := br.Peek(512)
		if len(head) == 0 {
			return &retryableError{fmt.Errorf("connection dropped before any body arrived (%v)", perr)}
		}
		// Belt and braces: some error pages come mislabeled as binary.
		if sniff.IsHTML("", head) {
			body, _ := io.ReadAll(io.LimitReader(br, 256<<10))
			return f.classifyHTMLPage(p, final, body)
		}
		kind = sniff.Kind(head)
		if kind == "" {
			if perr != nil {
				return &retryableError{fmt.Errorf("connection dropped mid-header (%v)", perr)}
			}
			return &fatalError{msg: fmt.Sprintf("part %03d: response is not a recognized archive (starts with % x) — refusing to write it", p.Num, head[:min(8, len(head))])}
		}
	}

	if expected > 0 {
		f.update(func() { p.ExpectedSize = expected })
	}

	flags := os.O_CREATE | os.O_WRONLY
	if resuming {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(partPath, flags, 0644)
	if err != nil {
		return &fatalError{msg: fmt.Sprintf("part %03d: opening %s: %v", p.Num, partPath, err)}
	}
	closed := false
	defer func() {
		if !closed {
			file.Close()
		}
	}()

	// Inline verification: for a fresh gzip body, decompress alongside the
	// write so a clean download is fully verified with zero extra disk reads.
	// Quick mode skips it unless the length is unknown (then it's the only
	// truncation detector we have).
	var verifier *sniff.StreamVerifier
	if kind == "gzip" && (f.opt.FullVerify || expected <= 0) {
		verifier = sniff.NewGzipStreamVerifier()
	}
	var dst io.Writer = file
	if verifier != nil {
		dst = io.MultiWriter(file, verifier)
	}

	prog := f.trackPart(p.Num, offset, expected)
	defer f.untrackPart(p.Num)

	watchStop := make(chan struct{})
	defer close(watchStop)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		last := prog.cur.Load()
		lastChange := time.Now()
		for {
			select {
			case <-watchStop:
				return
			case <-actx.Done():
				return
			case <-t.C:
				if cur := prog.cur.Load(); cur != last {
					last, lastChange = cur, time.Now()
				} else if time.Since(lastChange) > f.opt.StallTimeout {
					acancel(errStalled)
					return
				}
			}
		}
	}()

	copyBuf := make([]byte, 1<<20)
	n, cerr := io.CopyBuffer(dst, &progressReader{r: br, f: f, prog: prog}, copyBuf)
	if serr := file.Sync(); cerr == nil && serr != nil {
		cerr = serr
	}
	closed = true
	if clErr := file.Close(); cerr == nil && clErr != nil {
		cerr = clErr
	}
	if cerr != nil {
		if verifier != nil {
			verifier.Abort()
		}
		if errors.Is(context.Cause(actx), errStalled) {
			return &retryableError{fmt.Errorf("stalled: no data for %s", f.opt.StallTimeout)}
		}
		if ctx.Err() != nil {
			return context.Canceled
		}
		return &retryableError{fmt.Errorf("transfer failed after %s: %w", HumanBytes(n), cerr)}
	}

	size := offset + n
	if expected > 0 && size != expected {
		if verifier != nil {
			verifier.Abort()
		}
		if size < expected {
			return &retryableError{fmt.Errorf("short download: got %s of %s (will resume)", HumanBytes(size), HumanBytes(expected))}
		}
		return &fatalError{msg: fmt.Sprintf("part %03d: downloaded %s but expected %s — refusing to trust it", p.Num, HumanBytes(size), HumanBytes(expected))}
	}

	inlineOK := false
	if verifier != nil {
		if verr := verifier.Finish(); verr != nil {
			// The bytes are what Google sent; keep them for inspection but
			// record the part as corrupt.
			_ = os.Remove(final)
			_ = os.Rename(partPath, final)
			f.update(func() {
				p.ActualSize = size
				p.Status = state.Corrupt
				p.LastError = "inline verification failed: " + verr.Error()
			})
			f.logf("part %03d: VERIFICATION FAILED during download: %v — file kept at %s; requeue with `carryout get -redo %d`", p.Num, verr, final, p.Num)
			return nil
		}
		inlineOK = true
	}

	// A resumed download stitched two transfers together; never trust it on
	// size alone, even in quick mode.
	return f.finalizePart(p, partPath, final, size, expected, inlineOK, resuming)
}

// finalizePart moves a byte-complete .part into place and records its status:
// Done when verified (inline) or when quick mode's checks suffice, Downloaded
// (= verification pending) otherwise. requireVerify forces the Downloaded
// path for bytes we didn't watch arrive end-to-end.
func (f *Fetcher) finalizePart(p *state.Part, partPath, final string, size, expected int64, inlineVerified, requireVerify bool) error {
	// Quick check on the assembled file (catches a garbage head from an
	// earlier interrupted attempt that we then resumed on top of).
	if kind, kerr := sniff.FileKind(partPath); kerr != nil || kind == "" {
		_ = os.Remove(partPath)
		return &retryableError{errors.New("assembled file is not a valid archive; discarded it to restart cleanly")}
	}

	// Windows can't always rename over an existing file; clear leftovers.
	_ = os.Remove(final)
	if err := os.Rename(partPath, final); err != nil {
		return &fatalError{msg: fmt.Sprintf("part %03d: renaming into place: %v", p.Num, err)}
	}

	now := time.Now().UTC()
	status := state.Downloaded
	note := " (verification queued)"
	switch {
	case inlineVerified:
		status = state.Done
		note = " (verified inline)"
	case requireVerify:
		// keep Downloaded
	case !f.opt.FullVerify && expected > 0 && size == expected:
		status = state.Done
		note = " (size matches; quick mode)"
	}
	f.update(func() {
		p.ActualSize = size
		if expected > 0 {
			p.ExpectedSize = expected
		}
		p.CompletedAt = now
		p.LastError = ""
		p.Status = status
		if inlineVerified {
			p.VerifiedAt = now
		}
	})
	f.logf("part %03d: downloaded %s%s", p.Num, HumanBytes(size), note)
	return nil
}

func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func (f *Fetcher) recordServed(p *state.Part, resuming bool) {
	var attempts int
	f.update(func() {
		p.Attempts++
		attempts = p.Attempts
	})
	kind := "download"
	if resuming {
		kind = "resume"
	}
	f.logf("part %03d: %s started (served download #%d for this part)", p.Num, kind, attempts)
	if attempts >= f.opt.AttemptWarnAt {
		f.logf("part %03d: WARNING — Google has served this part %d times (including any browser downloads); Takeout is believed to limit downloads per part", p.Num, attempts)
	}
}

// VerifyPartFile fully verifies one part's file on disk and records the
// verdict in state. Shared by the in-run verifiers and `carryout verify` so
// the two can't drift. Returns true when the part verified clean.
func VerifyPartFile(ctx context.Context, st *state.State, dir string, p *state.Part, logf func(string, ...any)) bool {
	path := filepath.Join(dir, p.Filename)
	logf("part %03d: verifying %s (%s)…", p.Num, p.Filename, HumanBytes(p.ActualSize))
	start := time.Now()
	if err := sniff.VerifyFile(ctx, path); err != nil {
		if ctx.Err() != nil {
			logf("part %03d: verification interrupted — `carryout verify` will finish it", p.Num)
			return false
		}
		update(st, logf, func() {
			p.Status = state.Corrupt
			p.LastError = "verification failed: " + err.Error()
		})
		logf("part %03d: VERIFICATION FAILED: %v — file kept at %s; requeue with `carryout get -redo %d`", p.Num, err, path, p.Num)
		return false
	}
	now := time.Now().UTC()
	update(st, logf, func() {
		p.Status = state.Done
		p.VerifiedAt = now
		p.LastError = ""
		if p.ActualSize == 0 {
			if fi, err := os.Stat(path); err == nil {
				p.ActualSize = fi.Size()
			}
		}
	})
	logf("part %03d: verified OK in %s", p.Num, time.Since(start).Round(time.Second))
	return true
}

func update(st *state.State, logf func(string, ...any), fn func()) {
	if err := st.Update(fn); err != nil {
		logf("WARNING: could not save %s: %v", state.FileName, err)
	}
}

type progressReader struct {
	r    io.Reader
	f    *Fetcher
	prog *partProgress
}

func (pr *progressReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	if n > 0 {
		pr.prog.cur.Add(int64(n))
		pr.f.runBytes.Add(int64(n))
	}
	return n, err
}

func (f *Fetcher) trackPart(num int, offset, expected int64) *partProgress {
	p := &partProgress{expected: expected}
	p.cur.Store(offset)
	f.progMu.Lock()
	f.progress[num] = p
	f.progMu.Unlock()
	return p
}

func (f *Fetcher) untrackPart(num int) {
	f.progMu.Lock()
	delete(f.progress, num)
	f.progMu.Unlock()
}

// report prints a progress line every 10 seconds while downloads are active.
func (f *Fetcher) report(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	var lastBytes int64
	lastTick := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		cur := f.runBytes.Load()
		speed := float64(cur-lastBytes) / now.Sub(lastTick).Seconds()
		lastBytes, lastTick = cur, now

		type act struct {
			num      int
			cur, exp int64
		}
		var active []act
		f.progMu.Lock()
		for num, p := range f.progress {
			active = append(active, act{num, p.cur.Load(), p.expected})
		}
		f.progMu.Unlock()
		if len(active) == 0 {
			continue
		}
		sort.Slice(active, func(i, j int) bool { return active[i].num < active[j].num })

		var parts []string
		for _, a := range active {
			if a.exp > 0 {
				parts = append(parts, fmt.Sprintf("part %03d %d%%", a.num, a.cur*100/a.exp))
			} else {
				parts = append(parts, fmt.Sprintf("part %03d %s", a.num, HumanBytes(a.cur)))
			}
		}
		_, _, doneN, _, _, doneBytes := f.st.Counts()
		line := fmt.Sprintf("%s · %s/s · done %d/%d (%s) · this run %s",
			strings.Join(parts, " · "), HumanBytes(int64(speed)), doneN, f.st.TotalParts, HumanBytes(doneBytes), HumanBytes(cur))

		if eta, ok := f.eta(speed); ok {
			line += " · ETA " + eta
		}
		f.opt.ProgressLogf("%s", line)
	}
}

// eta estimates time remaining from the shared state estimator at the current
// transfer speed, minus what active transfers already have on disk.
func (f *Fetcher) eta(speed float64) (string, bool) {
	if speed <= 0 {
		return "", false
	}
	remaining, ok := f.st.RemainingEstimate()
	if !ok {
		return "", false
	}
	f.progMu.Lock()
	for _, p := range f.progress {
		remaining -= p.cur.Load()
	}
	f.progMu.Unlock()
	if remaining <= 0 {
		return "", false
	}
	d := time.Duration(float64(remaining)/speed) * time.Second
	if d > 48*time.Hour {
		return fmt.Sprintf("%.1f days", d.Hours()/24), true
	}
	return d.Round(time.Minute).String(), true
}

// HumanBytes formats a byte count in IEC units. Exported so the CLI and this
// package can't drift apart in how they render the same numbers.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
