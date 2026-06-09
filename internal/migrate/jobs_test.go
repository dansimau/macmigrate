package migrate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestJobArgs(t *testing.T) {
	j := Job{Srcs: []string{"/a/"}, Dst: "h:/b/", Excludes: []string{".DS_Store"}}
	got := j.Args(true)
	want := []string{
		"-aE", "--info=progress2", "--delete", "--rsync-path=sudo -n /usr/bin/rsync", "--dry-run",
		"--exclude=.DS_Store", "--", "/a/", "h:/b/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Args = %v\nwant %v", got, want)
	}
}

func TestJobArgsMultiSource(t *testing.T) {
	j := Job{Srcs: []string{"/r/.gitignore", "/r/README"}, Dst: "h:/r/"}
	got := j.Args(false)
	want := []string{
		"-aE", "--info=progress2", "--delete", "--rsync-path=sudo -n /usr/bin/rsync",
		"--", "/r/.gitignore", "/r/README", "h:/r/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Args = %v\nwant %v", got, want)
	}
}

// TestDefaultExcludeAuthorizedKeys guards the authorized_keys exclusion: copying
// the source's ~/.ssh/authorized_keys over the destination's would cut off ssh
// access mid-migration. The ~/.ssh transfer is rooted at .ssh/, so the pattern
// must be a bare basename to match.
func TestDefaultExcludeAuthorizedKeys(t *testing.T) {
	found := false
	for _, p := range DefaultRsyncExclude {
		if p == "authorized_keys" {
			found = true
		}
	}
	if !found {
		t.Fatalf("DefaultRsyncExclude = %v, missing %q", DefaultRsyncExclude, "authorized_keys")
	}

	j := Job{Srcs: []string{"/h/.ssh/"}, Dst: "h:/h/.ssh/", Excludes: DefaultRsyncExclude}
	for _, arg := range j.Args(false) {
		if arg == "--exclude=authorized_keys" {
			return
		}
	}
	t.Fatalf("Args = %v, missing --exclude=authorized_keys", j.Args(false))
}

func TestSplitRoot(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"bin", "Cellar"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"README", ".gitignore"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := root{local: dir, remote: dir, labelPrefix: "p", rootLabel: "p"}
	isRoot := map[string]bool{filepath.Join(dir, "Cellar"): true} // Cellar handled by its own entry
	jobs, err := splitRoot(Options{Dest: "h", RsyncExclude: DefaultRsyncExclude}, r, isRoot)
	if err != nil {
		t.Fatal(err)
	}

	// bin subdir job + one loose-files job; Cellar excluded.
	if got := labelsOf(jobs); !reflect.DeepEqual(got, []string{"p/bin", "p (files)"}) {
		t.Fatalf("labels = %v", got)
	}

	bin := jobs[0]
	if !reflect.DeepEqual(bin.Srcs, []string{dir + "/bin/"}) || bin.Dst != "h:"+dir+"/bin/" {
		t.Errorf("bin job = %+v", bin)
	}

	files := jobs[1]
	if !reflect.DeepEqual(files.Srcs, []string{dir + "/.gitignore", dir + "/README"}) {
		t.Errorf("loose-files Srcs = %v", files.Srcs)
	}
	if files.Dst != "h:"+dir+"/" {
		t.Errorf("loose-files Dst = %q", files.Dst)
	}
}

