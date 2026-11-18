// Package termlines turns what a program drew on a terminal into the lines it left there. The
// node runs on a terminal, so its log holds every frame of every spinner and progress bar; read
// back as text, a line is what it showed last.
package termlines

import (
	"bytes"
	"io"
	"strconv"
)

const (
	maxRows = 64       // rows a redraw can reach back over
	maxLine = 16 << 10 // a longer line is cut
)

// Writer writes the lines of the terminal output written to it. A line is written once the
// output has moved below it, where a redraw cannot reach it anymore; Close writes the rest.
type Writer struct {
	// Open leaves an unfinished last line without its newline on Close, for output that goes on
	// from there on a terminal: a running spinner redraws that line, a question stays one.
	Open bool

	w     io.Writer
	color bool
	// rows are the lines not written yet, the cursor on rows[row]. A nil row holds nothing: it
	// was erased, or it is one of the already written lines a redraw moved back up over.
	rows [][]byte
	row  int
	home bool   // the cursor went back to the first column: text replaces the row
	esc  []byte // an escape sequence that is not complete yet
}

// NewWriter returns a Writer on w. With color the lines keep their colors; every other escape
// sequence is dropped either way.
func NewWriter(w io.Writer, color bool) *Writer {
	return &Writer{w: w, color: color, rows: [][]byte{{}}}
}

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
			if err := t.down(); err != nil {
				return 0, err
			}
		case b >= 0x20 || b == '\t':
			t.text(b)
		}
	}
	return len(p), nil
}

// Close writes the lines still held: the ones a redraw could reach, and an unfinished last line.
func (t *Writer) Close() error {
	n := len(t.rows) - 1
	for t.Open && n > 0 && t.rows[n] == nil {
		n-- // erased rows below the last line, as under a spinner that wrapped
	}
	last := t.rows[n]
	if len(last) == 0 || t.Open {
		t.rows = t.rows[:n] // the empty line after the last newline is not a line
	}
	err := t.flush()
	if err == nil && t.Open {
		_, err = t.w.Write(last)
	}
	t.rows, t.row = [][]byte{{}}, 0
	return err
}

func (t *Writer) text(b ...byte) {
	if t.home {
		t.rows[t.row], t.home = t.rows[t.row][:0], false
	}
	if len(t.rows[t.row]) < maxLine {
		t.rows[t.row] = append(t.rows[t.row], b...)
	}
}

// down is a newline. On the last row the output moves on: the rows above are final.
func (t *Writer) down() error {
	t.home = true
	if t.row < len(t.rows)-1 {
		t.row++
		return nil
	}
	if t.rows[t.row] == nil {
		t.rows[t.row] = []byte{} // an empty line, not an erased one
	}
	err := t.flush()
	t.rows, t.row = append(t.rows[:0], []byte{}), 0
	return err
}

func (t *Writer) flush() error {
	var out bytes.Buffer
	for _, r := range t.rows {
		if r == nil {
			continue
		}
		out.Write(r)
		if t.color && bytes.IndexByte(r, 0x1b) >= 0 {
			out.WriteString("\x1b[0m")
		}
		out.WriteByte('\n')
	}
	_, err := t.w.Write(out.Bytes())
	return err
}

// up moves the cursor n rows up. Above the rows held are lines already written, which the
// program now redraws: they are held again, and written again in their new state.
func (t *Writer) up(n int) {
	if over := min(n-t.row, maxRows-len(t.rows)); over > 0 {
		t.rows = append(make([][]byte, over, over+len(t.rows)), t.rows...)
		t.row += over
	}
	t.row = max(t.row-n, 0)
}

func (t *Writer) escape(seq []byte) {
	if len(seq) < 3 || seq[1] != '[' {
		return
	}
	final, n := seq[len(seq)-1], 0
	if v, err := strconv.Atoi(string(seq[2 : len(seq)-1])); err == nil {
		n = v
	}
	switch final {
	case 'm':
		if t.color {
			t.text(seq...)
		}
	case 'G': // to a column
		t.home = n <= 1
	case 'K': // erase in the line
		if n == 2 || n == 0 && t.home {
			t.rows[t.row] = nil
		}
	case 'J': // erase to the end of the screen
		if n == 0 && t.home {
			t.rows[t.row] = nil
			t.rows = t.rows[:t.row+1]
		}
	case 'A', 'F':
		t.up(max(n, 1))
		t.home = t.home || final == 'F'
	case 'B', 'E':
		t.row = min(t.row+max(n, 1), len(t.rows)-1)
		t.home = t.home || final == 'E'
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
