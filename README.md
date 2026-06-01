# macmigrate

Copy a user's home directory and third-party applications from one Mac to
another over ssh, running many `rsync` transfers in parallel so a fast direct
link (e.g. a Thunderbolt bridge / direct ethernet cable) is actually saturated.

## What it does

- **Home directory:** one `rsync` job per top-level subdirectory of `$HOME`. The
  large `~/Library` is sub-split into one job per child for extra parallelism.
- **/Applications:** lists the destination's `/Applications` first and copies
  only the apps that aren't already there.
- **Additional directories:** absolute paths like the Homebrew prefixes
  (`/usr/local`, `/opt/homebrew`) are copied to the **same absolute path** on the
  destination, one job per subdirectory. The root is created if missing (but not
  chowned — some roots like `/usr/local` are SIP-protected); the entries inside
  are copied with their own ownership. Add your own with `-include`.
- **Nested dirs are autodetected.** If you list both a directory and something
  inside it (the default list includes `/opt/homebrew` **and**
  `/opt/homebrew/Cellar`), the inner one is split into its own parallel jobs and
  the outer one skips it — so a huge tree like `Cellar` isn't a single serial
  job, with no double-copying. Deeper paths are dispatched first.
- **Top-level files are grouped.** Each root's loose files are copied in a single
  `… (files)` job (not one job per dotfile/README), and that job doesn't recurse
  or touch the root directory itself.
- Every transfer runs as root on the destination (`--rsync-path="sudo -n rsync"`)
  so ownership is preserved, using `rsync -aE --info=progress2` (archive +
  extended attributes, aggregate progress) — the same flags as the original
  script.

## Running as root locally

macmigrate must run as **root on the source** so `rsync` can read every file
regardless of owner or mode. You don't run it with `sudo` yourself: started
normally, it captures your username and **re-runs itself under `sudo -E`** (which
prompts for your password once and preserves your environment, including
`SSH_AUTH_SOCK`), passing `--user <you>` so the root instance knows whose home
(`/Users/<you>`) to migrate.

`rsync` itself runs as root for local reads, but every `ssh` connection (the
transfer transport and the preflight checks) is launched as **you** via
`sudo -E -u <you> ssh`, so it uses your ssh-agent, keys and `known_hosts` rather
than root's (root has none). That means the destination still needs **passwordless
sudo for your remote login user** — that part runs remotely and is unchanged.

## Requirements

- **rsync ≥ 3.1** on the source (for `--info=progress2`). macOS's Homebrew
  `rsync` works; stock `/usr/bin/rsync` may be too old (it must be on `PATH`).
- **Remote Login (SSH)** enabled on the destination
  (System Settings ▸ General ▸ Sharing), with **key-based auth** set up —
  parallel jobs can't answer password prompts.
- **Passwordless sudo** on the destination. Every transfer runs as `sudo rsync`
  so file ownership is preserved (your home directory included), and sudo can't
  prompt for a password across parallel ssh connections, so it must be
  non-interactive. On the **destination**:

  ```sh
  echo "$(id -un) ALL=(ALL) NOPASSWD: ALL" | sudo tee /etc/sudoers.d/macmigrate >/dev/null
  sudo chmod 440 /etc/sudoers.d/macmigrate
  # remove /etc/sudoers.d/macmigrate when the migration is done
  ```

  macmigrate checks this up front and exits with this guidance if it's missing.
- **Full Disk Access** for the terminal running it (to read parts of `~/Library`).
  macmigrate detects this during preflight (it probes the user's TCC database,
  which is only readable with FDA) and prints a clear warning if it's missing —
  the run still proceeds, copying everything that isn't TCC-protected.

During the migration macmigrate keeps **both** Macs awake with `caffeinate -s`
(local and over ssh on the destination), releasing them when it finishes.

## Build

```sh
go build -o macmigrate .
```

## Usage

Run it **without** `sudo` — it re-runs itself under `sudo` and prompts for your
password.

