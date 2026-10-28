package app_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"paygate/internal/app"
	"paygate/internal/platform/config"
)

var paramRe = regexp.MustCompile(`\{[^}]+\}`)

// TestAPIDocsCoverEveryRoute fails when a route is served but not documented
// in openapi.yaml (or documented but not served), so the docs cannot drift.
func TestAPIDocsCoverEveryRoute(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	resetSchemas(t, dbURL)
	cfg := config.Config{DatabaseURL: dbURL, DBMaxConns: 2, Bus: "memory",
		AuthSigningKey: bytes.Repeat([]byte{7}, 32), APIKeyPepper: strings.Repeat("p", 32), APIKeyPepperVersion: 1,
		EncryptionKey: bytes.Repeat([]byte{1}, 32), PublicBaseURL: "http://localhost"}
	a, err := app.Build(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())

	served := map[string]bool{}
	err = chi.Walk(a.Routes(), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/")
		if strings.Contains(route, "/*") {
			return nil
		}
		served[strings.ToLower(method)+" "+paramRe.ReplaceAllString(route, "{}")] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	spec, err := os.ReadFile("apidocs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	var path string
	inPaths := false
	for _, line := range strings.Split(string(spec), "\n") {
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && len(line) > 0 && line[0] != ' ':
			inPaths = false
		case inPaths && strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":"):
			path = paramRe.ReplaceAllString(strings.TrimSuffix(strings.TrimSpace(line), ":"), "{}")
		case inPaths && path != "":
			if m := regexp.MustCompile(`^    (get|post|put|patch|delete):`).FindStringSubmatch(line); m != nil {
				documented[m[1]+" "+path] = true
			}
		}
	}

	var missing, stale []string
	for r := range served {
		if !documented[r] {
			missing = append(missing, r)
		}
	}
	for r := range documented {
		if !served[r] {
			stale = append(stale, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 || len(stale) > 0 {
		t.Fatalf("API docs out of date.\nserved but not documented (%d):\n  %s\ndocumented but not served (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "), len(stale), strings.Join(stale, "\n  "))
	}
	t.Logf("%d routes, all documented", len(served))
}
