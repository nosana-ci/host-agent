// Package termlines turns what a program drew on a terminal into the lines it left there: the
// node's log holds every frame of every spinner, and as text a line is what it showed last.
package termlines

import (
	"bytes"
	"io"
)

const maxLine = 16 << 10 // a longer line is cut

// Writer writes the lines of the terminal output written to it, each when it ends. A line that
// was redrawn in place (a spinner, a progress bar) is its last drawing.
type Writer struct {
	// Open leaves an unfinished last line without its newline on Close, for a live view that
	// goes on from there.
	Open bool

	w     io.Writer
	color bool
	line  []byte // the line being drawn
	home  bool   // the cursor went back to the first column: text replaces the line
	esc   []byte // an escape sequence that is not complete yet
}

// NewWriter returns a Writer on w. With color the lines keep their colors; every other escape
// sequence is dropped.
func NewWriter(w io.Writer, color bool) *Writer { return &Writer{w: w, color: color} }

func (t *Writer) Write(p []byte) (int, error) {
	for _, b := range p {
		switch {
		case len(t.esc) > 0:
			t.esc = append(t.esc, b)
			if t.esc[1] != '[' || b >= 0x40 && len(t.esc) > 2 { // ESC and one byte, or ESC [ up to a letter
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
	if len(t.line) == 0 || t.Open {
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
	if newline != "" && bytes.IndexByte(t.line, 0x1b) >= 0 {
		t.line = append(t.line, "\x1b[0m"...)
	}
	_, err := t.w.Write(append(t.line, newline...))
	t.line, t.home = t.line[:0], false
	return err
}

func (t *Writer) escape(seq []byte) {
	if seq[1] != '[' {
		return
	}
	switch arg := string(seq[2 : len(seq)-1]); seq[len(seq)-1] {
	case 'm':
		if t.color {
			t.text(seq...)
		}
	case 'G': // to a column: ora's frames start at the first
		t.home = arg == "" || arg == "1"
	case 'K': // erase the line: all of it, or from the cursor on
		if arg == "2" || t.home {
			t.line = t.line[:0]
		}
	}
}
