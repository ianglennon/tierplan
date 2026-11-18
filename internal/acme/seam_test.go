// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/ecdsa"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
)

// memOrderStore is an OrderStore that is not *Store: one open order held in memory, and what
// StoreIssued was given. It is the shape internal/ingresscert's per-certificate store has (D-30).
type memOrderStore struct {
	mu     sync.Mutex
	open   *Order
	key    *ecdsa.PrivateKey
	calls  []string
	issued []byte
	iKey   *ecdsa.PrivateKey
	iID    string
}

func (m *memOrderStore) OpenOrder(context.Context) (Order, *ecdsa.PrivateKey, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.open == nil {
		return Order{}, nil, false, nil
	}
	return *m.open, m.key, true, nil
}

func (m *memOrderStore) InsertOrder(_ context.Context, o Order, key *ecdsa.PrivateKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "InsertOrder:"+o.ID)
	o.Status = "open"
	m.open, m.key = &o, key
	return nil
}

func (m *memOrderStore) UpdateOrder(_ context.Context, id, stage string, resumeCount int, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "UpdateOrder:"+id)
	if m.open != nil && m.open.ID == id {
		m.open.Stage, m.open.ResumeCount, m.open.UpdatedAt = stage, resumeCount, now
	}
	return nil
}

func (m *memOrderStore) CloseOrder(_ context.Context, id, _ string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "CloseOrder:"+id)
	if m.open != nil && m.open.ID == id {
		m.open = nil
	}
	return nil
}

func (m *memOrderStore) StoreIssued(_ context.Context, orderID string, chainPEM []byte, key *ecdsa.PrivateKey, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "StoreIssued:"+orderID)
	m.issued, m.iKey, m.iID = chainPEM, key, orderID
	m.open = nil
	return nil
}

// memAccountStore is an AccountStore that is not *Store: accounts by directory URL.
type memAccountStore struct {
	mu   sync.Mutex
	accs map[string]Account
	keys map[string]*ecdsa.PrivateKey
}

func newMemAccountStore() *memAccountStore {
	return &memAccountStore{accs: map[string]Account{}, keys: map[string]*ecdsa.PrivateKey{}}
}

func (m *memAccountStore) Account(_ context.Context, directoryURL string) (Account, *ecdsa.PrivateKey, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accs[directoryURL]
	if !ok {
		return Account{}, nil, false, nil
	}
	return a, m.keys[directoryURL], true, nil
}

func (m *memAccountStore) SaveAccount(_ context.Context, a Account, key *ecdsa.PrivateKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accs[a.DirectoryURL], m.keys[a.DirectoryURL] = a, key
	return nil
}

func (m *memAccountStore) ForgetAccount(_ context.Context, directoryURL string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.accs, directoryURL)
	delete(m.keys, directoryURL)
	return nil
}

// noopDNSSolver answers a dns-01 challenge by doing nothing: the fake CA marks dns-01 valid without
// looking when it has no LookupTXT.
type noopDNSSolver struct{}

func (noopDNSSolver) Type() string                                          { return ChallengeDNS01 }
func (noopDNSSolver) Present(context.Context, string, string, string) error { return nil }
func (noopDNSSolver) CleanUp(context.Context, string, string, string) error { return nil }

// TestSeamOrdererIssuesThroughStoresThatAreNotTheOwnStore is the Phase 20 tracer (D-30): a real ACME
// order runs to an issued certificate against the fake CA through an Orderer and a Client whose
// stores are not *Store, which is what the per-hostname manager does.
func TestSeamOrdererIssuesThroughStoresThatAreNotTheOwnStore(t *testing.T) {
	ctx := context.Background()
	clock := acmetest.NewClock(testNow)
	fake := acmetest.NewFake(t, acmetest.Options{Clock: clock, ChallengeType: ChallengeDNS01})
	dir := Directory{URL: fake.DirectoryURL(), Roots: fake.RootPool(), Overridden: true}

	accounts := newMemAccountStore()
	orders := &memOrderStore{}
	client := NewClient(dir, accounts, clock, "tierplan-test/0")
	if _, err := client.Register(ctx, "", true); err != nil {
		t.Fatalf("Register: %v", err)
	}

	issued, err := NewOrderer(client, orders, noopDNSSolver{}, clock).Run(ctx,
		OrderRequest{Kind: KindIssue, Names: []string{"app.example.com"}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := issued.Leaf.DNSNames; !slices.Equal(got, []string{"app.example.com"}) {
		t.Errorf("leaf DNSNames = %v, want [app.example.com]", got)
	}
	if len(orders.calls) < 2 || orders.calls[0] != "InsertOrder:"+issued.OrderID ||
		orders.calls[len(orders.calls)-1] != "StoreIssued:"+issued.OrderID {
		t.Errorf("order store calls = %v, want InsertOrder first and StoreIssued last for %q", orders.calls, issued.OrderID)
	}
	if orders.iID != issued.OrderID || len(orders.issued) == 0 || orders.iKey == nil {
		t.Errorf("StoreIssued kept id %q, %d chain bytes, key %v", orders.iID, len(orders.issued), orders.iKey != nil)
	}
	if _, _, found, _ := accounts.Account(ctx, fake.DirectoryURL()); !found || len(accounts.accs) != 1 {
		t.Errorf("account store holds %d accounts, found for the fake = %v; want exactly one for the fake", len(accounts.accs), found)
	}
	if n := len(fake.Orders()); n != 1 {
		t.Errorf("fake saw %d orders, want 1", n)
	}
}
