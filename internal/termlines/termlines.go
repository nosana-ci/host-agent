// Package termlines turns what a program drew on a terminal into the lines it left there. The
// node runs on a terminal, so its log holds every frame of every spinner and progress bar; read
// back as text, a line is what it showed last.
package termlines

import (
	"bytes"
	"io"
	"strconv"
)

const maxLine = 16 << 10 // a longer line is cut

// Writer writes the lines of the terminal output written to it, each when it ends. A line that
// was redrawn in place (a spinner, a progress bar) is its last drawing. Several lines redrawn
// together with cursor moves, as the layers of an image pull, are written at each redraw.
type Writer struct {
	// Open leaves an unfinished last line without its newline on Close, for output that goes on
	// from there on a terminal: a running spinner redraws that line, a question stays one.
	Open bool

	w     io.Writer
	color bool
	line  []byte // the line being drawn
	home  bool   // the cursor went back to the first column: text replaces the line
	esc   []byte // an escape sequence that is not complete yet
}

// NewWriter returns a Writer on w. With color the lines keep their colors; every other escape
// sequence is dropped either way.
func NewWriter(w io.Writer, color bool) *Writer { return &Writer{w: w, color: color} }

func (t *Writer) Write(p []byte) (int, error) {
	for _, b := range p {
		switch {
		case len(t.esc) > 0:
			t.esc = append(t.esc, b)
			if escDone(t.esc) {
				t.escape(t.esc)
				t.esc = t.esc[:0]
			}
		case b == 0x1b:
			t.esc = append(t.esc, b)
		case b == '\r':
			t.home = true
		case b == '\n':
			if err := t.end("\n"); err != nil {
				return 0, err
			}
		case b >= 0x20 || b == '\t':
			t.text(b)
		}
	}
	return len(p), nil
}

// Close writes a last line that did not end.
func (t *Writer) Close() error {
	switch {
	case len(t.line) == 0:
		return nil
	case t.Open:
		return t.end("")
	}
	return t.end("\n")
}

func (t *Writer) text(b ...byte) {
	if t.home {
		t.line, t.home = t.line[:0], false
	}
	if len(t.line) < maxLine {
		t.line = append(t.line, b...)
	}
}

// end writes the line, with its colors closed.
func (t *Writer) end(newline string) error {
	if t.color && newline != "" && bytes.IndexByte(t.line, 0x1b) >= 0 {
		t.line = append(t.line, "\x1b[0m"...)
	}
	_, err := t.w.Write(append(t.line, newline...))
	t.line, t.home = t.line[:0], false
	return err
}

func (t *Writer) escape(seq []byte) {
	if len(seq) < 3 || seq[1] != '[' {
		return
	}
	n, _ := strconv.Atoi(string(seq[2 : len(seq)-1]))
	switch seq[len(seq)-1] {
	case 'm':
		if t.color {
			t.text(seq...)
		}
	case 'G': // to a column
		t.home = n <= 1
	case 'K': // erase in the line: all of it, or from the first column on
		if n == 2 || n == 0 && t.home {
			t.line = t.line[:0]
		}
	}
}

// escDone reports whether seq, which starts with ESC, is a complete escape sequence.
func escDone(seq []byte) bool {
	if len(seq) < 2 {
		return false
	}
	last := seq[len(seq)-1]
	switch seq[1] {
	case '[': // CSI: parameters, then a final byte
		return len(seq) > 2 && last >= 0x40 && last <= 0x7e
	case ']': // OSC: up to BEL or ESC \
		return last == 0x07 || last == '\\' && seq[len(seq)-2] == 0x1b || len(seq) > 256
	case '(', ')': // character set: one more byte
		return len(seq) == 3
	}
	return true
}
