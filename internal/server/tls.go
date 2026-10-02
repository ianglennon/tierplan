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
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

	// A running server looks again at its certificate this often: hourly for
	// the one it generated (a renewal is weeks ahead of expiry), every minute
	// for supplied files (so a renewed pair is picked up promptly).
	selfSignedRecheck = time.Hour
	suppliedRecheck   = time.Minute
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
// <data>/tls, covering localhost, the loopback addresses, the machine host name
// (unless it is a container ID) and the host of publicURL. That certificate is
// generated when missing or unreadable, replaced when it no longer covers every
// name wanted, and replaced within 30 days of its expiry. The minimum version
// is TLS 1.2.
//
// The certificate is served through GetCertificate and Certificates is left
// empty on purpose: Go answers from a static Certificates list, and never calls
// GetCertificate, for a client that sends no server name, such as one that
// connects to an IP address. A running server therefore renews the generated
// certificate before it expires, and reloads a supplied pair that was replaced,
// without a restart (WR-09).
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
		src, err := newSuppliedSource(cfg.TLSCertFile, cfg.TLSKeyFile, now)
		if err != nil {
			return nil, fmt.Errorf("load the TLS certificate from BAYPLAN_TLS_CERT_FILE and BAYPLAN_TLS_KEY_FILE: %w", err)
		}
		return &tls.Config{GetCertificate: src.GetCertificate, MinVersion: tls.VersionTLS12}, nil
	case cfg.TLSSelfSigned:
		src, err := newSelfSignedSource(filepath.Join(cfg.DataDir, selfSignedDir), wantedNames(publicURL), now)
		if err != nil {
			return nil, fmt.Errorf("prepare the self-signed certificate (BAYPLAN_TLS_SELF_SIGNED): %w", err)
		}
		return &tls.Config{GetCertificate: src.GetCertificate, MinVersion: tls.VersionTLS12}, nil
	}
	return nil, nil
}

// certSource serves the current certificate and keeps it fresh. It holds the
// pair in memory, so a failed renewal or reload leaves the working certificate
// in place and a handshake never touches the disk more than once per interval.
type certSource struct {
	now func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate
	leaf    *x509.Certificate
	checked time.Time
	// refresh looks again at the certificate when the recheck interval has
	// passed. It runs with mu held and returns the new pair, or nil to keep the
	// current one.
	refresh  func(now time.Time, current *x509.Certificate) (*tls.Certificate, error)
	interval time.Duration
}

func (c *certSource) set(cert tls.Certificate, now time.Time) error {
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("read the certificate: %w", err)
	}
	c.cert, c.leaf, c.checked = &cert, leaf, now
	return nil
}

// GetCertificate implements tls.Config.GetCertificate. The hello is not read:
// one certificate serves every name.
func (c *certSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if now.Sub(c.checked) >= c.interval {
		c.checked = now
		next, err := c.refresh(now, c.leaf)
		switch {
		case err != nil:
			slog.Warn("keeping the current TLS certificate", "error", err)
		case next != nil:
			if err := c.set(*next, now); err != nil {
				slog.Warn("keeping the current TLS certificate", "error", err)
			}
		}
	}
	return c.cert, nil
}

// newSelfSignedSource loads or generates the self-signed certificate. Once
// within renewBefore of its expiry it is regenerated in place.
func newSelfSignedSource(dir string, want names, now func() time.Time) (*certSource, error) {
	t0 := now()
	cert, err := selfSigned(dir, want, t0)
	if err != nil {
		return nil, err
	}
	src := &certSource{now: now, interval: selfSignedRecheck}
	if err := src.set(cert, t0); err != nil {
		return nil, err
	}
	src.refresh = func(at time.Time, leaf *x509.Certificate) (*tls.Certificate, error) {
		if leaf.NotAfter.Sub(at) > renewBefore {
			return nil, nil
		}
		next, err := selfSigned(dir, want, at)
		if err != nil {
			return nil, fmt.Errorf("renew the self-signed certificate: %w", err)
		}
		return &next, nil
	}
	return src, nil
}

// newSuppliedSource loads the operator's pair. It reloads when either file's
// content changes, and keeps serving the pair it has when a replacement cannot
// be loaded (half written, mismatched, or broken).
func newSuppliedSource(certFile, keyFile string, now func() time.Time) (*certSource, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	t0 := now()
	src := &certSource{now: now, interval: suppliedRecheck}
	if err := src.set(cert, t0); err != nil {
		return nil, err
	}
	seen := readPair(certFile, keyFile)
	src.refresh = func(time.Time, *x509.Certificate) (*tls.Certificate, error) {
		cur := readPair(certFile, keyFile)
		if cur == seen {
			return nil, nil
		}
		// Remember what was looked at even when it fails, so a broken file is
		// reported once, not every minute; the next change is tried again.
		seen = cur
		next, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("reload %s and %s: %w", certFile, keyFile, err)
		}
		return &next, nil
	}
	return src, nil
}

// readPair is the content of the two files, joined, or "" for one that cannot
// be read. Comparing content is exact where a modification time can tie.
func readPair(certFile, keyFile string) string {
	c, err1 := os.ReadFile(certFile)
	k, err2 := os.ReadFile(keyFile)
	if err1 != nil || err2 != nil {
		return ""
	}
	return string(c) + "\x00" + string(k)
}

// fingerprint is the SHA-256 of the served certificate, as colon-separated
// upper-case hex, the form a browser shows. Empty when there is no certificate.
func fingerprint(c *tls.Config) string {
	if c == nil || c.GetCertificate == nil {
		return ""
	}
	cert, err := c.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || cert == nil || len(cert.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(cert.Certificate[0])
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

// looksLikeContainerID reports whether h is a Docker container ID used as a
// host name: Docker sets a container's host name to its 12-hex short ID unless
// told otherwise, and 64 hex for the full ID. Such a name changes every time
// the container is recreated, so a certificate that carried it would stop
// matching on every upgrade.
func looksLikeContainerID(h string) bool {
	if len(h) != 12 && len(h) != 64 {
		return false
	}
	for _, r := range h {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// wantedNames is the set of names the generated certificate must cover:
// localhost and the loopback addresses, the machine host name, and the host of
// the public URL. A host name that is a container ID is left out: it changes on
// every recreation, so including it would regenerate the certificate (and
// strand any browser that trusted the old one) at every upgrade (WR-09).
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
	if h, err := osHostname(); err == nil && !looksLikeContainerID(strings.ToLower(strings.TrimSpace(h))) {
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

// covers reports whether leaf carries every one of these names. Extra names
// are fine: a certificate made when the host name was still known must outlive
// a container recreation that no longer reports it.
func (n names) covers(leaf *x509.Certificate) bool {
	ips := make([]string, 0, len(leaf.IPAddresses))
	for _, ip := range leaf.IPAddresses {
		ips = append(ips, ip.String())
	}
	for _, d := range n.dns {
		if !slices.Contains(leaf.DNSNames, d) {
			return false
		}
	}
	for _, ip := range n.ips {
		if !slices.Contains(ips, ip) {
			return false
		}
	}
	return true
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
