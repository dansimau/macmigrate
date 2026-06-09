// Package migrate builds and runs the parallel rsync transfers that copy a
// user's home directory and third-party applications from one Mac to another.
// It distils the essence of the original migrate.sh: split the work into many
// independent rsync invocations and run a bounded number of them at once so a
// fast direct link between the machines is actually saturated.
package migrate

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultSkip lists home/Library entries that are wasteful or problematic to
// copy to a new Mac. It mirrors migrate.sh's Library excludes (Accounts,
// AppleMediaServices) and adds caches, the trash, and iCloud Drive — which
// re-syncs from the cloud on the destination. Entries are matched by their path
// relative to $HOME, e.g. "Library/Caches".
var DefaultSkip = []string{
	".Trash",
	"Library/Caches",
	"Library/Accounts",
	"Library/AppleMediaServices",
	"Library/Mobile Documents",
}

// DefaultRsyncExclude lists rsync --exclude patterns applied to every transfer.
// A pattern with no slash matches by basename at any depth, so "io.kandji*"
// skips the Kandji MDM agent's files wherever they appear — they're managed by
// MDM on the destination and can't be overwritten (Operation not permitted).
// "authorized_keys" keeps the source's ~/.ssh/authorized_keys from clobbering
// the destination's, which would cut off the very ssh access the migration runs
// over. (The ~/.ssh transfer lists the directory's entries as explicit sources
// — see flatDirJob — and rsync applies excludes to explicit sources by their
// basename, so a bare basename pattern is what reaches it.)
var DefaultRsyncExclude = []string{
	"*/Caches/",
	"io.kandji*",
	"authorized_keys",
}

// defaultApplicationsDir is where third-party apps live on both Macs unless
// overridden through Options (used by the integration tests to point both
// sides at temporary roots).
const defaultApplicationsDir = "/Applications"

// DefaultDirs are absolute directories migrated in addition to $HOME and
// /Applications when they exist locally. Each is copied to the same absolute
// path on the destination, as root. Nested entries are allowed and handled
// automatically: a directory that is itself listed is split on its own and
// excluded from its parent's split (so e.g. the large /opt/homebrew/Cellar
// transfers as many parallel jobs instead of one, with no overlap).
//
// Besides the Homebrew prefixes, this includes the system-wide /Library items
// that hold third-party data not under $HOME: app support, fonts, audio plug-ins
// and presets, ColorSync profiles, system-level launch agents/daemons, and
// system Services. (The parallelizing of /Library/Application Support — the
// largest — falls out of the per-subdirectory split.)
var DefaultDirs = []string{
	"/usr/local",
	"/opt/homebrew",
	"/opt/homebrew/Cellar",
	"/Library/Application Support",
	"/Library/Fonts",
	"/Library/Audio",
	"/Library/ColorSync",
	"/Library/LaunchAgents",
	"/Library/LaunchDaemons",
	"/Library/Services",
}

// DefaultFiles are individual absolute files migrated to the same absolute path
// on the destination, as root, when they exist locally. Unlike DefaultDirs
// these are single files rather than directory trees: /etc/hosts carries the
// user's custom hostname mappings across to the new Mac. Each file is copied as
// an explicit rsync source into its parent directory (see fileJobs), so the
// parent directory's own owner and mode are never rewritten and rsync's
// --delete can't prune the directory's other entries — both essential when
// copying into a shared system directory like /etc.
var DefaultFiles = []string{
	"/etc/hosts",
}

// Job is a single rsync transfer.
type Job struct {
	Label       string   // short name shown in the UI and log, e.g. "Documents" or "Library/Mail"
	Srcs        []string // local source path(s); a single dir with a trailing slash copies its contents
	Dst         string   // destination as "[user@]host:/path"
	Excludes    []string // rsync --exclude patterns
	RemoteShell string   // rsync -e value (the ssh command); empty leaves rsync's default
	Chown       *Chown   // ownership pass run after the transfer; nil when the usernames match
}

