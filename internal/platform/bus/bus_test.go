package bus_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/authn"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/reqctx"
)

// ---- a tiny contract, as a module would define it in internal/contracts ----

type Thing struct {
	Name, Merchant, Correlation string
}

type GetThing struct {
	bus.Returns[Thing]
	ID string `json:"id"`
}

func (GetThing) Route() string { return "things.get" }

type ThingCreated struct {
	ID string `json:"id"`
}

func (ThingCreated) EventTopic() string { return "thing.created.v1" }

var errThingNotFound = apperror.NotFound("thing_not_found", "thing not found")

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func serve(b bus.Bus) {
	bus.Handle(b, func(ctx context.Context, q GetThing) (Thing, error) {
		if q.ID == "missing" {
			return Thing{}, errThingNotFound
		}
		p, _ := reqctx.PrincipalFrom(ctx)
		return Thing{Name: "thing " + q.ID, Merchant: p.MerchantID, Correlation: reqctx.CorrelationID(ctx)}, nil
	})
}

// signer is the auth service; every bus gets its verifier.
var signer, _ = authn.NewSigner(make([]byte, 32))

func secured(b bus.Bus) bus.Bus {
	v, _ := authn.NewVerifier(signer.PublicKey())
	b.SetVerifier(v)
	return b
}

func token(t *testing.T, s *authn.Signer, merchantID string) string {
	t.Helper()
	tok, _, err := s.Issue(reqctx.Principal{Kind: reqctx.KindAPIKey, Subject: "k1", MerchantID: merchantID}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func exercise(t *testing.T, caller bus.Bus) {
	t.Helper()
	ctx := reqctx.WithToken(reqctx.WithCorrelationID(context.Background(), "corr-1"), token(t, signer, "m1"))

	got, err := bus.Ask(ctx, caller, GetThing{ID: "42"}) // response type inferred
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "thing 42" || got.Merchant != "m1" || got.Correlation != "corr-1" {
		t.Fatalf("unexpected %+v", got)
	}
	if _, err := bus.Ask(ctx, caller, GetThing{ID: "missing"}); !errors.Is(err, errThingNotFound) {
		t.Fatalf("errors.Is must work across the bus, got %v", err)
	}
}

func TestMemoryRequests(t *testing.T) {
	b := secured(bus.NewMemory(quiet()))
	serve(b)
	exercise(t, b)
}

// A caller cannot claim an identity: only a token signed by the auth service
// is accepted, and a bare principal in the context is never forwarded.
func TestBusIdentityCannotBeForged(t *testing.T) {
	b := secured(bus.NewMemory(quiet()))
	serve(b)

	evil, _ := authn.NewSigner(append(make([]byte, 31), 9))
	ctx := reqctx.WithToken(context.Background(), token(t, evil, "victim"))
	if _, err := bus.Ask(ctx, b, GetThing{ID: "1"}); err == nil {
		t.Fatal("token signed with another key must be rejected")
	}

	ctx = reqctx.WithPrincipal(context.Background(), reqctx.Principal{MerchantID: "victim"})
	got, err := bus.Ask(ctx, b, GetThing{ID: "1"})
	if err != nil || got.Merchant != "" {
		t.Fatalf("unsigned principal must not cross the bus: %+v %v", got, err)
	}
}

func TestMemoryNoHandler(t *testing.T) {
	b := bus.NewMemory(quiet())
	if _, err := bus.Ask(context.Background(), b, GetThing{ID: "1"}); !errors.Is(err, bus.ErrNoHandler) {
		t.Fatalf("want ErrNoHandler, got %v", err)
	}
}

func TestMemoryEvents(t *testing.T) {
	b := bus.NewMemory(quiet())
	var got []string
	bus.On(b, "test.consumer", func(_ context.Context, _ bus.Envelope, e ThingCreated) error {
		got = append(got, e.ID)
		return nil
	})
	payload, _ := json.Marshal(ThingCreated{ID: "t1"})
	if err := b.Publish(context.Background(), bus.Envelope{ID: "e1", Topic: "thing.created.v1", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "t1" {
		t.Fatalf("event not delivered: %v", got)
	}
}

// TestNATSAcrossProcesses simulates two services: one handles requests,
// the other only calls. Requires a NATS server with JetStream:
//
//	TEST_NATS_URL=nats://localhost:4222 go test ./internal/platform/bus/
func TestNATSAcrossProcesses(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}
	ctx := context.Background()
	server, err := bus.NewNATS(ctx, url, quiet())
	if err != nil {
		t.Fatal(err)
	}
	secured(server)
	defer server.Close()
	client, err := bus.NewNATS(ctx, url, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	serve(server)
	var mu sync.Mutex
	received := map[string]bool{}
	bus.On(server, "test.nats_consumer_"+time.Now().Format("150405.000000"),
		func(_ context.Context, _ bus.Envelope, e ThingCreated) error {
			mu.Lock()
			received[e.ID] = true
			mu.Unlock()
			return nil
		})
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	exercise(t, client) // request crosses NATS to the "server" process

	id := "t-" + time.Now().Format("150405.000000")
	payload, _ := json.Marshal(ThingCreated{ID: id})
	if err := client.Publish(ctx, bus.Envelope{ID: id, Topic: "thing.created.v1", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := received[id]
		mu.Unlock()
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("event did not cross NATS")
}
