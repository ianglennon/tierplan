// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

const healthcheckTimeout = 3 * time.Second

// Healthcheck probes /healthz on the loopback address of the configured
// listen port. It returns nil only for a 200 answer. A distroless image has no
// curl, so the container HEALTHCHECK runs `bayplan healthcheck` (D-17, D-86).
// Plan 27 adds TLS.
func Healthcheck(ctx context.Context, listen string) error {
	return healthcheck(ctx, listen, http.DefaultClient)
}

func healthcheck(ctx context.Context, listen string, client *http.Client) error {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("cannot read the port from listen address %q: %w", listen, err)
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()

	url := "http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz"
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
