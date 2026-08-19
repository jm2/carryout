// SPDX-License-Identifier: GPL-3.0-or-later

// carryout picks up your Google Takeout order: give it one copied-as-cURL
// download request plus the export's file list and it fetches every archive
// part, verifies each one, and keeps going until the whole export is on disk.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jm2/carryout/internal/curlcmd"
	"github.com/jm2/carryout/internal/fetch"
	"github.com/jm2/carryout/internal/state"
	"github.com/jm2/carryout/internal/takeout"
	"github.com/jm2/carryout/internal/ui"
)

// version is the release version, overridden at build time via
// -ldflags "-X main.version=…" by the release workflow. Nothing has been
// released yet; the first tag will be v0.1.0 (the internal 0.1/0.2 numbering
// during initial development shipped to nobody).
var version = "0.1.0-dev"

const usageText = `carryout — picks up your Google Takeout order

Google Takeout hands you a multi-terabyte export as dozens of 50 GiB archives
behind expiring links and a session that dies every few files. carryout
downloads all of them from one pasted browser capture plus the export's file
list: it constructs every part URL, fetches with a few parallel workers,
verifies each archive, halts the moment your session dies (so no download
attempts are wasted), and resumes where it left off.

Usage:
  carryout <command> [flags]

Commands:
  init      Register an export from a cURL capture + the page's file list
  get       Download and verify all remaining parts (safe to re-run)
  status    Show per-part progress, sizes, and serve counts
  verify    Run full integrity verification on downloaded parts
  auth      Update session cookies from a fresh capture
  version   Print version

Run 'carryout <command> -h' for flags. Typical first session:

  cd /big/disk/takeout
  carryout init            # paste the capture, then the file list
  carryout get -dry-run    # eyeball every URL it will hit
  carryout get -only 1     # end-to-end test on one part
  carryout get             # fetch everything
`

type exitErr struct {
	code int
	err  error
}

func (e *exitErr) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usageText)
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "get":
		err = cmdGet(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "auth":
		err = cmdAuth(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("carryout " + version)
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "carryout: unknown command %q\n\n%s", os.Args[1], usageText)
		os.Exit(1)
	}
	if err != nil {
		code := 1
		var ee *exitErr
		if errors.As(err, &ee) {
			code = ee.code
			err = ee.err
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "carryout: "+err.Error())
		}
		os.Exit(code)
	}
}

// parseFlags wraps FlagSet.Parse so a usage typo exits 1 (flag.ExitOnError
// would exit 2, colliding with 2 = "session expired, run carryout auth").
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &exitErr{code: 0}
		}
		return &exitErr{code: 1} // flag package already printed the error
	}
	return nil
}

// gate is the process-wide console: Plain timestamped logs by default,
// swapped for the live renderer in cmdGet when stdout is a real terminal.
var gate ui.Console = ui.NewPlain(os.Stdout)

func logf(format string, args ...any) { gate.Logf(format, args...) }

