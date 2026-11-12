// Command wallet-sim runs a mobile wallet simulator (telebirr / CBE Birr /
// M-Pesa stand-in) for local development. Open http://127.0.0.1:9200/phone to
// play the customer's phone: approve, decline or run out of balance.
//
//	go run ./cmd/wallet-sim
//
// Point a wallet's TEST account at it (test mode only):
//
//	PROVIDER_TEST_ROUTES=telebirr=telebirr_test
//	TELEBIRR_TEST_BASE_URL=http://127.0.0.1:9200
//	TELEBIRR_TEST_API_KEY=sim-key
//	TELEBIRR_TEST_WEBHOOK_SECRET=sim-secret
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"paygate/internal/modules/provider/walletmock"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9200", "listen address (keep it on localhost)")
	key := flag.String("key", "sim-key", "API key")
	secret := flag.String("secret", "sim-secret", "webhook signing secret")
	flag.Parse()
	fmt.Printf("Wallet simulator on http://%s  (phone: http://%s/phone)\n", *addr, *addr)
	srv := &http.Server{Addr: *addr, Handler: walletmock.NewSim(*key, *secret), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
