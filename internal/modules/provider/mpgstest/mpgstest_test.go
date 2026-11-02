package mpgstest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCardChecks(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for _, c := range TestCards {
		n := strings.ReplaceAll(c.Number, " ", "")
		if !Luhn(n) || Brand(n) != c.Brand {
			t.Errorf("test card %s: luhn %v brand %q", c.Number, Luhn(n), Brand(n))
		}
	}
	cases := map[string][4]string{
		"number": {"5123450000000009", "01/39", "123", "A"}, // wrong check digit
		"expiry": {"5123450000000008", "10/26", "123", "A"}, // ended October 2026
		"cvv":    {"5123450000000008", "01/39", "12", "A"},
	}
	for field, c := range cases {
		if errs := ValidateCard(c[0], c[1], c[2], c[3], now); errs[field] == "" {
			t.Errorf("%s not rejected: %v", field, errs)
		}
	}
	if errs := ValidateCard("6011000990139424", "01/39", "123", "A", now); !strings.Contains(errs["number"], "Mastercard and Visa") {
		t.Fatalf("Discover accepted: %v", errs)
	}
	if errs := ValidateCard("5123450000000008", "01/39", "123", "A", now); len(errs) != 0 {
		t.Fatal(errs)
	}
}

// TestBrowserFlow: invalid card → stays with an error; insufficient funds →
// shown, order FAILED; another card → approved, back to the shop.
func TestBrowserFlow(t *testing.T) {
	s := New("TESTM", "pw")
	defer s.Close()
	body, _ := json.Marshal(map[string]any{"apiOperation": "INITIATE_CHECKOUT",
		"order":       map[string]any{"id": "0123456789abcdef", "amount": "50.00", "currency": "USD"},
		"interaction": map[string]any{"operation": "PURCHASE", "returnUrl": "https://shop.example/back"}})
	req, _ := http.NewRequest("POST", s.URL+"/api/rest/version/100/merchant/TESTM/session", bytes.NewReader(body))
	req.SetBasicAuth("merchant.TESTM", "pw")
	resp, _ := s.Client().Do(req)
	var sess struct{ Session struct{ ID string } }
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()
	page := s.URL + "/checkout/pay/" + sess.Session.ID
	hc := s.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	post := func(u string, v url.Values) (*http.Response, string) {
		r, err := hc.PostForm(u, v)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		return r, string(b)
	}
	card := func(exp string) url.Values {
		return url.Values{"number": {"5123 4500 0000 0008"}, "expiry": {exp}, "cvv": {"123"}, "name": {"ABEBE"}}
	}

	bad := card("01/39")
	bad.Set("number", "5123 4500 0000 0001")
	if r, b := post(page, bad); r.StatusCode != 422 || !strings.Contains(b, "Card number is not valid") {
		t.Fatalf("invalid card: %d", r.StatusCode)
	}
	r, _ := post(page, card("02/39"))
	if r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), "/checkout/3ds/") {
		t.Fatalf("to 3-D Secure: %d", r.StatusCode)
	}
	r, _ = post(s.URL+r.Header.Get("Location"), url.Values{"otp": {OTP}})
	if !strings.Contains(r.Header.Get("Location"), "declined=INSUFFICIENT_FUNDS") {
		t.Fatalf("insufficient funds: %s", r.Header.Get("Location"))
	}
	g, _ := s.Client().Get(s.URL + r.Header.Get("Location"))
	b, _ := io.ReadAll(g.Body)
	g.Body.Close()
	if !strings.Contains(string(b), "insufficient funds") {
		t.Fatal("customer not told about insufficient funds")
	}
	if s.orders["0123456789abcdef"].Status != "FAILED" {
		t.Fatal("order not failed")
	}
	r, _ = post(page, card("01/39"))
	r, _ = post(s.URL+r.Header.Get("Location"), url.Values{"otp": {OTP}})
	if !strings.HasPrefix(r.Header.Get("Location"), "https://shop.example/back?") || s.orders["0123456789abcdef"].Status != "CAPTURED" {
		t.Fatalf("approved: %s", r.Header.Get("Location"))
	}
}
