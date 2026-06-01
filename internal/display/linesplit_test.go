package display

import (
	"reflect"
	"testing"
)

func TestLineWriterSplitsOnCRandLF(t *testing.T) {
	var got []string
	w := NewLineWriter(func(s string) { got = append(got, s) })

	// rsync mixes '\r' progress updates with '\n' line breaks, sometimes split
	// across separate Write calls.
	w.Write([]byte("hello\r"))
	w.Write([]byte("wor"))
	w.Write([]byte("ld\nfoo"))
	w.Flush()

	want := []string{"hello", "world", "foo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestLineWriterSkipsEmptySegments(t *testing.T) {
	var got []string
	w := NewLineWriter(func(s string) { got = append(got, s) })
	w.Write([]byte("\r\n\r\n"))
	w.Flush()
	if len(got) != 0 {
		t.Fatalf("expected no lines, got %v", got)
	}
}
