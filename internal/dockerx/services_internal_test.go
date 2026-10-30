// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockerx

import (
	"errors"
	"testing"
)

// The engine words a service spec naming a network it does not have as "network <name> not found" with no status
// an errdefs class matches (plan 06-37, observed on a real Engine 29), so the facade recognises the words. Nothing
// else that mentions a network or says not found is a rejection of the spec.
func TestIsMissingNetworkRecognisesTheEnginesWords(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New("Error response from daemon: network cm-no-such-network not found"), true},
		{errors.New("update service on tcp://x: Error response from daemon: network web_default not found"), true},
		{errors.New("Error response from daemon: service web not found"), false},
		{errors.New("Error response from daemon: network web_default is in use"), false},
		{errors.New("Error response from daemon: network not found"), false},
		{errors.New("connection refused"), false},
		{nil, false},
	} {
		if got := isMissingNetwork(tc.err); got != tc.want {
			t.Errorf("isMissingNetwork(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
