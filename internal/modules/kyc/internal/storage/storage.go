// Package storage keeps KYC files in object storage.
//
// The database stores only the OBJECT KEY (kyc/<merchant>/<document>.pdf),
// never a URL. A download URL is built on every read and expires within
// minutes, so a leaked link is useless soon after, and the storage location
// can change (bucket, host, CDN) without touching stored data.
//
//	MinIO  production: any S3-compatible store (MinIO, AWS S3, Ceph...). Private
//	       bucket; URLs are pre-signed GET links (AWS Signature v4).
//	       Server-side encryption with KYC_S3_SSE=true.
//	Local  development and tests: encrypted files on disk (AES-256-GCM); URLs
//	       are links signed by this server (HMAC), served by GET /kyc/files/{token}.
package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/encrypt"

	"paygate/internal/platform/secrets"
)

type Files interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Delete(ctx context.Context, key string) error
	// URL returns a short-lived download link for the object (built now, never stored).
	URL(ctx context.Context, key, downloadName string, ttl time.Duration) (string, error)
	// Ready prepares the store (e.g. creates the bucket) at start-up.
	Ready(ctx context.Context) error
}

var keyRe = regexp.MustCompile(`^kyc/[0-9a-f-]{36}(/fayda)?/[0-9a-f-]{36}\.(pdf|jpg|png)$`)

// ValidKey: keys are built by the service from ids only; anything else is refused.
func ValidKey(key string) bool { return keyRe.MatchString(key) }

func disposition(name string) string {
	name = strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(name)
	return `attachment; filename="` + name + `"`
}

// ---- MinIO / S3 ----

type MinIOConfig struct {
	Endpoint       string // host:port the server uses, e.g. minio:9000
	PublicEndpoint string // host:port in download links (browsers), e.g. files.example.com; default Endpoint
	AccessKey      string
	SecretKey      string
	Bucket         string
	Region         string
	UseSSL         bool
	PublicUseSSL   bool
	SSE            bool // ask for server-side encryption (needs KMS configured on MinIO)
}

type MinIO struct {
	cfg     MinIOConfig
	client  *minio.Client // talks to the store
	presign *minio.Client // signs links for the public endpoint (offline, no network)
}

func NewMinIO(cfg MinIOConfig) (*MinIO, error) {
	if cfg.Endpoint == "" || cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Bucket == "" {
		return nil, errors.New("MinIO needs MINIO_ENDPOINT, MINIO_ACCESS_KEY, MINIO_SECRET_KEY and MINIO_BUCKET")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1" // fixed region: no bucket-location lookups
	}
	if cfg.PublicEndpoint == "" {
		cfg.PublicEndpoint, cfg.PublicUseSSL = cfg.Endpoint, cfg.UseSSL
	}
	creds := credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	c, err := minio.New(cfg.Endpoint, &minio.Options{Creds: creds, Secure: cfg.UseSSL, Region: cfg.Region})
	if err != nil {
		return nil, err
	}
	p, err := minio.New(cfg.PublicEndpoint, &minio.Options{Creds: creds, Secure: cfg.PublicUseSSL, Region: cfg.Region})
	if err != nil {
		return nil, err
	}
	return &MinIO{cfg: cfg, client: c, presign: p}, nil
}

func (m *MinIO) Ready(ctx context.Context) error {
	ok, err := m.client.BucketExists(ctx, m.cfg.Bucket)
	if err != nil {
		return fmt.Errorf("MinIO bucket %s: %w", m.cfg.Bucket, err)
	}
	if !ok {
		if err := m.client.MakeBucket(ctx, m.cfg.Bucket, minio.MakeBucketOptions{Region: m.cfg.Region}); err != nil {
			return fmt.Errorf("create MinIO bucket %s: %w", m.cfg.Bucket, err)
		}
	}
	return nil // new buckets are private: no anonymous policy is ever set
}

func (m *MinIO) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if !ValidKey(key) {
		return errors.New("invalid object key")
	}
	opts := minio.PutObjectOptions{ContentType: contentType}
	if m.cfg.SSE {
		opts.ServerSideEncryption = encrypt.NewSSE()
	}
	_, err := m.client.PutObject(ctx, m.cfg.Bucket, key, bytes.NewReader(data), int64(len(data)), opts)
	return err
}

func (m *MinIO) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return errors.New("invalid object key")
	}
	return m.client.RemoveObject(ctx, m.cfg.Bucket, key, minio.RemoveObjectOptions{})
}

func (m *MinIO) URL(ctx context.Context, key, downloadName string, ttl time.Duration) (string, error) {
	if !ValidKey(key) {
		return "", errors.New("invalid object key")
	}
	q := url.Values{"response-content-disposition": {disposition(downloadName)}}
	u, err := m.presign.PresignedGetObject(ctx, m.cfg.Bucket, key, ttl, q)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// ---- Local disk (development, tests) ----

type Local struct {
	Dir     string
	Box     *secrets.Box
	BaseURL string // public address of this server
	signKey []byte
	now     func() time.Time
}

func NewLocal(dir, baseURL string, box *secrets.Box, signKey []byte) (*Local, error) {
	if box == nil || len(signKey) == 0 {
		return nil, errors.New("local KYC storage needs DATA_ENCRYPTION_KEY")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("KYC storage dir: %w", err)
	}
	return &Local{Dir: dir, Box: box, BaseURL: strings.TrimRight(baseURL, "/"), signKey: signKey, now: time.Now}, nil
}

func (l *Local) Ready(context.Context) error { return nil }

func (l *Local) path(key string) (string, error) {
	if !ValidKey(key) {
		return "", errors.New("invalid object key")
	}
	return filepath.Join(l.Dir, filepath.FromSlash(key)), nil
}

func (l *Local) Put(_ context.Context, key string, data []byte, _ string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	sealed, err := l.Box.SealBytes(data, []byte(key)) // bound to its key: files cannot be swapped
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// URL: <base>/kyc/files/<token>, token = base64(key|expiry|name).hmac
func (l *Local) URL(_ context.Context, key, downloadName string, ttl time.Duration) (string, error) {
	if !ValidKey(key) {
		return "", errors.New("invalid object key")
	}
	payload := key + "|" + strconv.FormatInt(l.now().Add(ttl).Unix(), 10) + "|" + downloadName
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return l.BaseURL + "/kyc/files/" + enc + "." + l.sign(enc), nil
}

func (l *Local) sign(s string) string {
	m := hmac.New(sha256.New, l.signKey)
	m.Write([]byte("kyc-file-link:" + s))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

var ErrBadLink = errors.New("this link is invalid or has expired")

// Open checks a signed link and returns the decrypted file and its name.
func (l *Local) Open(token string) (data []byte, name string, err error) {
	enc, sig, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(l.sign(enc))) {
		return nil, "", ErrBadLink
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return nil, "", ErrBadLink
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 {
		return nil, "", ErrBadLink
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || l.now().Unix() > exp {
		return nil, "", ErrBadLink
	}
	p, err := l.path(parts[0])
	if err != nil {
		return nil, "", ErrBadLink
	}
	sealed, err := os.ReadFile(p)
	if err != nil {
		return nil, "", ErrBadLink
	}
	data, err = l.Box.OpenBytes(sealed, []byte(parts[0]))
	if err != nil {
		return nil, "", err
	}
	name = parts[2]
	if name == "" {
		name = path.Base(parts[0])
	}
	return data, name, nil
}
