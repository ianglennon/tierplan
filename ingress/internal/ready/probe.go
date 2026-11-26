// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ready

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// ProbeTimeout is how long the image's health check waits for the readiness endpoint (D-18). It is below
// Docker's own health-check timeout, so a stuck endpoint makes the probe report its own failure instead of
// Docker killing it.
const ProbeTimeout = 4 * time.Second

// DefaultReadyURL is the readiness endpoint on this node.
func DefaultReadyURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(ingressbundle.ReadyPort) + ingressbundle.ReadyPath
}

// PublishedReadyURL is the readiness endpoint as the node's own loopback sees it on a published host port,
// which is how the keepalived container, in the host network namespace, reaches the proxy (D-13).
func PublishedReadyURL(port int) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + ingressbundle.ReadyPath
}

// NewProbeClient returns the client the health check uses. It ignores proxy variables, so an HTTP_PROXY in
// the container's environment cannot redirect a loopback probe, and it does not follow redirects, because
// only a 200 from the readiness URL itself counts.
func NewProbeClient() *http.Client {
	return &http.Client{
		Timeout:   ProbeTimeout,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Probe returns nil only when url answers 200. A 503, a 404, a refused connection and a timeout each return
// an error. The body is drained and closed and never put into the error.
func Probe(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("readiness: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("readiness: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness: status %d", resp.StatusCode)
	}
	return nil
}
