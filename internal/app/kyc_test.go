package app_test

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fmt"

	"paygate/internal/modules/kyc/faydatest"
	"paygate/internal/platform/config"
)

var (
	samplePDF = []byte("%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")
	samplePNG = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 64)...)
)

// upload sends one KYC document as multipart/form-data.
func (c *client) upload(token, docType, name string, data []byte) rawResp {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("type", docType)
	part, _ := w.CreateFormFile("file", name)
	_, _ = part.Write(data)
	_ = w.Close()
	req, _ := http.NewRequest("POST", c.base+"/v1/kyc/documents", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return rawResp{code: -1}
	}
	defer resp.Body.Close()
	out := rawResp{code: resp.StatusCode, header: resp.Header, body: map[string]any{}}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out.body)
	return out
}

var kycProfile = map[string]any{
	"business_type": "plc", "legal_name": "Abebe Coffee PLC", "trade_name": "Abebe Coffee",
	"registration_number": "MT/AA/2/0012345/2016", "tin": "0012345678", "trade_license_number": "AA/14/123/2024",
	"trade_license_expiry": time.Now().AddDate(1, 0, 0).Format("2006-01-02"), "applicant_role": "general_manager",
	"industry": "Coffee retail", "expected_monthly_volume_etb": 500000,
	"address": map[string]string{"region": "Addis Ababa", "city": "Addis Ababa", "sub_city": "Bole", "woreda": "03"},
	"owner": map[string]string{"full_name": "Abebe Kebede", "id_type": "passport", "id_number": "EP99887766",
		"date_of_birth": "1985-04-12", "nationality": "ET", "phone": "+251911234567"},
}

// uploadRequired uploads every document the application still asks for.
func (c *client) uploadRequired(owner string) {
	c.t.Helper()
	v := c.do("GET", "/v1/kyc", owner, "", nil, 200)
	have := map[string]bool{}
	for _, d := range v["documents"].([]any) {
		have[d.(map[string]any)["type"].(string)] = true
	}
	for _, d := range v["required_documents"].([]any) {
		if have[d.(string)] {
			continue
		}
		if r := c.upload(owner, d.(string), d.(string)+".pdf", samplePDF); r.code != 201 {
			c.t.Fatalf("upload %s: %d %v", d, r.code, r.body)
		}
	}
}

