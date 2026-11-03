// Package solana reads the node wallet's SOL balance from the network's RPC.
package solana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// RPCURL is the RPC of the network the node runs on: the defaults of the Nosana kit
// (packages/kit/src/config/defaultConfigs.ts). On mainnet that is Nosana's Ironforge endpoint,
// whose key is public on purpose (scoped and rate limited); the public mainnet RPC refuses Go
// clients ("Your IP or provider is blocked from this endpoint").
func RPCURL(network string) string {
	if network == "devnet" {
		return "https://api.devnet.solana.com"
	}
	return "https://rpc.ironforge.network/mainnet?apiKey=01J4RYMAWZC65B6CND9DTZZ5BK"
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
	var out struct {
		Result *struct {
			Value uint64 `json:"value"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&out)
	switch {
	case out.Error != nil: // also on an HTTP error, which says why there
		return 0, errors.New("solana rpc: " + out.Error.Message)
	case resp.StatusCode != http.StatusOK:
		return 0, fmt.Errorf("solana rpc: %s", resp.Status)
	case decodeErr != nil:
		return 0, fmt.Errorf("solana rpc: %w", decodeErr)
	}
	if out.Result == nil {
		return 0, errors.New("solana rpc: no result")
	}
	return float64(out.Result.Value) / 1e9, nil
}
