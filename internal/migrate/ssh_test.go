package migrate

import "testing"

func TestChownCmd(t *testing.T) {
	c := &Chown{
		Paths:   []string{"/Users/new/My Docs", "/Users/new/.zshrc"},
		SrcUser: "olduser",
		SrcUID:  "501",
		DstUser: "newuser",
	}
	got := chownCmd(c)
	want := `if id -u olduser >/dev/null 2>&1; then U=olduser; else U=501; fi; ` +
		`sudo -n find '/Users/new/My Docs' /Users/new/.zshrc -user "$U" -exec chown -h newuser {} +`
	if got != want {
		t.Fatalf("chownCmd =\n%s\nwant\n%s", got, want)
	}
}