// fetch downloads a document link (no credentials: the link is the proof).
func fetch(t *testing.T, link string) (int, []byte, http.Header) {
	t.Helper()
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

// passKYC fills in and submits a complete verification, approves it as
// staff, and waits for the merchant to become active.
func (c *client) passKYC(owner, merchantID string) {
	c.t.Helper()
	c.do("PUT", "/v1/kyc/profile", owner, "", kycProfile, 200)
	c.uploadRequired(owner)
	c.do("POST", "/v1/kyc/submit", owner, "", nil, 200)
	c.do("POST", "/admin/kyc/"+merchantID+"/approve", c.admin, "", map[string]any{"note": "documents verified"}, 200)
	c.eventually("merchant activated by KYC approval", func() bool {
		return c.do("GET", "/v1/merchant", owner, "", nil, 200)["status"] == "active"
	})
}

// TestKYC: a self-signed-up merchant verifies its business before going live.
func TestKYC(t *testing.T) {
	storageDir := t.TempDir()
	c, _, stop := startApp(t, func(cfg *config.Config) { cfg.KYCStorageDir = storageDir })
	defer stop()
	owner, key := c.signup("kyc@abebe.test")
	me := c.do("GET", "/v1/merchant", owner, "", nil, 200)
	mid := me["id"].(string)
	if me["kyc_status"] != "not_started" {
		t.Fatalf("new merchant: %v", me)
	}

	// Not verified: staff cannot simply activate it.
	if e := c.do("POST", "/admin/merchants/"+mid+"/status", c.admin, "", map[string]any{"status": "active"}, 409); e["error"].(map[string]any)["code"] != "kyc_not_approved" {
		t.Fatalf("activation without KYC: %v", e)
	}

	// Start: nothing filled in.
	v := c.do("GET", "/v1/kyc", owner, "", nil, 200)
	if v["status"] != "not_started" || len(v["missing"].([]any)) == 0 {
		t.Fatalf("empty application: %v", v)
	}
	c.do("POST", "/v1/kyc/submit", owner, "", nil, 422) // incomplete
	bad := map[string]any{"business_type": "plc", "tin": "123"}
	c.do("PUT", "/v1/kyc/profile", owner, "", bad, 400)
	v = c.do("PUT", "/v1/kyc/profile", owner, "", kycProfile, 200)
	req := fmt.Sprint(v["required_documents"])
	for _, d := range []string{"trade_license", "commercial_registration", "tin_certificate", "memorandum_of_association",
		"shareholders_list", "business_address_proof", "bank_account_proof", "owner_id"} {
		if !strings.Contains(req, d) {
			t.Errorf("PLC must provide %s: %s", d, req)
		}
	}
	if v["status"] != "draft" || strings.Contains(req, "power_of_attorney") || strings.Contains(req, "vat_certificate") {
		t.Fatalf("profile saved: %v", v)
	}
	if v["owner_identity"].(map[string]any)["fayda_available"] != false {
		t.Fatal("Fayda is not configured in this test")
	}
	c.do("POST", "/v1/kyc/fayda/start", owner, "", nil, 502) // not configured

	// No file over 5 MB.
	big := append(append([]byte{}, samplePDF...), make([]byte, 5<<20)...)
	if r := c.upload(owner, "trade_license", "big.pdf", big); r.code != 413 {
		t.Fatalf("file over 5 MB: %d %v", r.code, r.body)
	}

	// Documents: only real PDF / JPEG / PNG, only by logged-in users.
	if r := c.upload(owner, "owner_id", "virus.pdf", []byte("MZ\x90\x00 not a document")); r.code != 400 {
		t.Fatalf("fake pdf accepted: %d %v", r.code, r.body)
	}
	if r := c.upload(key, "owner_id", "id.pdf", samplePDF); r.code != 403 {
		t.Fatalf("API key upload: %d", r.code)
	}
	if r := c.upload(owner, "selfie", "x.pdf", samplePDF); r.code != 400 {
		t.Fatalf("unknown document type: %d", r.code)
	}
	ids := map[string]string{}
	for _, d := range []string{"trade_license", "tin_certificate", "commercial_registration", "memorandum_of_association",
		"shareholders_list", "business_address_proof", "bank_account_proof"} {
		r := c.upload(owner, d, "../../"+d+".pdf", samplePDF)
		if r.code != 201 || strings.Contains(r.body["file_name"].(string), "/") {
			t.Fatalf("upload %s: %d %v", d, r.code, r.body)
		}
		ids[d] = r.body["id"].(string)
	}
	idDoc := c.upload(owner, "owner_id", "passport.png", samplePNG)
	if idDoc.code != 201 || idDoc.body["content_type"] != "image/png" {
		t.Fatalf("png upload: %v", idDoc.body)
	}

	// At rest: files are encrypted, the owner's personal data too.
	_ = filepath.WalkDir(storageDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("%PDF")) || bytes.Contains(b, []byte("PNG")) {
				t.Errorf("document stored in clear text: %s", p)
			}
		}
		return nil
	})
	var plain int64
	db := openTestDB(t)
	db.Raw("SELECT count(*) FROM kyc.kyc_applications WHERE owner_encrypted LIKE '%EP99887766%'").Scan(&plain)
	if plain != 0 {
		t.Fatal("owner ID number stored in clear text")
	}
	// The database keeps object keys only, never URLs.
	var keys []string
	db.Raw("SELECT object_key FROM kyc.kyc_documents WHERE merchant_id = ?", mid).Scan(&keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, "kyc/"+mid+"/") || strings.Contains(k, "http") {
			t.Fatalf("stored value is not an object key: %q", k)
		}
	}
	if len(keys) != 8 {
		t.Fatalf("object keys: %v", keys)
	}

	// Submit: frozen while under review.
	if v = c.do("POST", "/v1/kyc/submit", owner, "", nil, 200); v["status"] != "submitted" {
		t.Fatalf("submit: %v", v)
	}
	c.do("PUT", "/v1/kyc/profile", owner, "", kycProfile, 409)
	if r := c.upload(owner, "other", "x.pdf", samplePDF); r.code != 409 {
		t.Fatalf("upload while submitted: %d", r.code)
	}
	c.eventually("received email", func() bool {
		m, _ := c.outbox.Last("kyc@abebe.test")
		return strings.Contains(m.Subject, "received your business verification")
	})

	// Review: queue, details with history, document download (recorded).
	queue := c.do("GET", "/admin/kyc?status=submitted", c.admin, "", nil, 200)["data"].([]any)
	if len(queue) != 1 || queue[0].(map[string]any)["merchant_id"] != mid {
		t.Fatalf("queue: %v", queue)
	}
	link := c.do("GET", "/admin/kyc/"+mid+"/documents/"+ids["tin_certificate"], c.admin, "", nil, 200)
	url := link["url"].(string)
	if !strings.HasPrefix(url, c.base+"/kyc/files/") || link["expires_at"] == nil {
		t.Fatalf("document link: %v", link)
	}
	code, got, hdr := fetch(t, url)
	if code != 200 || !bytes.Equal(got, samplePDF) || hdr.Get("Content-Type") != "application/pdf" ||
		!strings.Contains(hdr.Get("Content-Disposition"), "attachment") || hdr.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("download: %d %q %v", code, got, hdr)
	}
	if code, _, _ := fetch(t, url[:len(url)-3]+"abc"); code != 404 {
		t.Fatalf("tampered link: %d", code)
	}
	// The merchant's own view carries fresh links too (built on every read).
	for _, d := range c.do("GET", "/v1/kyc", owner, "", nil, 200)["documents"].([]any) {
		if u, _ := d.(map[string]any)["url"].(string); !strings.HasPrefix(u, c.base+"/kyc/files/") {
			t.Fatalf("document without link: %v", d)
		}
	}
	other, _ := c.signup("other@abebe.test")
	c.do("GET", "/v1/kyc/documents/"+ids["tin_certificate"], other, "", nil, 404) // not theirs
	rev := c.do("GET", "/admin/kyc/"+mid, c.admin, "", nil, 200)
	if rev["profile"].(map[string]any)["owner"].(map[string]any)["id_number"] != "EP99887766" ||
		!strings.Contains(fmt.Sprint(rev["history"]), "document_viewed") {
		t.Fatalf("review: %v", rev)
	}

	// Who may do what.
	support := c.do("POST", "/admin/users", c.admin, "", map[string]any{"email": "support@paygate.test", "name": "S",
		"password": "support-password-1", "roles": []string{"support"}}, 201)
	_ = support
	sup := c.login("support@paygate.test", "support-password-1", 200)["access_token"].(string)
	c.do("POST", "/admin/kyc/"+mid+"/approve", sup, "", nil, 403) // no kyc:review
	c.do("GET", "/v1/kyc", key, "", nil, 403)                     // API keys never see KYC

	// Reject with a reason: the merchant fixes and resubmits.
	c.do("POST", "/admin/kyc/"+mid+"/reject", c.admin, "", map[string]any{"reason": ""}, 400)
	c.do("POST", "/admin/kyc/"+mid+"/reject", c.admin, "", map[string]any{"reason": "The TIN certificate is unreadable"}, 200)
	c.eventually("rejection email with the reason", func() bool {
		m, _ := c.outbox.Last("kyc@abebe.test")
		return strings.Contains(m.Text, "The TIN certificate is unreadable")
	})
	if v = c.do("GET", "/v1/kyc", owner, "", nil, 200); v["status"] != "rejected" || v["rejection_reason"] == nil {
		t.Fatalf("rejected: %v", v)
	}
	c.do("POST", "/admin/kyc/"+mid+"/approve", c.admin, "", nil, 409) // not submitted
	c.do("DELETE", "/v1/kyc/documents/"+ids["tin_certificate"], owner, "", nil, 204)
	c.do("POST", "/v1/kyc/submit", owner, "", nil, 422) // tin_certificate missing now
	if r := c.upload(owner, "tin_certificate", "tin-clear.pdf", samplePDF); r.code != 201 {
		t.Fatalf("re-upload: %d", r.code)
	}
	c.do("POST", "/v1/kyc/submit", owner, "", nil, 200)

	// Approve: the merchant goes live automatically.
	c.do("POST", "/admin/kyc/"+mid+"/approve", c.admin, "", map[string]any{"note": "verified with ERCA"}, 200)
	c.eventually("merchant active after approval", func() bool {
		m := c.do("GET", "/v1/merchant", owner, "", nil, 200)
		return m["status"] == "active" && m["kyc_status"] == "approved"
	})
	c.eventually("live keys allowed", func() bool {
		return c.status2("POST", "/v1/api-keys", owner, map[string]any{"name": "live", "environment": "live",
			"scopes": []string{"payments:write"}}) == 201
	})
	c.do("POST", "/v1/kyc/submit", owner, "", nil, 409)                                 // approved: locked
	c.do("DELETE", "/v1/kyc/documents/"+ids["bank_account_proof"], owner, "", nil, 409) // approved: locked
}

