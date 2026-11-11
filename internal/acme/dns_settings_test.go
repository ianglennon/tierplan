// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme/dns"
)

// forgetToken removes the token the environment saved, so a test starts from an install with none.
func (e *dnsEnv) forgetToken() {
	e.t.Helper()
	if err := e.store.DeleteDNSToken(context.Background(), dnsProviderID); err != nil {
		e.t.Fatalf("DeleteDNSToken: %v", err)
	}
}

// savedToken is the token the store holds for the fake provider, "" when none.
func (e *dnsEnv) savedToken() string {
	e.t.Helper()
	tok, _, err := e.store.DNSToken(context.Background(), dnsProviderID)
	if err != nil {
		e.t.Fatalf("DNSToken: %v", err)
	}
	return tok
}

func TestSaveValidationTestsANewTokenAndSealsIt(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeHTTP01)
	e.forgetToken()
	before := counts(e.mgrEnv)

	res, err := e.mgr.SaveValidation(ctx, ValidationInput{ChallengeType: ChallengeDNS01, Token: dnsToken})
	if err != nil {
		t.Fatalf("SaveValidation: %v", err)
	}

	if !res.Saved || res.Test == nil || !res.Test.Passed {
		t.Fatalf("result = %+v, want saved after a passed test", res)
	}
	if len(res.Test.Names) != 1 {
		t.Fatalf("tested %d names, want the public host alone", len(res.Test.Names))
	}
	n := res.Test.Names[0]
	if n.Name != testHost || n.Record != dns.ChallengeFQDN(testHost) || n.Zone != "example.com" {
		t.Errorf("name test = %+v, want %s at %s in example.com", n, testHost, dns.ChallengeFQDN(testHost))
	}
	for i, id := range []string{TestStepZone, TestStepCreated, TestStepRead, TestStepDeleted} {
		if s := n.Steps[i]; s.ID != id || s.State != TestDone {
			t.Errorf("step %d = %+v, want %s done", i, s, id)
		}
	}
	if n.Info.State != InfoSeen || n.Info.Seconds != 0 {
		t.Errorf("info = %+v, want seen after 0 s", n.Info)
	}
	if n.Failure != nil {
		t.Errorf("failure = %+v, want none", n.Failure)
	}

	if got := e.savedToken(); got != dnsToken {
		t.Errorf("saved token = %q, want the typed one", got)
	}
	st := e.state()
	if st.ChallengeType != ChallengeDNS01 || !st.ValidationUntested {
		t.Errorf("state = challenge %q, untested %v, want dns-01 and untested", st.ChallengeType, st.ValidationUntested)
	}
	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v after the test", recs)
	}
	if rows := e.ledger(); len(rows) != 0 {
		t.Errorf("the ledger still holds %+v after the test", rows)
	}
	e.noCARequests(before)
}
