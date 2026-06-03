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
		"-aE", "--info=progress2", "--rsync-path=sudo -n rsync", "--dry-run",
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
		"-aE", "--info=progress2", "--rsync-path=sudo -n rsync",
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

func labelsOf(jobs []Job) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.Label)
	}
	return out
}
