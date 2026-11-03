package manifest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestVerify(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	raw := []byte(`{"version":"1.2.0","agent":{"artifacts":{}},"runtime":{"version":"5.8.8","artifacts":{}},"node":{"image":"x"}}`)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))

	m, err := verify(raw, sig, pub)
	if err != nil || m.Version != "1.2.0" || m.Runtime.Version != "5.8.8" {
		t.Fatalf("got %+v, %v", m, err)
	}
	if _, err := verify(append(raw, ' '), sig, pub); err == nil {
		t.Fatal("a changed manifest verified")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := verify(raw, sig, other); err == nil {
		t.Fatal("another key verified")
	}
	if _, err := verify(raw, "not base64!", pub); err == nil {
		t.Fatal("garbage verified")
	}
}
