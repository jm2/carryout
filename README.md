# carryout

**Picks up your Google Takeout order.**

Google Takeout will happily export 5+ TB of Google Photos — as a hundred-plus
50 GiB archives behind links that expire in 7 days, gated by a session that
dies every few files, with (reportedly) a small number of download attempts
allowed per part. Clicking through that in a browser for a week is the whole
problem. carryout automates the pickup:

- **One capture + the file list, every part.** Paste a single "Copy as cURL"
  of any part download plus the export summary off the Takeout page; carryout
  builds every download URL from them. Nothing about Google's numbering is
  assumed — real exports ship as several groups (`…-3-001.tgz`, `…-3-002.tgz`,
  … `…-8-010.tgz`) with a separate flat index, so the filename↔index mapping
  is taken from your page and calibrated against your capture, and a
  wrong-file tripwire (Content-Disposition) stops the run if Google ever
  serves something other than what was asked for.
- **Halts instead of hammering.** Every response is classified. A sign-in
  page pauses the whole queue for fresh cookies — dead sessions never burn
  download attempts across workers, and if fresh cookies don't help (per-part
  download cap, expired link) the part is flagged instead of looping. HTTP 429
  honors `Retry-After`. Transfers already streaming when something fails are
  left to finish; those bytes count.
- **Verifies everything, mostly for free.** Fresh downloads are decompressed
  and CRC-checked *inline* while they stream, so a clean part is fully
  verified (the `gzip -t` equivalent) with zero extra disk reads. Resumed,
  adopted, or unknown-length parts get a full verification pass afterwards.
- **Resumes anything.** Interrupted parts resume with validated Range
  requests; a byte-complete `.part` left by a crash is finalized without
  spending another download; re-running `carryout get` is always safe.
- **Honest bookkeeping.** The `SERVED` counter is seeded from the page's own
  "Number of times already downloaded" and increments the moment Google
  starts serving bytes — it never lags Google's counter, and warns at 4.
- Zero dependencies — pure Go standard library. Linux, macOS, and Windows.

## Install

```sh
go install github.com/jm2/carryout@latest
# or, from a checkout:
go build -o carryout .
```

## Walkthrough

