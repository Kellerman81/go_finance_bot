package broker

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/pkcs12"
)

// TokenSource supplies a currently-valid Saxo bearer token, refreshing as
// needed. The Saxo broker calls it on every request.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticTokenSource returns a fixed token (e.g. the 24-hour SIM developer
// token). Simple and correct for SIM development; the token must be replaced
// manually when it expires.
type StaticTokenSource struct{ token string }

// NewStaticTokenSource wraps a fixed bearer token.
func NewStaticTokenSource(token string) *StaticTokenSource { return &StaticTokenSource{token: token} }

func (s *StaticTokenSource) Token(context.Context) (string, error) {
	if s.token == "" {
		return "", fmt.Errorf("saxo: no token configured")
	}
	return s.token, nil
}

// CertConfig configures certificate-based (CBA) authentication for unattended
// live trading. The app, user and certificate are provisioned via Saxo's
// MyAccount once a live application is approved.
type CertConfig struct {
	AppKey       string // client_id (iss claim)
	AppSecret    string // client_secret (HTTP Basic)
	AppURL       string // spurl claim
	UserID       string // sub claim — the user the certificate is issued for
	AuthURL      string // e.g. https://live.logonvalidation.net (no trailing slash)
	CertPath     string // path to a .p12/.pfx or PEM certificate+key
	CertPassword string // password for the certificate file
}

// certTokenSource implements certificate-based auth: it signs a JWT assertion
// with the certificate's private key, exchanges it for an access token via the
// personal-jwt grant, then keeps the token fresh using the returned refresh
// token (falling back to a new assertion when the refresh token expires).
type certTokenSource struct {
	cfg        CertConfig
	tokenURL   string
	key        *rsa.PrivateKey
	x5t        string // base64url SHA-1 thumbprint of the certificate
	http       *http.Client

	mu         sync.Mutex
	access     string
	accessExp  time.Time
	refresh    string
	refreshExp time.Time
}

// NewCertTokenSource loads the certificate and prepares a CBA token source.
func NewCertTokenSource(cfg CertConfig) (*certTokenSource, error) {
	if cfg.AppKey == "" || cfg.UserID == "" || cfg.CertPath == "" {
		return nil, fmt.Errorf("saxo cert auth requires app_key, user_id and cert_path")
	}
	if cfg.AuthURL == "" {
		return nil, fmt.Errorf("saxo cert auth requires auth_url")
	}
	key, cert, err := loadCertificate(cfg.CertPath, cfg.CertPassword)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum(cert.Raw)
	return &certTokenSource{
		cfg:      cfg,
		tokenURL: strings.TrimRight(cfg.AuthURL, "/") + "/token",
		key:      key,
		x5t:      base64.RawURLEncoding.EncodeToString(sum[:]),
		http:     &http.Client{Timeout: 20 * time.Second},
	}, nil
}

func (c *certTokenSource) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if c.access != "" && now.Before(c.accessExp.Add(-30*time.Second)) {
		return c.access, nil
	}
	// Prefer a cheap refresh while the refresh token is still valid.
	if c.refresh != "" && now.Before(c.refreshExp.Add(-30*time.Second)) {
		if err := c.exchange(ctx, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {c.refresh},
		}); err == nil {
			return c.access, nil
		}
	}
	// Otherwise (or on refresh failure) mint a fresh token from a JWT assertion.
	assertion, err := c.signAssertion()
	if err != nil {
		return "", err
	}
	if err := c.exchange(ctx, url.Values{
		"grant_type": {"urn:saxobank:oauth:grant-type:personal-jwt"},
		"assertion":  {assertion},
	}); err != nil {
		return "", err
	}
	return c.access, nil
}

// signAssertion builds and RS256-signs the JWT used by the personal-jwt grant.
func (c *certTokenSource) signAssertion() (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT", "x5t": c.x5t}
	now := time.Now()
	payload := map[string]any{
		"iss":   c.cfg.AppKey,
		"sub":   c.cfg.UserID,
		"aud":   c.tokenURL,
		"spurl": c.cfg.AppURL,
		"exp":   now.Add(2 * time.Minute).Unix(),
	}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(payload)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// exchange POSTs to the token endpoint and stores the resulting tokens. Caller
// holds c.mu.
func (c *certTokenSource) exchange(ctx context.Context, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.cfg.AppSecret != "" {
		req.SetBasicAuth(c.cfg.AppKey, c.cfg.AppSecret)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             int    `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
		Error                 string `json:"error"`
		ErrorDescription      string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("saxo token decode: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || body.AccessToken == "" {
		return fmt.Errorf("saxo token endpoint: status %d: %s %s", resp.StatusCode, body.Error, body.ErrorDescription)
	}

	now := time.Now()
	c.access = body.AccessToken
	c.accessExp = now.Add(time.Duration(body.ExpiresIn) * time.Second)
	if body.RefreshToken != "" {
		c.refresh = body.RefreshToken
		c.refreshExp = now.Add(time.Duration(body.RefreshTokenExpiresIn) * time.Second)
	}
	return nil
}

// loadCertificate reads an RSA private key and certificate from a .p12/.pfx
// bundle or a PEM file (certificate and key, possibly concatenated).
func loadCertificate(path, password string) (*rsa.PrivateKey, *x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read cert %s: %w", path, err)
	}
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".p12") || strings.HasSuffix(lower, ".pfx") {
		priv, cert, err := pkcs12.Decode(data, password)
		if err != nil {
			return nil, nil, fmt.Errorf("decode pkcs12: %w", err)
		}
		key, ok := priv.(*rsa.PrivateKey)
		if !ok {
			return nil, nil, fmt.Errorf("pkcs12: private key is not RSA")
		}
		return key, cert, nil
	}
	return parsePEM(data)
}

func parsePEM(data []byte) (*rsa.PrivateKey, *x509.Certificate, error) {
	var key *rsa.PrivateKey
	var cert *x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		switch {
		case block.Type == "CERTIFICATE":
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, nil, fmt.Errorf("parse certificate: %w", err)
			}
			if cert == nil {
				cert = c
			}
		case strings.Contains(block.Type, "PRIVATE KEY"):
			k, err := parsePrivateKey(block.Bytes)
			if err != nil {
				return nil, nil, err
			}
			key = k
		}
	}
	if key == nil || cert == nil {
		return nil, nil, fmt.Errorf("PEM must contain both a CERTIFICATE and an RSA PRIVATE KEY")
	}
	return key, cert, nil
}

func parsePrivateKey(der []byte) (*rsa.PrivateKey, error) {
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA")
	}
	return rk, nil
}
