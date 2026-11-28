// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Spike S12 (Phase 28, SELF-04, D-12, A-01): TierPlan's own HTTP-01 challenge has to reach TierPlan
// over plain HTTP on port 80, ahead of the proxy's redirect to https. The redirect stays in the static
// file as `redirections.entryPoint` with `priority: 1`, so it is an ordinary router at the lowest
// priority and a dynamic router on `web` for the challenge path outranks it. These tests start the
// pinned Traefik with the shipped static file, not a hand-written one, so a Traefik bump or a change
// to the static file that undoes the ordering fails here (Pitfall 13).
const (
	// s12Host is TierPlan's own name, the one the carve-out is for.
	s12Host = "tierplan.example.org"
	// s12ChallengePath is a path under the ACME HTTP-01 prefix.
	s12ChallengePath = "/.well-known/acme-challenge/tok"
	// s12ReadyHost names the sentinel router that tells a test the dynamic file has been loaded. It is
	// on websecure without tls, so it answers plain HTTP on the secure entry point's port and is not
	// subject to the port-80 redirect: that matters in the control, where the redirect pre-empts every
	// request on web and a 301 on port 80 would prove nothing about the reload.
	s12ReadyHost = "ready.example.org"
	// The ports the static file opens inside the container.
	s12WebPort    = "8080"
	s12SecurePort = "8443"
	// s12PriorityLine is the exact line of the static file the control removes.
	s12PriorityLine = "          priority: 1\n"
)

// s12Doc is the dynamic file: the carve-out router on web at priority 10000 with no tls, and the
// sentinel router, both to the one plain HTTP backend.
const s12Doc = "http:\n" +
	"  routers:\n" +
	"    s12-carve:\n" +
	"      rule: Host(`" + s12Host + "`) && PathPrefix(`/.well-known/acme-challenge/`)\n" +
	"      entryPoints:\n        - web\n" +
	"      priority: 10000\n" +
	"      service: s12-plain\n" +
	"    s12-ready:\n" +
	"      rule: Host(`" + s12ReadyHost + "`)\n" +
	"      entryPoints:\n        - websecure\n" +
	"      service: s12-plain\n" +
	"  services:\n" +
	"    s12-plain:\n" +
	"      loadBalancer:\n" +
	"        servers:\n" +
	"          - url: http://s12_plain:80\n"

// s12Site is one Traefik whose dynamic file has been loaded, and the address to reach it at.
type s12Site struct {
	tr *traefiktest.Traefik
}

// startS12 starts a plain HTTP backend and a Traefik with static, applies s12Doc, and returns once the
// sentinel router answers, which is when the whole file has been read.
func startS12(t *testing.T, static []byte) *s12Site {
	t.Helper()
	env := traefiktest.NewEnv(t)
	env.StartWhoami(t, traefiktest.WhoamiOptions{Alias: "s12_plain", Hostname: "s12-plain"})
	env.Apply(t, []traefiktest.FileSpec{s1Dynamic(s12Doc)})
	s := &s12Site{tr: env.StartTraefik(t, traefiktest.TraefikOptions{Static: static})}
	testsupport.Poll(t, testsupport.Deadline, "the sentinel router to answer 200 once the dynamic file is loaded", func() (bool, string) { // testsupport: external, Traefik reloads on its own schedule
		status, _, _, err := s.get(s12SecurePort, s12ReadyHost, "/")
		if err != nil {
			return false, err.Error()
		}
		return status == http.StatusOK, "status " + strconv.Itoa(status)
	})
	return s
}

// get requests http://host<target> from the entry point at port without following a redirect. host
// reaches Traefik in the Host header only; the connection goes to the container's address.
func (s *s12Site) get(port, host, target string) (status int, location, body string, err error) {
	tp := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(s.tr.IP(), port))
		},
		DisableKeepAlives: true,
	}
	hc := &http.Client{
		Transport:     tp,
		Timeout:       5 * time.Second, // wall-clock: one request of a poll, whose own deadline is generous
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+host+target, nil)
	if err != nil {
		return 0, "", "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, "", "", err
	}
	return resp.StatusCode, resp.Header.Get("Location"), string(b), nil
}

// web is get on the port-80 entry point.
func (s *s12Site) web(t *testing.T, host, target string) (status int, location, body string) {
	t.Helper()
	status, location, body, err := s.get(s12WebPort, host, target)
	if err != nil {
		t.Fatalf("GET http://%s%s on the web entry point: %v", host, target, err)
	}
	return status, location, body
}

// TestS12CarveOutAheadOfTheRedirect proves D-12's carve-out on the pinned Traefik with the shipped
// static file (A-01): a router for TierPlan's own name and the challenge path, at priority 10000 on
// web without tls, answers from the plain backend ahead of the redirect, and the redirect still answers
// every other request on port 80 (SELF-04 must-NOT: no other plain-HTTP path is opened).
func TestS12CarveOutAheadOfTheRedirect(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	s := startS12(t, Static())

	// The challenge path on TierPlan's own name reaches the backend over plain HTTP.
	status, location, body := s.web(t, s12Host, s12ChallengePath)
	t.Logf("%s%s answered %d (Location %q)", s12Host, s12ChallengePath, status, location)
	if status != http.StatusOK || !strings.Contains(body, "Hostname: s12-plain") {
		t.Errorf("%s%s answered %d, want 200 from the plain backend:\n%s", s12Host, s12ChallengePath, status, body)
	}

	// Another host on the same path is redirected: the carve-out is for TierPlan's name only.
	status, location, _ = s.web(t, "other.example.org", s12ChallengePath)
	t.Logf("other.example.org%s answered %d (Location %q)", s12ChallengePath, status, location)
	if status != http.StatusMovedPermanently || location != "https://other.example.org"+s12ChallengePath {
		t.Errorf("another host answered %d with Location %q, want 301 to https://other.example.org%s", status, location, s12ChallengePath)
	}

	// The same host on another path is redirected: the carve-out is for the challenge prefix only.
	status, location, _ = s.web(t, s12Host, "/login")
	t.Logf("%s/login answered %d (Location %q)", s12Host, status, location)
	if status != http.StatusMovedPermanently || location != "https://"+s12Host+"/login" {
		t.Errorf("another path answered %d with Location %q, want 301 to https://%s/login", status, location, s12Host)
	}
}

// TestS12RedirectWithoutPriorityPreemptsTheCarveOut is the control (F1): with the redirect at its
// default priority, the static file as it was before A-01, the same dynamic file loses to the redirect
// and the challenge request answers 301. It shows the `priority: 1` line is what makes the carve-out
// win, so removing it fails the test above for the reason this one records.
func TestS12RedirectWithoutPriorityPreemptsTheCarveOut(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	static := Static()
	without := bytes.Replace(static, []byte(s12PriorityLine), nil, 1)
	if bytes.Equal(without, static) {
		t.Fatalf("the static file holds no line %q to remove: the control would test the shipped file", s12PriorityLine)
	}
	s := startS12(t, without)

	status, location, _ := s.web(t, s12Host, s12ChallengePath)
	t.Logf("%s%s answered %d (Location %q) with the redirect at its default priority", s12Host, s12ChallengePath, status, location)
	if status != http.StatusMovedPermanently {
		t.Errorf("with no redirect priority the carve-out answered %d, want 301: the redirect no longer pre-empts it, so priority: 1 may be unnecessary", status)
	}
}