// ---- init ----

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := fs.String("dir", ".", "directory to download into (state lives here too)")
	curlFile := fs.String("curl-file", "", "read the cURL capture from this file instead of prompting")
	manifestFile := fs.String("manifest-file", "", "read the export's file list (pasted Takeout page text) from this file")
	parts := fs.Int("parts", 0, "total part count — ONLY for simple single-sequence exports; grouped exports need the file list")
	verifyMode := fs.String("verify", "full", "verification mode: full (decompress and check every byte) or quick (magic bytes + size)")
	force := fs.Bool("force", false, "overwrite an existing carryout.json")
	reuse := fs.Bool("reuse-capture", false, "with -force: reuse the captured URL, headers, and cookies already in this directory instead of pasting a new capture")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if *verifyMode != "full" && *verifyMode != "quick" {
		return fmt.Errorf("-verify must be full or quick, not %q", *verifyMode)
	}
	if state.Exists(*dir) && !*force {
		return fmt.Errorf("%s already exists — use `carryout get` to resume, or -force to start over", state.Path(*dir))
	}
	release, err := acquireLock(*dir)
	if err != nil {
		return err
	}
	defer release()

	in := bufio.NewReader(os.Stdin)

	var capturedURL, cookie string
	var headers map[string]string
	if *reuse {
		if *curlFile != "" {
			return errors.New("-reuse-capture and -curl-file are mutually exclusive")
		}
		old, err := state.Load(*dir)
		if err != nil {
			return fmt.Errorf("-reuse-capture needs the previous state: %w", err)
		}
		c, err := state.LoadCookie(*dir)
		if err != nil {
			return fmt.Errorf("-reuse-capture needs the existing cookies: %w", err)
		}
		capturedURL, cookie, headers = old.CapturedURL, c, old.Headers
	} else {
		text, err := readCapture(in, *curlFile,
			`Paste the "Copy as cURL" capture of one part download (the request to
takeout-download…usercontent.google.com), then press Enter on a blank line:`)
		if err != nil {
			return err
		}
		capture, err := curlcmd.Parse(text)
		if err != nil {
			return err
		}
		capturedURL = capture.URL
		cookie = capture.Headers.Get("Cookie")
		headers = replayHeaders(capture.Headers)
		if cookie == "" {
			return errors.New("the capture has no Cookie header — copy the cURL for the request your logged-in browser made")
		}
	}

	tmpl, err := takeout.Derive(capturedURL)
	if err != nil {
		return err
	}
	if err := tmpl.SelfCheck(); err != nil {
		return err
	}

	entries, fromPaste, err := readInventory(in, tmpl, *manifestFile, *parts)
	if err != nil {
		return err
	}
	if err := takeout.Calibrate(entries, tmpl); err != nil {
		return err
	}
	if fromPaste && isTTY(os.Stdin) {
		// The one check that catches every inventory mishap: a human compares
		// the parsed count against the page.
		fmt.Printf("\nParsed: %s\n", takeout.GroupSummary(entries))
		fmt.Print("Does that match the file count shown on the Takeout page? [y/N]: ")
		line, _ := in.ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			return errors.New("aborted — if the count is short, the paste was cut off (terminals cap pasted lines at 4 KiB); save the summary to a file and re-run with -manifest-file")
		}
	}

	st := state.New(*dir)
	st.CapturedURL = capturedURL
	st.JobID = tmpl.JobID
	st.TotalParts = len(entries)
	st.VerifyMode = *verifyMode
	st.Headers = headers
	seeded := 0
	for i, e := range entries {
		if !takeout.ValidFilename(e.Filename) {
			return fmt.Errorf("unsafe filename in inventory: %q", e.Filename)
		}
		p := &state.Part{Num: i + 1, Filename: e.Filename, Index: e.Index, Status: state.Pending}
		if e.Downloaded > 0 {
			p.Attempts = e.Downloaded
			seeded++
		}
		st.Parts = append(st.Parts, p)
	}
	if err := state.SaveCookie(*dir, cookie); err != nil {
		return err
	}
	if err := st.Save(); err != nil {
		return err
	}

	fmt.Println()
	if st.JobID != "" {
		fmt.Printf("Registered export job %s: %s\n", st.JobID, takeout.GroupSummary(entries))
	} else {
		fmt.Printf("Registered export: %s\n", takeout.GroupSummary(entries))
	}
	for i, e := range entries {
		if e.Filename == tmpl.CapturedName {
			if tmpl.HasIndex {
				fmt.Printf("  calibrated on your capture: %s = list position %d ↔ i=%d\n", e.Filename, i+1, e.Index)
			}
			break
		}
	}
	if !tmpl.HasIndex {
		fmt.Println("  note: no i= index parameter in the captured URL; part URLs will vary only by filename")
	}
	if seeded > 0 {
		fmt.Printf("  seeded served-download counts from the page for %d part(s)\n", seeded)
	}
	fmt.Printf("  first: %s\n", tmpl.BuildURL(entries[0].Filename, entries[0].Index))
	last := entries[len(entries)-1]
	fmt.Printf("  last:  %s\n", tmpl.BuildURL(last.Filename, last.Index))
	cookieNote := " (0600)"
	if runtime.GOOS == "windows" {
		cookieNote = " (restrict access to this file yourself — Windows file permissions are not set)"
	}
	fmt.Printf("  cookies: %s%s   state: %s\n", state.CookiePath(*dir), cookieNote, state.Path(*dir))
	fmt.Println()
	fmt.Println("Next:")
	fmt.Println("  carryout get -dry-run    # review every URL before anything is fetched")
	fmt.Println("  carryout get -only 1     # end-to-end test on the first part")
	if takeout.GroupSummaryIsGrouped(entries) {
		fmt.Println("  (grouped export: also spot-check one part from another group, e.g. -only " + strconv.Itoa(len(entries)) + ")")
	}
	fmt.Println("  carryout get             # fetch everything")
	return nil
}

