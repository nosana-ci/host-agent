// Package solana reads the node wallet's balances from the network's RPC, and asks the devnet
// faucet for SOL.
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

// NOSMint is the NOS token's mint on the network (the Nosana kit's defaults).
func NOSMint(network string) string {
	if network == "devnet" {
		return "devr1BGQndEW5k5zfvG5FsLyZv1Ap73vNgAHcQ9sUVP"
	}
	return "nosXBVoaCTtYdLvKY6Csb4AC8JCdQKKAaWYtx2ZMoo7"
}

const confirmed = "confirmed"

// Balance is address's SOL balance at the confirmed commitment.
func Balance(ctx context.Context, url, address string) (float64, error) {
	var out struct {
		Value uint64 `json:"value"`
	}
	if err := call(ctx, url, "getBalance", []any{address, map[string]string{"commitment": confirmed}}, &out); err != nil {
		return 0, err
	}
	return float64(out.Value) / 1e9, nil
}

// TokenBalance is what address holds of the token mint, over all its token accounts.
func TokenBalance(ctx context.Context, url, address, mint string) (float64, error) {
	var out struct {
		Value []struct {
			Account struct {
				Data struct {
					Parsed struct {
						Info struct {
							TokenAmount struct {
								UIAmount *float64 `json:"uiAmount"`
							} `json:"tokenAmount"`
						} `json:"info"`
					} `json:"parsed"`
				} `json:"data"`
			} `json:"account"`
		} `json:"value"`
	}
	params := []any{address, map[string]string{"mint": mint}, map[string]string{"encoding": "jsonParsed", "commitment": confirmed}}
	if err := call(ctx, url, "getTokenAccountsByOwner", params, &out); err != nil {
		return 0, err
	}
	total := 0.0
	for _, v := range out.Value {
		if a := v.Account.Data.Parsed.Info.TokenAmount.UIAmount; a != nil {
			total += *a
		}
	}
	return total, nil
}

// RequestAirdrop asks the faucet (devnet only) for sol SOL and returns the transaction signature.
func RequestAirdrop(ctx context.Context, url, address string, sol float64) (string, error) {
	var signature string
	err := call(ctx, url, "requestAirdrop", []any{address, uint64(sol * 1e9)}, &signature)
	return signature, err
}

// Healthy reports whether the RPC answers getHealth with "ok"; the error says what it answered.
func Healthy(ctx context.Context, url string) error {
	var health string
	if err := call(ctx, url, "getHealth", []any{}, &health); err != nil {
		return err
	}
	if health != "ok" {
		return fmt.Errorf("solana rpc: health %q", health)
	}
	return nil
}

// call sends one JSON-RPC request and decodes its result into out.
func call(ctx context.Context, url, method string, params []any, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var answer struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&answer)
	switch {
	case answer.Error != nil: // also on an HTTP error, which says why there
		return errors.New("solana rpc: " + answer.Error.Message)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("solana rpc: %s", resp.Status)
	case decodeErr != nil:
		return fmt.Errorf("solana rpc: %w", decodeErr)
	case len(answer.Result) == 0:
		return errors.New("solana rpc: no result")
	}
	return json.Unmarshal(answer.Result, out)
}