**1. Create the export** at [takeout.google.com](https://takeout.google.com):
Photos (or whatever you're extracting), `.tgz`, 50 GB chunks. Wait for the
"your export is ready" email.

**2. Capture one download.** "Copy as cURL" works because your browser's
DevTools record every request the page makes — URL, headers, and the session
cookies that authorize it — and can re-emit any of them as a runnable `curl`
command. carryout never runs that command; it parses out the URL shape, the
headers, and the cookies, and replays the same request itself for every part.

In your logged-in browser:

1. Open DevTools → **Network** tab (enable *Preserve log*).
2. Click **Download** on the FIRST file in the export list and complete any
   re-auth prompt.
3. When the download begins, **cancel it** in the browser — carryout will
   re-fetch this part. (The click itself likely spent one download attempt
   for that part; that's unavoidable and only affects this one part.)
4. In the Network list, find the request to
   `takeout-download…usercontent.google.com` whose path ends in the archive
   filename (`…-001.tgz`). Right-click → Copy → **Copy as cURL** —
   on Windows pick the **bash** variant, not cmd or PowerShell.

   Not the `takeout.google.com/settings/takeout/download` request — that's
   the redirect, and session cookies are bound to the final download host.
   carryout rejects redirect captures with instructions.

**3. Copy the file list.** On the Takeout page, select and copy the export
summary — the block listing every `takeout-…tgz` with its "Number of times
already downloaded" counter. carryout parses filenames, order, and counters
out of that text; formatting doesn't matter.

⚠️ **Big exports: use a file, not a terminal paste.** Terminals silently cut
a pasted line at 4 KiB — enough for only ~50 entries of that one-line summary.
Save the clipboard to a file and pass it instead:

```sh
wl-paste > manifest.txt        # Wayland   (xclip -o > manifest.txt on X11,
carryout init -manifest-file manifest.txt   # pbpaste on macOS, Get-Clipboard on Windows)
```

carryout defends in depth here: a paste that ends mid-entry is rejected, a
4 KiB line arriving through a terminal is rejected, and interactive init asks
you to confirm the parsed file count against the page before registering
anything.

**4. Register and test** (run in the directory the archives should land in):

```sh
carryout init          # paste the capture, then the file list
carryout get -dry-run  # review every URL before anything is fetched
carryout get -only 1   # end-to-end test on one part
carryout get -only 20  # for grouped exports: spot-check another group too
```

**5. Fetch everything** (inside tmux/screen — this runs for days):

```sh
carryout get
```

Three workers download in part order, logging progress, speed, and ETA every
10 seconds. When the session dies mid-run, carryout pauses all workers and
prompts; paste a fresh capture (or just the cookie string) and it resumes.

For unattended runs (`nohup carryout get -no-prompt &`), carryout exits with
code 2 when the session dies. Refresh and resume with:

```sh
carryout auth   # paste a fresh capture or cookie string
carryout get    # picks up exactly where it stopped
```

**6. Audit:**

```sh
carryout status     # per-part table: file, status, sizes, serve count
carryout verify     # (re)verify anything not yet fully verified
```

## Flags worth knowing

Every command takes `-dir` (default: current directory) and `-h` for the full
list. The non-obvious ones:

| Flag | What |
|---|---|
| `init -curl-file F` / `init -manifest-file F` | Read the capture / file list from files instead of prompting (scripted setup). |
| `init -parts N` | Skip the file list for simple single-sequence exports only; grouped exports are refused. |
| `init -force -reuse-capture` | Re-register (e.g. with a corrected file list) reusing the captured URL, headers, and cookies already in the directory — no new browser capture needed. Files already on disk are adopted and re-verified, not re-downloaded. |
| `get -verify quick` | Skip full decompression checks on clean known-length downloads (magic bytes + size only). Resumed, adopted, and unknown-length parts are always fully verified regardless. |
| `get -workers / -max-tries / -stall-timeout / -retry-delay` | Tuning; the defaults (3 / 2 / 2m / 30s) are deliberately conservative. |
| `get -redo N` | Reset a corrupt/failed part and re-download it (deletes its file; spends a download attempt). |
| `status -v` | Include completed parts in the table. |
| `verify -all` | Re-verify everything on disk, even already-verified parts. |

## Account safety

Designed so the answer to "will this endanger my Google account?" is no:

- **No credentials, no login automation.** carryout never sees your password
  or 2FA and never drives a browser. You authenticate in your own browser;
  carryout replays the download request your browser already made — same
  cookies, same User-Agent, same headers. (Credential-bearing headers like
  `Authorization` are never persisted; cookies live only in `cookies.txt`.)
- **Downloading your own export with an HTTP client is normal usage**, not
  circumvention — the links are authorized for your session, and curl/wget
  pickups of large Takeout exports are long-established practice.
- **Every request is user-initiated and predictable.** Nothing runs in the
  background; `get -dry-run` shows the exact URL list beforehand; the only
  hosts contacted are the ones in your capture.
- **Conservative by default and fails closed.** 3 connections; max 2 tries
  per part per run; `Retry-After`-aware cooldowns on 429 (giving up entirely
  if throttling persists); anything unrecognized flags the part or stops the
  run rather than retrying into a wall.

## Files carryout writes

| File | What |
|---|---|
| `carryout.json` | State and audit trail: per-part status, expected vs. actual sizes, serve counts. No secrets. Previous version kept as `.bak`. |
| `cookies.txt` | Your session cookie header, mode 0600 (on Windows, restrict the directory yourself). Delete it when the pickup is done. |
| `takeout-…-NNN.tgz` | The archives. `.part` while downloading, renamed when complete. |
| `*.error.html` | Saved copy of any unexpected HTML page Google returns, for diagnosis. |
| `carryout.lock` | Guards against two carryouts in one directory. |

## The deadline math

Links expire 7 days after the export is created. ~7 TB in 6 remaining days
needs a sustained ~110 Mbit/s. Check `carryout status` daily; if the ETA
outruns the deadline, prioritize which parts matter with `-only`.

## Caveats

- carryout drives an undocumented corner of Google's infrastructure; URL
  shapes can change without notice. That's why testing one part per group
  before trusting the queue is part of the workflow — and why the wrong-file
  tripwire exists.
- The per-part attempt limit and whether aborted transfers count against it
  are not documented by Google. carryout assumes the pessimistic
  interpretation everywhere.
- A part flagged `corrupt` is kept on disk for inspection and only re-fetched
  with an explicit `carryout get -redo N`, because re-downloading spends an
  attempt.
- The file-list order is assumed to match the export's internal index order
  (calibrated against your capture). If Google ever breaks that assumption,
  the Content-Disposition tripwire stops the run on the first mismatched
  part.

## Exit codes

`0` success · `1` error · `2` session expired, run `carryout auth` · `3` some
requested parts need attention (`carryout status`)

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
