// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/ianglennon/bayplan/internal/config"
)

const healthcheckTimeout = 3 * time.Second

// Healthcheck probes /healthz on the address the server listens on. A listen
// address with a concrete host (BAYPLAN_LISTEN=192.0.2.10:7480) is dialled as
// given, because a listener bound to one interface does not answer on
// loopback; an empty or wildcard host (":7480", "0.0.0.0:7480", "[::]:7480")
// is dialled on 127.0.0.1. It returns nil only for a 200 answer. A distroless
// image has no curl, so the container HEALTHCHECK runs `bayplan healthcheck`
// (D-17, D-86, WR-10).
//
// An empty certFile probes over plain HTTP. Otherwise the probe speaks https and
// verifies the listener's certificate against the one in certFile: the listener
// is in the same container and its certificate file is the one it serves, so the
// file is the trust anchor and the name checked is the first one the certificate
// carries. The certificate is therefore verified, never waved through, which is
// also what lets the repository keep its one reviewed place for switching
// verification off (scripts/insecure-tls-check.sh). The probe sends no
// credentials and reads no body.
func Healthcheck(ctx context.Context, listen, certFile string) error {
	client := http.DefaultClient
	if certFile != "" {
		tlsCfg, err := ownCertificateTrust(certFile)
		if err != nil {
			return err
		}
		client = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
		defer client.CloseIdleConnections()
	}
	return healthcheck(ctx, listen, certFile != "", client)
}

// HealthcheckCertFile is the certificate file the listener serves, for
// Healthcheck: the operator's own when one is supplied, the generated
// self-signed one when that is enabled, and empty for plain HTTP.
func HealthcheckCertFile(cfg config.Config) string {
	switch {
	case cfg.TLSCertFile != "":
		return cfg.TLSCertFile
	case cfg.TLSSelfSigned:
		return filepath.Join(cfg.DataDir, selfSignedDir, selfSignedCertFile)
	}
	return ""
}

// ownCertificateTrust is a TLS configuration that trusts exactly the
// certificates in certFile and expects the first of them to name the host.
func ownCertificateTrust(certFile string) (*tls.Config, error) {
	data, err := os.ReadFile(certFile) //nolint:gosec // the operator-configured certificate path, or Bayplan's own under its data directory
	if err != nil {
		return nil, fmt.Errorf("read the listener's certificate for the healthcheck: %w", err)
	}
	pool := x509.NewCertPool()
	var leaf *x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("read the certificate in %s: %w", certFile, err)
		}
		pool.AddCert(c)
		if leaf == nil {
			leaf = c
		}
	}
	if leaf == nil {
		return nil, fmt.Errorf("no certificate in %s", certFile)
	}
	var name string
	switch {
	case len(leaf.DNSNames) > 0:
		name = leaf.DNSNames[0]
	case len(leaf.IPAddresses) > 0:
		name = leaf.IPAddresses[0].String()
	default:
		name = leaf.Subject.CommonName
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: name}, nil
}

func healthcheck(ctx context.Context, listen string, useTLS bool, client *http.Client) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("cannot read the port from listen address %q: %w", listen, err)
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()

	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	url := scheme + "://" + net.JoinHostPort(probeHost(host), port) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s failed: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %d, want 200", url, resp.StatusCode)
	}
	return nil
}

// probeHost is the host to dial for a listen host: the host itself when it
// names one address, and loopback for an empty or wildcard host.
func probeHost(host string) string {
	if host == "" {
		return "127.0.0.1"
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsUnspecified() {
		return "127.0.0.1"
	}
	return host
}
