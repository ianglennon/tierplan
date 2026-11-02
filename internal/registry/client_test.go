// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package registry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/regclient/regclient/types/errs"
)

// A registry that answers 302 to a plain-HTTP URL is refused, whether regclient or the probe
// meets it (T-05-38).
func TestARedirectToPlainHTTPIsRefused(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+f.host()+"/v2/", http.StatusFound)
	})
	c := f.client("bot", "x")
	if err := c.Ping(context.Background()); !errors.Is(err, errs.ErrHTTPRedirectRefused) {
		t.Errorf("Ping err = %v, want ErrHTTPRedirectRefused", err)
	}
	got := Check(context.Background(), c, f.client("", ""), CheckInput{})
	if got.Reachable != (StepResult{State: StepFailed, Code: CodeRegistryHTTP}) {
		t.Errorf("reachable = %+v, want failed registry_http", got.Reachable)
	}
}

// Setting a custom CA makes regclient rebuild the TLS settings of the transport it was given;
// the dialer must survive that, or the egress guard would be gone exactly for self-hosted
// registries (D-15).
func TestRegclientKeepsTheEgressDialerWhenItAppliesTLSSettings(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	pem := pemOf(f.srv.Certificate())
	c := NewClient(HostOptions{Host: "169.254.169.254", Username: "bot", Password: "x", CAPEM: pem}, NewEgressPolicy(nil), "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.Ping(context.Background()); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("Ping err = %v, want ErrEgressBlocked through a host with its own CA", err)
	}
	if _, err := c.Probe(context.Background()); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("Probe err = %v, want ErrEgressBlocked through a host with its own CA", err)
	}
}

func TestTheClientLogsNoRequestOrResponseHeaders(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.Level(-100)}))
	c := NewClient(HostOptions{Host: f.host(), Username: f.user, Password: f.pass}, f.egress(), "test", log)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); strings.Contains(out, "headers") || strings.Contains(out, f.pass) || strings.Contains(out, "Bearer good") {
		t.Fatalf("the log carries headers or a secret:\n%s", out)
	}
}

