# carryout

**Picks up your Google Takeout order.**

Google Takeout will happily export 5 TB of Google Photos — as a hundred-plus
50 GiB archives behind links that expire in 7 days, gated by a session that
dies every few files, with (reportedly) a small number of download attempts
allowed per part. Clicking through that in a browser for a week is the whole
problem. carryout automates the pickup:

- **One capture, every part.** Paste a single "Copy as cURL" of any part
  download; carryout derives the URL pattern from it and constructs the rest.
  Nothing about Google's numbering is assumed — the offset between the
  filename number and the `i=` index is measured from your own capture.
- **Halts instead of hammering.** Every response is sniffed. The moment one
  looks like a sign-in page, the whole queue pauses and carryout asks for
  fresh cookies — dead sessions never burn download attempts across workers.
  Transfers already streaming are left to finish; those bytes count.
- **Verifies everything.** A part is only *done* after its size matches
  Content-Length and the archive decompresses cleanly end to end
  (the equivalent of `gzip -t`; zip entries are CRC-checked too).
- **Resumes anything.** State lives in `carryout.json`; interrupted parts
  resume with Range requests; re-running `carryout get` is always safe.
- Zero dependencies — pure Go standard library.

## Install

```sh
go install github.com/jm2/carryout@latest
# or, from a checkout:
go build -o carryout .
```

## Walkthrough

**1. Create the export** at [takeout.google.com](https://takeout.google.com):
Photos (or whatever you're extracting), `.tgz`, 50 GB chunks. Wait for the
"your export is ready" email. Note the number of parts shown on the page.

**2. Capture one download.** In your logged-in browser:

1. Open DevTools → Network tab (enable *Preserve log*).
2. Click **Download** on part 1 and complete any re-auth prompt.
3. When the download begins, **cancel it** in the browser — carryout will
   re-fetch this part. (The click itself likely spent one download attempt
   for part 1; that's unavoidable and only affects part 1.)
4. In the Network list, find the request to
   `takeout-download…usercontent.google.com` whose path ends in the archive
   filename (`…-001.tgz`). Right-click → Copy → **Copy as cURL**.

   Not the `takeout.google.com/settings/takeout/download` request — that's
   the redirect, and session cookies are bound to the final download host.
   The [CurlWget](https://chromewebstore.google.com/detail/curlwget) extension
   is an alternative that captures the resolved request directly.

**3. Register and test** (run in the directory the archives should land in —
somewhere with enough free space):

```sh
carryout init          # paste the capture, enter the part count
carryout get -dry-run  # review every URL before anything is fetched
carryout get -only 1   # end-to-end test on one part before trusting the queue
```

**4. Fetch everything** (inside tmux/screen — this runs for days):

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

**5. Audit:**

```sh
carryout status     # per-part table: status, actual/expected size, serve count
carryout verify     # (re)verify anything not yet fully verified
```

## Account safety

Designed so the answer to "will this endanger my Google account?" is no:

- **No credentials, no login automation.** carryout never sees your password
  or 2FA and never drives a browser. You authenticate in your own browser;
  carryout replays the download request your browser already made — same
  cookies, same User-Agent, same headers.
- **Downloading your own export with an HTTP client is normal usage**, not
  circumvention — the links are authorized for your session, and curl/wget
  pickups of large Takeout exports are long-established practice.
- **Every request is user-initiated and predictable.** Nothing runs in the
  background; `get -dry-run` shows the exact URL list beforehand; the only
  hosts contacted are the ones in your capture.
- **Conservative by default and fails closed.** 3 connections; downloads by
  served count, not by hammering: max 2 tries per part per run, one global
  5-minute cooldown on HTTP 429 (giving up entirely if throttling persists),
  full stop on any response it doesn't recognize. Auth death pauses
  everything rather than retrying into a wall.
- **Honest bookkeeping.** The `SERVED` column counts every time Google
  started serving a part — counted the moment binary content begins flowing,
  even if the transfer later fails — and warns at 4, since Takeout is
  believed to cap downloads per part.

## Files carryout writes

| File | What |
|---|---|
| `carryout.json` | State and audit trail: per-part status, expected vs. actual sizes, serve counts. No secrets. |
| `cookies.txt` | Your session cookie header, mode 0600. Delete it when the pickup is done. |
| `takeout-…-NNN.tgz` | The archives. `.part` while downloading, renamed when complete. |
| `*.error.html` | Saved copy of any unexpected HTML page Google returns, for diagnosis. |
| `carryout.lock` | Guards against two carryouts in one directory. |

## The deadline math

Links expire 7 days after the export is created. 5.5 TB in 7 days needs a
sustained ~75 Mbit/s. Check `carryout status` daily; if ETA outruns the
deadline, prioritize which parts matter with `-only`.

## Caveats

- carryout drives an undocumented corner of Google's infrastructure; URL
  shapes can change without notice. That's why `get -only 1` first is part of
  the workflow — if the pattern changed, you find out on attempt one, not
  attempt four hundred.
- The per-part attempt limit and whether aborted transfers count against it
  are not documented by Google. carryout assumes the pessimistic
  interpretation everywhere.
- A part flagged `corrupt` is kept on disk for inspection and only re-fetched
  with an explicit `carryout get -redo N`, because re-downloading spends an
  attempt.

## Exit codes

`0` success · `1` error · `2` session expired, run `carryout auth` · `3` some
parts need attention (`carryout status`)

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
