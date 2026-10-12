package docker

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestReadLinesMultiplexed(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, "hello\nwor"))
	in.Write(frame(2, "oops\r\n"))
	in.Write(frame(1, "ld\nlast without newline"))

	var got []string
	if err := ReadLines(&in, false, func(s, l string) { got = append(got, s+":"+l) }); err != nil {
		t.Fatal(err)
	}
	want := []string{"stdout:hello", "stderr:oops", "stdout:world", "stdout:last without newline"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestReadLinesTruncatedFrame(t *testing.T) {
	b := frame(1, "complete\nhalf")
	b = append(b, frame(1, "this frame is cut")[:12]...) // header + 4 bytes of a 17-byte payload
	var got []string
	if err := ReadLines(bytes.NewReader(b), false, func(_, l string) { got = append(got, l) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "complete" || got[1] != "halfthis" {
		t.Errorf("%q", got)
	}
}

func TestReadLinesTTYAndLongLines(t *testing.T) {
	long := strings.Repeat("x", maxLine+100)
	var got []string
	ReadLines(strings.NewReader("a\r\n"+long+"\nb"), true, func(s, l string) { got = append(got, l) })
	if len(got) != 3 || got[0] != "a" || len(got[1]) != maxLine || got[2] != "b" {
		t.Errorf("%d lines, first %q", len(got), got[0])
	}
}
