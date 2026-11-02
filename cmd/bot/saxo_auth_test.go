package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestSaxoAuthSelection guards which credential the live Saxo session
// uses: auth_mode "certificate" selects certificate auth and "" / "token"
// the static token, the
// static source hands out the configured token, and a certificate config
// missing its required fields fails at startup instead of on the first
// order.
func TestSaxoAuthSelection(t *testing.T) {
	t.Parallel()

	for mode, want := range map[string]string{
		"": "token", "token": "token", "certificate": "certificate",
		// KNOWN DEFECT TC-2, pinned so its fix is a deliberate change: a
		// differently-cased "Certificate" silently means token auth.
		// Flip this row when the fix lands.
		"Certificate": "token",
	} {
		var cfg config.Config

		cfg.Saxo.AuthMode = mode
		if got := saxoAuthMode(cfg); got != want {
			t.Errorf("saxoAuthMode(%q) = %q, want %q", mode, got, want)
		}
	}

	var cfg config.Config

	cfg.Saxo.Token = "tok"

	ts, err := saxoTokenSource(cfg)
	if err != nil {
		t.Fatalf("token mode: %v", err)
	}

	if _, ok := ts.(*broker.StaticTokenSource); !ok {
		t.Errorf("token mode built %T, want a static source", ts)
	}

	if tok, err := ts.Token(context.Background()); err != nil || tok != "tok" {
		t.Errorf("static Token = %q, %v", tok, err)
	}

	cfg.Saxo.AuthMode = "certificate"

	if _, err := saxoTokenSource(cfg); err == nil {
		t.Error("certificate mode with no app key, user or cert was accepted")
	}

	cfg.Saxo.AppKey, cfg.Saxo.UserID = "app", "user"
	cfg.Saxo.BaseURL = "https://gateway.saxobank.com/sim/openapi"
	cfg.Saxo.CertPath = writeSelfSignedPEM(t)

	ts, err = saxoTokenSource(cfg)
	if err != nil {
		t.Fatalf("certificate mode: %v", err)
	}

	if _, ok := ts.(*broker.StaticTokenSource); ok {
		t.Error("certificate mode built a static token source")
	}
}

// writeSelfSignedPEM writes a throwaway RSA certificate and key as PEM.
func writeSelfSignedPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)

	var buf bytes.Buffer

	_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = pem.Encode(&buf, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	path := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// mainConfigEnv names the config file a child process's main should load.
const mainConfigEnv = "GFB_BOT_MAIN_CONFIG"

// TestMainProcessHelper is not a test: it is main, run inside a child copy
// of the test binary by TestMainExitsWithRunsCode, because main calls
// os.Exit.
func TestMainProcessHelper(t *testing.T) {
	path := os.Getenv(mainConfigEnv)
	if path == "" {
		t.Skip("helper for TestMainExitsWithRunsCode; runs only in a child process")
	}

	os.Args = []string{"bot", "-config", path}
	flag.CommandLine = flag.NewFlagSet("bot", flag.ExitOnError)

	main()
}

// TestMainExitsWithRunsCode guards main's one job: the process must exit
// with the code run returns — a startup failure that exited 0 would let a
// supervisor (systemd, docker restart policies) treat a bot that never
// started as a clean stop and not restart or alert.
func TestMainExitsWithRunsCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("engine: [this is not valid yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcessHelper$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), mainConfigEnv+"="+path)

	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Errorf("main with an unparsable config: %v, want exit status 1", err)
	}
}
