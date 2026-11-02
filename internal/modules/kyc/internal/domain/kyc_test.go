package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"paygate/internal/contracts/kyc"
)

func complete() Profile {
	return Profile{BusinessType: PLC, LegalName: "Abebe Coffee PLC", TIN: "0012345678", TradeLicenseNumber: "AA/14/123/2024",
		TradeLicenseExpiry: time.Now().AddDate(1, 0, 0).Format("2006-01-02"), ApplicantRole: RoleGeneralManager,
		RegistrationNumber: "MT/AA/2/0012345/2016", Address: Address{Region: "Addis Ababa", City: "Addis Ababa"},
		Owner: Owner{FullName: "Abebe Kebede", IDType: "passport", IDNumber: "EP1234567", DateOfBirth: "1985-04-12", Nationality: "ET"}}
}

func docs(types ...string) []Document {
	var out []Document
	for _, t := range types {
		out = append(out, Document{Type: t})
	}
	return out
}

func TestLifecycle(t *testing.T) {
	now := time.Now()
	a := NewApplication("m1", now)
	if err := a.SetProfile(complete(), now); err != nil || a.Status != kyc.StatusDraft {
		t.Fatalf("profile: %v %s", err, a.Status)
	}
	err := a.Submit(docs(DocTradeLicense, DocTINCertificate), now)
	if err == nil || !strings.Contains(err.Error(), "commercial_registration") || !strings.Contains(err.Error(), "shareholders_list") ||
		!strings.Contains(err.Error(), "owner_id") {
		t.Fatalf("missing documents must block submission: %v", err)
	}
	all := docs(RequiredDocs(a.Profile, false)...)
	if err := a.Submit(all, now); err != nil || a.Status != kyc.StatusSubmitted {
		t.Fatalf("submit: %v", err)
	}
	if err := a.SetProfile(complete(), now); !errors.Is(err, ErrLocked) {
		t.Fatal("a submitted application must be frozen")
	}
	if err := a.Reject("staff", " ", now); err == nil {
		t.Fatal("rejection needs a reason")
	}
	if err := a.Reject("staff", "TIN certificate is unreadable", now); err != nil || !a.Editable() {
		t.Fatalf("reject: %v", err)
	}
	if err := a.Approve("staff", now); !errors.Is(err, ErrNotSubmitted) {
		t.Fatal("only submitted applications can be approved")
	}
	_ = a.Submit(all, now)
	if err := a.Approve("staff", now); err != nil || a.Status != kyc.StatusApproved || a.RejectionReason != "" {
		t.Fatalf("approve: %v %+v", err, a)
	}
}

func TestRequiredDocumentsFollowTheBusiness(t *testing.T) {
	sole := Profile{BusinessType: SoleProprietor, ApplicantRole: RoleOwner}
	got := strings.Join(RequiredDocs(sole, true), ",")
	if strings.Contains(got, DocMemorandum) || strings.Contains(got, DocOwnerID) || !strings.Contains(got, DocTradeLicense) {
		t.Fatalf("sole proprietor verified with Fayda: %s", got)
	}
	full := Profile{BusinessType: ShareCompany, VATRegistered: true, ApplicantRole: RoleRepresentative, ForeignOwned: true, RegulatedSector: true}
	got = strings.Join(RequiredDocs(full, false), ",")
	for _, d := range []string{DocMemorandum, DocShareholders, DocVATCertificate, DocPowerOfAttorney, DocInvestmentPermit,
		DocCertificateCompetence, DocOwnerID, DocBusinessAddress, DocBankProof} {
		if !strings.Contains(got, d) {
			t.Errorf("share company case misses %s: %s", d, got)
		}
	}
	ngo := strings.Join(RequiredDocs(Profile{BusinessType: NGO}, true), ",")
	if strings.Contains(ngo, DocTradeLicense) || !strings.Contains(ngo, DocRegistrationCert) {
		t.Fatalf("NGO: %s", ngo)
	}
}

