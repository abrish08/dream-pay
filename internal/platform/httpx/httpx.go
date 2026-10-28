// Package httpx holds HTTP helpers and middleware shared by all modules.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/reqctx"
)

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError maps any error to the standard error body:
// {"error":{"kind":"...","code":"...","message":"..."}}
func WriteError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	ae := apperror.From(err)
	status := apperror.Status(ae.Kind)
	if status >= 500 {
		log.Error("request failed", "path", r.URL.Path, "correlation_id", reqctx.CorrelationID(r.Context()), "err", err)
	}
	WriteJSON(w, status, map[string]any{"error": ae})
}

// DecodeJSON reads a JSON body (max 1 MB) and rejects unknown fields.
func DecodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperror.Invalid("invalid_json", err.Error())
	}
	return nil
}

// QueryTime parses an RFC 3339 query parameter ("" gives the zero time).
func QueryTime(r *http.Request, key string) (time.Time, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, apperror.Invalid("invalid_"+key, key+" must be an RFC 3339 time, e.g. 2026-01-31T00:00:00Z")
	}
	return t, nil
}

func QueryInt(r *http.Request, key string, def, maxV int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || v <= 0 {
		return def
	}
	return min(v, maxV)
}

// Principal returns the authenticated caller. Only call it behind Authenticate.
func Principal(r *http.Request) reqctx.Principal {
	p, _ := reqctx.PrincipalFrom(r.Context())
	return p
}

// Correlation assigns every request a correlation id (or keeps the caller's).
func Correlation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Correlation-ID")
		if id == "" || len(id) > 100 {
			id = uuid.NewString()
		}
		w.Header().Set("X-Correlation-ID", id)
		next.ServeHTTP(w, r.WithContext(reqctx.WithCorrelationID(r.Context(), id)))
	})
}

func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					WriteError(w, r, log, fmt.Errorf("panic: %v", rec))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog logs method, path, status and latency. Headers (and so API keys)
// are never logged.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			log.Info("http",
				"method", r.Method, "path", r.URL.Path, "status", ww.Status(),
				"ms", time.Since(start).Milliseconds(), "correlation_id", reqctx.CorrelationID(r.Context()))
		})
	}
}

// IsNotFound is a small helper for handlers that branch on missing records.
func IsNotFound(err error) bool {
	var ae *apperror.Error
	return errors.As(err, &ae) && ae.Kind == apperror.KindNotFound
}
