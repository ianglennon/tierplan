// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
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
// With useTLS the probe speaks https and does not verify the certificate. That
// is deliberate and safe: the address is the listener's own, in the same
// container, and a self-signed certificate (BAYPLAN_TLS_SELF_SIGNED) could not
// be verified anyway. The probe sends no credentials and reads no body.
func Healthcheck(ctx context.Context, listen string, useTLS bool) error {
	client := http.DefaultClient
	if useTLS {
		client = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true, //nolint:gosec // loopback healthcheck of our own listener, possibly self-signed
			},
		}}
		defer client.CloseIdleConnections()
	}
	return healthcheck(ctx, listen, useTLS, client)
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
