package node

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublic(t *testing.T) {
	answer := func(contentType, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	ctx := context.Background()

	info, err := Public(ctx, answer("application/json", `{"node":"A","state":"RUNNING_JOB"}`), "A")
	if err != nil || !Busy(info.State) {
		t.Fatalf("the node's own answer: %+v, %v", info, err)
	}
	if _, err := Public(ctx, answer("application/json", `{"node":"B"}`), "A"); err == nil {
		t.Fatal("another node's answer was taken for this one")
	}
	if _, err := Public(ctx, answer("text/html", "<html>starting</html>"), "A"); err == nil {
		t.Fatal("the tunnel's placeholder page was taken for the node")
	}
}
