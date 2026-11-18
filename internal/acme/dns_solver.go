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
	"time"

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

// resolveChallengeTarget finds where the TXT record for name goes. The work is the engine's
// (DNSEngine.resolveChallengeTarget), shared with the hostname certificates.
func (m *Manager) resolveChallengeTarget(ctx context.Context, p dns.Provider, name string, custom []dnscheck.Server) (challengeTarget, error) {
	return m.dns.resolveChallengeTarget(ctx, p, name, custom)
}

// isDNSClass reports whether err is a *dns.Error of class.
func isDNSClass(err error, class string) bool {
	var de *dns.Error
	return errors.As(err, &de) && de.Class == class
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

// open builds the provider from the environment's token or else the saved one, once per run.
func (s *dnsSolver) open(ctx context.Context) (dns.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.p != nil {
		return s.p, nil
	}
	token, found, err := s.m.token(ctx, s.provider)
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
// is visible. A Present that fails after the record was created deletes it before it returns, because
// the order code never calls CleanUp for a Present that failed.
func (s *dnsSolver) Present(ctx context.Context, domain, token, keyAuth string) (err error) {
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
	var ref dns.RecordRef
	var row *DNSRecord // set once the ledger holds the record's ID
	defer func() {
		if err != nil && (row != nil || ref != (dns.RecordRef{})) {
			s.rollback(ctx, p, ref, t.FQDN, row)
		}
	}()

	// An order resumed after its run was cut short presents the same value again. The record it made
	// before is still there when its delete failed or was never tried, and the provider would refuse a
	// second, identical one, so the ledger row is taken over instead (RESEARCH Pitfall 6).
	pending, found, err := s.m.d.Store.PendingDNSRecord(ctx, p.Name(), t.Zone.ID, t.FQDN, value)
	if err != nil {
		return err
	}
	if found {
		// The row is written on a context a cancel cannot end: it is how the record is found and deleted.
		if err = s.m.d.Store.RestampDNSRecord(context.WithoutCancel(ctx), pending.ID, OwnerOrder, orderID, token); err != nil {
			return err
		}
		pending.OwnerKind, pending.OwnerID, pending.Token = OwnerOrder, orderID, token
		pending.Attempts, pending.LastTryAt, pending.NextTryAt, pending.LastErrorClass = 0, time.Time{}, time.Time{}, ""
		row = &pending
		s.steps.recordCreated()
	} else {
		comment := dns.RecordComment(installID, orderID)
		if ref, err = p.CreateTXT(ctx, t.Zone, t.FQDN, value, comment); err != nil {
			return err
		}
		s.steps.recordCreated()
		var id string
		if id, err = s.rowID(); err != nil {
			return err
		}
		r := DNSRecord{
			ID: id, Provider: p.Name(), ZoneID: ref.ZoneID, ZoneName: t.Zone.Name, AccountID: t.Zone.AccountID, RecordID: ref.RecordID, Name: t.FQDN, Value: value,
			Comment: comment, OwnerKind: OwnerOrder, OwnerID: orderID, Token: token, CreatedAt: s.m.clock.Now(),
		}
		// The ID is durable before the CA is asked to look (D-16). It is written on a context a cancel
		// cannot end: a run cancelled between the create and this write would otherwise leave a record
		// at the provider that no ledger row names, and nothing would ever delete it.
		if err = s.m.d.Store.InsertDNSRecord(context.WithoutCancel(ctx), r); err != nil {
			return err
		}
		row = &r
	}

	servers, hint, resolvers := s.propagationServers(t)
	s.steps.RunningWith(StepPropagation, hint, map[string]string{"host": name, "record": t.FQDN, "resolvers": resolvers})
	if err = s.m.checker.WaitVisible(ctx, t.FQDN, value, servers, dnscheck.PropagationTimeout); err != nil {
		var nv *dnscheck.NotVisibleError
		if errors.As(err, &nv) {
			err = &dns.Error{Class: dns.ClassNotVisible, Host: name, Zone: t.Zone.Name, Resolvers: nv.Servers}
		}
		return err
	}
	return nil
}

// rollback deletes the record a failed Present created, even when the run was cancelled. With a ledger
// row the delete goes through it, so a failure keeps the row for the sweep; without one (the ledger
// write itself failed) the record is deleted by the reference the create returned. A record that
// cannot be deleted is counted as left over.
func (s *dnsSolver) rollback(ctx context.Context, p dns.Provider, ref dns.RecordRef, fqdn string, row *DNSRecord) {
	var err error
	if row != nil {
		err = s.m.deleteRecord(ctx, p, *row)
	} else {
		err = p.DeleteTXT(context.WithoutCancel(ctx), ref)
	}
	if err != nil {
		s.steps.recordLeftOver()
		s.m.log.Warn("a DNS record made for a failed challenge could not be deleted", "name", fqdn, "class", deleteClass(err))
	}
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

// deleteRecord deletes the record a ledger row stands for and removes the row (DNSEngine.deleteRecord).
func (m *Manager) deleteRecord(ctx context.Context, p dns.Provider, row DNSRecord) error {
	return m.dns.deleteRecord(ctx, p, row)
}

// zoneGone reports whether a refused delete was refused because the zone is gone (DNSEngine.zoneGone).
func (m *Manager) zoneGone(ctx context.Context, p dns.Provider, row DNSRecord, delErr error) bool { //nolint:unused // kept as a three-line wrapper by D-43: the own path keeps its shape
	return m.dns.zoneGone(ctx, p, row, delErr)
}

// stampFailed records a failed delete on the row (DNSEngine.stampFailed).
func (m *Manager) stampFailed(ctx context.Context, row DNSRecord, class string) { //nolint:unused // kept as a three-line wrapper by D-43: the own path keeps its shape
	m.dns.stampFailed(ctx, row, class)
}

// deleteClass is the class a failed delete is stamped with.
func deleteClass(err error) string {
	var de *dns.Error
	if errors.As(err, &de) && de.Class != "" {
		return de.Class
	}
	return ClassInternal
}