func TestBuildJobsNesting(t *testing.T) {
	parent := t.TempDir()
	for _, d := range []string{"bin", "Cellar/foo", "Cellar/bar"} {
		if err := os.MkdirAll(filepath.Join(parent, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cellar := filepath.Join(parent, "Cellar")

	opt := Options{Dest: "h", RsyncExclude: DefaultRsyncExclude, DoDirs: true, Dirs: []string{parent, cellar}}
	jobs, _, err := BuildJobs(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}

	relParent := strings.TrimPrefix(parent, "/")
	relCellar := strings.TrimPrefix(cellar, "/")
	// Cellar is deeper, so its jobs come first; the parent skips Cellar.
	want := []string{
		relCellar + "/bar", relCellar + "/foo",
		relParent + "/bin", relParent + " (files)",
	}
	if got := labelsOf(jobs); !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v\nwant %v", got, want)
	}
	for _, l := range labelsOf(jobs) {
		if l == relParent+"/Cellar" {
			t.Errorf("parent should not also sync Cellar")
		}
	}
}

func TestBuildJobsHomeSplitsLibraryAndSkips(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{"Documents", "Library/Mail", "Library/Caches", ".Trash"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	opt := Options{
		Dest: "h", Home: home, RemoteHome: "/remote",
		SkipNames: DefaultSkip, RsyncExclude: DefaultRsyncExclude, DoHome: true,
	}
	jobs, notes, err := BuildJobs(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %v", notes)
	}

	// Library (deeper) first; .Trash and Library/Caches excluded; loose files
	// (notes.txt) collapse into one "home (files)" job.
	want := []string{"Library/Mail", "Documents", "home (files)"}
	if got := labelsOf(jobs); !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for _, j := range jobs {
		if !reflect.DeepEqual(j.Excludes, DefaultRsyncExclude) {
			t.Errorf("job %s excludes = %v, want defaults", j.Label, j.Excludes)
		}
		if j.Chown != nil {
			t.Errorf("job %s Chown = %+v, want nil (no Options.ChownUID)", j.Label, j.Chown)
		}
	}
}

// TestBuildJobsSSHDirNotRewritten guards the shape of the ~/.ssh transfer: a
// trailing-slash directory source makes rsync (running as root on the
// destination) rewrite the destination ~/.ssh's own owner and mode. On a
// cross-username migration that re-owns it to the source uid, sshd's
// StrictModes then refuses authorized_keys — cutting off the migration's own
// ssh access before the chown pass can repair it (the repair needs a new ssh
// connection, which is exactly what just broke). The .ssh job must therefore
// list the directory's entries as explicit sources, which never touch the
// destination directory's own attributes. (rsync still applies --exclude to
// explicitly listed sources, so authorized_keys stays protected.)
func TestBuildJobsSSHDirNotRewritten(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".ssh/keys.d", "Documents", "Library"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".ssh/authorized_keys", ".ssh/config", ".ssh/id_test"} {
		if err := os.WriteFile(filepath.Join(home, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	opt := Options{
		Dest: "h", Home: home, RemoteHome: "/remote",
		SkipNames: DefaultSkip, RsyncExclude: DefaultRsyncExclude, DoHome: true,
		ChownUID: "501",
	}
	jobs, _, err := BuildJobs(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}

	var ssh *Job
	for i := range jobs {
		if jobs[i].Label == ".ssh" {
			ssh = &jobs[i]
		}
	}
	if ssh == nil {
		t.Fatalf("no .ssh job in %v", labelsOf(jobs))
	}

	wantSrcs := []string{
		filepath.Join(home, ".ssh/authorized_keys"), // rsync's --exclude drops it
		filepath.Join(home, ".ssh/config"),
		filepath.Join(home, ".ssh/id_test"),
		filepath.Join(home, ".ssh/keys.d"),
	}
	if !reflect.DeepEqual(ssh.Srcs, wantSrcs) {
		t.Errorf(".ssh job Srcs = %v\nwant explicit entries %v\n(a trailing-slash dir source rewrites the destination ~/.ssh's owner)", ssh.Srcs, wantSrcs)
	}
	if want := "h:/remote/.ssh/"; ssh.Dst != want {
		t.Errorf(".ssh job Dst = %q, want %q", ssh.Dst, want)
	}
	if want := (Chown{Path: "/remote/.ssh", UID: "501", Recurse: true}); ssh.Chown == nil || *ssh.Chown != want {
		t.Errorf(".ssh job Chown = %+v, want %+v", ssh.Chown, want)
	}

	// Ordinary subdirectories keep the trailing-slash contents transfer.
	for _, j := range jobs {
		if j.Label == "Documents" && !reflect.DeepEqual(j.Srcs, []string{filepath.Join(home, "Documents") + "/"}) {
			t.Errorf("Documents job Srcs = %v, want trailing-slash dir source", j.Srcs)
		}
	}
}

func TestBuildJobsChownPaths(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{"Documents", "Library/Mail"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	opt := Options{
		Dest: "h", SSH: SSH{User: "olduser"}, Home: home, RemoteHome: "/remote",
		SkipNames: DefaultSkip, RsyncExclude: DefaultRsyncExclude, DoHome: true,
		ChownUID: "501",
	}
	jobs, _, err := BuildJobs(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}

	// Subdir jobs scan their remote directory recursively; the loose-files job
	// stays at the top level so it never rescans the whole tree.
	want := map[string]Chown{
		"Library/Mail": {Path: "/remote/Library/Mail", UID: "501", Recurse: true},
		"Documents":    {Path: "/remote/Documents", UID: "501", Recurse: true},
		"home (files)": {Path: "/remote", UID: "501", Recurse: false},
	}
	for _, j := range jobs {
		if j.Chown == nil {
			t.Errorf("job %s: Chown = nil", j.Label)
			continue
		}
		if !reflect.DeepEqual(*j.Chown, want[j.Label]) {
			t.Errorf("job %s Chown = %+v, want %+v", j.Label, *j.Chown, want[j.Label])
		}
	}
}

// TestBuildJobsFiles covers the individual-file machinery (DefaultFiles, e.g.
// /etc/hosts): each file is copied to its matching path under RemoteRoot as an
// explicit rsync source, grouped one job per parent directory. The explicit
// source (no trailing slash) is what keeps --delete from pruning the
// destination directory's other entries; and these system files stay
// root-owned, so no chown pass is attached even when ChownUID is set.
func TestBuildJobsFiles(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	hosts := filepath.Join(etc, "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opt := Options{
		Dest: "h", Root: root, RemoteRoot: "/dst",
		RsyncExclude: DefaultRsyncExclude,
		DoFiles:      true, Files: []string{hosts},
		ChownUID: "501", // cross-username, yet system files must not be chowned
	}
	jobs, _, err := BuildJobs(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelsOf(jobs); !reflect.DeepEqual(got, []string{"etc (files)"}) {
		t.Fatalf("labels = %v, want [etc (files)]", got)
	}
	j := jobs[0]
	if !reflect.DeepEqual(j.Srcs, []string{hosts}) {
		t.Errorf("Srcs = %v, want [%s]", j.Srcs, hosts)
	}
	if want := "h:/dst/etc/"; j.Dst != want {
		t.Errorf("Dst = %q, want %q", j.Dst, want)
	}
	if j.Chown != nil {
		t.Errorf("Chown = %+v, want nil (system files are root-owned)", j.Chown)
	}
	for _, s := range j.Srcs {
		if strings.HasSuffix(s, "/") {
			t.Errorf("file source %q has a trailing slash; --delete would prune the destination dir", s)
		}
	}
}

func TestRemoteDirPath(t *testing.T) {
	cases := []struct {
		root, remoteRoot, localDir, want string
	}{
		{"", "", "/usr/local", "/usr/local"},                                 // production defaults: identity
		{"/", "/", "/Library/Fonts", "/Library/Fonts"},                       // explicit defaults: identity
		{"/tmp/src", "/tmp/dst", "/tmp/src/usr/local", "/tmp/dst/usr/local"}, // test roots: re-rooted
		{"/tmp/src", "/", "/tmp/src/Library/Fonts", "/Library/Fonts"},        // strip the local root only
	}
	for _, c := range cases {
		if got := RemoteDirPath(c.root, c.remoteRoot, c.localDir); got != c.want {
			t.Errorf("RemoteDirPath(%q, %q, %q) = %q, want %q", c.root, c.remoteRoot, c.localDir, got, c.want)
		}
	}
}

func labelsOf(jobs []Job) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.Label)
	}
	return out
}
