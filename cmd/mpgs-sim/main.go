// Command mpgs-sim runs a local Mastercard Gateway simulator so card payments
// can be tried in a browser without a bank account (development only).
//
//	go run ./cmd/mpgs-sim            listens on 127.0.0.1:9100
//
// Then point the gateway's TEST merchant at it (test mode only):
//
//	PROVIDER_TEST_ROUTES=card=mpgs_test
//	MPGS_TEST_BASE_URL=http://127.0.0.1:9100
//	MPGS_TEST_MERCHANT_ID=TESTPAYGATE
//	MPGS_TEST_API_PASSWORD=sim-password
//	MPGS_TEST_CURRENCIES=USD,ETB
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"paygate/internal/modules/provider/mpgstest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9100", "listen address (keep it on localhost)")
	merchant := flag.String("merchant", env("MPGS_SIM_MERCHANT_ID", "TESTPAYGATE"), "merchant id")
	password := flag.String("password", env("MPGS_SIM_PASSWORD", "sim-password"), "API password")
	flag.Parse()

	fmt.Printf("Mastercard Gateway simulator on http://%s (merchant %s)\n", *addr, *merchant)
	fmt.Println("Test cards: 5123 4500 0000 0008 (Mastercard), 4508 7500 1574 1019 (Visa), CVV any 3 digits, 3-D Secure code", mpgstest.OTP)
	for _, o := range mpgstest.Outcomes {
		fmt.Printf("  expiry %s → %s\n", o.Expiry, o.Meaning)
	}
	srv := &http.Server{Addr: *addr, Handler: mpgstest.NewSim(*merchant, *password), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
