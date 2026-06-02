package migrate

import "testing"

func TestChownCmd(t *testing.T) {
	cases := []struct {
		name string
		c    *Chown
		want string
	}{
		{
			name: "recursive with space in path",
			c:    &Chown{Path: "/Users/new/My Docs", UID: "501", Recurse: true},
			want: `sudo -n find '/Users/new/My Docs' -user 501 -exec chown -h "$(id -un)" {} +`,
		},
		{
			name: "top level only",
			c:    &Chown{Path: "/Users/new", UID: "501"},
			want: `sudo -n find /Users/new -maxdepth 1 -user 501 -exec chown -h "$(id -un)" {} +`,
		},
	}
	for _, tc := range cases {
		if got := chownCmd(tc.c); got != tc.want {
			t.Errorf("%s: chownCmd =\n%s\nwant\n%s", tc.name, got, tc.want)
		}
	}
}
