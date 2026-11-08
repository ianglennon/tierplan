// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

//go:build ignore

// dev-cert writes the certificate the Vite dev server serves over https (plan 13.1-05, D-08):
// a small development CA and a leaf signed by it.
//
//	go run scripts/dev-cert.go -out .devdata/dev-tls [-name host]...
//
// It writes ca.pem and ca-key.pem (the CA, valid two years) and cert.pem and key.pem (the leaf,
// valid one year) for localhost, 127.0.0.1, ::1, dev.homelab.org.uk and every -name. The CA is
// kept while it has more than 30 days left; the leaf is kept while it has more than 30 days left
// and covers every name, and otherwise it is signed again by the same CA. A browser that trusted
// ca.pem once therefore keeps trusting every later leaf. Keys are mode 0600.
//
// The CA key is for development only: it lives under the git-ignored .devdata and the maintainer's
// own browser is the only thing that should trust it.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	caValidFor   = 2 * 365 * 24 * time.Hour
	leafValidFor = 365 * 24 * time.Hour
	// renewBelow: a certificate with less life than this is replaced.
	renewBelow = 30 * 24 * time.Hour
)

// defaultNames are the names the dev loop is opened by: this machine, and the maintainer's LAN name.
var defaultNames = []string{"localhost", "127.0.0.1", "::1", "dev.homelab.org.uk"}

type nameList []string

func (n *nameList) String() string { return strings.Join(*n, ",") }
func (n *nameList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("an empty -name")
	}
	*n = append(*n, v)
	return nil
}

func main() {
	out := flag.String("out", ".devdata/dev-tls", "directory for ca.pem, ca-key.pem, cert.pem and key.pem")
	var extra nameList
	flag.Var(&extra, "name", "an extra host name or IP address for the leaf (repeatable)")
	flag.Parse()
	if err := run(*out, extra); err != nil {
		fmt.Fprintln(os.Stderr, "dev-cert:", err)
		os.Exit(1)
	}
}

func run(dir string, extra []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	names := slices.Clone(defaultNames)
	for _, n := range extra {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	caPath, caKeyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")

	ca, caKey, newCA, err := keepOrMakeCA(caPath, caKeyPath)
	if err != nil {
		return err
	}
	if !newCA && leafKept(certPath, keyPath, ca, names) {
		fmt.Println("dev-cert: kept", certPath)
	} else {
		if err := writeLeaf(certPath, keyPath, ca, caKey, names); err != nil {
			return err
		}
		fmt.Println("dev-cert: wrote", certPath)
	}
	fmt.Println("dev-cert: CA", caPath)
	fmt.Println("dev-cert: names", strings.Join(names, " "))
	sum := sha256.Sum256(ca.Raw)
	fmt.Println("dev-cert: CA SHA-256", strings.ToUpper(hex.EncodeToString(sum[:])))
	return nil
}

// keepOrMakeCA returns the CA in dir when it is usable and has life left, else a new one.
func keepOrMakeCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, bool, error) {
	if cert, err := readCert(certPath); err == nil && time.Until(cert.NotAfter) > renewBelow {
		if key, kerr := readKey(keyPath); kerr == nil {
			if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); ok && pub.Equal(&key.PublicKey) {
				return cert, key, false, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, false, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, false, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "TierPlan development CA", Organization: []string{"TierPlan development"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidFor),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, false, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, false, err
	}
	if err := writeKeyPair(certPath, keyPath, der, key); err != nil {
		return nil, nil, false, err
	}
	return cert, key, true, nil
}

// leafKept reports whether the leaf in dir is still good: signed by ca, matching its key, more than
// renewBelow left, and covering every wanted name.
func leafKept(certPath, keyPath string, ca *x509.Certificate, names []string) bool {
	cert, err := readCert(certPath)
	if err != nil || time.Until(cert.NotAfter) <= renewBelow {
		return false
	}
	key, err := readKey(keyPath)
	if err != nil {
		return false
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return false
	}
	if cert.CheckSignatureFrom(ca) != nil {
		return false
	}
	for _, n := range names {
		if cert.VerifyHostname(n) != nil {
			return false
		}
	}
	return true
}

func writeLeaf(certPath, keyPath string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, names []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := newSerial()
	if err != nil {
		return err
	}
	now := time.Now()
	notAfter := now.Add(leafValidFor)
	if notAfter.After(ca.NotAfter) {
		notAfter = ca.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "TierPlan development server", Organization: []string{"TierPlan development"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	return writeKeyPair(certPath, keyPath, der, key)
}

func writeKeyPair(certPath, keyPath string, der []byte, key *ecdsa.PrivateKey) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	// WriteFile keeps the mode of a file that already exists, so state the mode.
	if err := os.Chmod(keyPath, 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM block", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func readKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM block", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an ECDSA key", path)
	}
	return ek, nil
}