```sh
# Everything (home + apps + default dirs) to a directly-connected Mac:
./macmigrate -dest 169.254.190.76

# Preview first — exercises the whole pipeline, writes nothing:
./macmigrate -dest 169.254.190.76 -n

# 12 parallel jobs:
./macmigrate -dest user@mac2.local -j 12

# Skip an extra Library subdir:
./macmigrate -dest mac2.local -exclude Library/Containers

# Add an extra directory beyond the defaults:
./macmigrate -dest mac2.local -include /opt/local
```

### Flags

| Flag        | Default        | Description                                                                 |
|-------------|----------------|-----------------------------------------------------------------------------|
| `-dest`     | *(required)*   | Destination `[user@]host`                                                   |
| `-j`        | `4`            | Max parallel rsync jobs                                                     |
| `-include`  | see below      | Additional absolute directory to migrate (repeatable)                       |
| `-n`        | `false`        | Dry run (`--dry-run`); writes nothing                                       |
| `-exclude`  | see below      | `$HOME`-relative entry to skip (repeatable)                                 |
| `-user`     | invoking user  | Username whose home (`/Users/<user>`) to migrate; set automatically on the sudo re-run |

Defaults add to (don't replace) the built-ins:

- `-exclude`: `.Trash`, `Library/Caches`, `Library/Accounts`,
  `Library/AppleMediaServices`, `Library/Mobile Documents` (iCloud Drive —
  re-syncs from the cloud on the new Mac).
- `-include`: the Homebrew prefixes `/usr/local`, `/opt/homebrew`,
  `/opt/homebrew/Cellar`, plus the system-wide `/Library` items that hold
  third-party data not under `$HOME` — `/Library/Application Support`,
  `/Library/Fonts`, `/Library/Audio`, `/Library/ColorSync`,
  `/Library/LaunchAgents`, `/Library/LaunchDaemons`, `/Library/Services`. All are
  included automatically when they exist locally. Each is copied to the same path
  on the destination, as root; the root is created if missing but not chowned.
  Listing a nested pair (like `/opt/homebrew` + `/opt/homebrew/Cellar`) makes the
  inner one split independently and be skipped by the outer — list any large
  nested tree to parallelize it. (`-include` paths must exist; missing defaults
  are silently skipped.)

## Output & logging

Each active job shows one live line — `[label] <latest rsync line>` — updated in
place; finished jobs print a permanent `✓` (done) / `⚠` (partial) / `✗` (failed)
line above the live region. Every job's full output is streamed to the log file
(prefixed `[label]`). The end-of-run summary reprints full detail for real
failures and a per-directory list — with each directory's error lines — for
partials:

```
134 done · 32 partial · 0 failed · 1m2s
Full log: macmigrate-20260602-093000.log

⚠ 32 directories had unreadable items (everything else copied):
    Library/Mail
        rsync: [sender] send_files failed to open "...": Operation not permitted (1)
    …
  These are macOS privacy-protected (TCC). To include data like Mail, Messages,
  Safari and Photos, grant Full Disk Access to your terminal, then re-run.
```

When stdout isn't a terminal (pipe / CI) the live region degrades to periodic
plain status lines.

## Exit codes & protected directories

macOS TCC protects much of `~/Library` and `~/Pictures`. Without Full Disk
Access, rsync can't read dirs like `Library/Mail` and exits **23** ("partial
transfer"); macmigrate classifies rsync **23/24** as a **partial** — the readable
data still copied. Process exit code:

- **0** — everything done, no partials or failures.
- **1** — any hard failure (a non-23/24 rsync error), or any partial transfer
  (so a scripted run notices protected dirs); partials are surfaced in the
  summary with their error lines, and the readable data still copied.
- **130** — interrupted.

To actually copy that protected data, grant **Full Disk Access** to the terminal
(System Settings ▸ Privacy & Security ▸ Full Disk Access) and re-run — most
partials disappear. A few system stores (e.g. `com.apple.TCC`) can never be
copied; `-exclude` them.

