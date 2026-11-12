// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/proto/authn"
)

// deprecationText is the start of the line logged for a leftover TIERPLAN_TLS_SELF_SIGNED.
const deprecationText = "TIERPLAN_TLS_SELF_SIGNED is deprecated and has no effect"

// buildLogged builds a server from env and returns it with everything it logged while starting.
func buildLogged(t *testing.T, env map[string]string) (*Server, string) {
	t.Helper()
	env["TIERPLAN_DATA_DIR"] = t.TempDir() + "/data"
	var out lockedBuffer
	srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.NewTextHandler(&out, nil)))
	if err != nil {
		t.Fatalf("Build with %v: %v", env, err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, out.String()
}

func TestTLSSelfSignedIsLoggedAsDeprecated(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		t.Run(value, func(t *testing.T) {
			srv, logged := buildLogged(t, map[string]string{"TIERPLAN_TLS_SELF_SIGNED": value})
			if n := strings.Count(logged, deprecationText); n != 1 {
				t.Errorf("the deprecation line appeared %d times, want exactly once; log:\n%s", n, logged)
			}
			if srv.TLSPin() == "" || len(srv.Tunnel().Pins()) != 1 {
				t.Errorf("TIERPLAN_TLS_SELF_SIGNED=%s did not leave the self-signed certificate in place: pin %q, welcome pins %v",
					value, srv.TLSPin(), srv.Tunnel().Pins())
			}
		})
	}
}

func TestNoDeprecationLineWhenUnset(t *testing.T) {
	_, logged := buildLogged(t, map[string]string{})
	if strings.Contains(logged, "deprecated") {
		t.Errorf("a deprecation line was logged with TIERPLAN_TLS_SELF_SIGNED unset; log:\n%s", logged)
	}
}

func TestCertFilesWinOverTheDefaultAndTheDeprecatedSwitch(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	want := writePair(t, crt, key, "supplied.example.com")
	files := map[string]string{"TIERPLAN_TLS_CERT_FILE": crt, "TIERPLAN_TLS_KEY_FILE": key}

	t.Run("files with the deprecated switch set", func(t *testing.T) {
		env := map[string]string{"TIERPLAN_TLS_SELF_SIGNED": "true"}
		for k, v := range files {
			env[k] = v
		}
		srv, logged := buildLogged(t, env)
		if got := leafOf(t, srv.https.Config()); !got.Equal(want) {
			t.Error("the server did not serve the supplied certificate")
		}
		// The install keeps a self-signed identity it does not serve, and welcomes list its pin so
		// an install that drops the files strands no agent (D-21). No install command carries it.
		if pins := srv.Tunnel().Pins(); len(pins) != 1 || pins[0] == authn.SPKIPin(want) || srv.TLSPin() != "" {
			t.Errorf("welcome pins = %v, install pin %q for a supplied certificate, want the self-signed pin and no install pin", pins, srv.TLSPin())
		}
		if n := strings.Count(logged, deprecationText); n != 1 {
			t.Errorf("the deprecation line appeared %d times, want once", n)
		}
	})
	t.Run("files alone", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range files {
			env[k] = v
		}
		srv, logged := buildLogged(t, env)
		if got := leafOf(t, srv.https.Config()); !got.Equal(want) {
			t.Error("the server did not serve the supplied certificate")
		}
		// The install keeps a self-signed identity it does not serve, and welcomes list its pin so
		// an install that drops the files strands no agent (D-21). No install command carries it.
		if pins := srv.Tunnel().Pins(); len(pins) != 1 || pins[0] == authn.SPKIPin(want) || srv.TLSPin() != "" {
			t.Errorf("welcome pins = %v, install pin %q for a supplied certificate, want the self-signed pin and no install pin", pins, srv.TLSPin())
		}
		if strings.Contains(logged, "deprecated") {
			t.Error("a deprecation line was logged with the switch unset")
		}
	})
	t.Run("a key file without a cert file, or the reverse, is still refused", func(t *testing.T) {
		for _, cfg := range []config.Config{
			{DataDir: t.TempDir(), TLSKeyFile: key},
			{DataDir: t.TempDir(), TLSCertFile: crt},
			{DataDir: t.TempDir(), TLSSelfSigned: true, TLSKeyFile: key},
		} {
			if _, _, err := newCertSource(context.Background(), cfg, newMemCertStore(), nil, nil, nowFn(fixedNow)); err == nil {
				t.Errorf("newCertSource accepted %+v, want an error", cfg)
			}
		}
	})
}
