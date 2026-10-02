// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ianglennon/bayplan/internal/config"
)

const (
	selfSignedDir      = "tls"
	selfSignedCertFile = "selfsigned.crt"
	selfSignedKeyFile  = "selfsigned.key"

	selfSignedValidity = 365 * 24 * time.Hour
	// renewBefore is how long before expiry a self-signed certificate is replaced.
	renewBefore = 30 * 24 * time.Hour
	// backdate covers a client whose clock runs a little behind.
	backdate = time.Hour
)

// TLSEnabled reports whether the settings ask for HTTPS, from files or from a
// generated certificate. The healthcheck uses it to pick http or https.
func TLSEnabled(cfg config.Config) bool {
	return cfg.TLSSelfSigned || cfg.TLSCertFile != "" || cfg.TLSKeyFile != ""
}

// TLSConfig returns the TLS configuration to serve with, or nil for plain HTTP (D-12).
//
// With BAYPLAN_TLS_CERT_FILE and BAYPLAN_TLS_KEY_FILE it serves that pair. With
// BAYPLAN_TLS_SELF_SIGNED it serves an ECDSA P-256 certificate kept under
// <data>/tls, covering localhost, the loopback addresses, the hostname and the
// host of publicURL. That certificate is generated when missing or unreadable,
// replaced when the names it must cover change, and replaced within 30 days of
// its expiry. The minimum version is TLS 1.2.
func TLSConfig(cfg config.Config, publicURL string, now func() time.Time) (*tls.Config, error) {
	files := cfg.TLSCertFile != "" || cfg.TLSKeyFile != ""
	if files && cfg.TLSSelfSigned {
		return nil, errors.New("BAYPLAN_TLS_SELF_SIGNED conflicts with BAYPLAN_TLS_CERT_FILE and BAYPLAN_TLS_KEY_FILE; use one or the other")
	}
	if now == nil {
		now = time.Now
	}

	switch {
	case files:
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load the TLS certificate from BAYPLAN_TLS_CERT_FILE and BAYPLAN_TLS_KEY_FILE: %w", err)
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
	case cfg.TLSSelfSigned:
		cert, err := selfSigned(filepath.Join(cfg.DataDir, selfSignedDir), wantedNames(publicURL), now())
		if err != nil {
			return nil, fmt.Errorf("prepare the self-signed certificate (BAYPLAN_TLS_SELF_SIGNED): %w", err)
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
	}
	return nil, nil
}

// fingerprint is the SHA-256 of the served certificate, as colon-separated
// upper-case hex, the form a browser shows. Empty when there is no certificate.
func fingerprint(c *tls.Config) string {
	if c == nil || len(c.Certificates) == 0 || len(c.Certificates[0].Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(c.Certificates[0].Certificate[0])
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// names is the set of subject alternative names a self-signed certificate covers.
type names struct {
	dns []string // sorted, lower case, unique
	ips []string // sorted canonical text, unique
}

// osHostname is the machine host name; a test replaces it.
var osHostname = os.Hostname

// looksLikeContainerID is a stub until the GREEN step.
func looksLikeContainerID(string) bool { return false }

func wantedNames(publicURL string) names {
	dns := map[string]bool{"localhost": true}
	ips := map[string]bool{"127.0.0.1": true, "::1": true}
	add := func(host string) {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			return
		}
		if ip := net.ParseIP(host); ip != nil {
			ips[ip.String()] = true
			return
		}
		dns[host] = true
	}
	if h, err := osHostname(); err == nil {
		add(h)
	}
	if u, err := url.Parse(publicURL); err == nil {
		add(u.Hostname())
	}
	return names{dns: sortedKeys(dns), ips: sortedKeys(ips)}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// covers reports whether leaf carries exactly these names.
func (n names) covers(leaf *x509.Certificate) bool {
	dns := slices.Clone(leaf.DNSNames)
	slices.Sort(dns)
	ips := make([]string, 0, len(leaf.IPAddresses))
	for _, ip := range leaf.IPAddresses {
		ips = append(ips, ip.String())
	}
	slices.Sort(ips)
	return slices.Equal(dns, n.dns) && slices.Equal(ips, n.ips)
}

// selfSigned loads the stored certificate when it is still right, and otherwise
// generates and stores a new one.
func selfSigned(dir string, want names, now time.Time) (tls.Certificate, error) {
	certPath := filepath.Join(dir, selfSignedCertFile)
	keyPath := filepath.Join(dir, selfSignedKeyFile)

	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		leaf, perr := x509.ParseCertificate(cert.Certificate[0])
		if perr == nil && want.covers(leaf) && leaf.NotAfter.Sub(now) > renewBefore && !now.Before(leaf.NotBefore) {
			return cert, nil
		}
	}
	// Missing, corrupt, a mismatched pair, other names or nearly expired: replace it.

	certPEM, keyPEM, err := generate(want, now)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("create %s: %w", dir, err)
	}
	// The key goes first. A crash between the two renames leaves a pair that
	// does not match, which the next start detects and replaces.
	if err := writeAtomic(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeAtomic(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// generate makes a one-year ECDSA P-256 server certificate for the names.
func generate(want names, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate a key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, fmt.Errorf("generate a serial number: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Bayplan self-signed", Organization: []string{"Bayplan"}},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              want.dns,
	}
	for _, s := range want.ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(s))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create the certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode the key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// writeAtomic writes data to a temporary file in the same directory, sets its
// mode and renames it over path, so a reader never sees a partial file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
