// Command loadtest measures how many requests per second paygate handles.
// It runs N goroutines ("virtual clients") that send requests back to back
// for a fixed time, then prints throughput, latency percentiles and status codes.
//
//	go run ./cmd/loadtest -url http://localhost:8080 -c 50 -d 20s -scenario create
//
// Scenarios:
//
//	health   GET /healthz                          (HTTP stack only, no DB)
//	get      GET /v1/payments/{id}                 (API key auth + one DB read)
//	list     GET /v1/payments?limit=20             (auth + indexed range scan)
//	create   POST /v1/payments                     (auth, merchant lookup, provider call, 3 DB writes)
//	mixed    80% get, 20% create                   (typical checkout traffic)
//
// Without -key it uses the platform admin (-admin-email / -admin-password,
// default BOOTSTRAP_ADMIN_EMAIL / _PASSWORD from the environment) to create a
// throwaway merchant and owner (admin-created accounts skip email
// verification), then a test key.
// Turn the server's rate limit off for a capacity test (RATE_LIMIT_RPS=0),
// otherwise you measure the limiter.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	latency time.Duration
	code    int // -1 = network error
}

func main() {
	base := flag.String("url", "http://localhost:8080", "server address")
	key := flag.String("key", "", "test API key (sk_test_...); empty = sign up a throwaway merchant")
	conc := flag.Int("c", 50, "concurrent clients (goroutines)")
	dur := flag.Duration("d", 15*time.Second, "test duration")
	scenario := flag.String("scenario", "mixed", "health | get | list | create | mixed")
	adminEmail := flag.String("admin-email", os.Getenv("BOOTSTRAP_ADMIN_EMAIL"), "platform admin (to create the test merchant)")
	adminPassword := flag.String("admin-password", os.Getenv("BOOTSTRAP_ADMIN_PASSWORD"), "platform admin password")
	flag.Parse()

	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		MaxIdleConns: *conc * 2, MaxIdleConnsPerHost: *conc * 2, IdleConnTimeout: 90 * time.Second}}
	c := &caller{hc: hc, base: strings.TrimRight(*base, "/")}

	if *key == "" && *scenario != "health" {
		var err error
		if *key, err = c.setup(*adminEmail, *adminPassword); err != nil {
			fail("setup: %v", err)
		}
	}
	var seedID string
	if *scenario != "health" {
		code, body := c.do("POST", "/v1/payments", *key, newIdem(), paymentBody())
		if code != 201 {
			fail("seed payment: HTTP %d %s", code, body)
		}
		var p struct{ ID string }
		_ = json.Unmarshal(body, &p)
		seedID = p.ID
	}

	request := func() int {
		switch *scenario {
		case "health":
			code, _ := c.do("GET", "/healthz", "", "", nil)
			return code
		case "get":
			code, _ := c.do("GET", "/v1/payments/"+seedID, *key, "", nil)
			return code
		case "list":
			code, _ := c.do("GET", "/v1/payments?limit=20", *key, "", nil)
			return code
		case "create":
			code, _ := c.do("POST", "/v1/payments", *key, newIdem(), paymentBody())
			return code
		case "mixed":
			if rand.IntN(100) < 20 {
				code, _ := c.do("POST", "/v1/payments", *key, newIdem(), paymentBody())
				return code
			}
			code, _ := c.do("GET", "/v1/payments/"+seedID, *key, "", nil)
			return code
		}
		fail("unknown scenario %q", *scenario)
		return 0
	}

	fmt.Printf("scenario=%s concurrency=%d duration=%s target=%s\n", *scenario, *conc, *dur, *base)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		all      []result
		inFlight atomic.Int64
		peak     atomic.Int64
	)
	deadline := time.Now().Add(*dur)
	start := time.Now()
	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func() { // one goroutine = one client sending requests back to back
			defer wg.Done()
			local := make([]result, 0, 4096)
			for time.Now().Before(deadline) {
				n := inFlight.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				t0 := time.Now()
				code := request()
				local = append(local, result{time.Since(t0), code})
				inFlight.Add(-1)
			}
			mu.Lock()
			all = append(all, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	report(all, time.Since(start), peak.Load())
}

func report(all []result, elapsed time.Duration, peak int64) {
	codes := map[int]int{}
	lat := make([]time.Duration, 0, len(all))
	ok := 0
	for _, r := range all {
		codes[r.code]++
		if r.code >= 200 && r.code < 300 {
			ok++
			lat = append(lat, r.latency)
		}
	}
	slices.Sort(lat)
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[min(len(lat)-1, int(float64(len(lat))*p))]
	}
	fmt.Printf("requests      %d in %s (peak %d in flight)\n", len(all), elapsed.Round(time.Millisecond), peak)
	fmt.Printf("throughput    %.0f req/s total, %.0f req/s successful\n",
		float64(len(all))/elapsed.Seconds(), float64(ok)/elapsed.Seconds())
	fmt.Printf("latency (2xx) p50 %s  p90 %s  p99 %s  max %s\n",
		pct(0.50).Round(100*time.Microsecond), pct(0.90).Round(100*time.Microsecond),
		pct(0.99).Round(100*time.Microsecond), pct(1).Round(100*time.Microsecond))
	keys := make([]int, 0, len(codes))
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		name := fmt.Sprint(k)
		if k == -1 {
			name = "network-error"
		}
		parts[i] = fmt.Sprintf("%s=%d", name, codes[k])
	}
	fmt.Printf("status codes  %s\n", strings.Join(parts, " "))
}

