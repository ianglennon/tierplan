// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
)

// An agent tunnel is a hijacked connection that sits idle between Docker calls.
// This asserts RESEARCH A10 against the real http.Server settings: the server's
// ReadTimeout does not reach a hijacked connection, so a tunnel that is idle for
// longer than it stays connected and still serves a request afterwards.
func TestTunnelIdleLongerThanTheReadTimeoutStaysConnected(t *testing.T) {
	prev := readTimeout
	readTimeout = time.Second
	t.Cleanup(func() { readTimeout = prev })

	srv := tunneltest.Start(t, tunneltest.Options{
		// The hub is served by the http.Server Bayplan itself builds.
		Server: func(h http.Handler) *http.Server { return newHTTPServer(h, nil) },
	})
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	c.AnswerDocker(t, http.StatusOK, "still here")
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	if s == nil {
		t.Fatal("no session")
	}

	time.Sleep(3 * readTimeout)
	select {
	case <-s.Done():
		t.Fatalf("the tunnel was closed after %v idle with a ReadTimeout of %v", 3*readTimeout, readTimeout)
	default:
	}

	client := &http.Client{Transport: &http.Transport{DialContext: s.Transport().DialContext}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("a request after the idle period failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "still here" {
		t.Fatalf("answer = %d %q", resp.StatusCode, body)
	}
}
