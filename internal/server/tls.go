// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
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
	"sync/atomic"
	"time"

	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/authn"
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

// TLSEnabled reports whether the environment asks for HTTPS, from files or from a
// generated certificate. The healthcheck and the redirect gate use it. It is
// config.Config.EnvTLS, the one definition shared with `tierplan https disable`.
func TLSEnabled(cfg config.Config) bool {
	_, ok := cfg.EnvTLS()
	return ok
}

// TLSConfig returns the TLS configuration to serve with, or nil for plain HTTP (D-12).
//
// With TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE it serves that pair. With
// TIERPLAN_TLS_SELF_SIGNED it serves an ECDSA P-256 certificate kept under
// <data>/tls, covering localhost, the loopback addresses, the machine host name
// (unless it is a container ID), the host of publicURL and the host of agentURL
// (the address agents dial when it is not the public one, D-18). That certificate is
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
func TLSConfig(cfg config.Config, publicURL, agentURL string, now func() time.Time) (*tls.Config, error) {
	src, _, err := newCertSource(cfg, publicURL, agentURL, now)
	if err != nil || src == nil {
		return nil, err
	}
	return &tls.Config{GetCertificate: src.GetCertificate, MinVersion: tls.VersionTLS12}, nil
}

// newCertSource builds the certificate source the environment's TLS settings
// ask for, and says which kind it is. It returns a nil source and tlsKindNone
// for plain HTTP. The source does not refresh itself: the server runs
// runCertRefresh beside it, so a handshake never waits on the disk.
func newCertSource(cfg config.Config, publicURL, agentURL string, now func() time.Time) (*certSource, tlsKind, error) {
	files := cfg.TLSCertFile != "" || cfg.TLSKeyFile != ""
	if files && cfg.TLSSelfSigned {
		return nil, tlsKindNone, errors.New("TIERPLAN_TLS_SELF_SIGNED conflicts with TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE; use one or the other")
	}
	if now == nil {
		now = time.Now
	}

	switch {
	case files:
		src, err := newSuppliedSource(cfg.TLSCertFile, cfg.TLSKeyFile, now)
		if err != nil {
			return nil, tlsKindNone, fmt.Errorf("load the TLS certificate from TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE: %w", err)
		}
		return src, tlsKindSupplied, nil
	case cfg.TLSSelfSigned:
		src, err := newSelfSignedSource(filepath.Join(cfg.DataDir, selfSignedDir), wantedNames(publicURL, agentURL), now)
		if err != nil {
			return nil, tlsKindNone, fmt.Errorf("prepare the self-signed certificate (TIERPLAN_TLS_SELF_SIGNED): %w", err)
		}
		return src, tlsKindSelfSigned, nil
	}
	return nil, tlsKindNone, nil
}

// certSource serves the current certificate and keeps it fresh. It holds the
// pair in memory, so a failed renewal or reload leaves the working certificate
// in place. A handshake only loads an atomic pointer: it takes no lock, reads no
// clock and touches no file. Refresh does the looking, on a ticker.
type certSource struct {
	now func() time.Time

	// cert is what GetCertificate answers with.
	cert atomic.Pointer[tls.Certificate]

	// mu serialises Refresh calls and guards leaf and checked. No handshake
	// takes it.
	mu      sync.Mutex
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
	c.leaf, c.checked = leaf, now
	c.cert.Store(&cert)
	return nil
}

// GetCertificate implements tls.Config.GetCertificate. The hello is not read:
// one certificate serves every name.
func (c *certSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.cert.Load(), nil
}

// Refresh looks again at the certificate when the recheck interval has passed
// since the last look, keeping the current pair when the new one cannot be had.
func (c *certSource) Refresh(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.checked) < c.interval {
		return
	}
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