// readInventory obtains the export's file list: from -manifest-file, from
// -parts synthesis (simple exports only), or interactively. fromPaste reports
// that the list came from an interactive paste (which deserves confirmation —
// terminals can silently truncate long pastes).
func readInventory(in *bufio.Reader, tmpl *takeout.Template, manifestFile string, parts int) (entries []takeout.Entry, fromPaste bool, err error) {
	if manifestFile != "" {
		b, err := os.ReadFile(manifestFile)
		if err != nil {
			return nil, false, err
		}
		entries, err = takeout.ParseInventory(string(b))
		return entries, false, err
	}
	if parts > 0 {
		entries, err = takeout.Synthesize(tmpl, parts)
		return entries, false, err
	}
	fmt.Println()
	fmt.Println(`Now the file list. On the Takeout page, select and copy the export summary
(the block listing every takeout-…tgz file), paste it here, and press Enter on
a blank line. If your terminal cuts long pastes, save the summary to a file
and re-run with -manifest-file instead. For a simple single-sequence export
you can type just the part count:`)
	fmt.Println()
	paste, err := readPaste(in)
	if err != nil {
		return nil, false, err
	}
	if n, aerr := strconv.Atoi(strings.TrimSpace(paste)); aerr == nil && n > 0 {
		entries, err = takeout.Synthesize(tmpl, n)
		return entries, false, err
	}
	entries, err = takeout.ParseInventory(paste)
	return entries, true, err
}

// replayHeaders keeps the browser's headers so replayed requests look exactly
// like the session that captured them, minus anything that would interfere
// with resumable bulk downloads — and minus credentials, which never belong
// in the world-readable state file.
func replayHeaders(h map[string][]string) map[string]string {
	drop := map[string]bool{
		"cookie": true, "authorization": true, "proxy-authorization": true,
		"range": true, "if-range": true, "if-modified-since": true,
		"if-none-match": true, "accept-encoding": true, "content-length": true,
		"host": true, "connection": true, "te": true, "transfer-encoding": true,
	}
	out := make(map[string]string)
	for k, vs := range h {
		if drop[strings.ToLower(k)] || len(vs) == 0 {
			continue
		}
		// repeated headers are rare but legitimate; preserve every value
		out[k] = strings.Join(vs, ", ")
	}
	return out
}

