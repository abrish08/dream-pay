package domain

import (
	"encoding/json"
	"os"
	"testing"
)

// TestSignatureMatchesPublishedVector: the merchant libraries in
// docs/integration verify against this exact vector; the server must keep
// producing it.
func TestSignatureMatchesPublishedVector(t *testing.T) {
	raw, err := os.ReadFile("../../../../../docs/integration/testdata/webhook-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Secret, Body, Header string
		Timestamp            int64
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if got := Sign(v.Secret, v.Timestamp, []byte(v.Body)); got != v.Header {
		t.Fatalf("server signature %s, published vector %s", got, v.Header)
	}
}
