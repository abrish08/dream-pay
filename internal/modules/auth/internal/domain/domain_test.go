package domain

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"paygate/internal/contracts/auth"
	"paygate/internal/platform/apperror"
)

var pepper = Pepper{Version: 1, Secret: []byte(strings.Repeat("p", 32))}

const (
	payRead, payWrite, refundWrite = "payments:read", "payments:write", "refunds:write"
	merchantsManage                = "merchants:manage"
)

var (
	keyScopes   = []string{payRead, payWrite, refundWrite} // marked APIKey by their services
	ownerPerms  = []string{payRead, payWrite, refundWrite, auth.APIKeysManage, auth.TeamManage}
	developerPs = []string{payRead, payWrite, auth.APIKeysManage}
)

func newKey(t *testing.T, scopes ...string) (*APIKey, string) {
	t.Helper()
	k, raw, err := NewKey(NewKeyInput{MerchantID: "m1", Name: "server", Environment: "live", Scopes: scopes,
		CreatorPermissions: ownerPerms, AllowedScopes: keyScopes}, pepper, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return k, raw
}

func TestKeyFormatAndStorage(t *testing.T) {
	k, raw := newKey(t, payWrite, payRead, payRead)
	if !strings.HasPrefix(raw, "sk_live_") || len(raw) != len("sk_live_")+49 || !WellFormed(raw) {
		t.Fatalf("bad key %q", raw)
	}
	if strings.Contains(k.Hash, raw) || k.Hash != HashKey(raw, pepper) || len(k.Hash) != 64 {
		t.Fatal("only the HMAC of the key may be stored")
	}
	if !strings.HasPrefix(raw, k.Prefix) || !strings.HasSuffix(raw, k.Last4) || len(k.Prefix) != 12 {
		t.Fatalf("display parts wrong: %q %q", k.Prefix, k.Last4)
	}
	if !slices.Equal(k.Scopes, []string{payRead, payWrite}) {
		t.Fatalf("scopes not normalised: %v", k.Scopes)
	}
	other := Pepper{Version: 2, Secret: []byte(strings.Repeat("q", 32))}
	if HashKey(raw, other) == k.Hash {
		t.Fatal("hash must depend on the pepper")
	}
	_, raw2 := newKey(t, payRead)
	if raw == raw2 {
		t.Fatal("keys must be random")
	}
}

func TestChecksumRejectsTypos(t *testing.T) {
	_, raw := newKey(t, payRead)
	b := []byte(raw)
	i := len("sk_live_") + 5
	if b[i] == 'a' {
		b[i] = 'b'
	} else {
		b[i] = 'a'
	}
	for _, bad := range []string{string(b), raw[:len(raw)-1], "sk_live_x", "sk_prod_" + raw[8:], "", raw + "0"} {
		if WellFormed(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestKeyScopeRules(t *testing.T) {
	cases := map[string]struct {
		scopes  []string
		creator []string
		code    string
	}{
		"no scopes":             {nil, ownerPerms, "invalid_scopes"},
		"management scope":      {[]string{auth.APIKeysManage}, ownerPerms, "invalid_scopes"},
		"platform scope":        {[]string{merchantsManage}, ownerPerms, "invalid_scopes"},
		"more than creator has": {[]string{refundWrite}, developerPs, "scope_not_held"},
	}
	for name, c := range cases {
		_, _, err := NewKey(NewKeyInput{MerchantID: "m1", Name: "k", Environment: "test", Scopes: c.scopes,
			CreatorPermissions: c.creator, AllowedScopes: keyScopes}, pepper, time.Now())
		var ae *apperror.Error
		if !errors.As(err, &ae) || ae.Code != c.code {
			t.Errorf("%s: want %s, got %v", name, c.code, err)
		}
	}
}

func TestKeyExpiry(t *testing.T) {
	now := time.Now()
	k, _, err := NewKey(NewKeyInput{MerchantID: "m1", Name: "k", Environment: "test", Scopes: []string{payRead},
		ExpiresInDays: 1, CreatorPermissions: []string{payRead}, AllowedScopes: keyScopes}, pepper, now)
	if err != nil {
		t.Fatal(err)
	}
	if !k.Usable(now) || k.Usable(now.Add(25*time.Hour)) {
		t.Fatal("expiry not applied")
	}
	k.RevokedAt = &now
	if k.Usable(now) {
		t.Fatal("revoked key usable")
	}
}

func TestPasswords(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") || strings.Contains(h, "correct") {
		t.Fatalf("unexpected hash %q", h)
	}
	if !CheckPassword("correct horse battery", h) || CheckPassword("correct horse batterY", h) {
		t.Fatal("check failed")
	}
	if h2, _ := HashPassword("correct horse battery"); h2 == h {
		t.Fatal("hashes must be salted")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
}

func TestLockout(t *testing.T) {
	now := time.Now()
	u := &User{}
	for i := 0; i < MaxFailedLogins; i++ {
		if u.Locked(now) {
			t.Fatalf("locked after %d failures", i)
		}
		u.LoginFailed(now)
	}
	if !u.Locked(now) || u.Locked(now.Add(LockoutDuration+time.Second)) {
		t.Fatal("lockout window wrong")
	}
}

func catalog() Catalog {
	dep := time.Now()
	return Catalog{
		payRead:            {Code: payRead, Audience: auth.AudienceMerchant, APIKey: true},
		payWrite:           {Code: payWrite, Audience: auth.AudienceMerchant, APIKey: true},
		refundWrite:        {Code: refundWrite, Audience: auth.AudienceMerchant, APIKey: true},
		auth.RolesManage:   {Code: auth.RolesManage, Audience: auth.AudienceBoth},
		merchantsManage:    {Code: merchantsManage, Audience: auth.AudiencePlatform},
		"old:thing":        {Code: "old:thing", Audience: auth.AudienceMerchant, DeprecatedAt: &dep},
		auth.APIKeysManage: {Code: auth.APIKeysManage, Audience: auth.AudienceMerchant},
	}
}

func TestEffectivePermissions(t *testing.T) {
	c := catalog()
	dev := &Role{ID: "r1", Audience: auth.AudienceMerchant, Permissions: []string{payRead, payWrite, "old:thing"}}
	refunds := &Role{ID: "r2", Audience: auth.AudienceMerchant, Permissions: []string{payRead, refundWrite}}
	got := c.Effective([]*Role{dev, refunds}, auth.AudienceMerchant)
	if !slices.Equal(got, []string{payRead, payWrite, refundWrite}) {
		t.Fatalf("union of roles, deprecated dropped: got %v", got)
	}
	owner := &Role{ID: "r3", Audience: auth.AudienceMerchant, AllPermissions: true}
	got = c.Effective([]*Role{owner}, auth.AudienceMerchant)
	if !slices.Equal(got, []string{auth.APIKeysManage, payRead, payWrite, refundWrite, auth.RolesManage}) {
		t.Fatalf("owner = every active merchant permission, no platform ones: got %v", got)
	}
	super := &Role{ID: "r4", Audience: auth.AudiencePlatform, AllPermissions: true}
	got = c.Effective([]*Role{super, dev}, auth.AudiencePlatform)
	if !slices.Equal(got, []string{merchantsManage, auth.RolesManage}) {
		t.Fatalf("platform user ignores merchant roles: got %v", got)
	}
	if !slices.Equal(c.APIKeyScopes(), []string{payRead, payWrite, refundWrite}) {
		t.Fatalf("api key scopes: %v", c.APIKeyScopes())
	}
}

func TestRoleRules(t *testing.T) {
	c := catalog()
	if err := c.CheckAssignable([]string{merchantsManage}, auth.AudienceMerchant); err == nil {
		t.Fatal("platform permission accepted in a merchant role")
	}
	if err := c.CheckAssignable([]string{"old:thing"}, auth.AudienceMerchant); err == nil {
		t.Fatal("deprecated permission accepted")
	}
	if err := c.CheckAssignable([]string{auth.RolesManage, payRead}, auth.AudienceMerchant); err != nil {
		t.Fatal(err)
	}
	if err := CanGrant([]string{payRead}, []string{payRead, payWrite}); err == nil {
		t.Fatal("escalation allowed")
	}
	for _, bad := range []string{"", "x", "Payments:read", "payments:", "a:b:c"} {
		if ValidatePermissionDef(auth.PermissionDef{Code: bad, Audience: auth.AudienceMerchant}) == nil {
			t.Errorf("accepted code %q", bad)
		}
	}
	if ValidatePermissionDef(auth.PermissionDef{Code: "ledger:read", Audience: auth.AudiencePlatform, APIKey: true}) == nil {
		t.Error("platform permission accepted as API key scope")
	}
	if _, err := NewRole(NewRoleInput{Name: "x"}, time.Now()); err == nil {
		t.Error("short role name accepted")
	}
	if SystemRoleID(auth.RoleOwner) != SystemRoleID(auth.RoleOwner) || SystemRoleID(auth.RoleOwner) == SystemRoleID(auth.RoleAdmin) {
		t.Error("system role ids must be stable and distinct")
	}
}

func TestUserPrincipal(t *testing.T) {
	u, err := NewUser(NewUserInput{Email: " Owner@Shop.CO ", Name: "O", Password: "long-enough-password", MerchantID: "m1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	roles := []*Role{
		{ID: "r1", Name: "developer", Audience: auth.AudienceMerchant, Permissions: []string{payRead, payWrite}},
		{ID: "r2", Name: "refunds", Audience: auth.AudienceMerchant, Permissions: []string{refundWrite}},
	}
	p := u.Principal(roles, catalog())
	if u.Email != "owner@shop.co" || p.MerchantID != "m1" || p.Role != "developer" ||
		!slices.Equal(p.Roles, []string{"r1", "r2"}) || !p.Can(refundWrite) || p.Can(auth.APIKeysManage) {
		t.Fatalf("unexpected principal %+v", p)
	}
}
