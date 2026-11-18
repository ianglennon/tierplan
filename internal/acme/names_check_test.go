// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/config"
)

func TestCheckName(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		checkPublic bool
		wantCode    string // "" means nil
		wantHost    string
	}{
		{name: "public name", host: "app.example.com", checkPublic: true},
		{name: "mixed case is canonicalised", host: "App.Example.COM", checkPublic: true},
		{name: "ip address", host: "192.0.2.1", checkPublic: true, wantCode: RefuseNamesIP, wantHost: "192.0.2.1"},
		{name: "ip address is refused without the public check too", host: "192.0.2.1", checkPublic: false, wantCode: RefuseNamesIP, wantHost: "192.0.2.1"},
		{name: "wildcard", host: "*.example.com", checkPublic: false, wantCode: RefuseNamesWildcard, wantHost: "*.example.com"},
		{name: "special-use suffix", host: "app.example.test", checkPublic: true, wantCode: RefuseNamesNotPublic, wantHost: "app.example.test"},
		{name: "public suffix itself", host: "co.uk", checkPublic: true, wantCode: RefuseNamesNotPublic, wantHost: "co.uk"},
		{name: "special-use suffix passes when the directory is overridden", host: "app.example.test", checkPublic: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckName(tc.host, tc.checkPublic)
			if tc.wantCode == "" {
				if got != nil {
					t.Fatalf("CheckName(%q, %v) = %+v, want nil", tc.host, tc.checkPublic, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("CheckName(%q, %v) = nil, want %s", tc.host, tc.checkPublic, tc.wantCode)
			}
			if got.Code != tc.wantCode || got.Host != tc.wantHost {
				t.Errorf("CheckName(%q, %v) = {%s %q}, want {%s %q}", tc.host, tc.checkPublic, got.Code, got.Host, tc.wantCode, tc.wantHost)
			}
		})
	}
}

// TestCheckNameAgreesWithWantedNames pins the D-21 claim that the second gate cannot drift from the own
// rule: for each host the two say the same thing.
func TestCheckNameAgreesWithWantedNames(t *testing.T) {
	for _, host := range []string{"app.example.com", "192.0.2.1", "*.example.com", "app.example.test", "co.uk", "localhost"} {
		for _, checkPublic := range []bool{true, false} {
			ns := WantedNames("https://"+host, "", checkPublic)
			got := CheckName(host, checkPublic)
			switch {
			case len(ns.Refusals) == 0 && got != nil:
				t.Errorf("%q public=%v: WantedNames accepts, CheckName refuses %s", host, checkPublic, got.Code)
			case len(ns.Refusals) > 0 && (got == nil || got.Code != ns.Refusals[0].Code):
				t.Errorf("%q public=%v: WantedNames refuses %s, CheckName says %v", host, checkPublic, ns.Refusals[0].Code, got)
			}
		}
	}
}

func TestNewDirectoryFor(t *testing.T) {
	override := config.Config{ACMEDirectoryURL: "https://pebble.test:14000/dir"}
	want, err := NewDirectory(override)
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}

	tests := []struct {
		name       string
		cfg        config.Config
		staging    bool
		wantURL    string
		wantOverid bool
	}{
		{name: "production by default", cfg: config.Config{}, staging: false, wantURL: LetsEncryptDirectory},
		{name: "staging by default", cfg: config.Config{}, staging: true, wantURL: LetsEncryptStagingDirectory},
		{name: "override serves production", cfg: override, staging: false, wantURL: want.URL, wantOverid: true},
		{name: "override serves staging", cfg: override, staging: true, wantURL: want.URL, wantOverid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewDirectoryFor(tc.cfg, tc.staging)
			if err != nil {
				t.Fatalf("NewDirectoryFor: %v", err)
			}
			if got.URL != tc.wantURL || got.Overridden != tc.wantOverid {
				t.Errorf("NewDirectoryFor = {%q overridden=%v}, want {%q overridden=%v}", got.URL, got.Overridden, tc.wantURL, tc.wantOverid)
			}
		})
	}

	if LetsEncryptStagingDirectory != "https://acme-staging-v02.api.letsencrypt.org/directory" {
		t.Errorf("LetsEncryptStagingDirectory = %q", LetsEncryptStagingDirectory)
	}
}
