// SPDX-License-Identifier: GPL-3.0-or-later

// Package fetch downloads Takeout archive parts: a small pool of workers,
// resume via Range requests, a stall watchdog, content sniffing on every
// response, and a global halt the moment authentication looks dead — so a
// dead session never burns download attempts across the whole queue.
package fetch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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
	errThrottled    = errors.New("throttled (HTTP 429)")
	errStalled      = errors.New("transfer stalled")
	errGaveUp       = errors.New("gave up on part") // non-fatal: part flagged, run continues
)

// fatalError stops the whole run.
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

type Options struct {
	Dir          string
	Workers      int
	MaxTries     int // per part per run, including the first try
	FullVerify   bool
	Only         map[int]bool // nil = all parts
	StallTimeout time.Duration
	RetryDelay   time.Duration
	Cooldown     time.Duration // wait after HTTP 429
	DryRun       bool
	// RefreshAuth is called (serialized; all new requests are held) when the
	// session looks expired. It returns a fresh Cookie header value.
	RefreshAuth func(reason string) (string, error)
	Logf        func(format string, args ...any)
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

func (f *Fetcher) newClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DisableCompression:    true, // archives are already compressed; keep Content-Length honest
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			host := strings.ToLower(req.URL.Hostname())
			if strings.Contains(host, "accounts.google") || strings.Contains(strings.ToLower(req.URL.Path), "servicelogin") {
				return errAuthRedirect
			}
			// Go drops Cookie on cross-host redirects; keep it for Google-owned hosts.
			if host == "google.com" || strings.HasSuffix(host, ".google.com") || strings.HasSuffix(host, ".googleusercontent.com") {
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
func (f *Fetcher) refreshAuth(seenGen int, reason string) error {
	f.authMu.Lock()
	defer f.authMu.Unlock()
	if seenGen != f.authGen {
		return nil // another worker already refreshed
	}
	if f.opt.RefreshAuth == nil {
		return &fatalError{
			msg:        "session expired (" + reason + ") — run `carryout auth` with a fresh capture, then `carryout get` to resume",
			authNeeded: true,
		}
	}
	cookie, err := f.opt.RefreshAuth(reason)
	if err != nil {
		return &fatalError{msg: "session expired (" + reason + "): " + err.Error(), authNeeded: true}
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

// plan decides what this run will do, adopting any stray complete files it
// finds (e.g. a part the browser already downloaded into the directory).
func (f *Fetcher) plan() (download, verify []*state.Part, notes []string) {
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
				if f.opt.FullVerify {
					verify = append(verify, p)
				} else {
					notes = append(notes, fmt.Sprintf("part %03d is downloaded but unverified — run `carryout verify` when convenient", p.Num))
				}
			case state.Corrupt:
				notes = append(notes, fmt.Sprintf("part %03d failed verification earlier — re-download costs an attempt, requeue explicitly with `carryout get -redo %d`", p.Num, p.Num))
			case state.Pending, state.Attention:
				if fi, err := os.Stat(final); err == nil {
					if kind, kerr := sniff.FileKind(final); kerr == nil && kind != "" {
						p.Status = state.Downloaded
						p.ActualSize = fi.Size()
						p.LastError = ""
						notes = append(notes, fmt.Sprintf("part %03d: adopted existing file %s (%s); it will be verified", p.Num, p.Filename, human(fi.Size())))
						if f.opt.FullVerify {
							verify = append(verify, p)
						} else {
							p.Status = state.Done
							p.CompletedAt = time.Now().UTC()
						}
						continue
					}
					notes = append(notes, fmt.Sprintf("part %03d: existing file %s is not a valid archive; it will be re-downloaded", p.Num, p.Filename))
				}
				if p.Status == state.Attention {
					p.Status = state.Pending
				}
				download = append(download, p)
			}
		}
	})
	if err := f.st.Save(); err != nil {
		f.logf("WARNING: could not save %s: %v", state.FileName, err)
	}
	sort.Slice(download, func(i, j int) bool { return download[i].Num < download[j].Num })
	return download, verify, notes
}

