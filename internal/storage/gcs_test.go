package storage

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"
)

func offlineGCS(t *testing.T) *GCS {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	credentials, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "signer@example.iam.gserviceaccount.com",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		"token_uri":    "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatal(err)
	}

	bucket, err := NewGCS(context.Background(), "test-bucket", option.WithAuthCredentialsJSON(option.ServiceAccount, credentials))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = bucket.Close() })

	return bucket
}

func signedHeaders(t *testing.T, raw string) (*url.URL, []string) {
	t.Helper()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}

	return parsed, strings.Split(parsed.Query().Get("X-Goog-SignedHeaders"), ";")
}

func TestSignUploadPinsTypeAndSize(t *testing.T) {
	bucket := offlineGCS(t)

	request, err := bucket.SignUpload("incoming/abc", "video/mp4", 42, false, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	parsed, headers := signedHeaders(t, request.URL)

	if request.Method != http.MethodPut || parsed.Path != "/test-bucket/incoming/abc" {
		t.Fatalf("unexpected request %s %s", request.Method, parsed.Path)
	}

	if strings.Join(headers, ";") != "content-type;host;x-goog-content-length-range" {
		t.Fatalf("type and size must be signed, got %v", headers)
	}

	if request.Headers[headerLengthRange] != "42,42" || request.Headers[headerContentType] != "video/mp4" {
		t.Fatalf("client headers must match the signature: %v", request.Headers)
	}

	if expires := parsed.Query().Get("X-Goog-Expires"); expires != "900" && expires != "899" {
		t.Fatalf("unexpected expiry %s", parsed.Query().Get("X-Goog-Expires"))
	}
}

func TestSignUploadResumableStartsSession(t *testing.T) {
	bucket := offlineGCS(t)

	request, err := bucket.SignUpload("incoming/abc", "video/mp4", 1<<30, true, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	_, headers := signedHeaders(t, request.URL)

	if request.Method != http.MethodPost || request.Headers[headerResumable] != resumableStartValue || headers[len(headers)-1] != headerResumable {
		t.Fatalf("resumable upload must sign the session start: %s %v %v", request.Method, request.Headers, headers)
	}
}

func TestSignDownloadSetsDisposition(t *testing.T) {
	bucket := offlineGCS(t)

	raw, err := bucket.SignDownload("hs/files/a.pdf", `attachment; filename="a.pdf"`, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	parsed, _ := signedHeaders(t, raw)

	if parsed.Query().Get("response-content-disposition") != `attachment; filename="a.pdf"` {
		t.Fatalf("disposition must be part of the signed URL: %s", raw)
	}
}
