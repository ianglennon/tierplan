// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dnscheck"
)

// DNSDeps is what the DNS-01 side of a Manager works with. Nil fields take the production defaults.
type DNSDeps struct {
	// Providers are the DNS providers a challenge can be answered through, by provider id.
	Providers map[string]dns.Factory
	// Querier sends the DNS questions of the propagation check and the CNAME probe.
	Querier dnscheck.Querier
	// LookupIP, NameserverAddrs and ResolvConf are the checker's seams (dnscheck.Checker): a test points
	// nameserver names at local responders with them.
	LookupIP        func(ctx context.Context, host string) ([]netip.Addr, error)
	NameserverAddrs func(ctx context.Context, host string) ([]string, error)
	ResolvConf      func() ([]string, error)
}

// defaultDNSProvider is the provider a DNS-01 run uses when none is saved.
const defaultDNSProvider = "cloudflare"

// The fixed sentences the DNS steps show while they run. They name nothing the DNS provider or the CA
// said; the names the page shows go in the item's values.
const (
	hintAddingRecord      = "Adding the TXT record at your DNS provider."
	hintAddingDelegated   = "Adding the TXT record at your DNS provider, where the challenge name is delegated to."
	hintPropagationNS     = "Waiting for the TXT record to show on the zone's name servers."
	hintPropagationCustom = "Waiting for the TXT record to show on the resolvers you listed."
	hintCleanup           = "Removing the TXT records TierPlan added."
)

// challengeTarget is where the TXT record for one certificate name is written.
type challengeTarget struct {
	// Name is the certificate name the CA is validating.
	Name string
	// FQDN is the name the TXT record is written at: _acme-challenge.<Name>, or the name its CNAME
	// delegates to.
	FQDN string
	// Zone is the provider zone FQDN is written in.
	Zone dns.Zone
	// DelegatedTo is the name _acme-challenge.<Name> is a CNAME to, empty when it is not delegated.
	DelegatedTo string
	// OwnZone is the name of the zone Name itself is in, empty when the provider holds no such zone.
	OwnZone string
}

// resolveChallengeTarget finds where the TXT record for name goes: the zone of
// _acme-challenge.<name> on the provider, and the name any CNAME there delegates to.
func (m *Manager) resolveChallengeTarget(ctx context.Context, p dns.Provider, name string, _ []dnscheck.Server) (challengeTarget, error) {
	fqdn := dns.ChallengeFQDN(name)
	t := challengeTarget{Name: name, FQDN: fqdn}
	zone, err := p.Zone(ctx, fqdn)
	if err != nil {
		return t, err
	}
	t.Zone, t.OwnZone = zone, zone.Name
	final, delegated, err := m.checker.FollowCNAME(ctx, fqdn, dnscheck.Authoritative(zone.NameServers))
	if err != nil && !errors.Is(err, dnscheck.ErrNoAnswer) {
		return t, err
	}
	if delegated {
		tz, err := p.Zone(ctx, final)
		if err != nil {
			return t, err
		}
		t.FQDN, t.Zone, t.DelegatedTo = final, tz, final
	}
	return t, nil
}

// dnsSolver answers DNS-01 challenges for one run: it writes the TXT record through the provider,
// records the provider's ID for it in the ledger before the CA is asked to look, waits until the
// record is visible, and deletes it by that ID afterwards.
type dnsSolver struct {
	m     *Manager
	steps *jobSteps
	// provider is the provider id; servers are the admin's resolvers, empty for the zone's own
	// nameservers.
	provider string
	servers  []dnscheck.Server
	// newID makes a ledger row ID; nil is a UUIDv7.
	newID func() (string, error)

	mu        sync.Mutex
	p         dns.Provider
	orderID   string
	installID string
}

var _ Solver = (*dnsSolver)(nil)

// Type reports ChallengeDNS01.
func (s *dnsSolver) Type() string { return ChallengeDNS01 }

// open builds the provider from the saved token, once per run.
func (s *dnsSolver) open(ctx context.Context) (dns.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.p != nil {
		return s.p, nil
	}
	token, found, err := s.m.d.Store.DNSToken(ctx, s.provider)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &dns.Error{Provider: s.provider, Op: "open token", Class: dns.ClassTokenInvalid}
	}
	factory, ok := s.m.d.DNS.Providers[s.provider]
	if !ok {
		return nil, fmt.Errorf("no DNS provider %q is built in", s.provider)
	}
	s.p = factory(token)
	return s.p, nil
}

// owner is the order the run's records belong to, and this install's ID, read once.
func (s *dnsSolver) owner(ctx context.Context) (orderID, installID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.orderID == "" {
		id, found, err := s.m.d.Store.OpenOrderID(ctx)
		if err != nil {
			return "", "", err
		}
		if !found {
			return "", "", errors.New("no ACME order is open to own the DNS record")
		}
		s.orderID = id
	}
	if s.installID == "" {
		id, err := s.m.d.Store.InstallID(ctx)
		if err != nil {
			return "", "", err
		}
		s.installID = id
	}
	return s.orderID, s.installID, nil
}

