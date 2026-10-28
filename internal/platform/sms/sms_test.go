package sms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendEt(t *testing.T) {
	var got map[string]string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["to"] == "+251900000000" {
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_parameter","message":"bad number"}}`))
			return
		}
		w.WriteHeader(202)
	}))
	defer srv.Close()
	s := SendEt{BaseURL: srv.URL + "/v1", APIKey: "sk_test_x", Sender: "Paygate"}
	if err := s.Send(context.Background(), Message{To: "+251911223344", Body: "code 123456"}); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk_test_x" || got["sender"] != "Paygate" || got["body"] != "code 123456" {
		t.Fatalf("request: %s %v", auth, got)
	}
	if err := s.Send(context.Background(), Message{To: "+251900000000", Body: "x"}); err == nil {
		t.Fatal("provider error not returned")
	}
}
