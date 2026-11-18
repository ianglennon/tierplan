// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dnscheck"
)

// TokenSource hands a DNSSolver the DNS token it builds its provider from: the token of one ingress
// domain's credential, opened at the moment it is wanted. The solver keeps it only long enough to build
// the provider and never logs it.
type TokenSource func(ctx context.Context) ([]byte, error)

// DNSNotes is where a DNSSolver reports what it is doing, for a job's steps. A nil DNSNotes is quiet.
type DNSNotes interface {
	// Running says a step is running, with the hint and the names the page shows for it.
	Running(step, hint string, values map[string]string)
	// RecordCreated counts a TXT record written at the provider.
	RecordCreated()
	// RecordLeftOver counts a record that could not be deleted.
	RecordLeftOver()
	// CleanupRunning says the clean-up step is running.
	CleanupRunning()
}

// DNSSolver answers DNS-01 challenges for one order over a DNSEngine: it builds its provider from a
// TokenSource, writes the TXT record, records the provider's ID for it in the ledger under CredentialRef
// before the CA is asked to look, waits until the record is visible, and deletes it by that ID
// afterwards. It is the exported twin of the own certificate's dnsSolver, which it follows step for step.
type DNSSolver struct {
	// Engine is the shared DNS-01 machinery; its ledger is the one the rows are written to.
	Engine *DNSEngine
	// Provider is the provider id the factory is looked up by.
	Provider string
	// Tokens opens the token of the credential; it is called once per solver.
	Tokens TokenSource
	// CredentialRef is stamped on every ledger row the solver writes, and the sweep builds the provider
	// that deletes the row from it.
	CredentialRef string
	// Owner returns the id of the order the records belong to.
	Owner func(ctx context.Context) (orderID string, err error)
	// Servers are the admin's resolvers, empty for the zone's own nameservers.
	Servers []dnscheck.Server
	// Notes receives the solver's progress; nil is quiet.
	Notes DNSNotes

	// newID makes a ledger row ID; nil is a UUIDv7.
	newID func() (string, error)

	mu        sync.Mutex
	p         dns.Provider
	orderID   string
	installID string
}

var _ Solver = (*DNSSolver)(nil)

// Type reports ChallengeDNS01.
func (s *DNSSolver) Type() string { return ChallengeDNS01 }

func (s *DNSSolver) running(step, hint string, values map[string]string) {
	if s.Notes != nil {
		s.Notes.Running(step, hint, values)
	}
}

func (s *DNSSolver) created() {
	if s.Notes != nil {
		s.Notes.RecordCreated()
	}
}

func (s *DNSSolver) leftOver() {
	if s.Notes != nil {
		s.Notes.RecordLeftOver()
	}
}

// open builds the provider from the credential's token, once per solver. An empty token is a token the
// provider will not accept, so it is the same class an empty one is on the own path.
func (s *DNSSolver) open(ctx context.Context) (dns.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.p != nil {
		return s.p, nil
	}
	if s.Tokens == nil {
		return nil, &dns.Error{Provider: s.Provider, Op: "open token", Class: dns.ClassTokenInvalid}
	}
	token, err := s.Tokens(ctx)
	if err != nil {
		return nil, err
	}
	if len(token) == 0 {
		return nil, &dns.Error{Provider: s.Provider, Op: "open token", Class: dns.ClassTokenInvalid}
	}
	factory, ok := s.Engine.providers[s.Provider]
	if !ok {
		return nil, fmt.Errorf("no DNS provider %q is built in", s.Provider)
	}
	s.p = factory(string(token))
	return s.p, nil
}

// owner is the order the solver's records belong to, and this install's ID, read once.
func (s *DNSSolver) owner(ctx context.Context) (orderID, installID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.orderID == "" {
		if s.Owner == nil {
			return "", "", errors.New("no order is set to own the DNS record")
		}
		id, err := s.Owner(ctx)
		if err != nil {
			return "", "", err
		}
		if id == "" {
			return "", "", errors.New("no ACME order is open to own the DNS record")
		}
		s.orderID = id
	}
	if s.installID == "" {
		id, err := s.Engine.ledger.InstallID(ctx)
		if err != nil {
			return "", "", err
		}
		s.installID = id
	}
	return s.orderID, s.installID, nil
}

