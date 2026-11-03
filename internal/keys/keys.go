// Package keys creates and reads the node's Solana wallet, which the agent signs host-manager
// requests with.
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"

	"github.com/nosana-ci/host-agent/internal/store"
)

// Wallet reads the node wallet at path (a Solana keypair file: 64 bytes as a JSON array),
// creating one when there is none.
func Wallet(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		var nums []int
		if json.Unmarshal(raw, &nums) != nil || len(nums) != 64 {
			return nil, fmt.Errorf("wallet %s is not a Solana keypair file", path)
		}
		seed := make([]byte, ed25519.SeedSize)
		for i, n := range nums[:ed25519.SeedSize] {
			seed[i] = byte(n)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	nums := make([]int, 64)
	for i, c := range key {
		nums[i] = int(c)
	}
	out, _ := json.Marshal(nums)
	return key, store.WriteFile(path, out, 0o600)
}

// Address is a key's public half as Solana writes it.
func Address(key ed25519.PrivateKey) string {
	return Base58(key.Public().(ed25519.PublicKey))
}

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// Base58 encodes b with the Bitcoin alphabet, as Solana does.
func Base58(b []byte) string {
	n := new(big.Int).SetBytes(b)
	base, mod := big.NewInt(58), new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