// TestMerchantRoleAndLiveMode: every new merchant gets the default "merchant"
// role (seeded once from MERCHANT_DEFAULT_PERMISSIONS); staff change its
// permissions and every merchant follows within seconds. Live mode needs
// approved KYC for every merchant, also those created by staff.
func TestMerchantRoleAndLiveMode(t *testing.T) {
	c, _, stop := startApp(t, func(cfg *config.Config) {
		cfg.MerchantDefaultPermissions = []string{"payments:read", "payments:write", "balance:read", "apikeys:manage",
			"kyc:read", "kyc:write", "refunds:write", "webhooks:write"}
	})
	defer stop()

	// Seeded role, visible to staff with its permissions.
	var roleID string
	for _, r := range c.do("GET", "/admin/roles?audience=merchant", c.admin, "", nil, 200)["data"].([]any) {
		if r := r.(map[string]any); r["name"] == "merchant" {
			roleID = r["id"].(string)
			if r["built_in"] != true || r["locked"] != false || len(r["permissions"].([]any)) != 8 {
				t.Fatalf("merchant role: %v", r)
			}
		}
	}
	if roleID == "" {
		t.Fatal("merchant role not seeded")
	}

	// New merchants get it.
	owner, key := c.signup("role@shop.test")
	me := c.do("GET", "/auth/me", owner, "", nil, 200)
	if me["role"] != "merchant" || strings.Contains(fmt.Sprint(me["permissions"]), "team:manage") {
		t.Fatalf("new merchant user: %v", me)
	}
	c.do("GET", "/v1/team/members", owner, "", nil, 403) // not in the seeded list

	// Staff add and remove permissions: every merchant follows.
	c.do("POST", "/admin/roles/"+roleID+"/permissions", c.admin, "", map[string]any{"permissions": []string{"team:manage"}}, 200)
	c.do("DELETE", "/admin/roles/"+roleID+"/permissions/refunds:write", c.admin, "", nil, 200)
	c.eventually("old token is stale after the role changed", func() bool {
		return c.status2("GET", "/auth/me", owner, nil) == 401
	})
	owner = c.login("role@shop.test", "owner-password-123", 200)["access_token"].(string)
	perms := fmt.Sprint(c.do("GET", "/auth/me", owner, "", nil, 200)["permissions"])
	if !strings.Contains(perms, "team:manage") || strings.Contains(perms, "refunds:write") {
		t.Fatalf("permissions after the change: %s", perms)
	}
	c.do("GET", "/v1/team/members", owner, "", nil, 200)
	// A merchant cannot edit the shared role itself.
	c.do("POST", "/v1/roles/"+roleID+"/permissions", owner, "", map[string]any{"permissions": []string{"payments:read"}}, 403)

	// Test mode works without KYC; live mode does not.
	if key == "" {
		t.Fatal("test key")
	}
	c.do("POST", "/v1/payments", key, "t-1", map[string]any{"amount": 1000, "currency": "ETB", "reference": "T-1"}, 201)
	e := c.do("POST", "/v1/api-keys", owner, "", map[string]any{"name": "live", "environment": "live", "scopes": []string{"payments:read"}}, 403)
	if e["error"].(map[string]any)["code"] != "kyc_required" {
		t.Fatalf("live key without KYC: %v", e)
	}

	// Staff-created merchant: active at once (test mode), but live still needs KYC.
	mid := c.do("POST", "/admin/merchants", c.admin, "", map[string]any{"name": "Staff Made", "email": "staff-made@shop.test"}, 201)
	if mid["status"] != "active" || mid["kyc_status"] != "not_started" {
		t.Fatalf("staff-created merchant: %v", mid)
	}
	u := c.do("POST", "/admin/users", c.admin, "", map[string]any{"email": "o@staffmade.test", "name": "O",
		"password": "owner-password-123", "merchant_id": mid["id"]}, 201)
	if u["role"] != "merchant" {
		t.Fatalf("merchant user created without a role gets the default: %v", u)
	}
	staffOwner := c.login("o@staffmade.test", "owner-password-123", 200)["access_token"].(string)
	c.do("POST", "/v1/api-keys", staffOwner, "", map[string]any{"name": "t", "environment": "test", "scopes": []string{"payments:read"}}, 201)
	c.do("POST", "/v1/api-keys", staffOwner, "", map[string]any{"name": "l", "environment": "live", "scopes": []string{"payments:read"}}, 403)

	// KYC can be submitted any time; approval opens live mode.
	c.passKYCActive(staffOwner, mid["id"].(string))
	c.eventually("live key after KYC approval", func() bool {
		return c.status2("POST", "/v1/api-keys", staffOwner, map[string]any{"name": "l", "environment": "live",
			"scopes": []string{"payments:read"}}) == 201
	})

	// Staff never need KYC.
	if c.status2("GET", "/admin/merchants", c.admin, nil) != 200 {
		t.Fatal("staff blocked")
	}
}

