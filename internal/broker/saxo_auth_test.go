package broker

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestCert generates a self-signed RSA cert + key and writes them as PEM.
func writeTestCert(t *testing.T) (string, *rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "cert.pem")
	var buf strings.Builder
	_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = pem.Encode(&buf, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, key, cert
}

func TestLoadCertificatePEM(t *testing.T) {
	path, key, cert := writeTestCert(t)
	gotKey, gotCert, err := loadCertificate(path, "")
	if err != nil {
		t.Fatalf("loadCertificate: %v", err)
	}
	if gotKey.D.Cmp(key.D) != 0 {
		t.Error("loaded key does not match")
	}
	if !gotCert.Equal(cert) {
		t.Error("loaded certificate does not match")
	}
}

func TestSignAssertion(t *testing.T) {
	path, key, cert := writeTestCert(t)
	src, err := NewCertTokenSource(CertConfig{
		AppKey:   "app-123",
		UserID:   "user-42",
		AppURL:   "https://example.test/app",
		AuthURL:  "https://sim.logonvalidation.net",
		CertPath: path,
	})
	if err != nil {
		t.Fatalf("NewCertTokenSource: %v", err)
	}

	// x5t must be the base64url SHA-1 thumbprint of the DER certificate.
	sum := sha1.Sum(cert.Raw)
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); src.x5t != want {
		t.Errorf("x5t = %q, want %q", src.x5t, want)
	}

	jwt, err := src.signAssertion()
	if err != nil {
		t.Fatalf("signAssertion: %v", err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts, want 3", len(parts))
	}

	// Header: RS256 + x5t present.
	var hdr map[string]string
	decodeJSON(t, parts[0], &hdr)
	if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" || hdr["x5t"] == "" {
		t.Errorf("bad header: %#v", hdr)
	}

	// Payload: the Saxo-required claims.
	var pl map[string]any
	decodeJSON(t, parts[1], &pl)
	if pl["iss"] != "app-123" || pl["sub"] != "user-42" ||
		pl["aud"] != "https://sim.logonvalidation.net/token" ||
		pl["spurl"] != "https://example.test/app" {
		t.Errorf("bad payload claims: %#v", pl)
	}
	if _, ok := pl["exp"]; !ok {
		t.Error("missing exp claim")
	}

	// Signature must verify against the certificate's public key.
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("signature verify failed: %v", err)
	}
}

func decodeJSON(t *testing.T, seg string, v any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal segment: %v", err)
	}
}