// runCertRefresh refreshes src once per its recheck interval until ctx ends.
func runCertRefresh(ctx context.Context, clk stackops.Clock, src *certSource) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-clk.After(src.interval):
			src.Refresh(clk.Now())
		}
	}
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
	c, err1 := os.ReadFile(certFile) //nolint:gosec // the operator-configured certificate path, read to detect a replaced pair
	k, err2 := os.ReadFile(keyFile)  //nolint:gosec // the operator-configured key path, read to detect a replaced pair
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
// localhost and the loopback addresses, the machine host name, and the hosts of
// the public URL and the agent URL, which agents dial and verify the certificate
// against (D-06, Pitfall 3). A host name that is a container ID is left out: it changes on
// every recreation, so including it would regenerate the certificate (and
// strand any browser that trusted the old one) at every upgrade (WR-09).
func wantedNames(publicURL, agentURL string) names {
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
	for _, raw := range []string{publicURL, agentURL} {
		if u, err := url.Parse(raw); err == nil {
			add(u.Hostname())
		}
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
// signs a new one.
//
// The key and the certificate are kept apart. The key is what agents pin (D-06),
// so it is created once and then only ever read: a renewal, or a change of the
// names wanted, signs a new certificate with the stored key and leaves its SPKI
// pin alone. A key is generated only when none can be loaded, and that is logged
// as a pin change because every pinned agent depends on the old one.
func selfSigned(dir string, want names, now time.Time) (tls.Certificate, error) {
	certPath := filepath.Join(dir, selfSignedCertFile)
	keyPath := filepath.Join(dir, selfSignedKeyFile)

	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		leaf, perr := x509.ParseCertificate(cert.Certificate[0])
		if perr == nil && want.covers(leaf) && leaf.NotAfter.Sub(now) > renewBefore && !now.Before(leaf.NotBefore) {
			return cert, nil
		}
	}
	// Missing, corrupt, a mismatched pair, other names or nearly expired: sign a
	// new certificate, with the stored key when it can be read.

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("create %s: %w", dir, err)
	}
	key, keyErr := loadKey(keyPath)
	newlyMade := keyErr != nil
	_, statErr := os.Stat(certPath)
	hadCert := statErr == nil
	if newlyMade {
		key, keyErr = newKey()
		if keyErr != nil {
			return tls.Certificate{}, keyErr
		}
		keyPEM, err := encodeKey(key)
		if err != nil {
			return tls.Certificate{}, err
		}
		// The key goes first. A crash between the two renames leaves a pair that
		// does not match, which the next start detects and replaces.
		if err := writeAtomic(keyPath, keyPEM, 0o600); err != nil {
			return tls.Certificate{}, err
		}
	}

	certDER, err := signCertificate(key, want, now)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := writeAtomic(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if newlyMade {
		logNewKey(hadCert, certDER)
	}
	return tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key}, nil
}

// logNewKey records that a key was created. When a certificate already existed
// the pin that agents hold is gone, which is worth a warning; on a first start
// it is just news.
func logNewKey(hadCert bool, certDER []byte) {
	var pin string
	if leaf, err := x509.ParseCertificate(certDER); err == nil {
		pin = authn.SPKIPin(leaf)
	}
	if hadCert {
		slog.Warn("the self-signed TLS key could not be loaded and was replaced, so tls_pin changed; agents pinned to the old key will refuse this server until they are reinstalled with the new pin",
			"tls_pin", pin)
		return
	}
	slog.Info("generated a self-signed TLS key", "tls_pin", pin)
}

// loadKey reads the stored ECDSA key, or reports why it cannot.
func loadKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path) //nolint:gosec // TierPlan's own key under its data directory
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("no PKCS#8 private key in " + path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("the stored key is not an ECDSA P-256 key")
	}
	return key, nil
}

// newKey makes the ECDSA P-256 key of a new self-signed identity.
func newKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate a key: %w", err)
	}
	return key, nil
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode the key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// signCertificate makes a one-year server certificate for the names, signed by
// key itself.
func signCertificate(key *ecdsa.PrivateKey, want names, now time.Time) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("generate a serial number: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "TierPlan self-signed", Organization: []string{"TierPlan"}},
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
		return nil, fmt.Errorf("create the certificate: %w", err)
	}
	return der, nil
}

// spkiPin is the SPKI pin ("sha256/" and base64) of the certificate a TLS
// config serves; empty when it serves none. The pin follows the key, so it
// survives every renewal.
func spkiPin(c *tls.Config) string {
	if c == nil || c.GetCertificate == nil {
		return ""
	}
	cert, err := c.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || cert == nil || len(cert.Certificate) == 0 {
		return ""
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return ""
	}
	return authn.SPKIPin(leaf)
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
