// Package solana reads from the network's public RPC: the node wallet's SOL balance.
package solana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// RPCURL is the public RPC of the network the node runs on.
func RPCURL(network string) string {
	if network == "devnet" {
		return "https://api.devnet.solana.com"
	}
	return "https://api.mainnet-beta.solana.com"
}

// Balance is address's SOL balance at the confirmed commitment.
func Balance(ctx context.Context, url, address string) (float64, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getBalance",
		"params": []any{address, map[string]string{"commitment": "confirmed"}},
	})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("solana rpc: %s", resp.Status)
	}
	var out struct {
		Result *struct {
			Value uint64 `json:"value"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("solana rpc: %w", err)
	}
	if out.Error != nil {
		return 0, errors.New("solana rpc: " + out.Error.Message)
	}
	if out.Result == nil {
		return 0, errors.New("solana rpc: no result")
	}
	return float64(out.Result.Value) / 1e9, nil
}
