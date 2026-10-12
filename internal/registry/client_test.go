// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	c := NewClient(HostOptions{Host: "169.254.169.254", Username: "bot", Password: "x", caPEM: pem}, NewEgressPolicy(nil), "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
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