// Run executes the plan and blocks until done, interrupted, or a fatal error.
func (f *Fetcher) Run(ctx context.Context) (Summary, error) {
	start := time.Now()
	download, verify, notes := f.plan()
	for _, n := range notes {
		f.logf("%s", n)
	}

	if f.opt.DryRun {
		for _, p := range download {
			f.logf("would download part %03d: GET %s", p.Num, f.tmpl.PartURL(p.Num))
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
					if f.opt.FullVerify {
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

	var vwg sync.WaitGroup
	vwg.Add(1)
	go func() {
		defer vwg.Done()
		for p := range verifyQ {
			// verification is local and free; only a hard abort skips it
			if ctx.Err() != nil {
				continue // drain; carryout verify can finish later
			}
			f.verifyPart(p)
		}
	}()

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
	for {
		if qctx.Err() != nil {
			return context.Canceled
		}
		f.waitCooldown(qctx)

		gen, cookie := f.snapshotAuth()
		tries++
		err := f.attempt(hctx, p, cookie)
		if err == nil {
			return nil
		}

		switch {
		case errors.Is(err, context.Canceled) || hctx.Err() != nil:
			return context.Canceled

		case errors.Is(err, errAuthExpired):
			tries-- // auth failures don't count against the part
			f.logf("part %03d: %v — holding the queue for fresh cookies", p.Num, err)
			if rerr := f.refreshAuth(gen, err.Error()); rerr != nil {
				return rerr
			}

		case errors.Is(err, errThrottled):
			tries--
			throttleWaits++
			if throttleWaits > 4 {
				return &fatalError{msg: fmt.Sprintf("part %03d: still throttled after %d cooldowns — stopping so we don't hammer Google; try again later", p.Num, throttleWaits)}
			}
			f.logf("part %03d: HTTP 429 — cooling down for %s", p.Num, f.opt.Cooldown)
			f.setCooldown(f.opt.Cooldown)

		default:
			var re *retryableError
			if !errors.As(err, &re) {
				f.update(func() { p.LastError = err.Error() })
				return err // fatal
			}
			if f.authChanged(gen) {
				continue // failure was collateral damage of a session death; retry free
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

	req, err := http.NewRequestWithContext(actx, http.MethodGet, f.tmpl.PartURL(p.Num), nil)
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
		return errThrottled
	case resp.StatusCode == http.StatusNotFound:
		return &fatalError{msg: fmt.Sprintf("part %03d: HTTP 404 — wrong total part count, a changed URL pattern, or an expired export; check the Takeout page", p.Num)}
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

	// An error page announces itself in the Content-Type header.
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml") {
		if resuming {
			return errAuthExpired
		}
		body, _ := io.ReadAll(io.LimitReader(br, 256<<10))
		return f.htmlPageError(p, final, body)
	}

	// Google is serving binary content: count it now, before anything (a
	// network blip, another worker's fatal abort) can interrupt the transfer.
	// An interrupted serve plausibly still counts against Takeout's per-part
	// download limit, so the on-disk counter must never lag Google's.
	f.recordServed(p, resuming)

	if !resuming {
		// Belt and braces: some error pages come mislabeled as binary.
		head, _ := br.Peek(512)
		if sniff.IsHTML("", head) {
			body, _ := io.ReadAll(io.LimitReader(br, 256<<10))
			return f.htmlPageError(p, final, body)
		}
		if sniff.Kind(head) == "" {
			return &fatalError{msg: fmt.Sprintf("part %03d: response is not a recognized archive (starts with % x) — refusing to write it", p.Num, head[:min(8, len(head))])}
		}
	}

	var expected int64 = -1
	if resuming {
		if total, ok := contentRangeTotal(resp.Header.Get("Content-Range")); ok {
			expected = total
		}
	} else if resp.ContentLength > 0 {
		expected = resp.ContentLength
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

	n, cerr := io.Copy(file, &progressReader{r: br, f: f, prog: prog})
	if serr := file.Sync(); cerr == nil && serr != nil {
		cerr = serr
	}
	closed = true
	if clErr := file.Close(); cerr == nil && clErr != nil {
		cerr = clErr
	}
	if cerr != nil {
		if errors.Is(context.Cause(actx), errStalled) {
			return &retryableError{fmt.Errorf("stalled: no data for %s", f.opt.StallTimeout)}
		}
		if ctx.Err() != nil {
			return context.Canceled
		}
		return &retryableError{fmt.Errorf("transfer failed after %s: %w", human(n), cerr)}
	}

	size := offset + n
	if expected > 0 && size != expected {
		if size < expected {
			return &retryableError{fmt.Errorf("short download: got %s of %s (will resume)", human(size), human(expected))}
		}
		return &fatalError{msg: fmt.Sprintf("part %03d: downloaded %s but expected %s — refusing to trust it", p.Num, human(size), human(expected))}
	}

	// Quick check on the assembled file (catches a garbage head from an
	// earlier interrupted attempt that we then resumed on top of).
	if kind, kerr := sniff.FileKind(partPath); kerr != nil || kind == "" {
		_ = os.Remove(partPath)
		return &retryableError{errors.New("assembled file is not a valid archive; discarded it to restart cleanly")}
	}

	if err := os.Rename(partPath, final); err != nil {
		return &fatalError{msg: fmt.Sprintf("part %03d: renaming into place: %v", p.Num, err)}
	}

	now := time.Now().UTC()
	f.update(func() {
		p.ActualSize = size
		p.CompletedAt = now
		p.LastError = ""
		if f.opt.FullVerify {
			p.Status = state.Downloaded
		} else {
			p.Status = state.Done
		}
	})
	sizeNote := ""
	if expected > 0 {
		sizeNote = " (size matches Content-Length)"
	}
	f.logf("part %03d: downloaded %s%s", p.Num, human(size), sizeNote)
	return nil
}

// htmlPageError classifies an HTML body: session death pauses the queue,
// anything else (quota page, unknown error) stops the run with the page saved
// for the user to read.
func (f *Fetcher) htmlPageError(p *state.Part, final string, body []byte) error {
	if sniff.LooksLikeSignIn(body) {
		return errAuthExpired
	}
	diag := final + ".error.html"
	_ = os.WriteFile(diag, body, 0644)
	return &fatalError{msg: fmt.Sprintf("part %03d: Google sent an HTML page instead of the archive (saved to %s) — open it in a browser to see why, then re-run `carryout get`", p.Num, diag)}
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
		f.logf("part %03d: WARNING — Google has served this part %d times; Takeout is believed to limit downloads per part", p.Num, attempts)
	}
}

func (f *Fetcher) verifyPart(p *state.Part) {
	path := filepath.Join(f.opt.Dir, p.Filename)
	f.logf("part %03d: verifying %s (%s)…", p.Num, p.Filename, human(p.ActualSize))
	start := time.Now()
	if err := sniff.VerifyFile(path); err != nil {
		f.update(func() {
			p.Status = state.Corrupt
			p.LastError = "verification failed: " + err.Error()
		})
		f.logf("part %03d: VERIFICATION FAILED: %v — file kept at %s; requeue with `carryout get -redo %d`", p.Num, err, path, p.Num)
		return
	}
	now := time.Now().UTC()
	f.update(func() {
		p.Status = state.Done
		p.VerifiedAt = now
		p.LastError = ""
	})
	f.logf("part %03d: verified OK in %s", p.Num, time.Since(start).Round(time.Second))
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
				parts = append(parts, fmt.Sprintf("part %03d %s", a.num, human(a.cur)))
			}
		}
		_, _, doneN, _, _, doneBytes := f.st.Counts()
		line := fmt.Sprintf("%s · %s/s · done %d/%d (%s) · this run %s",
			strings.Join(parts, " · "), human(int64(speed)), doneN, f.st.TotalParts, human(doneBytes), human(cur))

		if eta, ok := f.eta(speed); ok {
			line += " · ETA " + eta
		}
		f.logf("%s", line)
	}
}

// eta estimates time remaining from known expected sizes (or the average of
// completed parts where unknown) at the current transfer speed.
func (f *Fetcher) eta(speed float64) (string, bool) {
	if speed <= 0 {
		return "", false
	}
	var remaining int64
	var doneBytes, doneCount int64
	unknown := 0
	f.st.View(func() {
		for _, p := range f.st.Parts {
			switch p.Status {
			case state.Done, state.Downloaded:
				doneBytes += p.ActualSize
				doneCount++
			default:
				if p.ExpectedSize > 0 {
					remaining += p.ExpectedSize
				} else {
					unknown++
				}
			}
		}
	})
	if unknown > 0 {
		if doneCount == 0 {
			return "", false
		}
		remaining += int64(unknown) * (doneBytes / doneCount)
	}
	// subtract what active transfers already have on disk
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

func contentRangeTotal(h string) (int64, bool) {
	// "bytes start-end/total"
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(h, "bytes ") {
		return 0, false
	}
	_, totalStr, ok := strings.Cut(h[len("bytes "):], "/")
	if !ok || totalStr == "*" {
		return 0, false
	}
	total, err := strconv.ParseInt(totalStr, 10, 64)
	if err != nil {
		return 0, false
	}
	return total, true
}

func human(n int64) string {
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
