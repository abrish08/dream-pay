// Package config loads runtime configuration from environment variables.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"paygate/internal/platform/authn"
	"paygate/internal/platform/mail"
	"paygate/internal/platform/sms"
)

type Config struct {
	HTTPAddr string

	// Modules enabled in this process. nil means "all".
	Modules map[string]bool

	// DatabaseURL is the shared default. Each module connects with its own
	// search_path (its schema). DB_URL_<MODULE> overrides it per module,
	// which is how a module moves to its own database.
	DatabaseURL  string
	ModuleDBURLs map[string]string
	DBMaxConns   int

	// Bus is the central communication channel: "memory" (all modules in one
	// process) or "nats" (modules split across processes). No service URLs:
	// NATS routes each request to whichever process handles it.
	Bus     string
	NATSURL string

	// Auth. Only the auth service holds AUTH_SIGNING_KEY (private). Every
	// service holds AUTH_PUBLIC_KEYS to verify access tokens locally.
	AuthSigningKey         []byte // 32-byte Ed25519 seed
	AuthPublicKeys         []ed25519.PublicKey
	AuthKeyCacheTTL        time.Duration // how long a service caches an API key's token (0 = off)
	APIKeyPepper           string
	APIKeyPepperVersion    int
	APIKeyPepperPrevious   string // set only while rotating the pepper
	BootstrapAdminEmail    string // first super_admin, created once
	BootstrapAdminPassword string
	BootstrapAdminName     string

	EncryptionKey         []byte
	PublicBaseURL         string
	AllowInsecureWebhooks bool
	AllowMockInLive       bool

	// Rate limits (per process). 0 disables.
	RateLimitRPS     float64 // per API key / user on /v1 and /admin
	RateLimitBurst   float64
	AuthRateLimitMin float64 // per client IP on /auth/login, /auth/signup, /auth/refresh (per minute)
	TrustProxy       bool    // read the client IP from X-Forwarded-For (only behind your own proxy)
	CORSOrigins      []string

	// Payments without a final answer from the provider expire after this.
	// Sign in with Google / Apple: the client ids of YOUR apps (web client id,
	// iOS bundle id, Android client id, Apple services id). Empty = disabled.
	GoogleClientIDs []string
	AppleClientIDs  []string
	GoogleJWKSURL   string // override for tests; default Google's public keys
	AppleJWKSURL    string

	// Email (password reset, notices). Without SMTP_HOST emails are only
	// logged (development).
	SMTPHost, SMTPPort, SMTPUsername, SMTPPassword, MailFrom string
	DashboardURL                                             string        // where reset links point (your dashboard)
	PasswordResetTTL                                         time.Duration // default 30m
	Mailer                                                   mail.Sender   // set directly by tests / embedders; overrides SMTP

	// SMS through send.et (verification codes by SMS). Empty key = disabled.
	SendEtAPIKey, SendEtSender, SendEtBaseURL string
	SMS                                       sms.Sender // set directly by tests; overrides send.et

	// HSTS: tell browsers to use HTTPS only (enable when served over HTTPS).
	HSTS bool

	// ProviderRoutes: payment method -> providers in order of preference,
	// from PROVIDER_ROUTES="telebirr=telebirr;card=ethswitch,bank_x".
	ProviderRoutes map[string][]string

	PaymentTTL         time.Duration
	PaymentExpiryEvery time.Duration // how often overdue payments are checked (default 15s)
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:               env("HTTP_ADDR", ":8080"),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		ModuleDBURLs:           prefixed("DB_URL_"),
		DBMaxConns:             envInt("DB_MAX_CONNS_PER_MODULE", 10),
		Bus:                    env("BUS", "memory"),
		NATSURL:                env("NATS_URL", "nats://localhost:4222"),
		APIKeyPepper:           os.Getenv("API_KEY_PEPPER"),
		APIKeyPepperVersion:    envInt("API_KEY_PEPPER_VERSION", 1),
		APIKeyPepperPrevious:   os.Getenv("API_KEY_PEPPER_PREVIOUS"),
		AuthKeyCacheTTL:        30 * time.Second,
		BootstrapAdminEmail:    os.Getenv("BOOTSTRAP_ADMIN_EMAIL"),
		BootstrapAdminPassword: os.Getenv("BOOTSTRAP_ADMIN_PASSWORD"),
		BootstrapAdminName:     env("BOOTSTRAP_ADMIN_NAME", "Administrator"),
		PublicBaseURL:          strings.TrimRight(env("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		AllowInsecureWebhooks:  envBool("WEBHOOK_ALLOW_INSECURE", false),
		AllowMockInLive:        envBool("ALLOW_MOCK_PROVIDER_IN_LIVE", false),
	}

	if mods := strings.TrimSpace(env("MODULES", "all")); mods != "all" {
		c.Modules = map[string]bool{}
		for _, m := range strings.Split(mods, ",") {
			if m = strings.TrimSpace(m); m != "" {
				c.Modules[m] = true
			}
		}
	}

	if v := os.Getenv("AUTH_SIGNING_KEY"); v != "" {
		seed, err := authn.ParseSeed(v)
		if err != nil {
			return c, fmt.Errorf("AUTH_SIGNING_KEY %v", err)
		}
		c.AuthSigningKey = seed
	}
	keys, err := authn.ParsePublicKeys(os.Getenv("AUTH_PUBLIC_KEYS"))
	if err != nil {
		return c, fmt.Errorf("AUTH_PUBLIC_KEYS: %v", err)
	}
	c.AuthPublicKeys = keys
	if v := os.Getenv("AUTH_KEY_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || d > 5*time.Minute {
			return c, fmt.Errorf("AUTH_KEY_CACHE_TTL must be a duration between 0s and 5m")
		}
		c.AuthKeyCacheTTL = d
	}

	c.RateLimitRPS = envFloat("RATE_LIMIT_RPS", 50)
	c.RateLimitBurst = envFloat("RATE_LIMIT_BURST", 100)
	c.AuthRateLimitMin = envFloat("AUTH_RATE_LIMIT_PER_MIN", 20)
	c.TrustProxy = envBool("TRUST_PROXY", false)
	for _, o := range strings.Split(os.Getenv("CORS_ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}
	c.GoogleClientIDs = list(os.Getenv("GOOGLE_CLIENT_IDS"))
	c.AppleClientIDs = list(os.Getenv("APPLE_CLIENT_IDS"))
	c.GoogleJWKSURL, c.AppleJWKSURL = os.Getenv("GOOGLE_JWKS_URL"), os.Getenv("APPLE_JWKS_URL")
	c.SMTPHost, c.SMTPPort = os.Getenv("SMTP_HOST"), env("SMTP_PORT", "587")
	c.SMTPUsername, c.SMTPPassword = os.Getenv("SMTP_USERNAME"), os.Getenv("SMTP_PASSWORD")
	c.MailFrom = env("MAIL_FROM", "paygate <no-reply@example.com>")
	c.DashboardURL = env("DASHBOARD_URL", c.PublicBaseURL)
	c.PasswordResetTTL = 30 * time.Minute
	if v := os.Getenv("PASSWORD_RESET_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 5*time.Minute || d > 24*time.Hour {
			return c, fmt.Errorf("PASSWORD_RESET_TTL must be between 5m and 24h")
		}
		c.PasswordResetTTL = d
	}
	c.SendEtAPIKey, c.SendEtSender = os.Getenv("SENDET_API_KEY"), env("SENDET_SENDER", "Paygate")
	c.SendEtBaseURL = env("SENDET_BASE_URL", "https://api.send.et/v1")
	c.HSTS = envBool("HSTS", false)
	c.ProviderRoutes = map[string][]string{}
	for _, part := range strings.Split(os.Getenv("PROVIDER_ROUTES"), ";") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		method, list, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(method) == "" {
			return c, fmt.Errorf("PROVIDER_ROUTES: %q must look like method=provider1,provider2", part)
		}
		for _, p := range strings.Split(list, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.ProviderRoutes[strings.TrimSpace(method)] = append(c.ProviderRoutes[strings.TrimSpace(method)], p)
			}
		}
	}
	c.PaymentTTL = 30 * time.Minute
	if v := os.Getenv("PAYMENT_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute || d > 7*24*time.Hour {
			return c, fmt.Errorf("PAYMENT_TTL must be a duration between 1m and 168h")
		}
		c.PaymentTTL = d
	}

	c.PaymentExpiryEvery = 15 * time.Second
	if v := os.Getenv("PAYMENT_EXPIRY_CHECK_EVERY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 100*time.Millisecond {
			return c, fmt.Errorf("PAYMENT_EXPIRY_CHECK_EVERY must be a duration of at least 100ms")
		}
		c.PaymentExpiryEvery = d
	}

	if k := os.Getenv("DATA_ENCRYPTION_KEY"); k != "" {
		b, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(b) != 32 {
			return c, fmt.Errorf("DATA_ENCRYPTION_KEY must be 32 bytes, base64 encoded")
		}
		c.EncryptionKey = b
	}
	return c, nil
}

