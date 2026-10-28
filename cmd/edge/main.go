// Command edge is a minimal path-based API gateway for running modules as
// separate services locally. It forwards each request to the service that
// owns the path. In production use your real gateway (Nginx, Traefik, Envoy,
// Azure API Management...) with the same routing table.
//
//	EDGE_ADDR=:8080 \
//	ROUTES="/admin/merchants=http://merchant:8080,/admin/merchants/*/api-keys=http://auth:8080,..." \
//	go run ./cmd/edge
//
// Patterns match by path segment; "*" matches one segment. The most specific
// pattern (most literal segments) wins.
package main

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

type route struct {
	segments []string
	literals int
	proxy    *httputil.ReverseProxy
	target   string
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	routes, err := parse(os.Getenv("ROUTES"))
	if err != nil || len(routes) == 0 {
		log.Error("set ROUTES=pattern=url,pattern=url,...", "err", err)
		os.Exit(1)
	}
	addr := os.Getenv("EDGE_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			http.NotFound(w, r) // never expose internal endpoints
			return
		}
		rt := match(routes, r.URL.Path)
		if rt == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"kind":"not_found","code":"route_not_found","message":"no service owns this path"}}`))
			return
		}
		rt.proxy.ServeHTTP(w, r)
	})
	log.Info("edge started", "addr", addr, "routes", len(routes))
	if err := http.ListenAndServe(addr, h); err != nil {
		log.Error("edge stopped", "err", err)
		os.Exit(1)
	}
}

func parse(spec string) ([]route, error) {
	var out []route
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		pattern, target, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		u, err := url.Parse(target)
		if err != nil {
			return nil, err
		}
		segs := split(pattern)
		lit := 0
		for _, s := range segs {
			if s != "*" {
				lit++
			}
		}
		out = append(out, route{segments: segs, literals: lit, proxy: httputil.NewSingleHostReverseProxy(u), target: target})
	}
	return out, nil
}

func match(routes []route, path string) *route {
	segs := split(path)
	var best *route
	for i := range routes {
		rt := &routes[i]
		if len(rt.segments) > len(segs) {
			continue
		}
		ok := true
		for j, s := range rt.segments {
			if s != "*" && s != segs[j] {
				ok = false
				break
			}
		}
		if ok && (best == nil || rt.literals > best.literals ||
			(rt.literals == best.literals && len(rt.segments) > len(best.segments))) {
			best = rt
		}
	}
	return best
}

func split(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
