package solana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func rpc(t *testing.T, answer string) string {
	return rpcFor(t, "getBalance", answer)
}

// rpcFor answers every request of the method with answer; another method fails the test.
func rpcFor(t *testing.T, method, answer string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != method || len(req.Params) > 0 && req.Params[0] != "Addr" {
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

func TestTokenBalanceSumsTheAccounts(t *testing.T) {
	account := func(amount string) string {
		return `{"account":{"data":{"parsed":{"info":{"tokenAmount":{"uiAmount":` + amount + `}}}}}}`
	}
	url := rpcFor(t, "getTokenAccountsByOwner", `{"jsonrpc":"2.0","result":{"context":{"slot":1},"value":[`+account("12.5")+`,`+account("0.25")+`]},"id":1}`)
	nos, err := TokenBalance(context.Background(), url, "Addr", "Mint")
	if err != nil || nos != 12.75 {
		t.Fatalf("TokenBalance = %v, %v; want 12.75", nos, err)
	}
	nos, err = TokenBalance(context.Background(), rpcFor(t, "getTokenAccountsByOwner", `{"jsonrpc":"2.0","result":{"context":{"slot":1},"value":[]},"id":1}`), "Addr", "Mint")
	if err != nil || nos != 0 {
		t.Fatalf("TokenBalance without accounts = %v, %v; want 0", nos, err)
	}
}

func TestRequestAirdropReturnsTheSignature(t *testing.T) {
	sig, err := RequestAirdrop(context.Background(), rpcFor(t, "requestAirdrop", `{"jsonrpc":"2.0","result":"5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW","id":1}`), "Addr", 1)
	if err != nil || !strings.HasPrefix(sig, "5VERv8") {
		t.Fatalf("RequestAirdrop = %q, %v", sig, err)
	}
}

func TestHealthy(t *testing.T) {
	if err := Healthy(context.Background(), rpcFor(t, "getHealth", `{"jsonrpc":"2.0","result":"ok","id":1}`)); err != nil {
		t.Fatal(err)
	}
	err := Healthy(context.Background(), rpcFor(t, "getHealth", `{"jsonrpc":"2.0","error":{"code":-32005,"message":"Node is behind by 42 slots"},"id":1}`))
	if err == nil || err.Error() != "solana rpc: Node is behind by 42 slots" {
		t.Fatalf("err = %v", err)
	}
}