func (s *dnsSolver) rowID() (string, error) {
	if s.newID != nil {
		return s.newID()
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("make a DNS record ID: %w", err)
	}
	return id.String(), nil
}

// Present writes the TXT record that answers the challenge for domain and does not return before it
// is visible.
func (s *dnsSolver) Present(ctx context.Context, domain, token, keyAuth string) error {
	name := strings.ToLower(strings.TrimSuffix(domain, "."))
	s.steps.RunningWith(StepChallenge, hintAddingRecord, map[string]string{"host": name, "record": dns.ChallengeFQDN(name)})
	p, err := s.open(ctx)
	if err != nil {
		return err
	}
	t, err := s.m.resolveChallengeTarget(ctx, p, name, s.servers)
	if err != nil {
		return err
	}
	values := map[string]string{"host": name, "record": t.FQDN}
	hint := hintAddingRecord
	if t.DelegatedTo != "" {
		values["target"] = t.DelegatedTo
		hint = hintAddingDelegated
	}
	s.steps.RunningWith(StepChallenge, hint, values)

	orderID, installID, err := s.owner(ctx)
	if err != nil {
		return err
	}
	value := dns.TXTValue(keyAuth)
	comment := dns.RecordComment(installID, orderID)
	ref, err := p.CreateTXT(ctx, t.Zone, t.FQDN, value, comment)
	if err != nil {
		return err
	}
	s.steps.recordCreated()
	id, err := s.rowID()
	if err != nil {
		return err
	}
	row := DNSRecord{
		ID: id, Provider: p.Name(), ZoneID: ref.ZoneID, RecordID: ref.RecordID, Name: t.FQDN, Value: value,
		Comment: comment, OwnerKind: OwnerOrder, OwnerID: orderID, Token: token, CreatedAt: s.m.clock.Now(),
	}
	// The ID is durable before the CA is asked to look (D-16).
	if err := s.m.d.Store.InsertDNSRecord(ctx, row); err != nil {
		return err
	}

	servers, hint, resolvers := s.propagationServers(t)
	s.steps.RunningWith(StepPropagation, hint, map[string]string{"host": name, "record": t.FQDN, "resolvers": resolvers})
	if err := s.m.checker.WaitVisible(ctx, t.FQDN, value, servers, dnscheck.PropagationTimeout); err != nil {
		var nv *dnscheck.NotVisibleError
		if errors.As(err, &nv) {
			return &dns.Error{Class: dns.ClassNotVisible, Host: name, Zone: t.Zone.Name, Resolvers: nv.Servers}
		}
		return err
	}
	return nil
}

// propagationServers are the servers the propagation check asks, the hint for it and the resolvers it
// names: the admin's resolvers replace the zone's own nameservers entirely (D-12).
func (s *dnsSolver) propagationServers(t challengeTarget) (servers []dnscheck.Server, hint, resolvers string) {
	if len(s.servers) > 0 {
		names := make([]string, len(s.servers))
		for i, sv := range s.servers {
			names[i] = sv.Name
		}
		return s.servers, hintPropagationCustom, strings.Join(names, ", ")
	}
	return dnscheck.Authoritative(t.Zone.NameServers), hintPropagationNS, ""
}

// CleanUp deletes the records the run wrote for token, by the IDs the ledger holds.
func (s *dnsSolver) CleanUp(ctx context.Context, _, token, _ string) error {
	s.steps.cleanupRunning()
	orderID, _, err := s.owner(ctx)
	if err != nil {
		return err
	}
	rows, err := s.m.d.Store.DNSRecordsFor(ctx, OwnerOrder, orderID, token)
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		p, err := s.open(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.m.deleteRecord(ctx, p, row); err != nil {
			s.steps.recordLeftOver()
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deleteRecord deletes the record a ledger row stands for, by the provider's ID for it and nothing
// else, and removes the row. A failed delete leaves the row, stamped with the class of the failure and
// when the next try is due.
func (m *Manager) deleteRecord(ctx context.Context, p dns.Provider, row DNSRecord) error {
	bg := context.WithoutCancel(ctx)
	if err := p.DeleteTXT(bg, dns.RecordRef{ZoneID: row.ZoneID, RecordID: row.RecordID}); err != nil {
		m.stampFailed(bg, row, deleteClass(err))
		return err
	}
	return m.d.Store.DeleteDNSRecord(bg, row.ID)
}

// stampFailed records a failed delete on the row: one more attempt, and the next try on the backoff
// schedule.
func (m *Manager) stampFailed(ctx context.Context, row DNSRecord, class string) {
	now := m.clock.Now()
	attempts := row.Attempts + 1
	next := now.Add(BackoffDelay(attempts, 0, m.d.Jitter()))
	if err := m.d.Store.MarkDNSRecordFailed(ctx, row.ID, attempts, now, next, class); err != nil {
		m.log.Error("record a failed DNS record delete", "name", row.Name, "error", err)
	}
}

// deleteClass is the class a failed delete is stamped with.
func deleteClass(err error) string {
	var de *dns.Error
	if errors.As(err, &de) && de.Class != "" {
		return de.Class
	}
	return ClassInternal
}