// ---- get ----

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	dir := fs.String("dir", ".", "download directory (where carryout init ran)")
	workers := fs.Int("workers", 3, "parallel downloads (keep this modest)")
	maxTries := fs.Int("max-tries", 2, "attempts per part per run before flagging it for attention")
	only := fs.String("only", "", "limit to these parts, e.g. \"1\" or \"3,7-9\"")
	redo := fs.String("redo", "", "reset these parts and re-download them (deletes their files; costs an attempt)")
	dryRun := fs.Bool("dry-run", false, "print what would be fetched without touching the network or state")
	plain := fs.Bool("plain", false, "plain timestamped log output (no live progress display)")
	noPrompt := fs.Bool("no-prompt", false, "never prompt for cookies; exit 2 when the session dies")
	stall := fs.Duration("stall-timeout", 2*time.Minute, "abort an attempt when no data arrives for this long")
	retryDelay := fs.Duration("retry-delay", 30*time.Second, "wait between retries of a failed part")
	verifyFlag := fs.String("verify", "", "override verification mode for this run: full or quick")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := state.Load(*dir)
	if err != nil {
		return err
	}
	cookie, err := state.LoadCookie(*dir)
	if err != nil {
		return err
	}
	tmpl, err := takeout.Derive(st.CapturedURL)
	if err != nil {
		return fmt.Errorf("re-deriving URL template from %s: %w", state.FileName, err)
	}

	verifyMode := st.VerifyMode
	if *verifyFlag != "" {
		if *verifyFlag != "full" && *verifyFlag != "quick" {
			return fmt.Errorf("-verify must be full or quick, not %q", *verifyFlag)
		}
		verifyMode = *verifyFlag
	}

	onlySet, err := parsePartList(*only, st.TotalParts)
	if err != nil {
		return fmt.Errorf("-only: %w", err)
	}
	redoSet, err := parsePartList(*redo, st.TotalParts)
	if err != nil {
		return fmt.Errorf("-redo: %w", err)
	}

	if !*dryRun {
		release, err := acquireLock(*dir)
		if err != nil {
			return err
		}
		defer release()
	}

	// Live progress display when stdout is a real terminal; today's plain
	// logs otherwise (nohup, pipes, -plain, dry runs).
	var live *ui.Renderer
	if !*plain && !*dryRun {
		if r, ok := ui.NewLive(os.Stdout, isTTY(os.Stdout)); ok {
			live = r
			gate = r
			defer r.Close()
		}
	}

	if len(redoSet) > 0 {
		if err := applyRedo(st, *dir, redoSet, *dryRun); err != nil {
			return err
		}
	}

	warnDiskSpace(st, *dir)

	opts := fetch.Options{
		Dir:          *dir,
		Workers:      *workers,
		MaxTries:     *maxTries,
		FullVerify:   verifyMode == "full",
		Only:         onlySet,
		StallTimeout: *stall,
		RetryDelay:   *retryDelay,
		DryRun:       *dryRun,
		Logf:         logf,
		ProgressLogf: gate.Progressf,
	}
	if !*noPrompt && isTTY(os.Stdin) {
		opts.RefreshAuth = promptRefreshAuth
	}

	f := fetch.New(tmpl, st, cookie, opts)
	if live != nil {
		live.Start(f.Snapshot)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }() // a second Ctrl-C kills the process immediately

	sum, runErr := f.Run(ctx)
	if live != nil {
		live.Close() // idempotent; the summary below prints on a clean screen
	}

	if !*dryRun {
		fmt.Println()
		fmt.Printf("this run: %s in %s\n", fetch.HumanBytes(sum.BytesThisRun), sum.Elapsed)
		fmt.Printf("export:   %d done · %d downloaded (unverified) · %d pending · %d attention · %d corrupt (of %d)\n",
			sum.Done, sum.Downloaded, sum.Pending, sum.Attention, sum.Corrupt, st.TotalParts)
	}

	if runErr != nil {
		if fetch.AuthNeeded(runErr) {
			return &exitErr{code: 2, err: runErr}
		}
		return runErr
	}
	// exit code reflects only the parts this run was asked about
	if att, cor := selectionIssues(st, onlySet); att+cor > 0 {
		return &exitErr{code: 3, err: fmt.Errorf("%d part(s) need attention — see `carryout status`", att+cor)}
	}
	if sum.Complete() {
		fmt.Println("all parts downloaded and verified — your order is picked up ✔")
	}
	return nil
}

