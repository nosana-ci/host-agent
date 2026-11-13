// Package keys creates and reads the node's Solana wallet, which the agent signs host-manager
// requests with.
package keys

import (
	"bytes"
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
	file, err := os.ReadFile(path)
	if err == nil {
		key, _, err := decode(file)
		if err != nil {
			return nil, fmt.Errorf("wallet %s: %w", path, err)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return key, store.WriteFile(path, raw(key), 0o600)
}

// Read reads a keypair file someone else made, and checks that its public half belongs to its
// seed.
func Read(path string) (ed25519.PrivateKey, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, public, err := decode(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !bytes.Equal(public, key.Public().(ed25519.PublicKey)) {
		return nil, fmt.Errorf("%s: its public half does not belong to its private key", path)
	}
	return key, nil
}

// Import copies the keypair file at src to dst. A different key already at dst is never replaced.
func Import(src, dst string) (ed25519.PrivateKey, error) {
	key, err := Read(src)
	if err != nil {
		return nil, err
	}
	if old, err := os.ReadFile(dst); err == nil {
		if have, _, err := decode(old); err != nil || !have.Equal(key) {
			return nil, fmt.Errorf("%s already holds another key; it is not replaced", dst)
		}
		return key, nil
	}
	return key, store.WriteFile(dst, raw(key), 0o600)
}

// raw is a key as a Solana keypair file: its 64 bytes as a JSON array.
func raw(key ed25519.PrivateKey) []byte {
	nums := make([]int, len(key))
	for i, c := range key {
		nums[i] = int(c)
	}
	out, _ := json.Marshal(nums)
	return out
}

// decode reads a keypair file: the key from its seed, and the public half the file states.
func decode(file []byte) (ed25519.PrivateKey, []byte, error) {
	var nums []int
	if json.Unmarshal(file, &nums) != nil || len(nums) != 64 {
		return nil, nil, errors.New("not a Solana keypair file")
	}
	b := make([]byte, 64)
	for i, n := range nums {
		if n < 0 || n > 255 {
			return nil, nil, errors.New("not a Solana keypair file")
		}
		b[i] = byte(n)
	}
	return ed25519.NewKeyFromSeed(b[:ed25519.SeedSize]), b[ed25519.SeedSize:], nil
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
