// Package apidocs serves the OpenAPI description of the public API, for
// integrators and for generating client SDKs (openapi-generator, oapi-codegen).
//
//	GET /openapi.yaml   the spec
//	GET /docs           a readable reference page (Redoc)
//	GET /swagger        Swagger UI: try requests from the browser ("Authorize" with a key or token)
package apidocs

import (
	_ "embed"
	"net/http"

	"github.com/go-chi/chi/v5"
)

//go:embed openapi.yaml
var spec []byte

const page = `<!doctype html>
<html><head><meta charset="utf-8"><title>Paygate API</title>
<meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body><redoc spec-url="/openapi.yaml"></redoc>
<script src="https://cdn.jsdelivr.net/npm/redoc@2.1.5/bundles/redoc.standalone.js"></script>
</body></html>`

const swaggerPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Paygate API - Swagger UI</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css"></head>
<body><div id="ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url: "/openapi.yaml", dom_id: "#ui", persistAuthorization: false, tryItOutEnabled: true});</script>
</body></html>`

func Register(r chi.Router) {
	r.Get("/swagger", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(swaggerPage))
	})
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "no-cache") // always the version of the running server
		_, _ = w.Write(spec)
	})
	r.Get("/docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
}