func selectionIssues(st *state.State, only map[int]bool) (attention, corrupt int) {
	st.View(func() {
		for _, p := range st.Parts {
			if only != nil && !only[p.Num] {
				continue
			}
			switch p.Status {
			case state.Attention:
				attention++
			case state.Corrupt:
				corrupt++
			}
		}
	})
	return
}

func applyRedo(st *state.State, dir string, redoSet map[int]bool, dryRun bool) error {
	nums := make([]int, 0, len(redoSet))
	for n := range redoSet {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		p := st.Part(n)
		if p == nil {
			return fmt.Errorf("part %d doesn't exist", n)
		}
		final := filepath.Join(dir, p.Filename)
		if p.Status == state.Done {
			if _, err := os.Stat(final); err == nil {
				return fmt.Errorf("part %d is done and %s exists — refusing to -redo it; delete the file first if you really mean it", n, p.Filename)
			}
		}
		if dryRun {
			logf("would reset part %03d and delete %s(.part)", n, p.Filename)
			continue
		}
		_ = os.Remove(final)
		_ = os.Remove(final + ".part")
		err := st.Update(func() {
			p.Status = state.Pending
			p.ActualSize = 0
			p.LastError = ""
			p.CompletedAt = time.Time{}
			p.VerifiedAt = time.Time{}
		})
		if err != nil {
			return err
		}
		logf("part %03d reset for re-download (attempt history kept: %d served so far)", n, p.Attempts)
	}
	return nil
}

func promptRefreshAuth(ctx context.Context, reason string) (string, error) {
	// Silence background log output for the duration of the prompt so
	// in-flight transfers finishing behind the scenes can't garble the paste.
	gate.Hold()
	defer gate.Release()

	fmt.Println()
	fmt.Println("================================================================")
	fmt.Println("  Google session expired: " + reason)
	fmt.Println("  No NEW downloads will start; transfers already in flight are")
	fmt.Println("  finishing in the background (their log lines will appear")
	fmt.Println("  after you paste). No attempts are being burned.")
	fmt.Println()
	fmt.Println("  In your (still logged-in) browser: open the Takeout page,")
	fmt.Println("  start any part download, cancel it, and Copy as cURL the")
	fmt.Println("  request to takeout-download…usercontent.google.com.")
	fmt.Println()
	fmt.Println("  Paste the cURL command (or just the cookie string) below,")
	fmt.Println("  then press Enter on a blank line. Ctrl-C aborts (state is saved).")
	fmt.Println("================================================================")

	type pasteResult struct {
		text string
		err  error
	}
	const maxPasteTries = 3
	for attempt := 1; ; attempt++ {
		ch := make(chan pasteResult, 1)
		go func() {
			t, e := readPaste(bufio.NewReader(os.Stdin))
			ch <- pasteResult{t, e}
		}()
		select {
		case <-ctx.Done():
			return "", errors.New("interrupted while waiting for cookies — run `carryout auth`, then `carryout get` to resume")
		case r := <-ch:
			pasteErr := r.err
			if pasteErr == nil {
				cookie, cerr := curlcmd.CookieFromPaste(r.text)
				if cerr == nil {
					return cookie, nil
				}
				pasteErr = cerr
			}
			// A mangled paste must not kill a multi-hour run: explain and
			// re-prompt instead of exiting.
			if attempt >= maxPasteTries {
				return "", fmt.Errorf("%v — run `carryout auth`, then `carryout get` to resume", pasteErr)
			}
			fmt.Printf("\nThat paste didn't work: %v\n", pasteErr)
			fmt.Printf("Try again (attempt %d/%d), then press Enter on a blank line — or Ctrl-C to abort (state is saved):\n\n", attempt+1, maxPasteTries)
		}
	}
}

