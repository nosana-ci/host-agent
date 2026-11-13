package keys

import (
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

func TestBase58(t *testing.T) {
	// The Solana system program: 32 zero bytes.
	if got := Base58(make([]byte, 32)); got != "11111111111111111111111111111111" {
		t.Fatalf("got %s", got)
	}
	if got := Base58([]byte{0, 0, 1}); got != "112" {
		t.Fatalf("got %s", got)
	}
}

func TestImport(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "old.json"), filepath.Join(dir, "node", "nosana_key.json")
	key, err := Wallet(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Import(src, dst)
	if err != nil || !got.Equal(key) {
		t.Fatalf("import: %v", err)
	}
	if again, err := Wallet(dst); err != nil || !again.Equal(key) {
		t.Fatalf("the imported wallet reads back as another key: %v", err)
	}
	if _, err := Import(src, dst); err != nil {
		t.Fatalf("importing the same key again: %v", err)
	}

	other := filepath.Join(dir, "other.json")
	if _, err := Wallet(other); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(other, dst); err == nil {
		t.Fatal("another key replaced the wallet")
	}

	var nums []int
	raw, _ := os.ReadFile(src)
	_ = json.Unmarshal(raw, &nums)
	nums[63] ^= 1
	bad, _ := json.Marshal(nums)
	broken := filepath.Join(dir, "broken.json")
	_ = os.WriteFile(broken, bad, 0o600)
	if _, err := Import(broken, filepath.Join(dir, "fresh.json")); err == nil {
		t.Fatal("a keypair whose halves do not match was imported")
	}
}