func TestExpiredTradeLicenseBlocksSubmit(t *testing.T) {
	now := time.Now()
	a := NewApplication("m1", now)
	p := complete()
	p.TradeLicenseExpiry = now.AddDate(0, -1, 0).Format("2006-01-02")
	_ = a.SetProfile(p, now)
	if err := a.Submit(docs(RequiredDocs(p, false)...), now); err == nil || !strings.Contains(err.Error(), "renew") {
		t.Fatalf("expired license accepted: %v", err)
	}
}

func TestFaydaIdentity(t *testing.T) {
	now := time.Now()
	a := NewApplication("m1", now)
	p := complete()
	p.Owner = Owner{IDType: "fayda"}
	_ = a.SetProfile(p, now)
	if miss := strings.Join(a.Missing(nil, now), ";"); !strings.Contains(miss, "verify with Fayda") {
		t.Fatalf("fayda chosen but not verified: %s", miss)
	}
	if err := a.VerifyWithFayda(FaydaIdentity{Subject: "s1", Name: "Abebe Kebede Alemu", BirthDate: "1985-04-12",
		IndividualID: "1234 5678 9012", Nationality: "ETH", Phone: "+251911234567"}, now); err != nil {
		t.Fatal(err)
	}
	if a.Profile.Owner.FullName != "Abebe Kebede Alemu" || a.Profile.Owner.IDNumber != "••••••••9012" || !a.Profile.OwnerIdentityLocked {
		t.Fatalf("owner from Fayda: %+v", a.Profile.Owner)
	}
	// Later profile edits cannot change the verified owner.
	p.Owner = Owner{FullName: "Someone Else", IDType: "passport", IDNumber: "X1"}
	_ = a.SetProfile(p, now)
	if a.Profile.Owner.FullName != "Abebe Kebede Alemu" {
		t.Fatal("verified owner was overwritten")
	}
	if strings.Contains(strings.Join(RequiredDocs(a.Profile, a.FaydaVerified()), ","), DocOwnerID) {
		t.Fatal("owner ID document not needed after Fayda")
	}
	minor := NewApplication("m2", now)
	if err := minor.VerifyWithFayda(FaydaIdentity{Subject: "s2", Name: "Kid", BirthDate: now.AddDate(-10, 0, 0).Format("2006-01-02")}, now); err == nil {
		t.Fatal("under-18 owner accepted")
	}
}

func TestProfileValidation(t *testing.T) {
	now := time.Now()
	for name, mutate := range map[string]func(*Profile){
		"bad tin":    func(p *Profile) { p.TIN = "123" },
		"bad type":   func(p *Profile) { p.BusinessType = "llc" },
		"minor":      func(p *Profile) { p.Owner.DateOfBirth = now.AddDate(-10, 0, 0).Format("2006-01-02") },
		"bad id":     func(p *Profile) { p.Owner.IDType = "library_card" },
		"bad phone":  func(p *Profile) { p.Owner.Phone = "0911" },
		"bad role":   func(p *Profile) { p.ApplicantRole = "cousin" },
		"bad expiry": func(p *Profile) { p.TradeLicenseExpiry = "next year" },
	} {
		p := complete()
		mutate(&p)
		if err := NewApplication("m", now).SetProfile(p, now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestFiles(t *testing.T) {
	if ct, err := SniffFile([]byte("%PDF-1.7\n...")); err != nil || ct != "application/pdf" {
		t.Fatalf("pdf: %s %v", ct, err)
	}
	if _, err := SniffFile([]byte("MZ\x90\x00 an exe pretending to be a pdf")); err == nil {
		t.Fatal("non-document accepted")
	}
	big := append([]byte("%PDF-1.7\n"), make([]byte, MaxFileBytes)...)
	if _, err := SniffFile(big); err == nil || !strings.Contains(err.Error(), "5 MB") {
		t.Fatalf("file over 5 MB accepted: %v", err)
	}
	if n := CleanFileName("../../etc/passwd", "application/pdf"); strings.Contains(n, "/") {
		t.Fatalf("unsafe name kept: %s", n)
	}
	if k := ObjectKey("m1", "d1", "image/png"); k != "kyc/m1/d1.png" {
		t.Fatalf("object key: %s", k)
	}
}