// Chown is the second execution paired with a job's rsync when the source and
// destination usernames differ: the destination can't map the source owner by
// name, so the user's files arrive owned by the wrong uid, and this pass flips
// them to the destination login user. Targeting files owned by that uid leaves
// root-owned files (e.g. in /Library) untouched, and keeps re-runs cheap:
// ownership is never part of rsync's file comparison, so a mismatch only ever
// costs a metadata chown, never a re-copy.
type Chown struct {
	Path    string // remote path to scan
	UID     string // uid that mis-mapped files carry on the destination (probed once; see ChownUID)
	Recurse bool   // false limits the pass to Path's immediate entries (loose-files jobs)
}

// chownFor returns the ownership pass for one remote path, or nil when none is
// needed.
func chownFor(opt Options, path string, recurse bool) *Chown {
	if opt.ChownUID == "" {
		return nil
	}
	return &Chown{Path: path, UID: opt.ChownUID, Recurse: recurse}
}

// Options controls construction of the job list.
type Options struct {
	Dest          string   // [user@]host
	SSH           SSH      // how to reach the destination over ssh (user/identity)
	Home          string   // local $HOME
	RemoteHome    string   // resolved destination $HOME
	AppsDir       string   // local Applications directory; "" => /Applications
	RemoteAppsDir string   // destination Applications directory; "" => /Applications
	Root          string   // local root prefix for built-in paths; "" => /
	RemoteRoot    string   // destination root prefix for built-in paths; "" => /
	Dirs          []string // additional absolute directories to migrate (re-rooted per Root/RemoteRoot, as root)
	Files         []string // individual absolute files to migrate (re-rooted per Root/RemoteRoot, as root)
	SkipNames     []string // home/Library entries to skip (relative to $HOME)
	RsyncExclude  []string // rsync --exclude patterns applied to every job
	DoHome        bool
	DoApps        bool
	DoDirs        bool
	DoFiles       bool
	Debug         bool   // emit diagnostics to stderr (see Debugf)
	ChownUID      string // uid for the per-job ownership pass (see Chown); "" when the usernames match
}

func (o Options) appsDir() string {
	if o.AppsDir == "" {
		return defaultApplicationsDir
	}
	return o.AppsDir
}

func (o Options) remoteAppsDir() string {
	if o.RemoteAppsDir == "" {
		return defaultApplicationsDir
	}
	return o.RemoteAppsDir
}

// RemoteDirPath maps a local directory to its destination path: localDir
// relative to root, re-rooted at remoteRoot. With the default roots ("" or
// "/") a directory keeps its absolute path, which is the production
// behaviour; the roots only differ under the integration tests.
func RemoteDirPath(root, remoteRoot, localDir string) string {
	if root == "" {
		root = "/"
	}
	if remoteRoot == "" {
		remoteRoot = "/"
	}
	rel, err := filepath.Rel(root, localDir)
	if err != nil {
		return localDir
	}
	return path.Join(remoteRoot, rel)
}

// Args returns the rsync argument list for the job (excluding the binary name).
// It preserves migrate.sh's flags: archive + extended attributes and a single
// aggregate progress line. Every transfer runs the remote rsync as root via
// `sudo -n /usr/bin/rsync` so ownership is preserved; `-n` keeps sudo
// non-interactive (it can't prompt across parallel ssh connections), and the
// explicit path pins the stock Apple rsync — the non-interactive ssh PATH
// wouldn't find a Homebrew one anyway, and the local rsync 3 interoperates
// with it. (--info=progress2 is interpreted by the local side only.) When the
// source and destination usernames differ, the transfer is followed by a
// Chown pass — the flags themselves never change.
//
// --delete prunes destination files no longer present at the source, so a sync
// re-run mirrors the source rather than accumulating files removed since the
// last pass. It only deletes within directories sent whole — the trailing-slash
// subdir jobs and the .app bundle jobs — so the explicit-file jobs (loose root
// files, ~/.ssh entries) can never delete their siblings on the destination.
// Excluded files (authorized_keys, io.kandji*, Caches) are protected from
// deletion by default, since we never pass --delete-excluded.
func (j Job) Args(dryRun bool) []string {
	args := []string{"-aE", "--info=progress2", "--delete", "--rsync-path=sudo -n /usr/bin/rsync"}
	if j.RemoteShell != "" {
		args = append(args, "-e", j.RemoteShell)
	}
	if dryRun {
		args = append(args, "--dry-run")
	}
	for _, ex := range j.Excludes {
		args = append(args, "--exclude="+ex)
	}
	args = append(args, "--")
	args = append(args, j.Srcs...)
	args = append(args, j.Dst)
	return args
}

