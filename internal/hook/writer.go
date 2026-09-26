package hook

import (
	"bytes"
	"io"
	"unicode/utf8"
)

// MaxLine is the longest line body a PrefixWriter emits. NZBGet reads the
// script's output with a fixed line buffer and treats an overlong line as
// several lines, so a longer body is continued on further prefixed lines
// here, where every line is checked, rather than cut by NZBGet at a point a
// hostile name could choose.
const MaxLine = 8000

// controlTag opens the lines NZBGet executes as commands (MARK=BAD,
// DIRECTORY=, NZBPR_...). Only Control may write a line that begins with it.
const controlTag = "[NZB]"

// neutralized precedes any line body that begins with the control tag but
// was written through Write, so a file name, a tag value or a message
// containing a newline can never become a command for NZBGet.
const neutralized = "(not a command) "

// PrefixWriter prefixes every complete line written through it. NZBGet reads
// stdout line by line and classifies lines by a leading [INFO], [WARNING],
// [ERROR], [DETAIL] or [DEBUG] tag, so the hook wraps its writers once and
// every print site stays unchanged. A line written through Write that would
// start with "[NZB]" is marked as not a command; only Control emits one.
type PrefixWriter struct {
	w      io.Writer
	prefix string
	buf    []byte
}

// NewPrefixWriter returns a writer that prefixes each line with prefix.
func NewPrefixWriter(w io.Writer, prefix string) *PrefixWriter {
	return &PrefixWriter{w: w, prefix: prefix}
}

// Write emits every complete line in b with the prefix and buffers a
// partial trailing line until the next Write or Flush. A partial line that
// grows past MaxLine is emitted in MaxLine pieces.
func (p *PrefixWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := p.buf[:i]
		rest := p.buf[i+1:]
		if err := p.emit(line); err != nil {
			p.buf = append([]byte(nil), rest...)
			return len(b), err
		}
		p.buf = append([]byte(nil), rest...)
	}
	for len(p.buf) > MaxLine {
		n := cut(p.buf, MaxLine)
		if err := p.emit(p.buf[:n]); err != nil {
			p.buf = append([]byte(nil), p.buf[n:]...)
			return len(b), err
		}
		p.buf = append([]byte(nil), p.buf[n:]...)
	}
	return len(b), nil
}

// Flush writes a pending partial line with the prefix and a newline. It
// writes nothing when no partial line is pending.
func (p *PrefixWriter) Flush() error {
	if len(p.buf) == 0 {
		return nil
	}
	line := p.buf
	p.buf = nil
	return p.emit(line)
}

// Control writes a control line for NZBGet, verbatim after the prefix. Any
// pending partial line is flushed first so the control line stands alone.
func (p *PrefixWriter) Control(line string) error {
	if err := p.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(p.w, p.prefix+line+"\n")
	return err
}

// emit writes one line body, split into MaxLine pieces when longer, each
// with the prefix and, when the body begins with the control tag, the
// neutralizing marker.
func (p *PrefixWriter) emit(line []byte) error {
	for {
		n := len(line)
		if n > MaxLine {
			n = cut(line, MaxLine)
		}
		out := make([]byte, 0, len(p.prefix)+len(neutralized)+n+1)
		out = append(out, p.prefix...)
		if hasTag(line[:n]) {
			out = append(out, neutralized...)
		}
		out = append(out, line[:n]...)
		out = append(out, '\n')
		if _, err := p.w.Write(out); err != nil {
			return err
		}
		line = line[n:]
		if len(line) == 0 {
			return nil
		}
	}
}

// hasTag reports whether the body begins with the control tag, ignoring
// case so that no spelling of it slips through.
func hasTag(body []byte) bool {
	return len(body) >= len(controlTag) && bytes.EqualFold(body[:len(controlTag)], []byte(controlTag))
}

// cut returns the largest n <= max that does not split a UTF-8 sequence in
// b, or max when b is not valid UTF-8 there.
func cut(b []byte, max int) int {
	n := max
	for k := 0; k < utf8.UTFMax && n > 0; k++ {
		if utf8.RuneStart(b[n]) {
			return n
		}
		n--
	}
	return max
}
