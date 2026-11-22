// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"context"
	"strings"
	"testing"
)

// TestIssuedTokenCarriesItsID: the ID IssueToken returns is the ID TokenStatus reads for the current token, a
// later token has another, and the ID is not the secret.
func TestIssuedTokenCarriesItsID(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	envID := f.insertEnv(t, "swarm")

	first := f.issue(t, envID)
	if first.ID == "" {
		t.Fatal("IssueToken returned no token ID")
	}
	st, err := f.svc.TokenStatus(ctx, envID)
	if err != nil || st.ID != first.ID {
		t.Fatalf("TokenStatus ID = %q (err %v), want the issued %q", st.ID, err, first.ID)
	}
	if strings.Contains(first.Token, first.ID) {
		t.Error("the token ID appears inside the token, so it would tell something of the secret")
	}

	second := f.issue(t, envID)
	if second.ID == "" || second.ID == first.ID {
		t.Fatalf("second token ID = %q, want one different from %q", second.ID, first.ID)
	}
	st, err = f.svc.TokenStatus(ctx, envID)
	if err != nil || st.ID != second.ID {
		t.Fatalf("TokenStatus ID after a rotation = %q (err %v), want %q", st.ID, err, second.ID)
	}
}
