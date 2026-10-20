package dockerlogsreceiver

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// logLine is one line of container output.
type logLine struct {
	stream string // stdout or stderr
	ts     time.Time
	text   string
}

// maxLineBytes bounds how much of one line is kept. Docker splits messages longer than 16 KiB
// into partial frames; they are joined back up to this size and the rest is dropped.
const maxLineBytes = 1 << 20

// readMultiplexed reads a non-TTY log stream. Every frame is an 8-byte header — stream type,
// three zero bytes, big-endian payload length — and then one log message.
func readMultiplexed(r io.Reader, emit func(logLine) error) error {
	a := newAssembler(emit)
	var hdr [8]byte
	buf := make([]byte, 32*1024)
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return a.flush()
			}
			return err
		}
		size := int(binary.BigEndian.Uint32(hdr[4:]))
		if size > len(buf) {
			buf = make([]byte, size)
		}
		payload := buf[:size]
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}
		stream := "stdout"
		switch hdr[0] {
		case 2:
			stream = "stderr"
		case 0:
			continue // stdin is never part of a log stream
		}
		if err := a.feed(stream, payload); err != nil {
			return err
		}
	}
}

// readRaw reads a TTY container's stream: no headers, one stream, lines as the terminal wrote
// them (\r\n endings included).
func readRaw(r io.Reader, emit func(logLine) error) error {
	a := newAssembler(emit)
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if ferr := a.feed("stdout", line); ferr != nil {
				return ferr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return a.flush()
			}
			return err
		}
	}
}

// assembler turns frames into lines. A frame normally holds one "<timestamp> <text>\n"
// message; a frame without the trailing newline is a partial message whose rest follows in
// the next frame of the same stream, carrying its own timestamp prefix, which is dropped.
type assembler struct {
	emit    func(logLine) error
	pending map[string]*logLine
}

func newAssembler(emit func(logLine) error) *assembler {
	return &assembler{emit: emit, pending: map[string]*logLine{}}
}

func (a *assembler) feed(stream string, payload []byte) error {
	for len(payload) > 0 {
		chunk := payload
		complete := false
		if i := bytes.IndexByte(payload, '\n'); i >= 0 {
			chunk, payload, complete = payload[:i], payload[i+1:], true
		} else {
			payload = nil
		}
		ts, text, hasTS := splitTimestamp(chunk)

		if p := a.pending[stream]; p != nil {
			if room := maxLineBytes - len(p.text); room > 0 {
				if len(text) > room {
					text = text[:room]
				}
				p.text += string(text)
			}
			if complete {
				delete(a.pending, stream)
				if err := a.send(*p); err != nil {
					return err
				}
			}
			continue
		}

		if !hasTS {
			ts = time.Time{}
		}
		if len(text) > maxLineBytes {
			text = text[:maxLineBytes]
		}
		l := logLine{stream: stream, ts: ts, text: string(text)}
		if complete {
			if err := a.send(l); err != nil {
				return err
			}
		} else {
			a.pending[stream] = &l
		}
	}
	return nil
}

// flush sends what is left when a stream ends mid-line (the container exited without a final
// newline).
func (a *assembler) flush() error {
	for s, p := range a.pending {
		delete(a.pending, s)
		if err := a.send(*p); err != nil {
			return err
		}
	}
	return nil
}

func (a *assembler) send(l logLine) error {
	l.text = trimCR(l.text)
	if l.text == "" {
		return nil
	}
	return a.emit(l)
}

func trimCR(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// splitTimestamp splits "2026-10-19T12:00:00.123456789Z text" into its time and text.
func splitTimestamp(b []byte) (time.Time, []byte, bool) {
	i := bytes.IndexByte(b, ' ')
	if i < 20 || i > 40 {
		return time.Time{}, b, false
	}
	ts, err := time.Parse(time.RFC3339Nano, string(b[:i]))
	if err != nil {
		return time.Time{}, b, false
	}
	return ts, b[i+1:], true
}
