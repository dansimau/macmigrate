package display

import "testing"

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		w    int
		want string
	}{
		{"hello", 0, "hello"},  // unknown width: no truncation
		{"hello", 10, "hello"}, // fits
		{"hello", 5, "hello"},  // exact
		{"hello", 4, "hel…"},   // cut to w-1 runes + ellipsis
		{"hi", 1, "…"},
		{"héllo", 4, "hél…"}, // multibyte runes counted, not bytes
	}
	for _, c := range cases {
		if got := truncate(c.in, c.w); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.w, got, c.want)
		}
	}
}
