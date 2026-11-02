// Package domain: merchant verification (KYC / KYB) rules.
//
//	not_started → draft ──submit──► submitted ──approve──► approved
//	                ▲                    │
//	                └──── (fix, resubmit) ◄── rejected (reason)
//
// While draft or rejected the merchant can edit the profile and documents;
// once submitted everything is frozen until a reviewer decides.
package domain

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"paygate/internal/contracts/kyc"
	"paygate/internal/platform/apperror"
)

// MaxFileBytes: no KYC upload may exceed 5 MB.
const MaxFileBytes = 5 << 20

// Business types (Ethiopian legal forms, Commercial Code 2021).
const (
	SoleProprietor = "sole_proprietor"
	PLC            = "plc"           // private limited company
	ShareCompany   = "share_company" // S.C.
	Partnership    = "partnership"   // general / limited partnership
	NGO            = "ngo"           // civil society organisation (ACSO)
)

var BusinessTypes = []string{SoleProprietor, PLC, ShareCompany, Partnership, NGO}

// Who submits for the business.
const (
	RoleOwner          = "owner"
	RoleGeneralManager = "general_manager"
	RoleRepresentative = "authorized_representative" // needs a power of attorney
)

// Document types.
const (
	DocTradeLicense          = "trade_license"           // business license (MoTRI / regional trade bureau), renewed yearly
	DocCommercialReg         = "commercial_registration" // commercial registration certificate
	DocTINCertificate        = "tin_certificate"         // taxpayer registration certificate (Ministry of Revenues)
	DocVATCertificate        = "vat_certificate"         // when VAT-registered
	DocMemorandum            = "memorandum_of_association"
	DocShareholders          = "shareholders_list" // beneficial owners (registry does not publish them)
	DocRegistrationCert      = "registration_certificate"
	DocBusinessAddress       = "business_address_proof" // lease agreement, title deed or utility bill
	DocBankProof             = "bank_account_proof"     // bank letter / statement header in the business name
	DocOwnerID               = "owner_id"               // passport / residence permit when not verified with Fayda
	DocPowerOfAttorney       = "power_of_attorney"
	DocInvestmentPermit      = "investment_permit" // foreign-owned businesses (EIC)
	DocCertificateCompetence = "certificate_of_competence"
	DocOther                 = "other"
)

var DocTypes = []string{DocTradeLicense, DocCommercialReg, DocTINCertificate, DocVATCertificate, DocMemorandum,
	DocShareholders, DocRegistrationCert, DocBusinessAddress, DocBankProof, DocOwnerID, DocPowerOfAttorney,
	DocInvestmentPermit, DocCertificateCompetence, DocOther}

// RequiredDocs: what the merchant must upload, from its legal form and
// situation. The owner's ID document is not needed when the owner verified
// their identity with Fayda.
func RequiredDocs(p Profile, faydaVerified bool) []string {
	var out []string
	if p.BusinessType == NGO {
		out = append(out, DocRegistrationCert)
	} else {
		out = append(out, DocTradeLicense, DocCommercialReg)
	}
	out = append(out, DocTINCertificate)
	switch p.BusinessType {
	case PLC, ShareCompany, Partnership:
		out = append(out, DocMemorandum, DocShareholders)
	}
	if p.VATRegistered {
		out = append(out, DocVATCertificate)
	}
	out = append(out, DocBusinessAddress, DocBankProof)
	if p.ApplicantRole == RoleRepresentative {
		out = append(out, DocPowerOfAttorney)
	}
	if p.ForeignOwned {
		out = append(out, DocInvestmentPermit)
	}
	if p.RegulatedSector {
		out = append(out, DocCertificateCompetence)
	}
	if !faydaVerified {
		out = append(out, DocOwnerID)
	}
	return out
}

type Address struct {
	Region      string `json:"region"`
	City        string `json:"city"`
	SubCity     string `json:"sub_city"`
	Woreda      string `json:"woreda"`
	HouseNumber string `json:"house_number"`
}

// Owner: the owner / general manager / legal representative. Personal data:
// stored encrypted. With Fayda, these fields come from the national ID.
type Owner struct {
	FullName    string `json:"full_name"`
	IDType      string `json:"id_type"` // fayda | passport | residence_permit
	IDNumber    string `json:"id_number"`
	DateOfBirth string `json:"date_of_birth"` // YYYY-MM-DD
	Nationality string `json:"nationality"`
	Phone       string `json:"phone"`
}

