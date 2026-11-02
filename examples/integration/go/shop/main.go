// Example shop: "Buy" -> paygate checkout -> webhook marks the order paid.
//
//	PAYGATE_API_KEY=sk_test_... PAYGATE_WEBHOOK_SECRET=whsec_... PAYGATE_BASE_URL=http://localhost:8080 go run ./shop
package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"example.com/paygate-example/paygate"
)

type order struct {
	Status string
	events map[string]bool
}

var (
	mu     sync.Mutex
	orders = map[string]*order{} // use your database
)

func main() {
	pg := paygate.New(os.Getenv("PAYGATE_API_KEY"), paygate.WithBaseURL(env("PAYGATE_BASE_URL", "https://api.paygate.example")))
	shop := env("SHOP_URL", "http://localhost:3002")
	secret := os.Getenv("PAYGATE_WEBHOOK_SECRET")

	// 1. The customer clicks "Buy": create the payment on the server, redirect to checkout_url.
	http.HandleFunc("POST /checkout", func(w http.ResponseWriter, r *http.Request) {
		orderID := "ORDER-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
		mu.Lock()
		orders[orderID] = &order{Status: "pending", events: map[string]bool{}}
		mu.Unlock()
		p, err := pg.CreatePayment(r.Context(), paygate.PaymentParams{
			Amount: 5000, Currency: "USD", PaymentMethod: "card",
			Reference: orderID, Description: "2 bags of coffee",
			Customer:  &paygate.Customer{Email: "buyer@example.com"},
			ReturnURL: shop + "/thanks?order=" + url.QueryEscape(orderID),
		}, orderID) // idempotency key = order id
		if err != nil {
			var pe *paygate.Error
			if errors.As(err, &pe) {
				log.Printf("paygate %d %s: %s", pe.Status, pe.Code, pe.Message)
			}
			http.Error(w, "payment service error", http.StatusBadGateway)
			return
		}
		http.Redirect(w, r, p.CheckoutURL, http.StatusSeeOther)
	})

	// 2. Back from checkout: show "processing" until the webhook says paid.
	http.HandleFunc("GET /thanks", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		o := orders[r.URL.Query().Get("order")]
		paid := o != nil && o.Status == "paid"
		mu.Unlock()
		if paid {
			io.WriteString(w, "Thank you, payment received.")
		} else {
			io.WriteString(w, "Payment is being confirmed...")
		}
	})

	// 3. Webhook: verify the RAW body, then update the order once.
	http.HandleFunc("POST /webhooks/paygate", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		e, err := paygate.VerifyWebhook(raw, r.Header.Get("Paygate-Signature"), []string{secret}, 5*time.Minute, time.Now())
		if err != nil {
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}
		var data struct {
			Reference string `json:"reference"`
			Reason    string `json:"reason"`
		}
		_ = json.Unmarshal(e.Data, &data)
		mu.Lock()
		if o := orders[data.Reference]; o != nil && !o.events[e.ID] { // events can arrive more than once
			o.events[e.ID] = true
			switch e.Type {
			case "payment.succeeded":
				o.Status = "paid"
			case "payment.failed":
				o.Status = "failed: " + data.Reason
			case "payment.expired":
				o.Status = "expired"
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK) // answer 2xx quickly, or paygate retries
	})

	u, _ := url.Parse(shop)
	log.Println("shop on", shop)
	log.Fatal(http.ListenAndServe("127.0.0.1:"+u.Port(), nil))
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
