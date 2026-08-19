// SPDX-License-Identifier: GPL-3.0-or-later

// carryout picks up your Google Takeout order: give it one copied-as-cURL
// download request and it fetches every archive part, verifies each one, and
// keeps going until the whole export is on disk.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jm2/carryout/internal/curlcmd"
	"github.com/jm2/carryout/internal/fetch"
	"github.com/jm2/carryout/internal/sniff"
	"github.com/jm2/carryout/internal/state"
	"github.com/jm2/carryout/internal/takeout"
)

const version = "0.1.0"

const usageText = `carryout — picks up your Google Takeout order

Google Takeout hands you a multi-terabyte export as dozens of 50 GiB archives
behind expiring links and a session that dies every few files. carryout
downloads all of them from one pasted browser capture: it constructs every
part URL, fetches with a few parallel workers, verifies each archive, halts
the moment your session dies (so no download attempts are wasted), and
resumes where it left off.

Usage:
  carryout <command> [flags]

Commands:
  init      Register an export from a pasted "Copy as cURL" capture
  get       Download and verify all remaining parts (safe to re-run)
  status    Show per-part progress, sizes, and attempt counts
  verify    Run full integrity verification on downloaded parts
  auth      Update session cookies from a fresh capture
  version   Print version

Run 'carryout <command> -h' for flags. Typical first session:

  cd /big/disk/takeout
  carryout init            # paste the capture, enter the part count
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

func logf(format string, args ...any) {
	fmt.Printf(time.Now().Format("15:04:05")+"  "+format+"\n", args...)
}

// ---- init ----

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", ".", "directory to download into (state lives here too)")
	curlFile := fs.String("curl-file", "", "read the cURL capture from this file instead of prompting")
	parts := fs.Int("parts", 0, "total number of archive parts (shown on the Takeout page)")
	verifyMode := fs.String("verify", "full", "verification mode: full (decompress and check every byte) or quick (magic bytes + size)")
	force := fs.Bool("force", false, "overwrite an existing carryout.json")
	fs.Parse(args)

	if *verifyMode != "full" && *verifyMode != "quick" {
		return fmt.Errorf("-verify must be full or quick, not %q", *verifyMode)
	}
	if state.Exists(*dir) && !*force {
		return fmt.Errorf("%s already exists — use `carryout get` to resume, or -force to start over", state.Path(*dir))
	}

	in := bufio.NewReader(os.Stdin)
	var text string
	if *curlFile != "" {
		b, err := os.ReadFile(*curlFile)
		if err != nil {
			return err
		}
		text = string(b)
	} else {
		fmt.Println(`Paste the "Copy as cURL" capture of one part download (the request to`)
		fmt.Println(`takeout-download…usercontent.google.com), then press Enter on a blank line:`)
		fmt.Println()
		var err error
		text, err = readPaste(in)
		if err != nil {
			return err
		}
	}

	capture, err := curlcmd.Parse(text)
	if err != nil {
		return err
	}
	tmpl, err := takeout.Derive(capture.URL)
	if err != nil {
		return err
	}
	cookie := capture.Headers.Get("Cookie")
	if cookie == "" {
		return errors.New("the capture has no Cookie header — copy the cURL for the request your logged-in browser made")
	}

	total := *parts
	for total < 1 {
		n, err := promptInt(in, "How many archive parts does the export have (shown on the Takeout page)? ")
		if err != nil {
			return err
		}
		total = n
	}
	if tmpl.CapturedNum > total {
		return fmt.Errorf("the capture is for part %d but you said the export has %d parts", tmpl.CapturedNum, total)
	}

	st := state.New(*dir)
	st.CapturedURL = capture.URL
	st.JobID = tmpl.JobID
	st.TotalParts = total
	st.VerifyMode = *verifyMode
	st.Headers = replayHeaders(capture.Headers)
	for n := 1; n <= total; n++ {
		st.Parts = append(st.Parts, &state.Part{Num: n, Filename: tmpl.Filename(n), Status: state.Pending})
	}
	if err := state.SaveCookie(*dir, cookie); err != nil {
		return err
	}
	if err := st.Save(); err != nil {
		return err
	}

	fmt.Println()
	if st.JobID != "" {
		fmt.Printf("Registered export job %s: %d parts\n", st.JobID, total)
	} else {
		fmt.Printf("Registered export: %d parts\n", total)
	}
	if tmpl.HasIndex {
		fmt.Printf("  captured part %0*d ↔ i=%d (offset %+d, measured from your capture)\n",
			tmpl.NumWidth, tmpl.CapturedNum, tmpl.CapturedNum-tmpl.Offset, tmpl.Offset)
	} else {
		fmt.Println("  note: no i= index parameter in the captured URL; part URLs will vary only by filename")
	}
	fmt.Printf("  first: %s\n", tmpl.PartURL(1))
	fmt.Printf("  last:  %s\n", tmpl.PartURL(total))
	fmt.Printf("  cookies: %s (0600)   state: %s\n", state.CookiePath(*dir), state.Path(*dir))
	fmt.Println()
	fmt.Println("Next:")
	fmt.Println("  carryout get -dry-run    # review every URL before anything is fetched")
	fmt.Println("  carryout get -only 1     # end-to-end test on the first part")
	fmt.Println("  carryout get             # fetch everything")
	return nil
}

// replayHeaders keeps the browser's headers so replayed requests look exactly
// like the session that captured them, minus anything that would interfere
// with resumable bulk downloads.
func replayHeaders(h map[string][]string) map[string]string {
	drop := map[string]bool{
		"cookie": true, "range": true, "if-range": true, "if-modified-since": true,
		"if-none-match": true, "accept-encoding": true, "content-length": true,
		"host": true, "connection": true, "te": true, "transfer-encoding": true,
	}
	out := make(map[string]string)
	for k, vs := range h {
		if drop[strings.ToLower(k)] || len(vs) == 0 {
			continue
		}
		out[k] = vs[0]
	}
	return out
}

// ---- get ----

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	dir := fs.String("dir", ".", "download directory (where carryout init ran)")
	workers := fs.Int("workers", 3, "parallel downloads (keep this modest)")
	maxTries := fs.Int("max-tries", 2, "attempts per part per run before flagging it for attention")
	only := fs.String("only", "", "limit to these parts, e.g. \"1\" or \"3,7-9\"")
	redo := fs.String("redo", "", "reset these parts and re-download them (deletes their files; costs an attempt)")
	dryRun := fs.Bool("dry-run", false, "print what would be fetched without touching the network")
	noPrompt := fs.Bool("no-prompt", false, "never prompt for cookies; exit 2 when the session dies")
	stall := fs.Duration("stall-timeout", 2*time.Minute, "abort an attempt when no data arrives for this long")
	retryDelay := fs.Duration("retry-delay", 30*time.Second, "wait between retries of a failed part")
	verifyFlag := fs.String("verify", "", "override verification mode for this run: full or quick")
	fs.Parse(args)

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
	}
	if !*noPrompt && isTTY(os.Stdin) {
		opts.RefreshAuth = promptRefreshAuth
	}

	f := fetch.New(tmpl, st, cookie, opts)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }() // a second Ctrl-C kills the process immediately

	sum, runErr := f.Run(ctx)

	if !*dryRun {
		fmt.Println()
		fmt.Printf("this run: %s in %s\n", humanBytes(sum.BytesThisRun), sum.Elapsed)
		fmt.Printf("export:   %d done · %d downloaded (unverified) · %d pending · %d attention · %d corrupt (of %d)\n",
			sum.Done, sum.Downloaded, sum.Pending, sum.Attention, sum.Corrupt, st.TotalParts)
	}

	if runErr != nil {
		if fetch.AuthNeeded(runErr) {
			return &exitErr{code: 2, err: runErr}
		}
		return runErr
	}
	if sum.Attention > 0 || sum.Corrupt > 0 {
		return &exitErr{code: 3, err: fmt.Errorf("%d part(s) need attention — see `carryout status`", sum.Attention+sum.Corrupt)}
	}
	if sum.Complete() {
		fmt.Println("all parts downloaded and verified — your order is picked up ✔")
	}
	return nil
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

func promptRefreshAuth(reason string) (string, error) {
	fmt.Println()
	fmt.Println("================================================================")
	fmt.Println("  Google session expired: " + reason)
	fmt.Println("  All workers are paused; no attempts are being burned.")
	fmt.Println()
	fmt.Println("  In your (still logged-in) browser: open the Takeout page,")
	fmt.Println("  start any part download, cancel it, and Copy as cURL the")
	fmt.Println("  request to takeout-download…usercontent.google.com.")
	fmt.Println()
	fmt.Println("  Paste the cURL command (or just the cookie string) below,")
	fmt.Println("  then press Enter on a blank line. Ctrl-C aborts (state is saved).")
	fmt.Println("================================================================")
	text, err := readPaste(bufio.NewReader(os.Stdin))
	if err != nil {
		return "", fmt.Errorf("%v — run `carryout auth`, then `carryout get` to resume", err)
	}
	return curlcmd.CookieFromPaste(text)
}

func warnDiskSpace(st *state.State, dir string) {
	free, ok := diskFree(dir)
	if !ok {
		return
	}
	var remaining, doneBytes, doneCount int64
	unknown := 0
	st.View(func() {
		for _, p := range st.Parts {
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
	if unknown > 0 && doneCount > 0 {
		remaining += int64(unknown) * (doneBytes / doneCount)
	}
	if remaining > 0 && free < uint64(remaining) {
		logf("WARNING: ~%s still to download but only %s free on this filesystem", humanBytes(remaining), humanBytes(int64(free)))
	}
}

// ---- status ----

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dir := fs.String("dir", ".", "download directory")
	verbose := fs.Bool("v", false, "show every part (default: only parts that aren't done)")
	fs.Parse(args)

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
	fmt.Fprintln(w, "PART\tSTATUS\tSIZE\tSERVED\tNOTE")
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
				size = fmt.Sprintf("%s / %s", humanBytes(p.ActualSize), humanBytes(p.ExpectedSize))
			case p.ActualSize > 0:
				size = humanBytes(p.ActualSize)
			case p.ExpectedSize > 0:
				size = "? / " + humanBytes(p.ExpectedSize)
			}
			note := p.LastError
			if p.Status == state.Done && !p.VerifiedAt.IsZero() {
				note = "verified"
			}
			if len(note) > 60 {
				note = note[:57] + "..."
			}
			fmt.Fprintf(w, "%03d\t%s\t%s\t%d\t%s\n", p.Num, p.Status, size, p.Attempts, note)
		}
	})
	if shown > 0 {
		w.Flush()
	}

	pending, downloaded, done, attention, corrupt, doneBytes := st.Counts()
	fmt.Println()
	fmt.Printf("%d/%d done (%s on disk) · %d downloaded-unverified · %d pending · %d attention · %d corrupt\n",
		done, st.TotalParts, humanBytes(doneBytes), downloaded, pending, attention, corrupt)
	if !*verbose && shown == 0 && done == st.TotalParts {
		fmt.Println("everything is downloaded and verified ✔")
	}
	return nil
}

// ---- verify ----

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", ".", "download directory")
	all := fs.Bool("all", false, "re-verify every part with a file on disk, even already-verified ones")
	fs.Parse(args)

	st, err := state.Load(*dir)
	if err != nil {
		return err
	}
	release, err := acquireLock(*dir)
	if err != nil {
		return err
	}
	defer release()

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
		path := filepath.Join(*dir, p.Filename)
		logf("part %03d: verifying %s (%s)…", p.Num, p.Filename, humanBytes(p.ActualSize))
		if err := sniff.VerifyFile(path); err != nil {
			corrupt++
			st.Update(func() {
				p.Status = state.Corrupt
				p.LastError = "verification failed: " + err.Error()
			})
			logf("part %03d: VERIFICATION FAILED: %v — requeue with `carryout get -redo %d`", p.Num, err, p.Num)
			continue
		}
		now := time.Now().UTC()
		st.Update(func() {
			p.Status = state.Done
			p.VerifiedAt = now
			p.LastError = ""
			if p.ActualSize == 0 {
				if fi, err := os.Stat(path); err == nil {
					p.ActualSize = fi.Size()
				}
			}
		})
		logf("part %03d: verified OK", p.Num)
	}
	if corrupt > 0 {
		return &exitErr{code: 3, err: fmt.Errorf("%d part(s) failed verification", corrupt)}
	}
	fmt.Printf("all %d part(s) verified OK\n", len(targets))
	return nil
}

// ---- auth ----

func cmdAuth(args []string) error {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)
	dir := fs.String("dir", ".", "download directory")
	curlFile := fs.String("curl-file", "", "read the capture from this file instead of prompting")
	fs.Parse(args)

	if !state.Exists(*dir) {
		return fmt.Errorf("no %s in %s — run `carryout init` first", state.FileName, *dir)
	}

	var text string
	if *curlFile != "" {
		b, err := os.ReadFile(*curlFile)
		if err != nil {
			return err
		}
		text = string(b)
	} else {
		fmt.Println("Paste a fresh cURL capture (or just the cookie string), then press Enter on a blank line:")
		fmt.Println()
		var err error
		text, err = readPaste(bufio.NewReader(os.Stdin))
		if err != nil {
			return err
		}
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

func readPaste(r *bufio.Reader) (string, error) {
	var lines []string
	for {
		line, err := r.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
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

func promptInt(r *bufio.Reader, prompt string) (int, error) {
	for {
		fmt.Print(prompt)
		line, err := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			n, aerr := strconv.Atoi(line)
			if aerr == nil && n > 0 {
				return n, nil
			}
			fmt.Println("please enter a positive number")
		}
		if err != nil {
			return 0, errors.New("no part count provided")
		}
	}
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
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, fmt.Errorf("lock file %s exists but is unreadable: %v", path, rerr)
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 && processAlive(pid) {
			return nil, fmt.Errorf("another carryout (pid %d) is already working in this directory", pid)
		}
		os.Remove(path) // stale lock from a dead process
	}
	return nil, errors.New("could not acquire lock")
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func humanBytes(n int64) string {
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
