package keys

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWalletIsCreatedOnceAndReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node", "nosana_key.json")
	first, err := Wallet(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Wallet(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(again) {
		t.Fatal("the second read returned another key")
	}
	raw, _ := os.ReadFile(path)
	var nums []int
	if json.Unmarshal(raw, &nums) != nil || len(nums) != 64 {
		t.Fatalf("not a Solana keypair file: %s", raw)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("wallet mode %o, want 0600", info.Mode().Perm())
	}
}

func TestDeviceKeyIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device_key")
	a, _ := Device(path)
	b, _ := Device(path)
	if !a.Equal(b) {
		t.Fatal("device key changed between reads")
	}
	if PublicBase64(a) == "" || len(a.Public().(ed25519.PublicKey)) != 32 {
		t.Fatal("bad public key")
	}
}

func TestBase58(t *testing.T) {
	// The Solana system program: 32 zero bytes.
	if got := Base58(make([]byte, 32)); got != "11111111111111111111111111111111" {
		t.Fatalf("got %s", got)
	}
	if got := Base58([]byte{0, 0, 1}); got != "112" {
		t.Fatalf("got %s", got)
	}
}
