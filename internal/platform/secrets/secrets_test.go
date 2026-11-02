package secrets

import (
	"bytes"
	"strings"
	"testing"
)

func TestBoxRoundTrip(t *testing.T) {
	box, err := NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	enc1, _ := box.Encrypt("whsec_abc")
	enc2, _ := box.Encrypt("whsec_abc")
	if enc1 == enc2 {
		t.Fatal("ciphertexts must differ (random nonce)")
	}
	plain, err := box.Decrypt(enc1)
	if err != nil || plain != "whsec_abc" {
		t.Fatalf("decrypt = %q, %v", plain, err)
	}
	tampered := enc1[:len(enc1)-2] + "AA"
	if _, err := box.Decrypt(tampered); err == nil {
		t.Fatal("tampered ciphertext must fail authentication")
	}
	other, _ := NewBox(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Decrypt(enc1); err == nil {
		t.Fatal("wrong key must fail")
	}
}

func TestSealBytes(t *testing.T) {
	b, _ := NewBox(make([]byte, 32))
	sealed, err := b.SealBytes([]byte("%PDF-1.7 secret"), []byte("doc-1"))
	if err != nil || strings.Contains(string(sealed), "secret") {
		t.Fatalf("seal: %v", err)
	}
	if got, err := b.OpenBytes(sealed, []byte("doc-1")); err != nil || string(got) != "%PDF-1.7 secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := b.OpenBytes(sealed, []byte("doc-2")); err == nil {
		t.Fatal("a file moved to another record must not decrypt")
	}
}
