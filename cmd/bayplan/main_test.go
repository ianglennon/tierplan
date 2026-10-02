// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package main

import "testing"

// The image HEALTHCHECK runs `bayplan healthcheck` as root every 30 seconds.
// That command reads no data, so it must not make the privilege drop walk and
// chown /data (WR-08).
func TestHealthcheckNeverPreparesTheDataDirectory(t *testing.T) {
	t.Setenv("BAYPLAN_DATA_DIR", "/srv/bayplan-data")
	root := newRoot()

	for _, tc := range []struct {
		command string
		want    string
	}{
		{"healthcheck", ""},
		{"version", ""},
		{"serve", "/srv/bayplan-data"},
	} {
		cmd, _, err := root.Find([]string{tc.command})
		if err != nil || cmd.Name() != tc.command {
			t.Fatalf("find %s: %v", tc.command, err)
		}
		if err := cmd.ParseFlags(nil); err != nil {
			t.Fatal(err)
		}
		if got := dropOptions(cmd).DataDir; got != tc.want {
			t.Errorf("%s: DataDir = %q, want %q", tc.command, got, tc.want)
		}
	}
}
