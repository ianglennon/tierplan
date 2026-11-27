// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import "testing"

// TestHTTP01InUseThreeInstalls: only an active Let's Encrypt configuration on HTTP-01 needs port 80. A self-signed install
// that never set Let's Encrypt up reads HTTP-01 as its challenge (F4) and is not one (A-04).
func TestHTTP01InUseThreeInstalls(t *testing.T) {
	account := &AccountView{}
	cases := []struct {
		name string
		st   Status
		want bool
	}{
		{"self-signed default", Status{}, false},
		{"self-signed default, the challenge read as the default", Status{ChallengeType: ChallengeHTTP01}, false},
		{"Let's Encrypt on DNS-01", Status{Mode: ModeActive, ChallengeType: ChallengeDNS01, Account: account}, false},
		{"Let's Encrypt on HTTP-01", Status{Mode: ModeActive, ChallengeType: ChallengeHTTP01, HasState: true}, true},
		{"Let's Encrypt on HTTP-01, an account only", Status{Mode: ModeActive, ChallengeType: ChallengeHTTP01, Account: account}, true},
		{"Let's Encrypt on HTTP-01, active but nothing set up", Status{Mode: ModeActive, ChallengeType: ChallengeHTTP01}, false},
		{"HTTP-01 with certificate files", Status{Mode: ModeActive, ChallengeType: ChallengeHTTP01, HasState: true, CertFiles: true}, false},
		{"HTTP-01 paused by a restore", Status{Mode: ModeActive, ChallengeType: ChallengeHTTP01, HasState: true, PausedReason: PausedRestore}, false},
		{"HTTP-01 switched off", Status{Mode: ModeOff, ChallengeType: ChallengeHTTP01, HasState: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HTTP01InUse(tc.st); got != tc.want {
				t.Errorf("HTTP01InUse(%+v) = %v, want %v", tc.st, got, tc.want)
			}
		})
	}
}
