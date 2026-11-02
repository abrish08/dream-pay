package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestAllowed(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.9", "192.168.1.1", "169.254.169.254", "::1",
		"fd00::1", "fe80::1", "::ffff:10.0.0.1", "100.64.0.1", "0.0.0.0"} {
		if Allowed(netip.MustParseAddr(ip)) {
			t.Errorf("%s allowed", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "196.188.1.1", "2606:4700::1111"} {
		if !Allowed(netip.MustParseAddr(ip)) {
			t.Errorf("%s refused", ip)
		}
	}
}

func TestCheckHost(t *testing.T) {
	for _, h := range []string{"localhost", "127.0.0.1", "[::1]", "169.254.169.254", "metadata.google.internal", "db", "printer.local"} {
		if CheckHost(h) == nil {
			t.Errorf("%s accepted", h)
		}
	}
	if err := CheckHost("hooks.merchant.et"); err != nil {
		t.Fatal(err)
	}
}

func TestDialerRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	hc := &http.Client{Transport: &http.Transport{DialContext: Dialer(time.Second)}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if _, err := hc.Do(req); err == nil || !errors.Is(err, ErrBlocked) {
		t.Fatalf("loopback reached: %v", err)
	}
}