// BuildJobs constructs the full job list for the given options. The second
// return value holds informational messages (e.g. apps already on the
// destination) to surface to the user before the transfer starts.
func BuildJobs(ctx context.Context, opt Options) ([]Job, []string, error) {
	roots := buildRoots(opt)
	isRoot := make(map[string]bool, len(roots))
	for _, r := range roots {
		isRoot[r.local] = true
	}

	var jobs []Job
	for _, r := range roots {
		rjobs, err := splitRoot(opt, r, isRoot)
		if err != nil {
			return nil, nil, err
		}
		jobs = append(jobs, rjobs...)
	}

	if opt.DoFiles {
		jobs = append(jobs, fileJobs(opt)...)
	}

	var notes []string
	if opt.DoApps {
		ajobs, anotes, err := appJobs(ctx, opt)
		if err != nil {
			return nil, nil, err
		}
		jobs = append(jobs, ajobs...)
		notes = anotes
	}
	return jobs, notes, nil
}

// root is a directory whose immediate children are split into parallel jobs.
type root struct {
	local       string          // absolute local directory
	remote      string          // absolute destination directory for its contents
	labelPrefix string          // child label prefix ("" => children labelled by bare name)
	rootLabel   string          // label for this root's loose-files job
	skip        map[string]bool // entries to skip (by child label or bare name)
	flatten     map[string]bool // subdirs copied via explicit entry sources (see flatDirJob)
}

// buildRoots assembles every directory to split. Home contributes $HOME and
// $HOME/Library (so Library keeps its own deeper split); each additional
// directory contributes itself. Within each group the deepest paths come first,
// so a nested dir's jobs dispatch before its parent's.
func buildRoots(opt Options) []root {
	var home, dirs []root
	if opt.DoHome {
		skip := toSet(opt.SkipNames)
		// .ssh is flattened so the destination ~/.ssh directory's own owner and
		// mode are never rewritten — sshd's StrictModes would refuse
		// authorized_keys and cut off the migration's own ssh access.
		home = append(home, root{
			local: opt.Home, remote: opt.RemoteHome, rootLabel: "home",
			skip: skip, flatten: map[string]bool{".ssh": true},
		})
		if !skip["Library"] {
			home = append(home, root{
				local:       filepath.Join(opt.Home, "Library"),
				remote:      path.Join(opt.RemoteHome, "Library"),
				labelPrefix: "Library", rootLabel: "Library", skip: skip,
			})
		}
	}
	if opt.DoDirs {
		for _, d := range opt.Dirs {
			d = filepath.Clean(d)
			// Label by the path relative to Root so it stays stable ("usr/local")
			// even when the local root is a temporary test directory.
			rel := strings.TrimPrefix(RemoteDirPath(opt.Root, "/", d), "/")
			dirs = append(dirs, root{
				local: d, remote: RemoteDirPath(opt.Root, opt.RemoteRoot, d),
				labelPrefix: rel, rootLabel: rel,
			})
		}
	}
	deepestFirst(home)
	deepestFirst(dirs)
	return append(home, dirs...)
}

