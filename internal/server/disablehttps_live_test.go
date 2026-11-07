// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

const liveHTTPSURL = "https://tierplan.example.com:8443"

// liveServer builds a server from env whose settings watcher waits on a clock the test moves, with a
// data directory of its own. A second store handle on that directory stands in for the CLI process.
func liveServer(t *testing.T, env map[string]string) (*Server, *stepClock, *store.Store, *audit.Writer) {
	t.Helper()
	if env == nil {
		env = map[string]string{}
	}
	env["TIERPLAN_DATA_DIR"] = t.TempDir() + "/data"
	cfg := testConfig(t, env)
	clock := newStepClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler), WithSettingsClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	st2, err := store.Open(context.Background(), cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	v, err := vault.Load(cfg.EncryptionKey, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	return srv, clock, st2, audit.NewWriter(st2, v, slog.New(slog.DiscardHandler), nil)
}

// seedHTTPSPublicURL puts the https public URL where an operator's setup leaves it: in the settings row
// and in the running server's own cache. Both are needed. DisableHTTPS reads the row, so Apply alone
// would make it report nothing to change; and the watcher's first poll would put the empty row value
// back into the cache.
func seedHTTPSPublicURL(t *testing.T, srv *Server, st *store.Store) {
	t.Helper()
	if err := st.Q.UpsertSetting(context.Background(), sqlc.UpsertSettingParams{
		Key: instance.SettingPublicURL, Value: liveHTTPSURL, UpdatedAt: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	srv.Instance().Apply(srv.Instance().Name(), liveHTTPSURL)
}

func settingsUpdatedRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Read.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'settings.updated'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func plainGet(srv *Server, path string) *httptest.ResponseRecorder {
	return plainFrom(srv, http.MethodGet, path, nonLoopbackPeer, nil)
}

// pollOnce moves the settings clock by one poll interval and waits until the watcher has applied what
// it read and armed its next wait, so the change is visible when it returns.
func pollOnce(t *testing.T, clock *stepClock) {
	t.Helper()
	clock.Tick(t, instance.SettingsPollInterval)
	clock.WaitArmed(t)
}

func TestDisableHTTPSLiveStopsTheRedirectOnARuntimeTLSInstall(t *testing.T) {
	srv, clock, st2, w := liveServer(t, nil)
	seedHTTPSPublicURL(t, srv, st2)
	installSuppliedCert(t, srv)

	if rec := plainGet(srv, "/api/version"); rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("before the flip: status = %d, want 307", rec.Code)
	}

	res, err := instance.DisableHTTPS(context.Background(), st2, srv.cfg, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || res.From != liveHTTPSURL {
		t.Fatalf("DisableHTTPS = %+v, want one change from %s (proof it read the seeded row)", res, liveHTTPSURL)
	}

	// The CLI has written, but the clock has not moved: the server has not looked yet.
	if rec := plainGet(srv, "/api/version"); rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("before the poll: status = %d, want 307", rec.Code)
	}

	pollOnce(t, clock)

	if rec := plainGet(srv, "/api/version"); rec.Code != http.StatusOK {
		t.Fatalf("after the poll: status = %d (Location %q), want 200", rec.Code, rec.Header().Get("Location"))
	}
	if got, want := srv.Instance().PublicURL(), "http://tierplan.example.com:8443"; got != want {
		t.Errorf("PublicURL = %q, want %q", got, want)
	}
	if got, want := auth.CookieName(srv.Instance().CookieScope()), "tierplan_session"; got != want {
		t.Errorf("session cookie = %q, want %q", got, want)
	}
}

func TestDisableHTTPSLiveLeavesAnEnvSelfSignedInstallUnchanged(t *testing.T) {
	srv, clock, st2, w := liveServer(t, map[string]string{"TIERPLAN_TLS_SELF_SIGNED": "true"})
	seedHTTPSPublicURL(t, srv, st2)
	if !srv.https.Live() {
		t.Fatal("TLS is not live")
	}
	if rec := plainGet(srv, "/api/version"); rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("before: status = %d, want 307", rec.Code)
	}
	pin := srv.TLSPin()
	if pin == "" {
		t.Fatal("no self-signed pin published")
	}

	// docker exec hands the command the container's environment, so its config has the variable set.
	cliCfg := testConfig(t, map[string]string{"TIERPLAN_TLS_SELF_SIGNED": "true", "TIERPLAN_DATA_DIR": srv.cfg.DataDir})
	res, err := instance.DisableHTTPS(context.Background(), st2, cliCfg, w)
	if !errors.Is(err, instance.ErrTLSSetByEnv) {
		t.Fatalf("err = %v, want ErrTLSSetByEnv", err)
	}
	notes := strings.Join(res.Notes, "\n")
	for _, want := range []string{"TIERPLAN_TLS_SELF_SIGNED", "The way in is " + liveHTTPSURL} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes %q lack %q", notes, want)
		}
	}
	if n := settingsUpdatedRows(t, st2); n != 0 {
		t.Fatalf("%d settings.updated rows, want 0", n)
	}

	pollOnce(t, clock)

	if rec := plainGet(srv, "/api/version"); rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("after the poll: plain status = %d, want 307 still", rec.Code)
	}
	if got := srv.Instance().PublicURL(); got != liveHTTPSURL {
		t.Errorf("PublicURL = %q, want %q unchanged", got, liveHTTPSURL)
	}
	overTLS := func(r *http.Request) { r.TLS = &tls.ConnectionState{} }
	if rec := plainFrom(srv, http.MethodGet, "/api/version", nonLoopbackPeer, overTLS); rec.Code != http.StatusOK {
		t.Errorf("over TLS: status = %d, want 200", rec.Code)
	}
	if rec := plainGet(srv, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", rec.Code)
	}
	if got := srv.TLSPin(); got != pin {
		t.Errorf("TLSPin = %q, want %q unchanged", got, pin)
	}
}
