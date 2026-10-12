package docker

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

const maxLine = 64 << 10 // longer lines are cut (a runaway log must not eat memory)

// ReadLines splits a container log stream into lines and calls fn with the stream name
// ("stdout"/"stderr"). Without a TTY, Docker multiplexes both streams into frames with an
// 8-byte header (stream, 0, 0, 0, size uint32 BE); a line can span frames, so each stream
// keeps its own partial line. With a TTY the stream is raw stdout.
func ReadLines(r io.Reader, tty bool, fn func(stream, line string)) error {
	if tty {
		return scanLines(r, func(l string) { fn("stdout", l) })
	}
	partial := map[byte]*bytes.Buffer{1: {}, 2: {}}
	emit := func(stream byte, b []byte) {
		buf := partial[stream]
		if buf == nil {
			return // stdin (0) or unknown: ignore
		}
		for len(b) > 0 {
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				room := maxLine - buf.Len()
				if room > 0 {
					buf.Write(b[:min(room, len(b))])
				}
				return
			}
			room := maxLine - buf.Len()
			if room > 0 {
				buf.Write(b[:min(room, i)])
			}
			fn(streamName(stream), trimCR(buf.String()))
			buf.Reset()
			b = b[i+1:]
		}
	}
	br := bufio.NewReaderSize(r, 32<<10)
	hdr := make([]byte, 8)
	frame := make([]byte, 0, 32<<10)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			flush(partial, fn)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		size := int(binary.BigEndian.Uint32(hdr[4:]))
		if cap(frame) < size {
			frame = make([]byte, size)
		}
		frame = frame[:size]
		if n, err := io.ReadFull(br, frame); err != nil {
			emit(hdr[0], frame[:n])
			flush(partial, fn)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		emit(hdr[0], frame)
	}
}

func flush(partial map[byte]*bytes.Buffer, fn func(stream, line string)) {
	for _, s := range []byte{1, 2} {
		if b := partial[s]; b.Len() > 0 {
			fn(streamName(s), trimCR(b.String()))
			b.Reset()
		}
	}
}

func scanLines(r io.Reader, fn func(string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if len(l) > maxLine {
			l = l[:maxLine]
		}
		fn(trimCR(l))
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func streamName(b byte) string {
	if b == 2 {
		return "stderr"
	}
	return "stdout"
}

func trimCR(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\r' {
		return s[:n-1]
	}
	return s
}