// Profile is what the merchant fills in.
type Profile struct {
	BusinessType        string  `json:"business_type"`
	LegalName           string  `json:"legal_name"`
	TradeName           string  `json:"trade_name"`
	RegistrationNumber  string  `json:"registration_number"` // commercial registration number
	TIN                 string  `json:"tin"`
	TradeLicenseNumber  string  `json:"trade_license_number"`
	TradeLicenseExpiry  string  `json:"trade_license_expiry"` // YYYY-MM-DD; licenses are renewed yearly
	VATRegistered       bool    `json:"vat_registered"`
	VATNumber           string  `json:"vat_number"`
	ApplicantRole       string  `json:"applicant_role"` // owner | general_manager | authorized_representative
	ForeignOwned        bool    `json:"foreign_owned"`
	RegulatedSector     bool    `json:"regulated_sector"` // needs a certificate of competence (health, finance, ...)
	Industry            string  `json:"industry"`
	Website             string  `json:"website"`
	Description         string  `json:"description"`
	MonthlyVolumeETB    int64   `json:"expected_monthly_volume_etb"`
	Address             Address `json:"address"`
	Owner               Owner   `json:"owner"`
	OwnerIdentityLocked bool    `json:"owner_identity_locked"` // set by Fayda; owner fields then cannot be edited
}

// FaydaIdentity: what the national ID system confirmed about the owner.
type FaydaIdentity struct {
	Subject      string    `json:"sub"`           // stable pseudonymous id at Fayda
	IndividualID string    `json:"individual_id"` // masked when shown
	Name         string    `json:"name"`
	BirthDate    string    `json:"birthdate"`
	Gender       string    `json:"gender"`
	Phone        string    `json:"phone_number"`
	Email        string    `json:"email"`
	Nationality  string    `json:"nationality"`
	Address      string    `json:"address"`
	PhotoKey     string    `json:"photo_key,omitempty"` // object key of the photo, never a URL
	VerifiedAt   time.Time `json:"verified_at"`
}

