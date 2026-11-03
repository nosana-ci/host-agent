package solana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func rpc(t *testing.T, answer string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "getBalance" || req.Params[0] != "Addr" {
			t.Errorf("unexpected request %+v (%v)", req, err)
		}
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestBalance(t *testing.T) {
	sol, err := Balance(context.Background(), rpc(t, `{"jsonrpc":"2.0","result":{"context":{"slot":1},"value":4500000},"id":1}`), "Addr")
	if err != nil || sol != 0.0045 {
		t.Fatalf("Balance = %v, %v; want 0.0045", sol, err)
	}
}

func TestBalanceRefusedSaysWhy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":403,"message":"Your IP or provider is blocked from this endpoint"},"id":1}`))
	}))
	defer srv.Close()
	_, err := Balance(context.Background(), srv.URL, "Addr")
	if err == nil || err.Error() != "solana rpc: Your IP or provider is blocked from this endpoint" {
		t.Fatalf("err = %v", err)
	}
}

func TestBalanceRPCError(t *testing.T) {
	_, err := Balance(context.Background(), rpc(t, `{"jsonrpc":"2.0","error":{"code":-32602,"message":"Invalid param"},"id":1}`), "Addr")
	if err == nil || err.Error() != "solana rpc: Invalid param" {
		t.Fatalf("err = %v", err)
	}
}
