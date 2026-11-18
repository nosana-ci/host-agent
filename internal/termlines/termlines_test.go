package termlines

import (
	"bytes"
	"strings"
	"testing"
)

func render(color bool, chunks ...string) string {
	var out bytes.Buffer
	w := NewWriter(&out, color)
	for _, c := range chunks {
		_, _ = w.Write([]byte(c))
	}
	_ = w.Close()
	return out.String()
}

func TestLines(t *testing.T) {
	const frame = "\x1b[1G\x1b[0K"
	for name, c := range map[string]struct{ in, want string }{
		"plain lines, as from a container without a terminal": {"one\ntwo\n", "one\ntwo\n"},
		"terminal line ends and an empty line":                {"one\r\n\r\ntwo\r\n", "one\n\ntwo\n"},
		"a spinner is the line it ended as": {
			"start\r\n\x1b[?25l" + frame + "⠋ Pulling" + frame + "⠙ Pulling" + frame + "\x1b[32m✔\x1b[39m Pulled\r\n\x1b[?25hnext\r\n",
			"start\n✔ Pulled\nnext\n"},
		"a spinner that still runs is its last frame": {"a\r\n" + frame + "⠋ Waiting" + frame + "⠙ Waiting", "a\n⠙ Waiting\n"},
		"a spinner that was cleared leaves nothing":   {"a\r\n" + frame + "⠋ Waiting" + frame + "b\r\n", "a\nb\n"},
		"carriage return redraws":                     {"10%\r50%\r100%\r\ndone\r\n", "100%\ndone\n"},
		"a block of bars redrawn in place is written again in its last state": {
			"a 1\r\nb 1\r\n" + "\x1b[2A\x1b[2Ka 2\r\n\x1b[2Kb 2\r\n" + "\x1b[2A\x1b[2Ka 3\r\n\x1b[2Kb 3\r\n" + "done\r\n",
			"a 1\nb 1\na 3\nb 3\ndone\n"},
		"a wrapped spinner erased with cursor-up leaves no rows behind": {
			strings.Repeat(frame+"\x1b[1A\x1b[0K"+"⠋ a text wider than the terminal", 500) + frame + "\x1b[1A\x1b[0K" + "✔ ok\r\n",
			"✔ ok\n"},
		"other sequences are dropped": {"\x1b]0;title\x07\x1b(Bhi\x1b[10;5H\x1b[?2004h\x1b7!\r\n", "hi!\n"},
	} {
		if got := render(false, c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}

func TestColorIsKeptAndReset(t *testing.T) {
	if got := render(true, "\x1b[1G\x1b[0K\x1b[32m✔\x1b[39m ok\r\nplain\r\n"); got != "\x1b[32m✔\x1b[39m ok\x1b[0m\nplain\n" {
		t.Fatalf("got %q", got)
	}
}

// The log arrives in arbitrary pieces: a sequence cut in two is still one sequence.
func TestSplitWrites(t *testing.T) {
	in := "a\r\n\x1b[1G\x1b[0K⠋ Waiting\x1b[1G\x1b[0K\x1b[32m✔\x1b[39m Done\r\n"
	want := render(false, in)
	for i := 1; i < len(in); i++ {
		if got := render(false, in[:i], in[i:]); got != want {
			t.Fatalf("split at %d: got %q, want %q", i, got, want)
		}
	}
}

// A line is written as soon as the output is below it, so a followed log does not lag.
func TestLinesAreWrittenAsTheyEnd(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, false)
	_, _ = w.Write([]byte("one\r\ntw"))
	if out.String() != "one\n" {
		t.Fatalf("got %q", out.String())
	}
}

// Before a live view takes over, the unfinished line stays open for it.
func TestOpenLeavesTheLastLineUnfinished(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, false)
	w.Open = true
	_, _ = w.Write([]byte("one\r\n? Your Email Address "))
	_ = w.Close()
	if out.String() != "one\n? Your Email Address " {
		t.Fatalf("got %q", out.String())
	}

	// A spinner wider than the terminal is erased over two rows; it is still the open line.
	out.Reset()
	_, _ = w.Write([]byte("one\r\n\x1b[1G\x1b[0K\x1b[1A\x1b[0K⠋ wide\x1b[1G\x1b[0K\x1b[1A\x1b[0K⠙ wide"))
	_ = w.Close()
	if out.String() != "one\n⠙ wide" {
		t.Fatalf("got %q", out.String())
	}
}
