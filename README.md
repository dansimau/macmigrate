# macmigrate

Migrate a Mac to a new Mac over ssh — home directory, apps, Homebrew — using
many parallel `rsync` jobs so a direct cable (Thunderbolt / ethernet) is
actually saturated.

## Quick start

```sh
go build -o macmigrate .

./macmigrate setup 169.254.190.76     # one-time: ssh key + passwordless sudo
./macmigrate sync 169.254.190.76 -n   # dry run — writes nothing
./macmigrate sync 169.254.190.76     # the migration
./macmigrate cleanup 169.254.190.76   # undo setup when done
```

`<dest>` is `[user@]host`. Run `sync` **without** `sudo` — it re-runs itself
under `sudo -E` (one password prompt) so rsync can read every file, while all
ssh connections still run as you (your keys, your agent).

## What gets copied

| What | How |
|------|-----|
| `$HOME` | One rsync job per top-level dir; `~/Library` split further |
| `/Applications` | Only apps missing on the destination |
| Homebrew (`/usr/local`, `/opt/homebrew`) | Same absolute path, as root |
| System `/Library` extras (Fonts, LaunchAgents, …) | Same, when present |

- Add dirs with `--include /path`, skip home entries with `--exclude Library/Foo`.
- Nested includes (e.g. `/opt/homebrew` + `/opt/homebrew/Cellar`) are
  autodetected: the inner tree gets its own parallel jobs, the outer skips it.
- Everything runs as root on the destination, so ownership survives.
- Both Macs are kept awake (`caffeinate`) for the duration.

## Requirements

- **rsync ≥ 3.1** on the source (Homebrew's works; stock may be too old).
- **Remote Login** enabled on the destination (System Settings ▸ General ▸ Sharing).
- **Key-based ssh + passwordless sudo** on the destination — `setup` configures
  both; `cleanup` reverses it. `sync` checks up front and points you at `setup`
  if anything's missing.
- **Full Disk Access** for your terminal, to read protected parts of
  `~/Library` (Mail, Messages, Safari…). Detected at preflight; without it the
  run still proceeds and the summary lists what was skipped.

## Commands

### `setup <dest>`

Reuses an existing ssh key (or generates `~/.ssh/id_macmigrate`), installs it
in the destination's `authorized_keys`, and grants passwordless sudo via
`/etc/sudoers.d/macmigrate`. One ssh authentication total; all prompting is
ssh's own. Idempotent — re-running is safe. Flags: `-i <key>`, `--skip-keygen`.

### `sync <dest>`

| Flag | Default | Description |
|------|---------|-------------|
| `-j`, `--jobs` | `4` | Max parallel rsync jobs |
| `-n`, `--dry-run` | off | Preview; writes nothing |
| `-i`, `--identity` | auto | SSH key (`~/.ssh/id_macmigrate` if present) |
| `--include` | Homebrew + `/Library` extras | Extra absolute dir (repeatable) |
| `--exclude` | `.Trash`, caches, iCloud Drive | `$HOME`-relative skip (repeatable) |
| `--debug` | off | Diagnostics to stderr |

`--include`/`--exclude` add to the defaults rather than replacing them.

### `cleanup <dest>`

Removes the sudoers entry and the macmigrate key from the destination. The
local key is left in place.

## Output & exit codes

Each job shows a live progress line, then a permanent `✓` / `⚠` / `✗` line;
full output goes to a timestamped log file. The summary lists every partial
with its error lines:

```
134 done · 32 partial · 0 failed · 1m2s
⚠ 32 directories had unreadable items (everything else copied) …
```

- **0** — clean run. **1** — any failure or partial. **130** — interrupted.
- Partials are almost always TCC-protected dirs: grant **Full Disk Access**
  (System Settings ▸ Privacy & Security) and re-run. A few system stores
  (e.g. `com.apple.TCC`) can never be copied — `--exclude` them.

## Testing

```sh
go test ./...                          # unit tests
sudo go test -tags=integration -count=1 .   # end-to-end over ssh to localhost
```

Integration tests need root and Remote Login; anything missing makes them skip
with instructions. The first run provisions a local `macmigratetest` user
(persisted for speed); set `MACMIGRATE_TEST_CLEANUP=1` to remove it after.