func warnDiskSpace(st *state.State, dir string) {
	free, ok := diskFree(dir)
	if !ok {
		return
	}
	remaining, ok := st.RemainingEstimate()
	if !ok || remaining <= 0 {
		return
	}
	if free < uint64(remaining) {
		logf("WARNING: ~%s still to download but only %s free on this filesystem", fetch.HumanBytes(remaining), fetch.HumanBytes(int64(free)))
	}
}

// ---- status ----

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	dir := fs.String("dir", ".", "download directory")
	verbose := fs.Bool("v", false, "show every part (default: only parts that aren't done)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := state.Load(*dir)
	if err != nil {
		return err
	}

	if st.JobID != "" {
		fmt.Printf("export job %s · %d parts · verify mode: %s\n", st.JobID, st.TotalParts, st.VerifyMode)
	} else {
		fmt.Printf("export · %d parts · verify mode: %s\n", st.TotalParts, st.VerifyMode)
	}
	if fi, err := os.Stat(state.CookiePath(*dir)); err == nil {
		fmt.Printf("cookies refreshed %s ago\n", time.Since(fi.ModTime()).Round(time.Minute))
	}
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PART\tFILE\tSTATUS\tSIZE\tSERVED\tNOTE")
	shown := 0
	st.View(func() {
		for _, p := range st.Parts {
			if !*verbose && p.Status == state.Done {
				continue
			}
			shown++
			size := "-"
			switch {
			case p.ActualSize > 0 && p.ExpectedSize > 0:
				size = fmt.Sprintf("%s / %s", fetch.HumanBytes(p.ActualSize), fetch.HumanBytes(p.ExpectedSize))
			case p.ActualSize > 0:
				size = fetch.HumanBytes(p.ActualSize)
			case p.ExpectedSize > 0:
				size = "? / " + fetch.HumanBytes(p.ExpectedSize)
			}
			note := p.LastError
			if p.Status == state.Done && !p.VerifiedAt.IsZero() {
				note = "verified"
			}
			if r := []rune(note); len(r) > 60 {
				note = string(r[:57]) + "..."
			}
			fmt.Fprintf(w, "%03d\t%s\t%s\t%s\t%d\t%s\n", p.Num, p.Filename, p.Status, size, p.Attempts, note)
		}
	})
	if shown > 0 {
		w.Flush()
	}

	pending, downloaded, done, attention, corrupt, doneBytes := st.Counts()
	fmt.Println()
	fmt.Printf("%d/%d done (%s on disk) · %d downloaded-unverified · %d pending · %d attention · %d corrupt\n",
		done, st.TotalParts, fetch.HumanBytes(doneBytes), downloaded, pending, attention, corrupt)
	if !*verbose && shown == 0 && done == st.TotalParts {
		fmt.Println("everything is downloaded and verified ✔")
	}
	return nil
}

// ---- verify ----

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", ".", "download directory")
	all := fs.Bool("all", false, "re-verify every part with a file on disk, even already-verified ones")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	st, err := state.Load(*dir)
	if err != nil {
		return err
	}
	release, err := acquireLock(*dir)
	if err != nil {
		return err
	}
	defer release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var targets []*state.Part
	st.View(func() {
		for _, p := range st.Parts {
			path := filepath.Join(*dir, p.Filename)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			if *all || p.VerifiedAt.IsZero() {
				targets = append(targets, p)
			}
		}
	})
	if len(targets) == 0 {
		fmt.Println("nothing to verify")
		return nil
	}

	corrupt := 0
	for _, p := range targets {
		if ctx.Err() != nil {
			return errors.New("interrupted — run `carryout verify` again to finish")
		}
		if !fetch.VerifyPartFile(ctx, st, *dir, p, logf) && ctx.Err() == nil {
			corrupt++
		}
	}
	if corrupt > 0 {
		return &exitErr{code: 3, err: fmt.Errorf("%d part(s) failed verification", corrupt)}
	}
	fmt.Printf("all %d part(s) verified OK\n", len(targets))
	return nil
}

// ---- auth ----

