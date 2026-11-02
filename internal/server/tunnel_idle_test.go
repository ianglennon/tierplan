// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// An agent tunnel is a hijacked connection that sits idle between Docker calls.
// This asserts RESEARCH A10 against the real http.Server settings: the server's
// ReadTimeout does not reach a hijacked connection, so a tunnel that is idle for
// longer than it stays connected and still serves a request afterwards.
func TestTunnelIdleLongerThanTheReadTimeoutStaysConnected(t *testing.T) {
	const readTimeout = time.Second

	srv := tunneltest.Start(t, tunneltest.Options{
		// The hub is served by the http.Server TierPlan itself builds.
		Server: func(h http.Handler) *http.Server { return newHTTPServer(h, nil, readTimeout) },
	})
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	c.AnswerDocker(t, http.StatusOK, "still here")
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	if s == nil {
		t.Fatal("no session")
	}

	// This asserts an absence, so time has to pass: the session must stay open for
	// three read timeouts. It returns as soon as the session ends, which is the
	// failure, instead of waiting out the whole period.
	select {
	case <-s.Done():
		t.Fatalf("the tunnel was closed within %v idle with a ReadTimeout of %v", 3*readTimeout, readTimeout)
	case <-time.After(3 * readTimeout): // wall-clock: absence under test; a ReadTimeout regression closes the tunnel within one read timeout, so three can only miss a late close, never fail a correct run
	}

	client := &http.Client{Transport: &http.Transport{DialContext: s.Transport().DialContext}}
	ctx := testsupport.Context(t)
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
