// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dnscheck"
)

// The rows of one name's token test, in the fixed order they are listed in.
const (
	TestStepZone    = "zone"
	TestStepCreated = "created"
	TestStepRead    = "read"
	TestStepDeleted = "deleted"
)

// The states of a test row.
const (
	TestDone    = "done"
	TestFailed  = "failed"
	TestSkipped = "skipped"
)

// The states of the info row, which never changes the verdict (D-09).
const (
	// InfoSeen: every resolver asked showed the test record.
	InfoSeen = "seen"
	// InfoNotSeen: some resolver had not shown it when the wait ended.
	InfoNotSeen = "not_seen"
	// InfoNotChecked: the record was never looked for, because an earlier step failed or there was no
	// resolver to ask.
	InfoNotChecked = "not_checked"
)

// testValuePrefix marks the value of a test record as TierPlan's own.
const testValuePrefix = "tierplan-test-"

// TokenTest is the result of testing a DNS token against the certificate names. It carries no token.
type TokenTest struct {
	// Passed is true when every name passed all four rows. The info row has no say.
	Passed bool
	// Names are the names tested, in certificate-name order (the public host first).
	Names []NameTest
}

// NameTest is the test of one certificate name.
type NameTest struct {
	Name string
	// Record is the name the test record was written at (or would have been): _acme-challenge.<Name>, or
	// the name its CNAME delegates to.
	Record string
	// Zone is the provider zone Record is in, the target's when the name is delegated.
	Zone string
	// DelegatedTo is the name _acme-challenge.<Name> is a CNAME to, empty when it is not delegated.
	DelegatedTo string
	// OwnZone is set only when the name's own zone is not on the provider and the challenge name is
	// delegated into one that is (D-15): the name of the zone that is not there.
	OwnZone string
	// Steps are the four rows, in the order zone, created, read, deleted.
	Steps []TestStep
	Info  TestInfo
	// Failure classifies the first step that failed, nil when none did.
	Failure *Failure
}

// TestStep is one row of a name's test.
type TestStep struct {
	ID    string
	State string
	// Class is the class of a failed step.
	Class string
}

// TestInfo is the non-blocking row: whether the resolvers showed the test record.
type TestInfo struct {
	State string
	// Resolvers are the servers asked (seen), or the ones that had not shown the record (not_seen).
	Resolvers []string
	// Seconds is how long the wait took on the clock, at most dnscheck.TestObserveLimit.
	Seconds int
}

// passed reports whether all four rows are done.
func (n NameTest) passed() bool {
	for _, s := range n.Steps {
		if s.State != TestDone {
			return false
		}
	}
	return true
}

// newNameTest is a test of name with nothing done: every row skipped and the info row not checked.
func newNameTest(name string) NameTest {
	return NameTest{
		Name: name, Record: dns.ChallengeFQDN(name),
		Steps: []TestStep{
			{ID: TestStepZone, State: TestSkipped}, {ID: TestStepCreated, State: TestSkipped},
			{ID: TestStepRead, State: TestSkipped}, {ID: TestStepDeleted, State: TestSkipped},
		},
		Info: TestInfo{State: InfoNotChecked},
	}
}

// fail ends row i as failed with the class of err. Only the first failure is kept as the name's
// Failure; a later row that fails still shows its own class.
func (n *NameTest) fail(i int, err error) {
	f := Classify(err, ClassifyInput{ChallengeType: ChallengeDNS01})
	if f.Host == "" {
		f.Host = n.Name
	}
	n.Steps[i] = TestStep{ID: n.Steps[i].ID, State: TestFailed, Class: f.Class}
	if n.Failure == nil {
		n.Failure = &f
	}
}

func (n *NameTest) done(i int) { n.Steps[i].State = TestDone }

// invalidTokenTest is the test of a name for a token that cannot be a bearer token: it fails the first
// row as dns_token_invalid, and no request is made.
func invalidTokenTest(name string) NameTest {
	n := newNameTest(name)
	n.fail(0, &dns.Error{Provider: defaultDNSProvider, Op: "check token", Class: dns.ClassTokenInvalid, Host: name})
	return n
}