func TestTheClientCacheReusesAClientUntilTheCredentialIsEdited(t *testing.T) {
	cc := NewClientCache(NewEgressPolicy(nil), "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	o := HostOptions{Host: "registry.example.org:5000", Username: "bot", Password: "x"}
	a := cc.ForCredential("c1", at, o)
	if b := cc.ForCredential("c1", at, o); a != b {
		t.Error("the same credential at the same time got a new client")
	}
	if b := cc.ForCredential("c1", at.Add(time.Second), o); a == b {
		t.Error("an edited credential reused the old client")
	}
	if b := cc.ForCredential("c2", at, o); a == b {
		t.Error("another credential shared the client")
	}
	first := cc.Anonymous("ghcr.io")
	if again := cc.Anonymous("ghcr.io"); first != again || first == cc.Anonymous("quay.io") {
		t.Error("anonymous clients are not kept per host")
	}
}

// The server's registry client always talks to the upstream registry it was given, and reads
// nothing from the host: no per-host mirror list, no daemon mirror setting, no Docker config,
// certificates or credential helper (D-15, T-05-43). The names are spelled in pieces so this
// file does not trip the check it is.
func TestProductionCodeNamesNoMirrorSettingNoDockerConfigAndNoCredentialHelper(t *testing.T) {
	forbidden := regexp.MustCompile(strings.Join([]string{
		"Mirr" + "ors", "Registry" + "Config", "registry-" + "mirrors", "WithDocker" + "Creds", "WithDocker" + "Certs",
		"CredHelper",
	}, "|"))
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v, %d files", err, len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f) //nolint:gosec // G304: a Go file of this package, found by Glob
		if err != nil {
			t.Fatal(err)
		}
		if m := forbidden.Find(b); m != nil {
			t.Errorf("%s names %q", f, m)
		}
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A registry with its own certificate authority fails reachable with registry_tls until the
// credential carries that CA, and then passes every step (D-14).
func TestClientTrustsTheCredentialsOwnCAAndNoOther(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	// The default policy trusts the system roots only, so the test server's CA is unknown.
	without := NewClient(HostOptions{Host: f.host(), Username: f.user, Password: f.pass}, NewEgressPolicy(nil), "test", quietLog())
	got := Check(context.Background(), without, without, CheckInput{TestRepository: "acme/app"})
	if got.Reachable != (StepResult{State: StepFailed, Code: CodeRegistryTLS}) {
		t.Fatalf("without the CA: reachable = %+v, want failed registry_tls", got.Reachable)
	}

	opts := HostOptions{Host: f.host(), Username: f.user, Password: f.pass, CAPEM: pemOf(f.srv.Certificate())}
	with := NewClient(opts, NewEgressPolicy(nil), "test", quietLog())
	anon := NewClient(HostOptions{Host: f.host(), CAPEM: opts.CAPEM}, NewEgressPolicy(nil), "test", quietLog())
	got = Check(context.Background(), with, anon, CheckInput{TestRepository: "acme/app"})
	want := CheckResult{StepResult{State: StepPassed}, StepResult{State: StepPassed}, StepResult{State: StepPassed}}
	if got != want {
		t.Fatalf("with the CA: result = %+v, want all passed", got)
	}

	// The CA belongs to that client alone: the one that was not given it still fails.
	got = Check(context.Background(), without, without, CheckInput{})
	if got.Reachable.Code != CodeRegistryTLS {
		t.Fatalf("a client without the CA after another had it: %+v, want registry_tls", got.Reachable)
	}
}

// The switch reaches an HTTPS server whose CA nobody gave us, and a server that answers plain
// HTTP; with it off, plain HTTP is registry_http (D-14).
func TestClientInsecureSwitchReachesUnknownCAAndFallsBackToPlainHTTP(t *testing.T) {
	check := func(f *fakeRegistry, insecure bool) CheckResult {
		c := NewClient(HostOptions{Host: f.host(), Username: f.user, Password: f.pass, Insecure: insecure}, NewEgressPolicy(nil), "test", quietLog())
		a := NewClient(HostOptions{Host: f.host(), Insecure: insecure}, NewEgressPolicy(nil), "test", quietLog())
		return Check(context.Background(), c, a, CheckInput{TestRepository: "acme/app"})
	}
	allPassed := CheckResult{StepResult{State: StepPassed}, StepResult{State: StepPassed}, StepResult{State: StepPassed}}

	if got := check(newFakeRegistry(t, http.StatusUnauthorized), true); got != allPassed {
		t.Errorf("https with an unknown CA, switch on: %+v, want all passed", got)
	}
	if got := check(newFakeRegistry(t, http.StatusUnauthorized), false); got.Reachable.Code != CodeRegistryTLS {
		t.Errorf("https with an unknown CA, switch off: %+v, want registry_tls", got.Reachable)
	}
	if got := check(newPlainFakeRegistry(t, http.StatusUnauthorized), true); got != allPassed {
		t.Errorf("plain http, switch on: %+v, want all passed", got)
	}
	if got := check(newPlainFakeRegistry(t, http.StatusUnauthorized), false); got.Reachable != (StepResult{State: StepFailed, Code: CodeRegistryHTTP}) {
		t.Errorf("plain http, switch off: %+v, want failed registry_http", got.Reachable)
	}

	// A wrong password is still refused over the fallback.
	f := newPlainFakeRegistry(t, http.StatusUnauthorized)
	c := NewClient(HostOptions{Host: f.host(), Username: f.user, Password: "wrong", Insecure: true}, NewEgressPolicy(nil), "test", quietLog())
	a := NewClient(HostOptions{Host: f.host(), Insecure: true}, NewEgressPolicy(nil), "test", quietLog())
	if got := Check(context.Background(), c, a, CheckInput{}); got.SignedIn != (StepResult{State: StepFailed, Code: CodeSignInRefused}) {
		t.Errorf("plain http, wrong password: signed in = %+v, want failed sign_in_refused", got.SignedIn)
	}
}

// The plain HTTP fallback dials through the same egress policy: a blocked address is refused
// there too, and so is an operator-blocked private range (T-05-74).
func TestClientInsecureFallbackStillRefusesBlockedAddresses(t *testing.T) {
	c := NewClient(HostOptions{Host: "169.254.169.254", Insecure: true}, NewEgressPolicy(nil), "test", quietLog())
	if c.fallback == nil {
		t.Fatal("the switch built no plain HTTP client")
	}
	if _, err := c.fallback.Probe(context.Background()); !errors.Is(err, ErrEgressBlocked) {
		t.Errorf("fallback probe err = %v, want ErrEgressBlocked", err)
	}
	if _, err := c.Probe(context.Background()); !errors.Is(err, ErrEgressBlocked) {
		t.Errorf("probe err = %v, want ErrEgressBlocked", err)
	}
	if err := c.fallback.Ping(context.Background()); !errors.Is(err, ErrEgressBlocked) {
		t.Errorf("fallback ping err = %v, want ErrEgressBlocked", err)
	}

	f := newPlainFakeRegistry(t, http.StatusUnauthorized)
	fenced := NewEgressPolicy([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	d := NewClient(HostOptions{Host: f.host(), Insecure: true}, fenced, "test", quietLog())
	if _, err := d.fallback.Probe(context.Background()); !errors.Is(err, ErrEgressBlocked) {
		t.Errorf("operator-blocked range on the fallback: err = %v, want ErrEgressBlocked", err)
	}
	got := Check(context.Background(), d, d, CheckInput{})
	if got.Reachable != (StepResult{State: StepFailed, Code: CodeRegistryUnreachable}) {
		t.Errorf("operator-blocked range: reachable = %+v, want failed registry_unreachable", got.Reachable)
	}
}

// One answer in plain HTTP is not permanent: calls go straight to the plain client for a minute,
// then TLS is tried first again (WR-A03).
func TestClientPlainHTTPFallbackExpiresSoTLSIsTriedAgain(t *testing.T) {
	c := NewClient(HostOptions{Host: "registry.example.org", Insecure: true}, NewEgressPolicy(nil), "test", quietLog())
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	plainNow = func() time.Time { return now }
	t.Cleanup(func() { plainNow = time.Now })

	var asked []*Client
	call := func(cl *Client, _ context.Context) (int, error) {
		asked = append(asked, cl)
		if cl == c {
			return 0, errors.New("http: server gave HTTP response to HTTPS client")
		}
		return 1, nil
	}
	which := func() string {
		asked = nil
		if _, err := withFallback(context.Background(), c, call); err != nil {
			t.Fatal(err)
		}
		switch {
		case len(asked) == 2 && asked[0] == c && asked[1] == c.fallback:
			return "tls then plain"
		case len(asked) == 1 && asked[0] == c.fallback:
			return "plain only"
		}
		return "unexpected"
	}
	if got := which(); got != "tls then plain" {
		t.Fatalf("first call = %s", got)
	}
	now = now.Add(plainTTL - time.Second)
	if got := which(); got != "plain only" {
		t.Errorf("within the minute = %s, want plain only", got)
	}
	now = now.Add(2 * time.Second)
	if got := which(); got != "tls then plain" {
		t.Errorf("after the minute = %s, want TLS tried first again", got)
	}
}

// Without the switch there is no plain HTTP client at all.
func TestClientWithoutTheSwitchHasNoPlainHTTPFallback(t *testing.T) {
	if c := NewClient(HostOptions{Host: "registry.example.org"}, NewEgressPolicy(nil), "test", quietLog()); c.fallback != nil {
		t.Error("a client without the switch can fall back to plain HTTP")
	}
}

// A registry that streams a tag list without end is stopped at the connection, not by the 30
// second call timeout after gigabytes are in memory (WR-A01).
func TestTagListOfAnEndlessAnswerFailsAtTheSizeBound(t *testing.T) {
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/tags/list") {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(`{"name":"acme/app","tags":["`))
		chunk := []byte(strings.Repeat("a", 64<<10))
		for {
			n, err := w.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(HostOptions{Host: strings.TrimPrefix(srv.URL, "http://"), Insecure: true}, NewEgressPolicy(nil), "test", quietLog())
	_, err := c.TagList(context.Background(), "acme/app", 100)
	if err == nil {
		t.Fatal("an endless tag list was read")
	}
	if !errors.Is(err, ErrResponseTooLarge) || classify(err) != CodeRegistryHTTP {
		t.Errorf("err = %v (code %s), want ErrResponseTooLarge", err, classify(err))
	}
	// Promptness is proved by what ended the read, not by a stopwatch: had the size
	// bound not stopped it, the endless body would have run until the call timeout and
	// the error would be that deadline, not ErrResponseTooLarge.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v: the read ran into the %v call timeout instead of stopping at the size bound", err, callTimeout)
	}
	if n := sent.Load(); n > 4*responseBytesMax {
		t.Errorf("server wrote %d bytes before the client stopped, want about %d", n, responseBytesMax)
	}
}
