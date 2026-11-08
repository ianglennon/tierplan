// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"log/slog"
	"strings"
	"testing"
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