type caller struct {
	hc   *http.Client
	base string
}

func (c *caller) do(method, path, token, idem string, body any) (int, []byte) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return -1, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// setup creates a throwaway merchant and owner as the platform admin and
// returns a test API key.
func (c *caller) setup(adminEmail, adminPassword string) (string, error) {
	if adminEmail == "" || adminPassword == "" {
		return "", fmt.Errorf("pass -key, or -admin-email and -admin-password (or set BOOTSTRAP_ADMIN_*)")
	}
	token := func(email, password string) (string, error) {
		code, body := c.do("POST", "/auth/login", "", "", map[string]string{"email": email, "password": password})
		var t struct {
			AccessToken string `json:"access_token"`
			MFARequired bool   `json:"mfa_required"`
		}
		_ = json.Unmarshal(body, &t)
		if code != 200 || t.AccessToken == "" {
			return "", fmt.Errorf("login %s: HTTP %d %s (an admin with an authenticator cannot be used here)", email, code, body)
		}
		return t.AccessToken, nil
	}
	admin, err := token(adminEmail, adminPassword)
	if err != nil {
		return "", err
	}
	n := time.Now().UnixNano()
	email := fmt.Sprintf("load-%d@loadtest.local", n)
	code, body := c.doAs("POST", "/admin/merchants", admin, map[string]any{"name": "Load Test", "email": email, "fee_bps": 0})
	if code != 201 {
		return "", fmt.Errorf("create merchant HTTP %d: %s", code, body)
	}
	var m struct{ ID string }
	_ = json.Unmarshal(body, &m)
	if code, body = c.doAs("POST", "/admin/users", admin, map[string]any{"email": email, "name": "Load",
		"password": "load-test-password-1", "role": "owner", "merchant_id": m.ID}); code != 201 {
		return "", fmt.Errorf("create owner HTTP %d: %s", code, body)
	}
	owner, err := token(email, "load-test-password-1")
	if err != nil {
		return "", err
	}
	code, body = c.doAs("POST", "/v1/api-keys", owner, map[string]any{"name": "loadtest",
		"environment": "test", "scopes": []string{"payments:read", "payments:write"}})
	if code != 201 {
		return "", fmt.Errorf("create key HTTP %d: %s", code, body)
	}
	var k struct{ Secret string }
	_ = json.Unmarshal(body, &k)
	return k.Secret, nil
}

func (c *caller) doAs(method, path, token string, body any) (int, []byte) {
	return c.do(method, path, token, "", body)
}

func paymentBody() map[string]any {
	return map[string]any{"amount": 1000 + rand.IntN(100000), "currency": "ETB", "provider": "mock",
		"reference": fmt.Sprintf("LT-%d", rand.Int64()), "metadata": map[string]string{"source": "loadtest"}}
}

func newIdem() string { return fmt.Sprintf("lt-%d-%d", time.Now().UnixNano(), rand.Int64()) }

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "loadtest: "+format+"\n", args...)
	os.Exit(1)
}
