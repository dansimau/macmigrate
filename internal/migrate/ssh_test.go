package migrate

import (
	"reflect"
	"testing"
)

func TestSSHPrefix(t *testing.T) {
	cases := []struct {
		name string
		ssh  SSH
		want []string
	}{
		{"plain", SSH{}, []string{"ssh"}},
		{"as user", SSH{User: "alice"}, []string{"sudo", "-E", "-u", "alice", "ssh"}},
		{"identity", SSH{Identity: "/k"}, []string{"ssh", "-i", "/k", "-o", "IdentitiesOnly=yes"}},
		{"batch", SSH{Identity: "/k", BatchMode: true}, []string{"ssh", "-i", "/k", "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes"}},
		{"control path", SSH{ControlPath: "/tmp/ctl", TTY: true},
			[]string{"ssh", "-o", "ControlPath=/tmp/ctl", "-t"}},
		{"tty as user with identity", SSH{User: "bob", Identity: "/k", TTY: true},
			[]string{"sudo", "-E", "-u", "bob", "ssh", "-i", "/k", "-o", "IdentitiesOnly=yes", "-t"}},
	}
	for _, tc := range cases {
		if got := tc.ssh.prefix(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: prefix() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRsyncRemoteShell(t *testing.T) {
	cases := []struct {
		name string
		ssh  SSH
		want string
	}{
		{"default", SSH{}, ""},
		{"as user", SSH{User: "alice"}, "sudo -E -u alice ssh"},
		{"identity only", SSH{Identity: "/k"}, "ssh -i /k -o IdentitiesOnly=yes"},
		// A pty must never leak into rsync's remote shell, even if TTY is set.
		{"tty is dropped", SSH{User: "alice", Identity: "/k", TTY: true},
			"sudo -E -u alice ssh -i /k -o IdentitiesOnly=yes"},
		// BatchMode carries over: a transfer connection that can't authenticate
		// must fail instead of freezing a parallel job on a password prompt.
		{"batchmode is kept", SSH{User: "alice", BatchMode: true},
			"sudo -E -u alice ssh -o BatchMode=yes"},
	}
	for _, tc := range cases {
		if got := tc.ssh.RsyncRemoteShell(); got != tc.want {
			t.Errorf("%s: RsyncRemoteShell() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

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
