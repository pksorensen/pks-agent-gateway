package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// `gateway identity init` (ADR 0006): generates the gateway's own workload
// identity — an RSA key and a self-signed certificate — for entra-cert
// credentials. It never overwrites: an existing pair is only described, so it
// is safe to run on every container start.

const (
	identityKeyFile  = "key.pem"
	identityCertFile = "cert.pem"
)

var errIdentityPartial = errors.New("identity dir holds only one of key.pem / cert.pem; refusing to overwrite")

// runIdentityCmd is the `gateway identity …` entry point; it returns the
// process exit code.
func runIdentityCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "init" {
		fmt.Fprintln(stderr, "usage: gateway identity init [--dir /data/identity] [--bits 3072] [--cn pks-agent-gateway] [--years 2]")
		return 2
	}
	fs := flag.NewFlagSet("identity init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", env("GATEWAY_IDENTITY_DIR", filepath.Join(env("USER_DATA_DIR", "./data"), "identity")), "directory for key.pem + cert.pem")
	bits := fs.Int("bits", 3072, "RSA key size (2048 or 3072)")
	cn := fs.String("cn", "pks-agent-gateway", "certificate subject CN")
	years := fs.Int("years", 2, "certificate validity in years")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	info, created, err := initIdentity(*dir, *bits, *cn, *years, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "identity init:", err)
		return 1
	}
	if created {
		fmt.Fprintf(stdout, "Created a new identity in %s\n\n", *dir)
	} else {
		fmt.Fprintf(stdout, "Identity already present in %s — not overwritten\n\n", *dir)
	}
	printIdentity(stdout, info)
	return 0
}

// identityInfo is what an operator needs to register the certificate.
type identityInfo struct {
	KeyPath    string
	CertPath   string
	CertDER    []byte
	Thumbprint string // hex SHA-1, uppercase — what the Entra portal shows
	X5t        string // base64url SHA-1 — the client assertion header
	NotAfter   time.Time
}

func initIdentity(dir string, bits int, cn string, years int, now time.Time) (*identityInfo, bool, error) {
	if bits != 2048 && bits != 3072 {
		return nil, false, fmt.Errorf("--bits must be 2048 or 3072")
	}
	if years < 1 || years > 5 {
		return nil, false, fmt.Errorf("--years must be 1..5")
	}
	keyPath := filepath.Join(dir, identityKeyFile)
	certPath := filepath.Join(dir, identityCertFile)
	_, kerr := os.Stat(keyPath)
	_, cerr := os.Stat(certPath)
	switch {
	case kerr == nil && cerr == nil:
		info, err := describeIdentity(dir)
		return info, false, err
	case kerr == nil || cerr == nil:
		return nil, false, errIdentityPartial
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, false, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, false, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(years, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, false, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, false, err
	}
	// O_EXCL: a concurrent init (two replicas starting) loses instead of
	// clobbering.
	if err := writeExclusive(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		return nil, false, err
	}
	if err := writeExclusive(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, false, err
	}
	info, err := describeIdentity(dir)
	return info, true, err
}

func writeExclusive(p string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// describeIdentity loads the pair through the same checks a mint uses.
func describeIdentity(dir string) (*identityInfo, error) {
	id, err := loadAssertionIdentity(dir, identityKeyFile, identityCertFile)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(id.certDER)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum(id.certDER)
	return &identityInfo{
		KeyPath:    filepath.Join(dir, identityKeyFile),
		CertPath:   filepath.Join(dir, identityCertFile),
		CertDER:    id.certDER,
		Thumbprint: strings.ToUpper(hex.EncodeToString(sum[:])),
		X5t:        id.x5t(),
		NotAfter:   cert.NotAfter,
	}, nil
}

func printIdentity(w io.Writer, info *identityInfo) {
	b64 := base64.StdEncoding.EncodeToString(info.CertDER)
	fmt.Fprintf(w, "key:        %s (0600 — never copy it off this volume)\n", info.KeyPath)
	fmt.Fprintf(w, "cert:       %s\n", info.CertPath)
	fmt.Fprintf(w, "thumbprint: %s (SHA-1, as shown in Entra)\n", info.Thumbprint)
	fmt.Fprintf(w, "x5t:        %s\n", info.X5t)
	fmt.Fprintf(w, "expires:    %s\n\n", info.NotAfter.UTC().Format(time.RFC3339))
	kc, _ := json.MarshalIndent(map[string]any{
		"type":        "AsymmetricX509Cert",
		"usage":       "Verify",
		"key":         b64,
		"displayName": "pks-agent-gateway " + info.Thumbprint[:8],
	}, "", "  ")
	fmt.Fprintf(w, "keyCredentials entry for the upstream app registration\n")
	fmt.Fprintf(w, "(PATCH /applications/{id} REPLACES the whole list — include every key to keep):\n%s\n\n", kc)
	fmt.Fprintf(w, "base64 DER:\n%s\n", b64)
}