// splitRoot turns one root into jobs: one per subdirectory (unless that subdir
// is itself a listed root, in which case it is left to its own entry), plus a
// single job for the root's loose files. Files are passed to rsync as explicit
// sources so the transfer neither recurses into subdirectories nor rewrites the
// root directory's own owner/mode (important for SIP-protected roots).
func splitRoot(opt Options, r root, isRoot map[string]bool) ([]Job, error) {
	entries, err := os.ReadDir(r.local)
	if err != nil {
		return nil, err
	}
	var jobs []Job
	var files []string
	for _, e := range entries {
		name := e.Name()
		label := name
		if r.labelPrefix != "" {
			label = r.labelPrefix + "/" + name
		}
		if r.skip[label] || r.skip[name] {
			continue
		}
		childLocal := filepath.Join(r.local, name)
		if e.IsDir() {
			if isRoot[childLocal] {
				continue // split by its own root entry — don't copy it here too
			}
			if r.flatten[name] {
				j, err := flatDirJob(opt, label, childLocal, path.Join(r.remote, name))
				if err != nil {
					return nil, err
				}
				if j != nil {
					jobs = append(jobs, *j)
				}
				continue
			}
			jobs = append(jobs, subdirJob(opt, label, childLocal, path.Join(r.remote, name)))
		} else {
			files = append(files, childLocal)
		}
	}
	sortJobs(jobs)
	if len(files) > 0 {
		sort.Strings(files)
		jobs = append(jobs, looseFilesJob(opt, r.rootLabel+" (files)", files, r.remote))
	}
	return jobs, nil
}

// subdirJob syncs the contents of one subdirectory into the matching remote
// directory (trailing slashes on both sides).
func subdirJob(opt Options, label, localDir, remoteDir string) Job {
	return Job{
		Label:       label,
		Srcs:        []string{ensureSlash(localDir)},
		Dst:         opt.Dest + ":" + ensureSlash(remoteDir),
		Excludes:    opt.RsyncExclude,
		RemoteShell: opt.SSH.RsyncRemoteShell(),
		Chown:       chownFor(opt, remoteDir, true),
	}
}

// flatDirJob copies the contents of one subdirectory by listing its entries as
// explicit rsync sources, so the destination directory's own owner and mode
// are never transferred (rsync only rewrites a destination directory's
// attributes for a trailing-slash directory source). Used for ~/.ssh: rsync
// runs as root on the destination, and re-owning ~/.ssh — to the source uid on
// a cross-username migration — trips sshd's StrictModes, which refuses
// authorized_keys and cuts off the very ssh access the remaining jobs and the
// repairing chown pass need. rsync still applies --exclude patterns to
// explicit sources, so authorized_keys stays protected; the chown pass covers
// the copied contents. Returns nil for an empty directory.
func flatDirJob(opt Options, label, localDir, remoteDir string) (*Job, error) {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	srcs := make([]string, 0, len(entries))
	for _, e := range entries {
		srcs = append(srcs, filepath.Join(localDir, e.Name()))
	}
	sort.Strings(srcs)
	return &Job{
		Label:       label,
		Srcs:        srcs,
		Dst:         opt.Dest + ":" + ensureSlash(remoteDir),
		Excludes:    opt.RsyncExclude,
		RemoteShell: opt.SSH.RsyncRemoteShell(),
		Chown:       chownFor(opt, remoteDir, true),
	}, nil
}

// looseFilesJob copies the given top-level files into remoteDir in one transfer.
// Its chown pass stays at remoteDir's top level (no recursion), so it never
// rescans the subdirectories that other jobs already cover.
func looseFilesJob(opt Options, label string, files []string, remoteDir string) Job {
	return Job{
		Label:       label,
		Srcs:        files,
		Dst:         opt.Dest + ":" + ensureSlash(remoteDir),
		Excludes:    opt.RsyncExclude,
		RemoteShell: opt.SSH.RsyncRemoteShell(),
		Chown:       chownFor(opt, remoteDir, false),
	}
}

