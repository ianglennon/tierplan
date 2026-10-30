// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package auth_test

import (
	"context"
	"regexp"
	"testing"
)

// A session is addressed by a public ID that is not the hash of its cookie, so a
// list of sessions can name one for revoking without handing out anything a
// browser could present (plan 03-15, RESEARCH A9, T-03-59).

var publicIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestCreateTxGivesEverySessionADistinctThirtyTwoHexCharacterPublicID(t *testing.T) {
	f := newSessionFixture(t)
	rawA, hashA := f.create(t, false)
	rawB, hashB := f.create(t, true)

	seen := map[string]bool{}
	for _, c := range []struct {
		raw  string
		hash []byte
	}{{rawA, hashA}, {rawB, hashB}} {
		row, err := f.st.RQ.GetSessionByHash(context.Background(), c.hash)
		if err != nil {
			t.Fatal(err)
		}
		if !row.PublicID.Valid || !publicIDPattern.MatchString(row.PublicID.String) {
			t.Fatalf("public_id = %+v, want 32 lower-case hex characters", row.PublicID)
		}
		if row.PublicID.String == c.raw {
			t.Fatal("the public ID is the raw session ID")
		}
		seen[row.PublicID.String] = true
	}
	if len(seen) != 2 {
		t.Fatalf("public IDs = %v, want two distinct ones", seen)
	}
}
