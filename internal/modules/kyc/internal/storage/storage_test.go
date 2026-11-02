package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"paygate/internal/platform/secrets"
)

const key = "kyc/3f6c2a9e-8d41-4b7e-a0c5-91e2d7f4b8a1/9a1d3c5e-7b2f-4e6a-8c0d-2f4b6d8e0a1c.pdf"

var pdf = []byte("%PDF-1.4 trade license")

// fakeS3 is a tiny S3 endpoint: it requires AWS Signature v4 on every call
// and keeps objects in memory.
type fakeS3 struct {
	mu      sync.Mutex
	buckets map[string]bool
	objects map[string][]byte
	sse     map[string]string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket, obj, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case obj == "" && r.Method == http.MethodHead:
		if !f.buckets[bucket] {
			w.WriteHeader(http.StatusNotFound)
		}
	case obj == "" && r.Method == http.MethodPut:
		f.buckets[bucket] = true
	case r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			b = unchunk(b) // signed chunked upload (aws-chunked)
		}
		f.objects[bucket+"/"+obj] = b
		f.sse[bucket+"/"+obj] = r.Header.Get("X-Amz-Server-Side-Encryption")
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
	case r.Method == http.MethodDelete:
		delete(f.objects, bucket+"/"+obj)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// unchunk decodes "<hex size>;chunk-signature=...\r\n<data>\r\n" frames.
func unchunk(b []byte) []byte {
	var out []byte
	for len(b) > 0 {
		head, rest, ok := bytes.Cut(b, []byte("\r\n"))
		if !ok {
			break
		}
		sizeHex, _, _ := strings.Cut(string(head), ";")
		var n int
		fmt.Sscanf(sizeHex, "%x", &n)
		if n == 0 || len(rest) < n {
			break
		}
		out = append(out, rest[:n]...)
		b = bytes.TrimPrefix(rest[n:], []byte("\r\n"))
	}
	return out
}

func TestMinIO(t *testing.T) {
	fake := &fakeS3{buckets: map[string]bool{}, objects: map[string][]byte{}, sse: map[string]string{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	m, err := NewMinIO(MinIOConfig{Endpoint: host, PublicEndpoint: "files.paygate.example", PublicUseSSL: true,
		AccessKey: "minio", SecretKey: "minio-secret", Bucket: "kyc-documents", SSE: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.Ready(ctx); err != nil || !fake.buckets["kyc-documents"] {
		t.Fatalf("bucket not created: %v", err)
	}
	if err := m.Put(ctx, key, pdf, "application/pdf"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fake.objects["kyc-documents/"+key], pdf) || fake.sse["kyc-documents/"+key] != "AES256" {
		t.Fatalf("object not stored with server-side encryption: %v", fake.sse)
	}
	if err := m.Put(ctx, "../../etc/passwd", pdf, "application/pdf"); err == nil {
		t.Fatal("arbitrary key accepted")
	}

	link, err := m.URL(ctx, key, "trade-license.pdf", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	q := u.Query()
	if u.Scheme != "https" || u.Host != "files.paygate.example" || u.Path != "/kyc-documents/"+key ||
		q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" || q.Get("X-Amz-Expires") != "300" || q.Get("X-Amz-Signature") == "" ||
		!strings.Contains(q.Get("response-content-disposition"), "trade-license.pdf") {
		t.Fatalf("presigned link: %s", link)
	}
	if err := m.Delete(ctx, key); err != nil || fake.objects["kyc-documents/"+key] != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestLocalSignedLinks(t *testing.T) {
	box, _ := secrets.NewBox(bytes.Repeat([]byte{3}, 32))
	dir := t.TempDir()
	l, err := NewLocal(dir, "http://localhost:8080", box, []byte("link-key"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	l.now = func() time.Time { return now }
	ctx := context.Background()
	if err := l.Put(ctx, key, pdf, "application/pdf"); err != nil {
		t.Fatal(err)
	}
	link, _ := l.URL(ctx, key, "license.pdf", 5*time.Minute)
	if !strings.HasPrefix(link, "http://localhost:8080/kyc/files/") || strings.Contains(link, key) {
		t.Fatalf("link must not expose the key or a path: %s", link)
	}
	token := strings.TrimPrefix(link, "http://localhost:8080/kyc/files/")
	if data, name, err := l.Open(token); err != nil || !bytes.Equal(data, pdf) || name != "license.pdf" {
		t.Fatalf("open: %q %q %v", data, name, err)
	}
	if _, _, err := l.Open(token[:len(token)-2] + "xx"); err == nil {
		t.Fatal("tampered link accepted")
	}
	now = now.Add(6 * time.Minute)
	if _, _, err := l.Open(token); err == nil {
		t.Fatal("expired link accepted")
	}
}
