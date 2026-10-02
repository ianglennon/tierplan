// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"
)

const healthcheckTimeout = 3 * time.Second

// Healthcheck probes /healthz on the loopback address of the configured
// listen port. It returns nil only for a 200 answer. A distroless image has no
// curl, so the container HEALTHCHECK runs `bayplan healthcheck` (D-17, D-86).
//
// With useTLS the probe speaks https and does not verify the certificate. That
// is deliberate and safe: the address is always 127.0.0.1, the port is our own
// listener, and a self-signed certificate (BAYPLAN_TLS_SELF_SIGNED) could not
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
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("cannot read the port from listen address %q: %w", listen, err)
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()

	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	url := scheme + "://" + net.JoinHostPort("127.0.0.1", port) + "/healthz"
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
