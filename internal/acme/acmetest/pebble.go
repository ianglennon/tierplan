// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package acmetest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PebbleImage is the Pebble release the integration suite runs against. The tag has no leading "v".
const PebbleImage = "ghcr.io/letsencrypt/pebble:2.10.1"

const (
	pebbleACMEPort       = "14000/tcp"
	pebbleManagementPort = "15000/tcp"
	// pebbleStartup bounds the pull, the start and the first answer of the container.
	pebbleStartup = 5 * time.Minute
)

// PebbleOptions configure StartPebble.
type PebbleOptions struct {
	// HTTPPort is the port Pebble connects to for an HTTP-01 check (its config's httpPort), such as the
	// port of a Relay. Required.
	HTTPPort int
	// Hosts are names the container resolves to the host running the test (the engine's
	// host-gateway), such as "tierplan.test": the names the certificate is ordered for.
	Hosts []string
}

// Pebble is a running Pebble ACME server.
type Pebble struct {
	acmeURL, mgmtURL string
	caFile           string
	caCert           *x509.Certificate
}

// StartPebble runs Pebble in a container and removes it when the test ends. Its randomness is switched
// off, so a run is the same every time: no validation sleep, no rejected nonces and no reused
// authorizations. Pebble still picks a profile at random for an order that names none, so every order
// must name one ("default" or "shortlived").
//
// Its HTTPS certificate is generated here, valid from an hour ago for a day, and signed by a CA
// generated here too; CAFile is that CA, for TIERPLAN_ACME_CA_FILE. The image's own certificate has a
// fixed expiry date, which would make the suite fail on a date, and nothing here skips verification.
func StartPebble(t testing.TB, o PebbleOptions) *Pebble {
	t.Helper()
	if o.HTTPPort <= 0 {
		t.Fatal("acmetest: StartPebble needs the HTTP-01 port")
	}
	dir := t.TempDir()
	caCert, caPEM, certPEM, keyPEM := generatePebbleTLS(t)
	caFile := filepath.Join(dir, "pebble-test-ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("acmetest: write the Pebble CA: %v", err)
	}
	cfg, err := json.Marshal(map[string]any{"pebble": map[string]any{
		"listenAddress":                  "0.0.0.0:14000",
		"managementListenAddress":        "0.0.0.0:15000",
		"certificate":                    "/cfg/tls.crt",
		"privateKey":                     "/cfg/tls.key",
		"httpPort":                       o.HTTPPort,
		"tlsPort":                        5001,
		"ocspResponderURL":               "",
		"externalAccountBindingRequired": false,
		"domainBlocklist":                []string{"blocked-domain.example"},
		"retryAfter":                     map[string]int{"authz": 1, "order": 1},
		"keyAlgorithm":                   "ecdsa",
		"profiles": map[string]any{
			"default":    map[string]any{"description": "The default profile", "validityPeriod": 7776000},
			"shortlived": map[string]any{"description": "A short-lived profile", "validityPeriod": 518400},
		},
	}})
	if err != nil {
		t.Fatalf("acmetest: encode the Pebble config: %v", err)
	}

	// The same pool the clients use to reach Pebble, so the wait below verifies the certificate too.
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	extraHosts := make([]string, 0, len(o.Hosts))
	for _, h := range o.Hosts {
		extraHosts = append(extraHosts, h+":host-gateway")
	}

	ctx, cancel := context.WithTimeout(context.Background(), pebbleStartup)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: PebbleImage,
			Cmd:   []string{"-config", "/cfg/pebble-config.json"},
			Env: map[string]string{
				"PEBBLE_VA_NOSLEEP":      "1",
				"PEBBLE_WFE_NONCEREJECT": "0",
				"PEBBLE_AUTHZREUSE":      "0",
			},
			ExposedPorts: []string{pebbleACMEPort, pebbleManagementPort},
			Files: []testcontainers.ContainerFile{
				{Reader: bytes.NewReader(cfg), ContainerFilePath: "/cfg/pebble-config.json", FileMode: 0o644},
				{Reader: bytes.NewReader(certPEM), ContainerFilePath: "/cfg/tls.crt", FileMode: 0o644},
				{Reader: bytes.NewReader(keyPEM), ContainerFilePath: "/cfg/tls.key", FileMode: 0o644},
			},
			HostConfigModifier: func(hc *container.HostConfig) { hc.ExtraHosts = extraHosts },
			// The image has no shell, so the wait is a request from here, over verified TLS.
			WaitingFor: wait.ForHTTP("/dir").WithPort(pebbleACMEPort).WithTLS(true, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}).
				WithStartupTimeout(pebbleStartup),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("acmetest: start %s: %v", PebbleImage, err)
	}
	// Registered after the container's own clean-up, so it runs before it: a failed test shows what
	// Pebble logged, which is the only place its reason for refusing a request is written.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		logCtx, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		rd, err := ctr.Logs(logCtx)
		if err != nil {
			t.Logf("acmetest: read Pebble's log: %v", err)
			return
		}
		defer func() { _ = rd.Close() }()
		out, _ := io.ReadAll(rd)
		t.Logf("acmetest: Pebble's log:\n%s", out)
	})
	acmePort, err := ctr.MappedPort(ctx, pebbleACMEPort)
	if err != nil {
		t.Fatalf("acmetest: find Pebble's ACME port: %v", err)
	}
	mgmtPort, err := ctr.MappedPort(ctx, pebbleManagementPort)
	if err != nil {
		t.Fatalf("acmetest: find Pebble's management port: %v", err)
	}
	return &Pebble{
		acmeURL: "https://" + net.JoinHostPort("127.0.0.1", acmePort.Port()),
		mgmtURL: "https://" + net.JoinHostPort("127.0.0.1", mgmtPort.Port()),
		caFile:  caFile,
		caCert:  caCert,
	}
}

// DirectoryURL is the ACME directory, for TIERPLAN_ACME_DIRECTORY_URL.
func (p *Pebble) DirectoryURL() string { return p.acmeURL + "/dir" }

// CAFile is the path of the PEM file holding the CA that signed Pebble's HTTPS certificate, for
// TIERPLAN_ACME_CA_FILE.
func (p *Pebble) CAFile() string { return p.caFile }

// IssuerRoots are the root certificates of the CA Pebble issues from, as its management interface
// reports them. Pebble makes a new root every time it starts. Trust them to verify a certificate
// Pebble issued.
func (p *Pebble) IssuerRoots(t testing.TB) []*x509.Certificate {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(p.caCert)
	hc := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}},
	}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, p.mgmtURL+"/roots/0", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("acmetest: read Pebble's root: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("acmetest: read Pebble's root: status %d: %v %s", resp.StatusCode, err, body)
	}
	var roots []*x509.Certificate
	for rest := body; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("acmetest: parse Pebble's root: %v", err)
		}
		roots = append(roots, c)
	}
	if len(roots) == 0 {
		t.Fatalf("acmetest: Pebble's /roots/0 answered no certificate: %s", body)
	}
	return roots
}

// generatePebbleTLS makes a CA and a server certificate for localhost and 127.0.0.1 signed by it.
func generatePebbleTLS(t testing.TB) (ca *x509.Certificate, caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "TierPlan Pebble test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return ca,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(fmt.Errorf("acmetest: make a serial number: %w", err))
	}
	return n
}
