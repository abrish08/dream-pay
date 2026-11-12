package httpx

import "testing"

func TestActionOf(t *testing.T) {
	for _, c := range []struct{ method, route, want string }{
		{"POST", "/v1/payments", "payments.create"},
		{"GET", "/v1/payments", "payments.list"},
		{"GET", "/v1/payments/{id}", "payments.view"},
		{"GET", "/v1/payments/{id}/events", "payments.events.list"},
		{"DELETE", "/v1/api-keys/{id}", "api_keys.delete"},
		{"POST", "/v1/api-keys/{id}/roll", "api_keys.roll"},
		{"POST", "/v1/kyc/fayda/start", "kyc.fayda.start"},
		{"PUT", "/v1/kyc/profile", "kyc.profile.update"},
		{"POST", "/admin/payouts/{id}/paid", "admin.payouts.paid"},
		{"POST", "/admin/ledger/adjustments", "admin.ledger.adjustments.create"},
		{"GET", "/admin/audit-logs", "admin.audit_logs.list"},
		{"POST", "/auth/login", "auth.login"},
		{"POST", "/auth/mfa/totp/confirm", "auth.mfa.totp.confirm"},
		{"GET", "/v1/reports/payments.csv", "reports.payments.export"},
		{"POST", "/providers/{provider}/callback", "providers.callback"},
		{"POST", "/pay/{id}", "pay"},
	} {
		if got := ActionOf(c.method, c.route); got != c.want {
			t.Errorf("%s %s: got %q, want %q", c.method, c.route, got, c.want)
		}
	}
}