type Application struct {
	MerchantID      string
	Status          string
	Profile         Profile
	Fayda           *FaydaIdentity
	SubmittedAt     *time.Time
	ReviewedAt      *time.Time
	ReviewedBy      string
	RejectionReason string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Document struct {
	ID          string
	MerchantID  string
	Type        string
	FileName    string
	ContentType string
	Size        int64
	SHA256      string
	ObjectKey   string // where the file lives in object storage; URLs are built on read
	UploadedBy  string
	CreatedAt   time.Time
}

var (
	ErrLocked = apperror.Conflict("kyc_locked",
		"the verification is submitted or approved and cannot be changed now")
	ErrNotSubmitted = apperror.Conflict("kyc_not_submitted", "only a submitted verification can be reviewed")
)

func NewApplication(merchantID string, now time.Time) *Application {
	return &Application{MerchantID: merchantID, Status: kyc.StatusNotStarted, CreatedAt: now, UpdatedAt: now}
}

// Editable: the merchant may change the profile and documents.
func (a *Application) Editable() bool {
	return a.Status == kyc.StatusNotStarted || a.Status == kyc.StatusDraft || a.Status == kyc.StatusRejected
}

func (a *Application) FaydaVerified() bool { return a.Fayda != nil }

var (
	tinRe   = regexp.MustCompile(`^[0-9]{10}$`) // Ethiopian TIN: 10 digits
	dateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	phoneRe = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
)

var (
	idTypes        = []string{"fayda", "passport", "residence_permit"}
	applicantRoles = []string{RoleOwner, RoleGeneralManager, RoleRepresentative}
)

// SetProfile stores a (possibly incomplete) profile; completeness is checked
// on submit. After a Fayda verification the owner's identity fields keep the
// values confirmed by Fayda.
func (a *Application) SetProfile(p Profile, now time.Time) error {
	if !a.Editable() {
		return ErrLocked
	}
	p = trimProfile(p)
	if a.Fayda != nil {
		p.Owner, p.OwnerIdentityLocked = a.Profile.Owner, true
	} else {
		p.OwnerIdentityLocked = false
	}
	if p.BusinessType != "" && !slices.Contains(BusinessTypes, p.BusinessType) {
		return apperror.Invalid("invalid_business_type", "business_type must be one of: "+strings.Join(BusinessTypes, ", "))
	}
	if p.ApplicantRole != "" && !slices.Contains(applicantRoles, p.ApplicantRole) {
		return apperror.Invalid("invalid_applicant_role", "applicant_role must be one of: "+strings.Join(applicantRoles, ", "))
	}
	if p.TIN != "" && !tinRe.MatchString(p.TIN) {
		return apperror.Invalid("invalid_tin", "tin must be the 10-digit Taxpayer Identification Number")
	}
	if p.TradeLicenseExpiry != "" {
		if _, err := time.Parse("2006-01-02", p.TradeLicenseExpiry); err != nil {
			return apperror.Invalid("invalid_trade_license_expiry", "trade_license_expiry must be YYYY-MM-DD")
		}
	}
	if !a.FaydaVerified() {
		if p.Owner.DateOfBirth != "" {
			if !dateRe.MatchString(p.Owner.DateOfBirth) {
				return apperror.Invalid("invalid_date_of_birth", "date_of_birth must be YYYY-MM-DD")
			}
			if dob, err := time.Parse("2006-01-02", p.Owner.DateOfBirth); err != nil || dob.After(now.AddDate(-18, 0, 0)) {
				return apperror.Invalid("invalid_date_of_birth", "the owner must be at least 18 years old")
			}
		}
		if p.Owner.IDType != "" && !slices.Contains(idTypes, p.Owner.IDType) {
			return apperror.Invalid("invalid_id_type", "owner.id_type must be fayda, passport or residence_permit")
		}
		if p.Owner.Phone != "" && !phoneRe.MatchString(p.Owner.Phone) {
			return apperror.Invalid("invalid_phone", "owner.phone must be in international format, e.g. +251911000000")
		}
	}
	if p.MonthlyVolumeETB < 0 {
		return apperror.Invalid("invalid_volume", "expected_monthly_volume_etb cannot be negative")
	}
	for _, f := range []string{p.LegalName, p.TradeName, p.Description, p.Owner.FullName, p.Address.City} {
		if len(f) > 500 {
			return apperror.Invalid("field_too_long", "text fields are at most 500 characters")
		}
	}
	a.Profile, a.UpdatedAt = p, now
	if a.Status == kyc.StatusNotStarted {
		a.Status = kyc.StatusDraft
	}
	return nil
}

// VerifyWithFayda records the owner identity confirmed by the national ID
// system and takes the owner's details from it (they become read-only).
func (a *Application) VerifyWithFayda(id FaydaIdentity, now time.Time) error {
	if !a.Editable() {
		return ErrLocked
	}
	if id.Subject == "" || id.Name == "" {
		return apperror.Unprocessable("fayda_incomplete", "Fayda did not share the owner's identity (name)")
	}
	if id.BirthDate != "" {
		if dob, err := time.Parse("2006-01-02", id.BirthDate); err == nil && dob.After(now.AddDate(-18, 0, 0)) {
			return apperror.Unprocessable("owner_under_18", "the owner must be at least 18 years old")
		}
	}
	id.VerifiedAt = now
	a.Fayda = &id
	a.Profile.Owner = Owner{FullName: id.Name, IDType: "fayda", IDNumber: MaskID(id.IndividualID),
		DateOfBirth: id.BirthDate, Nationality: id.Nationality, Phone: id.Phone}
	a.Profile.OwnerIdentityLocked = true
	a.UpdatedAt = now
	if a.Status == kyc.StatusNotStarted {
		a.Status = kyc.StatusDraft
	}
	return nil
}

// MaskID keeps the last 4 characters of an identity number.
func MaskID(id string) string {
	id = strings.ReplaceAll(id, " ", "")
	if len(id) <= 4 {
		return id
	}
	return strings.Repeat("•", len(id)-4) + id[len(id)-4:]
}

// Missing lists what is still needed before the application can be submitted.
func (a *Application) Missing(docs []Document, now time.Time) []string {
	p := a.Profile
	var miss []string
	need := func(ok bool, what string) {
		if !ok {
			miss = append(miss, what)
		}
	}
	need(p.BusinessType != "", "business_type")
	need(p.LegalName != "", "legal_name")
	need(p.TIN != "", "tin")
	need(p.ApplicantRole != "", "applicant_role")
	need(p.Address.Region != "" && p.Address.City != "", "address.region and address.city")
	if p.BusinessType != NGO {
		need(p.TradeLicenseNumber != "", "trade_license_number")
		if p.TradeLicenseExpiry == "" {
			need(false, "trade_license_expiry")
		} else if exp, err := time.Parse("2006-01-02", p.TradeLicenseExpiry); err == nil && exp.Before(now) {
			need(false, "a valid trade license (it expired on "+p.TradeLicenseExpiry+"; renew it)")
		}
	}
	need(p.RegistrationNumber != "", "registration_number")
	if p.VATRegistered {
		need(p.VATNumber != "", "vat_number")
	}
	if p.Owner.IDType == "fayda" && !a.FaydaVerified() {
		need(false, "owner identity: verify with Fayda (POST /v1/kyc/fayda/start)")
	}
	if !a.FaydaVerified() {
		need(p.Owner.FullName != "", "owner.full_name")
		need(p.Owner.IDType != "" && p.Owner.IDNumber != "", "owner.id_type and owner.id_number")
		need(p.Owner.DateOfBirth != "", "owner.date_of_birth")
	}
	have := map[string]bool{}
	for _, d := range docs {
		have[d.Type] = true
	}
	for _, t := range RequiredDocs(p, a.FaydaVerified()) {
		need(have[t], "document: "+t)
	}
	return miss
}

func (a *Application) Submit(docs []Document, now time.Time) error {
	if a.Status == kyc.StatusSubmitted || a.Status == kyc.StatusApproved || a.Status == kyc.StatusWaived {
		return ErrLocked
	}
	if miss := a.Missing(docs, now); len(miss) > 0 {
		return apperror.Unprocessable("kyc_incomplete", "missing: "+strings.Join(miss, "; "))
	}
	a.Status, a.SubmittedAt, a.RejectionReason, a.UpdatedAt = kyc.StatusSubmitted, &now, "", now
	return nil
}

func (a *Application) Approve(reviewer string, now time.Time) error {
	if a.Status != kyc.StatusSubmitted {
		return ErrNotSubmitted
	}
	a.Status, a.ReviewedAt, a.ReviewedBy, a.UpdatedAt = kyc.StatusApproved, &now, reviewer, now
	return nil
}

func (a *Application) Reject(reviewer, reason string, now time.Time) error {
	if a.Status != kyc.StatusSubmitted {
		return ErrNotSubmitted
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 1000 {
		return apperror.Invalid("invalid_reason", "a rejection reason is required (it is shown to the merchant)")
	}
	a.Status, a.ReviewedAt, a.ReviewedBy, a.RejectionReason, a.UpdatedAt = kyc.StatusRejected, &now, reviewer, reason, now
	return nil
}

// ---- Uploaded files ----

var allowedTypes = map[string]string{"application/pdf": ".pdf", "image/jpeg": ".jpg", "image/png": ".png"}

// SniffFile decides the type from the bytes, never from the client's claim,
// and refuses anything that is not a PDF, JPEG or PNG, or larger than 5 MB.
func SniffFile(data []byte) (string, error) {
	if len(data) == 0 {
		return "", apperror.Invalid("empty_file", "the file is empty")
	}
	if len(data) > MaxFileBytes {
		return "", apperror.TooLarge("file_too_large", fmt.Sprintf("files are at most %d MB", MaxFileBytes>>20))
	}
	ct := http.DetectContentType(data)
	if _, ok := allowedTypes[ct]; !ok {
		return "", apperror.Invalid("unsupported_file_type", "upload a PDF, JPEG or PNG")
	}
	return ct, nil
}

// ObjectKey: where a document is stored. Only ids, never names or personal
// data: kyc/<merchant id>/<document id>.<ext>
func ObjectKey(merchantID, documentID, contentType string) string {
	return "kyc/" + merchantID + "/" + documentID + allowedTypes[contentType]
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._ -]`)

// CleanFileName keeps a safe display name (never used as a path or key).
func CleanFileName(name, contentType string) string {
	name = unsafeName.ReplaceAllString(strings.TrimSpace(name), "_")
	if len(name) > 100 {
		name = name[len(name)-100:]
	}
	if name == "" {
		name = "document" + allowedTypes[contentType]
	}
	return name
}

func ValidDocType(t string) bool { return slices.Contains(DocTypes, t) }

func trimProfile(p Profile) Profile {
	t := strings.TrimSpace
	p.BusinessType, p.LegalName, p.TradeName = t(p.BusinessType), t(p.LegalName), t(p.TradeName)
	p.RegistrationNumber, p.TIN, p.TradeLicenseNumber = t(p.RegistrationNumber), strings.ReplaceAll(t(p.TIN), " ", ""), t(p.TradeLicenseNumber)
	p.TradeLicenseExpiry, p.VATNumber, p.ApplicantRole = t(p.TradeLicenseExpiry), t(p.VATNumber), t(p.ApplicantRole)
	p.Industry, p.Website, p.Description = t(p.Industry), t(p.Website), t(p.Description)
	p.Address = Address{Region: t(p.Address.Region), City: t(p.Address.City), SubCity: t(p.Address.SubCity),
		Woreda: t(p.Address.Woreda), HouseNumber: t(p.Address.HouseNumber)}
	p.Owner = Owner{FullName: t(p.Owner.FullName), IDType: t(p.Owner.IDType), IDNumber: t(p.Owner.IDNumber),
		DateOfBirth: t(p.Owner.DateOfBirth), Nationality: t(p.Owner.Nationality), Phone: strings.ReplaceAll(t(p.Owner.Phone), " ", "")}
	return p
}
