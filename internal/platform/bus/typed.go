package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/reqctx"
)

// Request is a message that expects exactly one answer of type R.
// Contracts declare requests by embedding Returns[R]:
//
//	type GetMerchant struct {
//	    bus.Returns[Merchant]
//	    ID string `json:"id"`
//	}
//	func (GetMerchant) Route() string { return "merchants.get" }
type Request[R any] interface {
	Route() string
	responseType(R)
}

// Returns marks a request's response type. It adds no fields.
type Returns[R any] struct{}

func (Returns[R]) responseType(R) {}

// Event is a message announcing that something happened.
type Event interface {
	EventTopic() string
}

var ErrNoHandler = apperror.Unavailable("no_handler", "no module handles this request")

// meta travels with every request. The caller's identity is carried only as
// the signed access token; the receiving side verifies it before trusting
// it, so a service (or anyone else on the bus) cannot claim to be a
// merchant it is not.
type meta struct {
	CorrelationID string `json:"correlation_id,omitempty"`
	Token         string `json:"token,omitempty"`
}

type requestEnvelope struct {
	Meta meta            `json:"meta"`
	Body json.RawMessage `json:"body"`
}

type responseEnvelope struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *apperror.Error `json:"error,omitempty"`
}

// Ask sends a request and waits for its typed answer. The response type is
// inferred from the request: bus.Ask(ctx, b, merchants.GetMerchant{ID: id})
// returns (merchants.Merchant, error).
func Ask[R any](ctx context.Context, b Bus, req Request[R]) (R, error) {
	var zero R
	body, err := json.Marshal(req)
	if err != nil {
		return zero, err
	}
	m := meta{CorrelationID: reqctx.CorrelationID(ctx), Token: reqctx.TokenFrom(ctx)}
	raw, err := json.Marshal(requestEnvelope{Meta: m, Body: body})
	if err != nil {
		return zero, err
	}
	out, err := b.RequestRaw(ctx, req.Route(), raw)
	if err != nil {
		return zero, err
	}
	var res responseEnvelope
	if err := json.Unmarshal(out, &res); err != nil {
		return zero, apperror.Unavailable("bad_bus_response", fmt.Sprintf("%s: %v", req.Route(), err))
	}
	if res.Error != nil {
		return zero, res.Error // same Code as the handler's error, so errors.Is works
	}
	var r R
	if err := json.Unmarshal(res.Result, &r); err != nil {
		return zero, apperror.Unavailable("bad_bus_response", fmt.Sprintf("%s: %v", req.Route(), err))
	}
	return r, nil
}

// errorLogger is implemented by transports so unexpected handler errors are
// logged where they happen (callers only see a generic internal error).
type errorLogger interface {
	logHandlerError(route string, err error)
}

// Handle registers the handler that answers requests of type Q.
//
//	bus.Handle(b, func(ctx context.Context, q merchants.GetMerchant) (merchants.Merchant, error) {...})
func Handle[Q Request[R], R any](b Bus, fn func(ctx context.Context, q Q) (R, error)) {
	var zero Q
	route := zero.Route()
	b.HandleRaw(route, func(ctx context.Context, raw []byte) []byte {
		var env requestEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return encodeError(apperror.Invalid("bad_bus_request", err.Error()))
		}
		ctx = reqctx.WithCorrelationID(reqctx.WithoutIdentity(ctx), env.Meta.CorrelationID)
		if env.Meta.Token != "" {
			p, err := verifyToken(b, env.Meta.Token)
			if err != nil {
				return encodeError(apperror.From(err))
			}
			ctx = reqctx.WithToken(reqctx.WithPrincipal(ctx, p), env.Meta.Token)
		}
		var q Q
		if err := json.Unmarshal(env.Body, &q); err != nil {
			return encodeError(apperror.Invalid("bad_bus_request", err.Error()))
		}
		r, err := call(ctx, fn, q)
		if err != nil {
			ae := apperror.From(err)
			if ae.Kind == apperror.KindInternal {
				if l, ok := b.(errorLogger); ok {
					l.logHandlerError(route, err)
				}
			}
			return encodeError(ae)
		}
		body, err := json.Marshal(r)
		if err != nil {
			return encodeError(apperror.From(err))
		}
		out, _ := json.Marshal(responseEnvelope{Result: body})
		return out
	})
}

// TokenVerifier checks a signed access token (implemented by authn.Verifier).
type TokenVerifier interface {
	Verify(token string) (reqctx.Principal, error)
}

var errNoVerifier = apperror.Unauthorized("token_not_verifiable", "this service cannot verify access tokens")

// verifyToken fails closed: a token that cannot be verified is rejected.
func verifyToken(b Bus, token string) (reqctx.Principal, error) {
	s, ok := b.(interface{ tokenVerifier() TokenVerifier })
	if !ok || s.tokenVerifier() == nil {
		return reqctx.Principal{}, errNoVerifier
	}
	return s.tokenVerifier().Verify(token)
}

// security is embedded by every transport.
type security struct{ verifier TokenVerifier }

func (s *security) SetVerifier(v TokenVerifier)  { s.verifier = v }
func (s *security) tokenVerifier() TokenVerifier { return s.verifier }

func call[Q, R any](ctx context.Context, fn func(context.Context, Q) (R, error), q Q) (r R, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panic: %v", p)
		}
	}()
	return fn(ctx, q)
}

func encodeError(e *apperror.Error) []byte {
	out, _ := json.Marshal(responseEnvelope{Error: e})
	return out
}

// On subscribes a typed handler to event E.
//
//	bus.On(b, "ledger.on_payment_succeeded", func(ctx context.Context, env bus.Envelope, e payments.PaymentSucceeded) error {...})
func On[E Event](b Bus, consumer string, fn func(ctx context.Context, env Envelope, evt E) error) {
	var zero E
	b.Subscribe(zero.EventTopic(), consumer, func(ctx context.Context, env Envelope) error {
		var evt E
		if err := json.Unmarshal(env.Payload, &evt); err != nil {
			return fmt.Errorf("decode %s: %w", env.Topic, err)
		}
		return fn(ctx, env, evt)
	})
}

// OnEach subscribes fn to event E in every process (see Bus.SubscribeAll).
func OnEach[E Event](b Bus, replay time.Duration, fn func(ctx context.Context, evt E) error) {
	var zero E
	b.SubscribeAll(zero.EventTopic(), replay, func(ctx context.Context, env Envelope) error {
		var evt E
		if err := json.Unmarshal(env.Payload, &evt); err != nil {
			return fmt.Errorf("decode %s: %w", env.Topic, err)
		}
		return fn(ctx, evt)
	})
}
