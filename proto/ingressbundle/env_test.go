// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import "testing"

// TestEnvNames pins the exact strings: the deployer writes them into a service and the supervisor reads
// them, so a renamed constant must fail here rather than at a customer's node.
func TestEnvNames(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"EnvURL", EnvURL, "TIERPLAN_INGRESS_URL"},
		{"EnvPin", EnvPin, "TIERPLAN_INGRESS_PIN"},
		{"EnvJoinTokenFile", EnvJoinTokenFile, "TIERPLAN_INGRESS_JOIN_TOKEN_FILE"},
		{"EnvJoinToken", EnvJoinToken, "TIERPLAN_INGRESS_JOIN_TOKEN"},
		{"EnvNodeID", EnvNodeID, "TIERPLAN_INGRESS_NODE_ID"},
		{"EnvAccessLog", EnvAccessLog, "TIERPLAN_INGRESS_ACCESS_LOG"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
