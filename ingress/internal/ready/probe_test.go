// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ready

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

func statusServer(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeSucceedsOnlyOnA200(t *testing.T) {
	srv := statusServer(t, http.StatusOK, "")
	if err := Probe(context.Background(), NewProbeClient(), srv.URL); err != nil {
		t.Fatalf("Probe against 200 = %v, want nil", err)
	}
}

func TestProbeFailsOnNon200AndNamesTheStatusNotTheBody(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusNotFound, http.StatusNoContent, http.StatusInternalServerError} {
		srv := statusServer(t, code, "secret-body-text")
		err := Probe(context.Background(), NewProbeClient(), srv.URL)
		if err == nil {
			t.Fatalf("Probe against %d = nil, want an error", code)
		}
		if !strings.Contains(err.Error(), "status "+strconv.Itoa(code)) {
			t.Errorf("error %q does not name the status", err)
		}
		if strings.Contains(err.Error(), "secret-body-text") {
			t.Errorf("error %q carries the response body", err)
		}
	}
}

func TestProbeFailsWhenNothingListens(t *testing.T) {
	srv := statusServer(t, http.StatusOK, "")
	url := srv.URL
	srv.Close()
	err := Probe(context.Background(), NewProbeClient(), url)
	if err == nil {
		t.Fatal("Probe against a closed server = nil, want an error")
	}
	if !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "dial") {
		t.Errorf("error %q does not name the dial failure", err)
	}
}

func TestProbeFailsWhenTheServerDoesNotAnswerInTime(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	// A client whose bound is the real ProbeTimeout, so this is the production client timing out.
	if err := Probe(context.Background(), NewProbeClient(), srv.URL); err == nil {
		t.Fatal("Probe against a hung server = nil, want a timeout error")
	}
}

func TestProbeDoesNotFollowRedirects(t *testing.T) {
	target := statusServer(t, http.StatusOK, "")
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	t.Cleanup(redirect.Close)
	if err := Probe(context.Background(), NewProbeClient(), redirect.URL); err == nil {
		t.Fatal("Probe through a redirect = nil, want an error: only a 200 from the readiness URL counts")
	}
}

func TestProbeIgnoresProxyVariables(t *testing.T) {
	srv := statusServer(t, http.StatusOK, "")
	// A proxy variable must not send a loopback probe somewhere else. The address is one nothing listens on.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	if err := Probe(context.Background(), NewProbeClient(), srv.URL); err != nil {
		t.Fatalf("Probe with HTTP_PROXY set = %v, want nil", err)
	}
}

func TestProbeHonoursACancelledContext(t *testing.T) {
	srv := statusServer(t, http.StatusOK, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Probe(ctx, NewProbeClient(), srv.URL); err == nil {
		t.Fatal("Probe with a cancelled context = nil, want an error")
	}
}

func TestTimeoutsNest(t *testing.T) {
	if PingTimeout >= ProbeTimeout || ProbeTimeout >= ingressbundle.HealthcheckTimeout {
		t.Fatalf("want PingTimeout (%s) < ProbeTimeout (%s) < HealthcheckTimeout (%s)",
			PingTimeout, ProbeTimeout, ingressbundle.HealthcheckTimeout)
	}
}

func TestProbeDefaultReadyURLComesFromTheProtoConstants(t *testing.T) {
	if got, want := DefaultReadyURL(), "http://127.0.0.1:8083/readyz"; got != want {
		t.Fatalf("DefaultReadyURL = %q, want %q", got, want)
	}
}
