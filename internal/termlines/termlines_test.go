package termlines

import (
	"bytes"
	"testing"
)

const frame = "\x1b[1G\x1b[0K" // how ora starts a spinner's frame

func render(color, open bool, chunks ...string) string {
	var out bytes.Buffer
	w := NewWriter(&out, color)
	w.Open = open
	for _, c := range chunks {
		_, _ = w.Write([]byte(c))
	}
	_ = w.Close()
	return out.String()
}

func TestLines(t *testing.T) {
	for name, c := range map[string]struct{ in, want string }{
		"plain lines":                 {"one\r\n\r\ntwo\n", "one\n\ntwo\n"},
		"a spinner is its last frame": {"a\r\n\x1b[?25l" + frame + "⠋ Pull" + frame + "⠙ Pull" + frame + "\x1b[32m✔\x1b[39m Pulled\r\nb\r\n", "a\n✔ Pulled\nb\n"},
		"a spinner that still runs":   {"a\r\n" + frame + "⠋ Wait" + frame + "⠙ Wait", "a\n⠙ Wait\n"},
		"a cleared spinner":           {frame + "⠋ Wait" + frame + "b\r\n", "b\n"},
		"a spinner over two rows":     {frame + "\x1b[1A\x1b[0K⠋ wide" + frame + "\x1b[1A\x1b[0K✔ ok\r\n", "✔ ok\n"},
		"carriage return redraws":     {"10%\r100%\r\n", "100%\n"},
	} {
		if got := render(false, false, c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		for i := 1; i < len(c.in); i++ { // the log arrives in arbitrary pieces
			if got := render(false, false, c.in[:i], c.in[i:]); got != c.want {
				t.Fatalf("%s, split at %d: got %q", name, i, got)
			}
		}
	}
}

func TestColorAndOpen(t *testing.T) {
	if got := render(true, false, frame+"\x1b[32m✔\x1b[39m ok\r\nplain\r\n"); got != "\x1b[32m✔\x1b[39m ok\x1b[0m\nplain\n" {
		t.Errorf("color: got %q", got)
	}
	if got := render(false, true, "one\r\n? Your Email Address "); got != "one\n? Your Email Address " {
		t.Errorf("open: got %q", got)
	}
}