// passKYCActive: like passKYC for a merchant that is already active.
func (c *client) passKYCActive(owner, merchantID string) {
	c.t.Helper()
	c.do("PUT", "/v1/kyc/profile", owner, "", kycProfile, 200)
	c.uploadRequired(owner)
	c.do("POST", "/v1/kyc/submit", owner, "", nil, 200)
	c.do("POST", "/admin/kyc/"+merchantID+"/approve", c.admin, "", nil, 200)
	c.eventually("KYC approved on the merchant", func() bool {
		return c.do("GET", "/v1/merchant", owner, "", nil, 200)["kyc_status"] == "approved"
	})
}

// TestKYCFayda: the owner proves their identity with Fayda (national digital
// ID). Owner details come from Fayda and are locked; no ID scan is needed.
func TestKYCFayda(t *testing.T) {
	idp := faydatest.New("paygate-test-client")
	defer idp.Close()
	c, _, stop := startApp(t, func(cfg *config.Config) {
		cfg.FaydaIssuer, cfg.FaydaClientID, cfg.FaydaPrivateKey = idp.URL, idp.ClientID, idp.PrivateKeyJWK()
		cfg.FaydaRedirectURI = cfg.PublicBaseURL + "/kyc/fayda/callback"
		cfg.DashboardURL = "https://dashboard.paygate.test"
	})
	defer stop()
	owner, _ := c.signup("fayda@abebe.test")
	mid := c.do("GET", "/v1/merchant", owner, "", nil, 200)["id"].(string)

	p := map[string]any{}
	for k, v := range kycProfile {
		p[k] = v
	}
	p["owner"] = map[string]string{"id_type": "fayda"}
	v := c.do("PUT", "/v1/kyc/profile", owner, "", p, 200)
	if !strings.Contains(fmt.Sprint(v["missing"]), "Fayda") || v["owner_identity"].(map[string]any)["fayda_available"] != true {
		t.Fatalf("Fayda chosen, not yet verified: %v", v)
	}

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	signIn := func() string { // start → Fayda sign-in → callback; returns where the browser lands
		t.Helper()
		start := c.do("POST", "/v1/kyc/fayda/start", owner, "", nil, 200)
		resp, err := noFollow.Get(start["authorize_url"].(string))
		if err != nil || resp.StatusCode != 302 {
			t.Fatalf("authorize: %v %v", resp, err)
		}
		callback := resp.Header.Get("Location")
		resp, err = noFollow.Get(callback)
		if err != nil || resp.StatusCode != 303 {
			t.Fatalf("callback: %v %v", resp, err)
		}
		return resp.Header.Get("Location") + " " + callback
	}

	// Refused consent: nothing changes.
	idp.Deny = true
	if to := signIn(); !strings.HasPrefix(to, "https://dashboard.paygate.test/kyc?fayda=failed&reason=cancelled") {
		t.Fatalf("denied: %s", to)
	}
	idp.Deny = false

	to := signIn()
	if !strings.HasPrefix(to, "https://dashboard.paygate.test/kyc?fayda=verified") || !idp.LastAssertOK {
		t.Fatalf("verified: %s", to)
	}
	// The same callback cannot be used twice.
	callback := strings.Fields(to)[1]
	if resp, _ := noFollow.Get(callback); !strings.Contains(resp.Header.Get("Location"), "reason=expired") {
		t.Fatalf("replayed callback: %s", resp.Header.Get("Location"))
	}

	v = c.do("GET", "/v1/kyc", owner, "", nil, 200)
	id := v["owner_identity"].(map[string]any)
	o := v["profile"].(map[string]any)["owner"].(map[string]any)
	if id["verified"] != true || id["method"] != "fayda" || o["full_name"] != "Abebe Kebede Alemu" ||
		o["date_of_birth"] != "1985-04-12" || strings.Contains(fmt.Sprint(o["id_number"]), "1234 5678") ||
		id["photo_url"] != nil || strings.Contains(fmt.Sprint(v["required_documents"]), "owner_id") {
		t.Fatalf("after Fayda: %v", v)
	}
	// The owner can no longer be changed by hand.
	p["owner"] = map[string]string{"full_name": "Someone Else", "id_type": "passport", "id_number": "X1", "date_of_birth": "1990-01-01"}
	if o := c.do("PUT", "/v1/kyc/profile", owner, "", p, 200)["profile"].(map[string]any)["owner"].(map[string]any); o["full_name"] != "Abebe Kebede Alemu" {
		t.Fatalf("verified owner overwritten: %v", o)
	}
	var faydaRaw int64
	openTestDB(t).Raw("SELECT count(*) FROM kyc.kyc_applications WHERE fayda_encrypted LIKE '%Abebe%' OR fayda_encrypted LIKE '%1234%'").Scan(&faydaRaw)
	if faydaRaw != 0 {
		t.Fatal("Fayda identity stored in clear text")
	}

	c.uploadRequired(owner)
	c.do("POST", "/v1/kyc/submit", owner, "", nil, 200)
	rev := c.do("GET", "/admin/kyc/"+mid, c.admin, "", nil, 200)
	photo, _ := rev["owner_identity"].(map[string]any)["photo_url"].(string)
	if code, b, _ := fetch(t, photo); code != 200 || !bytes.HasPrefix(b, []byte("\xff\xd8\xff")) {
		t.Fatalf("staff see the Fayda photo: %d %q", code, photo)
	}
	if !strings.Contains(fmt.Sprint(rev["history"]), "fayda_verified") {
		t.Fatalf("history: %v", rev["history"])
	}
	c.do("POST", "/admin/kyc/"+mid+"/approve", c.admin, "", nil, 200)
}
