package httpx

import (
	"net/http"
	"strings"
)

// SecurityHeaders sets browser protections on every response:
//
//   - nosniff, no referrer, no framing (clickjacking), no camera/mic/location
//   - Content-Security-Policy: the JSON API allows nothing at all; the few HTML
//     pages (API docs, hosted checkout, sandbox) only their own resources
//   - HSTS when the gateway is served over HTTPS (HSTS=true)
func SecurityHeaders(hsts bool) func(http.Handler) http.Handler {
	const (
		apiCSP  = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'"
		pageCSP = "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; " +
			"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://fonts.googleapis.com; " +
			"font-src 'self' data: https://fonts.gstatic.com; img-src 'self' data: https:; worker-src blob:; " +
			"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"
	)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
			p := r.URL.Path
			if p == "/docs" || p == "/swagger" || strings.HasPrefix(p, "/pay/") || strings.HasPrefix(p, "/providers/mock/") {
				h.Set("Content-Security-Policy", pageCSP)
			} else {
				h.Set("Content-Security-Policy", apiCSP)
			}
			if hsts {
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