// testName tests one certificate name with p: it finds where the challenge record goes the way an
// order does (following a CNAME delegation), writes a test TXT record there, reads it back by the
// provider's ID, notes whether the resolvers show it, and deletes it. The record is in the ledger from
// the moment it exists, so a delete that fails leaves it for the sweep. Pass or fail comes from the four
// provider rows alone; the info row only reports. A row that fails does not stop the delete.
func (m *Manager) testName(ctx context.Context, p dns.Provider, name string, custom []dnscheck.Server, installID string) NameTest {
	nt := newNameTest(name)

	t, err := m.resolveChallengeTarget(ctx, p, name, custom)
	if err != nil {
		nt.fail(0, err)
		return nt
	}
	nt.Zone, nt.Record, nt.DelegatedTo = t.Zone.Name, t.FQDN, t.DelegatedTo
	if t.DelegatedTo != "" && t.OwnZone == "" {
		nt.OwnZone = ownZoneOf(name)
	}
	nt.done(0)

	testID := "test-" + randHex(4)
	value := testValuePrefix + randHex(8)
	comment := dns.RecordComment(installID, testID)
	ref, err := p.CreateTXT(ctx, t.Zone, t.FQDN, value, comment)
	if err != nil {
		nt.fail(1, err)
		return nt
	}
	row, err := m.trackTestRecord(ctx, p, ref, t, value, comment, testID)
	if err != nil {
		// The record exists but the ledger cannot name it, so it is deleted by the reference at once.
		if derr := p.DeleteTXT(context.WithoutCancel(ctx), ref); derr != nil {
			m.log.Warn("a test DNS record could not be deleted and is not in the ledger", "name", t.FQDN, "class", deleteClass(derr))
		}
		nt.fail(1, err)
		return nt
	}
	nt.done(1)

	rec, err := p.GetTXT(ctx, ref)
	if err == nil && rec.Content != value {
		err = &dns.Error{Provider: p.Name(), Op: "read record", Class: dns.ClassUnavailable}
	}
	if err != nil {
		nt.fail(2, err)
	} else {
		nt.done(2)
		nt.Info = m.observeTestRecord(ctx, t, custom, value)
	}

	// Even a failed row ends in a delete: the record must not outlive the test.
	if err := m.deleteRecord(ctx, p, row); err != nil {
		nt.fail(3, err)
	} else {
		nt.done(3)
	}
	return nt
}

// trackTestRecord writes the ledger row of a test record the provider just created.
func (m *Manager) trackTestRecord(ctx context.Context, p dns.Provider, ref dns.RecordRef, t challengeTarget, value, comment, testID string) (DNSRecord, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return DNSRecord{}, err
	}
	row := DNSRecord{
		ID: id.String(), Provider: p.Name(), ZoneID: ref.ZoneID, ZoneName: t.Zone.Name, RecordID: ref.RecordID, Name: t.FQDN, Value: value,
		Comment: comment, OwnerKind: OwnerTest, OwnerID: testID, CreatedAt: m.clock.Now(),
	}
	// Written on a context a cancel cannot end (a browser that disconnects cancels the request): the
	// record exists, and the ledger is the only way it is deleted if the delete after the test fails.
	return row, m.d.Store.InsertDNSRecord(context.WithoutCancel(ctx), row)
}

// observeTestRecord asks the propagation servers, the admin's resolvers when set else the zone's own
// nameservers, whether the test record shows, for at most dnscheck.TestObserveLimit on the clock.
func (m *Manager) observeTestRecord(ctx context.Context, t challengeTarget, custom []dnscheck.Server, value string) TestInfo {
	servers := custom
	if len(servers) == 0 {
		servers = dnscheck.Authoritative(t.Zone.NameServers)
	}
	if len(servers) == 0 {
		return TestInfo{State: InfoNotChecked}
	}
	obs := m.checker.Observe(ctx, t.FQDN, value, servers, dnscheck.TestObserveLimit)
	info := TestInfo{State: InfoNotSeen, Resolvers: obs.Servers, Seconds: min(int(obs.After/time.Second), int(dnscheck.TestObserveLimit/time.Second))}
	if obs.Seen {
		info.State = InfoSeen
	}
	return info
}

// ownZoneOf is the zone a name sits in when it is not on the provider: its registrable domain.
func ownZoneOf(name string) string {
	if z, err := publicsuffix.EffectiveTLDPlusOne(name); err == nil {
		return z
	}
	return name
}

// randHex is 2n random hex digits.
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails: crypto/rand.Read stops the program instead
	return hex.EncodeToString(b)
}