// fileJobs copies the individual files in opt.Files to their matching absolute
// path on the destination. Files sharing a parent directory are grouped into a
// single transfer that lists them as explicit rsync sources, so the
// destination parent directory's own owner and mode are never rewritten and
// --delete can't prune the directory's other entries — both important when
// copying into a shared system directory like /etc. These are system files,
// owned by root, so no per-user chown pass is attached even on a cross-username
// migration.
func fileJobs(opt Options) []Job {
	byDir := map[string][]string{}
	var order []string
	for _, f := range opt.Files {
		f = filepath.Clean(f)
		remoteDir := path.Dir(RemoteDirPath(opt.Root, opt.RemoteRoot, f))
		if _, seen := byDir[remoteDir]; !seen {
			order = append(order, remoteDir)
		}
		byDir[remoteDir] = append(byDir[remoteDir], f)
	}

	var jobs []Job
	for _, remoteDir := range order {
		files := byDir[remoteDir]
		sort.Strings(files)
		// Label by the parent's path relative to Root (e.g. "etc"), so it stays
		// stable even when the local root is a temporary test directory.
		rel := strings.TrimPrefix(RemoteDirPath(opt.Root, "/", path.Dir(files[0])), "/")
		jobs = append(jobs, Job{
			Label:       rel + " (files)",
			Srcs:        files, // explicit file sources: never a trailing-slash dir
			Dst:         opt.Dest + ":" + ensureSlash(remoteDir),
			Excludes:    opt.RsyncExclude,
			RemoteShell: opt.SSH.RsyncRemoteShell(),
		})
	}
	sortJobs(jobs)
	return jobs
}

// appJobs lists the destination's /Applications, then creates one sudo-rsync
// job per local .app that is not already present.
func appJobs(ctx context.Context, opt Options) ([]Job, []string, error) {
	existing, err := opt.SSH.remoteList(ctx, opt.Dest, opt.remoteAppsDir())
	if err != nil {
		return nil, nil, fmt.Errorf("listing %s on %s: %w", opt.remoteAppsDir(), opt.Dest, err)
	}
	if opt.Debug {
		names := make([]string, 0, len(existing))
		for n := range existing {
			names = append(names, n)
		}
		sort.Strings(names)
		Debugf(true, "apps: remote %s on %s has %d entries: %s",
			opt.remoteAppsDir(), opt.Dest, len(names), strings.Join(names, ", "))
	}
	entries, err := os.ReadDir(opt.appsDir())
	if err != nil {
		return nil, nil, err
	}
	var jobs []Job
	var notes []string
	ignored := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".app") {
			ignored++
			Debugf(opt.Debug, "apps: not a .app, ignoring: %s", name)
			continue
		}
		if existing[name] {
			Debugf(opt.Debug, "apps: exists on destination, skipping: %s", name)
			notes = append(notes, "SKIP (exists): "+name)
			continue
		}
		Debugf(opt.Debug, "apps: will copy: %s", name)
		jobs = append(jobs, Job{
			Label:       "App/" + strings.TrimSuffix(name, ".app"),
			Srcs:        []string{filepath.Join(opt.appsDir(), name)}, // no trailing slash: copy the bundle itself
			Dst:         opt.Dest + ":" + ensureSlash(opt.remoteAppsDir()),
			Excludes:    opt.RsyncExclude,
			RemoteShell: opt.SSH.RsyncRemoteShell(),
			Chown:       chownFor(opt, opt.remoteAppsDir()+"/"+name, true), // scoped to the copied bundle
		})
	}
	Debugf(opt.Debug, "apps: local %s has %d entries (%d non-.app ignored): %d to copy, %d skipped",
		opt.appsDir(), len(entries), ignored, len(jobs), len(notes))
	sortJobs(jobs)
	return jobs, notes, nil
}

func ensureSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

func sortJobs(jobs []Job) {
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].Label < jobs[k].Label })
}

// deepestFirst orders roots so that deeper paths come first, so a nested
// directory's jobs are dispatched before its parent's.
func deepestFirst(roots []root) {
	sort.Slice(roots, func(i, k int) bool {
		di, dk := strings.Count(roots[i].local, "/"), strings.Count(roots[k].local, "/")
		if di != dk {
			return di > dk
		}
		return roots[i].local < roots[k].local
	})
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