func cmdAuth(args []string) error {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	dir := fs.String("dir", ".", "download directory")
	curlFile := fs.String("curl-file", "", "read the capture from this file instead of prompting")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if !state.Exists(*dir) {
		return fmt.Errorf("no %s in %s — run `carryout init` first", state.FileName, *dir)
	}
	release, err := acquireLock(*dir)
	if err != nil {
		return err
	}
	defer release()

	text, err := readCapture(bufio.NewReader(os.Stdin), *curlFile,
		"Paste a fresh cURL capture (or just the cookie string), then press Enter on a blank line:")
	if err != nil {
		return err
	}
	cookie, err := curlcmd.CookieFromPaste(text)
	if err != nil {
		return err
	}
	if err := state.SaveCookie(*dir, cookie); err != nil {
		return err
	}
	fmt.Printf("cookies updated in %s — `carryout get` will resume where it left off\n", state.CookiePath(*dir))
	return nil
}

// ---- shared helpers ----

// readCapture reads paste-able input from a file or interactively.
func readCapture(in *bufio.Reader, file, prompt string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	fmt.Println(prompt)
	fmt.Println()
	return readPaste(in)
}

func readPaste(r *bufio.Reader) (string, error) {
	tty := isTTY(os.Stdin)
	var lines []string
	for {
		line, err := r.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		// Linux TTYs in canonical mode silently cut a pasted line at 4096
		// bytes; a line this long arriving through a terminal has almost
		// certainly lost its tail. Fail closed rather than act on a subset.
		if tty && len(trimmed) >= 4080 {
			return "", fmt.Errorf("a pasted line hit your terminal's 4 KiB line limit and was truncated by the kernel — save the paste to a file and use the -curl-file/-manifest-file flag instead (e.g. `wl-paste > file` on Wayland, `xclip -o > file` on X11)")
		}
		if strings.TrimSpace(trimmed) == "" {
			if len(lines) > 0 {
				break
			}
		} else {
			lines = append(lines, trimmed)
		}
		if err != nil {
			break // EOF
		}
	}
	if len(lines) == 0 {
		return "", errors.New("nothing pasted")
	}
	return strings.Join(lines, "\n"), nil
}

// parsePartList parses "1,3,7-9" into a set, or nil for the empty string.
func parsePartList(s string, total int) (map[int]bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	set := make(map[int]bool)
	for _, piece := range strings.Split(s, ",") {
		piece = strings.TrimSpace(piece)
		lo, hi, isRange := strings.Cut(piece, "-")
		start, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("bad part number %q", piece)
		}
		end := start
		if isRange {
			end, err = strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return nil, fmt.Errorf("bad range %q", piece)
			}
		}
		if start < 1 || end > total || start > end {
			return nil, fmt.Errorf("%q is outside 1-%d", piece, total)
		}
		for n := start; n <= end; n++ {
			set[n] = true
		}
	}
	return set, nil
}

func acquireLock(dir string) (func(), error) {
	path := filepath.Join(dir, "carryout.lock")
	self := []byte(strconv.Itoa(os.Getpid()) + "\n")
	for range 3 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			f.Write(self)
			f.Close()
			release := func() {
				// only remove a lock that is still ours
				if b, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(b, self) {
					os.Remove(path)
				}
			}
			return release, nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue // holder released between our O_EXCL and read; retry
			}
			return nil, fmt.Errorf("lock file %s exists but is unreadable: %v", path, rerr)
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 && processAlive(pid) {
			return nil, fmt.Errorf("another carryout (pid %d) is already working in this directory", pid)
		}
		// Stale lock. Narrow the takeover race: re-check that the lock still
		// holds the same dead pid before removing it.
		if b2, rerr := os.ReadFile(path); rerr != nil || !bytes.Equal(b, b2) {
			continue
		}
		os.Remove(path)
	}
	return nil, errors.New("could not acquire lock (contention); try again")
}
