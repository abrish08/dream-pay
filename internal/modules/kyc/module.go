// Package kyc verifies merchants (KYC / KYB) before they can go live.
package kyc

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	kycc "paygate/internal/contracts/kyc"
	"paygate/internal/modules/kyc/internal/fayda"
	"paygate/internal/modules/kyc/internal/service"
	"paygate/internal/modules/kyc/internal/storage"
	"paygate/internal/modules/kyc/internal/store"
	"paygate/internal/modules/kyc/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/mail"
	"paygate/internal/platform/module"
	"paygate/internal/platform/secrets"
)

const Name = "kyc"

type Deps struct {
	DB     *gorm.DB
	Bus    bus.Bus
	Log    *slog.Logger
	Box    *secrets.Box // encrypts the owner's personal data (and local files)
	Mailer mail.Sender

	Storage       string // "minio" or "local"
	StorageDir    string // local driver only
	MinIO         MinIOConfig
	PublicBaseURL string // signed links of the local driver point here
	DashboardURL  string // the owner returns here after Fayda
	SigningSecret []byte // signs local download links

	// Fayda (optional): empty ClientID = disabled.
	FaydaIssuer, FaydaClientID, FaydaPrivateKey, FaydaRedirectURI, FaydaACR string
	FaydaHTTP                                                               *http.Client // tests
}

// MinIOConfig: S3-compatible object storage (MinIO, AWS S3, Ceph...).
type MinIOConfig struct {
	Endpoint, PublicEndpoint, AccessKey, SecretKey, Bucket, Region string
	UseSSL, PublicUseSSL, SSE                                      bool
}

type Module struct {
	*module.Base
	svc   *service.Service
	files storage.Files
}

var _ module.Module = (*Module)(nil)

func New(d Deps) (*Module, error) {
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, nil)
	base.Models(store.Models()...) // GORM models, auto-migrated (additive only)
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, kycc.Permissions))

	var files storage.Files
	switch d.Storage {
	case "minio":
		m, err := storage.NewMinIO(storage.MinIOConfig{Endpoint: d.MinIO.Endpoint, PublicEndpoint: d.MinIO.PublicEndpoint,
			AccessKey: d.MinIO.AccessKey, SecretKey: d.MinIO.SecretKey, Bucket: d.MinIO.Bucket, Region: d.MinIO.Region,
			UseSSL: d.MinIO.UseSSL, PublicUseSSL: d.MinIO.PublicUseSSL, SSE: d.MinIO.SSE})
		if err != nil {
			return nil, err
		}
		files = m
	default:
		if d.Box == nil {
			return nil, fmt.Errorf("KYC needs DATA_ENCRYPTION_KEY")
		}
		sum := sha256.Sum256(append([]byte("kyc-local-links:"), d.SigningSecret...))
		l, err := storage.NewLocal(d.StorageDir, d.PublicBaseURL, d.Box, sum[:])
		if err != nil {
			return nil, err
		}
		files = l
	}
	base.AddStartTask("prepare document storage", func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return files.Ready(ctx)
	})

	opt := service.Options{Box: d.Box, DashboardURL: d.DashboardURL}
	if d.FaydaClientID != "" {
		key, kid, err := fayda.ParsePrivateKey(d.FaydaPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("FAYDA_PRIVATE_KEY: %w", err)
		}
		c, err := fayda.New(fayda.Config{Issuer: d.FaydaIssuer, ClientID: d.FaydaClientID, RedirectURI: d.FaydaRedirectURI,
			PrivateKey: key, KeyID: kid, ACR: d.FaydaACR, HTTP: d.FaydaHTTP})
		if err != nil {
			return nil, err
		}
		opt.Fayda = c
	}
	svc := service.New(store.New(d.DB, d.Box), files, base.Events, d.Bus, d.Mailer, opt, base.Log)
	return &Module{Base: base, svc: svc, files: files}, nil
}

func (m *Module) RegisterRoutes(r module.Routers) {
	local, _ := m.files.(*storage.Local)
	h := transport.NewHTTP(m.svc, local, m.Log)
	h.RegisterPublic(r.Public)
	h.RegisterAdmin(r.Admin)
	h.RegisterOpen(r.Open)
}

// Asks: merchants.GetMerchant (email). Publishes: kyc.StatusChanged.
func (m *Module) RegisterHandlers(bus.Bus) {}