// Validate checks names in MODULES and the settings each enabled module needs.
func (c Config) Validate(known []string) error {
	for m := range c.Modules {
		if !contains(known, m) {
			return fmt.Errorf("unknown module %q in MODULES (known: %s)", m, strings.Join(known, ","))
		}
	}
	if c.ModuleEnabled("auth") {
		if len(c.AuthSigningKey) == 0 {
			return fmt.Errorf("AUTH_SIGNING_KEY is required by the auth service (generate one: go run ./cmd/paygate keygen)")
		}
		if len(c.APIKeyPepper) < 32 {
			return fmt.Errorf("API_KEY_PEPPER must be at least 32 characters")
		}
		if c.APIKeyPepperPrevious != "" && c.APIKeyPepperVersion < 2 {
			return fmt.Errorf("API_KEY_PEPPER_VERSION must be raised when API_KEY_PEPPER_PREVIOUS is set")
		}
		if (c.BootstrapAdminEmail == "") != (c.BootstrapAdminPassword == "") {
			return fmt.Errorf("set both BOOTSTRAP_ADMIN_EMAIL and BOOTSTRAP_ADMIN_PASSWORD, or neither")
		}
		if c.BootstrapAdminPassword == "CHANGE_ME" || c.BootstrapAdminEmail == "admin@example.com" {
			return fmt.Errorf("BOOTSTRAP_ADMIN_EMAIL / BOOTSTRAP_ADMIN_PASSWORD still have the example values; set your own in .env")
		}
	}
	if len(c.AuthPublicKeys) == 0 && len(c.AuthSigningKey) == 0 {
		return fmt.Errorf("AUTH_PUBLIC_KEYS is required: every service verifies access tokens with the auth service's public key")
	}
	if (c.ModuleEnabled("webhook") || c.ModuleEnabled("auth")) && len(c.EncryptionKey) != 32 {
		return fmt.Errorf("DATA_ENCRYPTION_KEY is required by the webhook and auth modules")
	}
	if !c.AllEnabled(known) && c.Bus != "nats" {
		return fmt.Errorf("MODULES runs only part of the system here: set BUS=nats so modules in other processes can be reached")
	}
	return nil
}

func (c Config) ModuleEnabled(name string) bool { return c.Modules == nil || c.Modules[name] }

func (c Config) AllEnabled(known []string) bool {
	for _, m := range known {
		if !c.ModuleEnabled(m) {
			return false
		}
	}
	return true
}

// ModuleDSN returns the connection string for a module. The module's schema is
// always forced as search_path, so table names in code stay unqualified and
// the same code works on a shared database or a dedicated one.
func (c Config) ModuleDSN(name string) (string, error) {
	dsn := c.ModuleDBURLs[name]
	if dsn == "" {
		dsn = c.DatabaseURL
	}
	if dsn == "" {
		return "", fmt.Errorf("DATABASE_URL (or DB_URL_%s) is required", strings.ToUpper(name))
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return "", fmt.Errorf("database URL for %s must be a postgres:// URL", name)
	}
	q := u.Query()
	q.Set("search_path", name)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// prefixed collects PREFIX_<MODULE>=value into map[module]value.
func prefixed(prefix string) map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, prefix) && v != "" {
			out[strings.ToLower(strings.TrimPrefix(k, prefix))] = v
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// envFloat: unset means def; "0" is allowed (disables a limit).
func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil && v >= 0 {
		return v
	}
	return def
}

func list(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