func (s *DNSSolver) rowID() (string, error) {
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
func (s *DNSSolver) Present(ctx context.Context, domain, token, keyAuth string) (err error) {
	name := strings.ToLower(strings.TrimSuffix(domain, "."))
	s.running(StepChallenge, hintAddingRecord, map[string]string{"host": name, "record": dns.ChallengeFQDN(name)})
	p, err := s.open(ctx)
	if err != nil {
		return err
	}
	t, err := s.Engine.resolveChallengeTarget(ctx, p, name, s.Servers)
	if err != nil {
		return err
	}
	values := map[string]string{"host": name, "record": t.FQDN}
	hint := hintAddingRecord
	if t.DelegatedTo != "" {
		values["target"] = t.DelegatedTo
		hint = hintAddingDelegated
	}
	s.running(StepChallenge, hint, values)

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
	// second, identical one, so the ledger row is taken over instead. Only this credential's rows are
	// looked at.
	ledger := s.Engine.ledger
	pending, found, err := ledger.PendingDNSRecordFor(ctx, s.CredentialRef, p.Name(), t.Zone.ID, t.FQDN, value)
	if err != nil {
		return err
	}
	if found {
		// The row is written on a context a cancel cannot end: it is how the record is found and deleted.
		if err = ledger.RestampDNSRecord(context.WithoutCancel(ctx), pending.ID, OwnerOrder, orderID, token); err != nil {
			return err
		}
		pending.OwnerKind, pending.OwnerID, pending.Token = OwnerOrder, orderID, token
		pending.Attempts, pending.LastTryAt, pending.NextTryAt, pending.LastErrorClass = 0, time.Time{}, time.Time{}, ""
		row = &pending
		s.created()
	} else {
		comment := dns.RecordComment(installID, orderID)
		if ref, err = p.CreateTXT(ctx, t.Zone, t.FQDN, value, comment); err != nil {
			return err
		}
		s.created()
		var id string
		if id, err = s.rowID(); err != nil {
			return err
		}
		r := DNSRecord{
			ID: id, Provider: p.Name(), ZoneID: ref.ZoneID, ZoneName: t.Zone.Name, AccountID: t.Zone.AccountID, RecordID: ref.RecordID, Name: t.FQDN, Value: value,
			Comment: comment, OwnerKind: OwnerOrder, OwnerID: orderID, Token: token, CreatedAt: s.Engine.clock.Now(),
			CredentialRef: s.CredentialRef,
		}
		// The ID is durable before the CA is asked to look. It is written on a context a cancel cannot
		// end: a run cancelled between the create and this write would otherwise leave a record at the
		// provider that no ledger row names, and nothing would ever delete it.
		if err = ledger.InsertDNSRecord(context.WithoutCancel(ctx), r); err != nil {
			return err
		}
		row = &r
	}

	servers, hint, resolvers := s.propagationServers(t)
	s.running(StepPropagation, hint, map[string]string{"host": name, "record": t.FQDN, "resolvers": resolvers})
	if err = s.Engine.checker.WaitVisible(ctx, t.FQDN, value, servers, dnscheck.PropagationTimeout); err != nil {
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
func (s *DNSSolver) rollback(ctx context.Context, p dns.Provider, ref dns.RecordRef, fqdn string, row *DNSRecord) {
	var err error
	if row != nil {
		err = s.Engine.deleteRecord(ctx, p, *row)
	} else {
		err = p.DeleteTXT(context.WithoutCancel(ctx), ref)
	}
	if err != nil {
		s.leftOver()
		s.Engine.log.Warn("a DNS record made for a failed challenge could not be deleted", "name", fqdn, "class", deleteClass(err))
	}
}

// propagationServers are the servers the propagation check asks, the hint for it and the resolvers it
// names: the admin's resolvers replace the zone's own nameservers entirely.
func (s *DNSSolver) propagationServers(t challengeTarget) (servers []dnscheck.Server, hint, resolvers string) {
	if len(s.Servers) > 0 {
		names := make([]string, len(s.Servers))
		for i, sv := range s.Servers {
			names[i] = sv.Name
		}
		return s.Servers, hintPropagationCustom, strings.Join(names, ", ")
	}
	return dnscheck.Authoritative(t.Zone.NameServers), hintPropagationNS, ""
}

// CleanUp deletes the records the order wrote for token under this solver's credential, by the IDs the
// ledger holds. It reads through DNSRecordsForCredential, never the own DNSRecordsFor.
func (s *DNSSolver) CleanUp(ctx context.Context, _, token, _ string) error {
	if s.Notes != nil {
		s.Notes.CleanupRunning()
	}
	orderID, _, err := s.owner(ctx)
	if err != nil {
		return err
	}
	rows, err := s.Engine.ledger.DNSRecordsForCredential(ctx, s.CredentialRef, OwnerOrder, orderID, token)
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
		if err := s.Engine.deleteRecord(ctx, p, row); err != nil {
			s.leftOver()
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
