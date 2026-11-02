package podman

import (
	"bytes"
	"testing"
)

func TestDemuxUnwrapsLogFrames(t *testing.T) {
	stream := []byte{1, 0, 0, 0, 0, 0, 0, 6, 'h', 'e', 'l', 'l', 'o', '\n', 2, 0, 0, 0, 0, 0, 0, 4, 'o', 'o', 'p', 's'}
	var out bytes.Buffer
	if err := demux(bytes.NewReader(stream), &out); err != nil || out.String() != "hello\noops" {
		t.Fatalf("got %q, %v", out.String(), err)
	}
}
