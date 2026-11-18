// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"log/slog"
	mrand "math/rand/v2"
	"strings"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/cloudflare"
	"github.com/ianglennon/tierplan/internal/acme/dnscheck"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// DNSEngine is the DNS-01 machinery a Manager and a hostname certificate's order share: finding where a
// challenge record goes, deleting a record by the ID the ledger holds, deciding when a failed delete is
// really a zone that is gone, stamping a failed delete for a retry, and sweeping the ledger. It holds no
// token and opens no credential: the caller hands it a provider.
type DNSEngine struct {
	checker   *dnscheck.Checker
	clock     stackops.Clock
	log       *slog.Logger
	jitter    func() float64
	providers map[string]dns.Factory
	ledger    DNSLedger
}

// DNSEngineDeps is what NewDNSEngine builds an engine from. Nil fields take the defaults NewManager
// applies.
type DNSEngineDeps struct {
	// DNS carries the providers and the checker's seams; a nil Querier and a nil Providers take the
	// production defaults.
	DNS DNSDeps
	// Ledger is the record-ID ledger the engine deletes through.
	Ledger DNSLedger
	// Clock is the engine's clock; nil is the real one.
	Clock stackops.Clock
	// Log is where the engine logs; nil is the default logger.
	Log *slog.Logger
	// Jitter is the backoff jitter in [-0.1, 0.1]; nil is math/rand/v2.
	Jitter func() float64
}

// NewDNSEngine is an engine over d.
func NewDNSEngine(d DNSEngineDeps) *DNSEngine {
	if d.Clock == nil {
		d.Clock = stackops.RealClock()
	}
	if d.Jitter == nil {
		d.Jitter = func() float64 { return (mrand.Float64()*2 - 1) * maxJitter } //nolint:gosec // G404: spreads the retry time of one record delete; it is not a secret and needs no cryptographic randomness
	}
	if d.DNS.Providers == nil {
		d.DNS.Providers = map[string]dns.Factory{defaultDNSProvider: cloudflare.New(cloudflare.Options{UserAgent: "TierPlan"})}
	}
	if d.DNS.Querier == nil {
		d.DNS.Querier = dnscheck.NewQuerier()
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &DNSEngine{
		checker: &dnscheck.Checker{
			Q: d.DNS.Querier, Clock: d.Clock, LookupIP: d.DNS.LookupIP,
			NameserverAddrs: d.DNS.NameserverAddrs, ResolvConf: d.DNS.ResolvConf,
		},
		clock: d.Clock, log: log, jitter: d.Jitter, providers: d.DNS.Providers, ledger: d.Ledger,
	}
}

// Provider builds the provider id for token, with the same defaults the engine's own deletes use. ok is
// false for an id no provider is built in under. The token is held by the provider returned and nowhere
// else.
func (e *DNSEngine) Provider(id, token string) (p dns.Provider, ok bool) {
	factory, ok := e.providers[id]
	if !ok {
		return nil, false
	}
	return factory(token), true
}

// CredentialResolver builds the provider the rows of one (provider, credentialRef) group are deleted
// with. It returns a nil provider and the class the rows are stamped with when the credential cannot be
// opened: no token, one that does not open, or a credential that no longer exists.
type CredentialResolver func(ctx context.Context, provider, credentialRef string) (p dns.Provider, class string)

// Sweep deletes the DNS records rows stand for, in the order given, each by the ID TierPlan stored for it
// and never by name or value. Rows are grouped by (provider, credential_ref): one provider is built per
// group, the first time a row of it needs one, and a row is deleted with its own credential's provider
// only. A row whose group has no provider is stamped failed with the class the resolver gave. A failed
// delete is logged with the record's name and class only, and nothing is returned, because a failed
// sweep never blocks an order. A cancelled context stops the sweep before the next row.
func (e *DNSEngine) Sweep(ctx context.Context, rows []DNSRecord, resolve CredentialResolver) {
	type group struct{ provider, credentialRef string }
	type opened struct {
		p     dns.Provider
		class string // why p is nil
	}
	providers := map[group]opened{}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		g := group{row.Provider, row.CredentialRef}
		o, ok := providers[g]
		if !ok {
			o.p, o.class = resolve(ctx, row.Provider, row.CredentialRef)
			providers[g] = o
		}
		if o.p == nil {
			e.stampFailed(ctx, row, o.class)
			continue
		}
		if err := e.deleteRecord(ctx, o.p, row); err != nil {
			e.log.Warn("a DNS record could not be deleted", "name", row.Name, "class", deleteClass(err))
		}
	}
}

// resolveChallengeTarget finds where the TXT record for name goes: the zone of
// _acme-challenge.<name> on the provider, and the name any CNAME there delegates to (locked D-14 as
// amended, D-15).
//
// For a name whose zone the provider holds, the CNAME is asked of that zone's own nameservers first: a
// recursive resolver asked first could cache the answer "no such name" for the very name the TXT is then
// checked at. The custom resolvers are asked only when no nameserver of the zone answers. For a name whose
// zone the provider does not hold, the custom resolvers are asked, else the system's: a delegation into
// a provider zone is the only way such a name validates.
func (e *DNSEngine) resolveChallengeTarget(ctx context.Context, p dns.Provider, name string, custom []dnscheck.Server) (challengeTarget, error) {
	fqdn := dns.ChallengeFQDN(name)
	t := challengeTarget{Name: name, FQDN: fqdn}
	zone, zoneErr := p.Zone(ctx, fqdn)
	onProvider := zoneErr == nil
	if zoneErr != nil && !isDNSClass(zoneErr, dns.ClassZoneNotFound) {
		return t, zoneErr
	}

	var final string
	var delegated bool
	var err error
	if onProvider {
		t.Zone, t.OwnZone = zone, zone.Name
		final, delegated, err = e.checker.FollowCNAME(ctx, fqdn, dnscheck.Authoritative(zone.NameServers))
		if errors.Is(err, dnscheck.ErrNoAnswer) && len(custom) > 0 {
			final, delegated, err = e.checker.FollowCNAME(ctx, fqdn, custom)
		}
	} else {
		servers := custom
		if len(servers) == 0 {
			// No system resolver is no answer, and a name nobody can place is a zone not found.
			servers, _ = e.checker.SystemServers()
		}
		final, delegated, err = e.checker.FollowCNAME(ctx, fqdn, servers)
	}
	var loop *dnscheck.DelegationError
	switch {
	case errors.As(err, &loop):
		return t, &dns.Error{Class: dns.ClassDelegationTarget, Host: name, Target: loop.Target}
	case errors.Is(err, dnscheck.ErrNoAnswer):
		delegated = false // nobody answered, so nothing is known to be delegated
	case err != nil:
		return t, err
	}

	if !delegated {
		if !onProvider {
			return t, zoneErr
		}
		return t, nil
	}
	tz, err := p.Zone(ctx, final)
	if err != nil {
		if isDNSClass(err, dns.ClassZoneNotFound) {
			return t, &dns.Error{Class: dns.ClassDelegationTarget, Host: name, Target: final}
		}
		return t, err
	}
	t.FQDN, t.Zone, t.DelegatedTo = final, tz, final
	return t, nil
}

// deleteRecord deletes the record a ledger row stands for, by the provider's ID for it and nothing
// else, and removes the row. A failed delete leaves the row, stamped with the class of the failure and
// when the next try is due.
func (e *DNSEngine) deleteRecord(ctx context.Context, p dns.Provider, row DNSRecord) error {
	bg := context.WithoutCancel(ctx)
	if err := p.DeleteTXT(bg, dns.RecordRef{ZoneID: row.ZoneID, RecordID: row.RecordID}); err != nil {
		if e.zoneGone(bg, p, row, err) {
			// The record went with its zone: only the ledger row is left, and it is dropped by its own ID.
			e.log.Info("a DNS record went with its zone, so it is no longer tracked", "name", row.Name, "zone", row.ZoneName)
			return e.ledger.DeleteDNSRecord(bg, row.ID)
		}
		e.stampFailed(bg, row, deleteClass(err))
		return err
	}
	return e.ledger.DeleteDNSRecord(bg, row.ID)
}

// zoneGone reports whether a delete the provider refused for the row's zone ID was refused because the
// zone no longer exists, so the record went with it. A provider answers a zone ID it does not know the
// same way as a token that may not edit the zone, or one that belongs to another account, so the
// question is asked of the provider by the zone's name inside the row's own account. The zone is gone
// only when the row names its account, the token still sees zones in that account, and none of them
// is that zone, or the name now has another ID there (it was deleted and made again). A row from before
// the account was kept, a token that sees no zone in the account (another account's), a lookup that
// fails and a zone that is still there under its ID all say "not shown to be gone", and the row is
// retried as it always was. The only call is the read-only zone lookup; nothing is deleted by name.
func (e *DNSEngine) zoneGone(ctx context.Context, p dns.Provider, row DNSRecord, delErr error) bool {
	if row.AccountID == "" || row.ZoneName == "" || !errors.Is(delErr, dns.ErrZoneRefused) {
		return false
	}
	z, err := p.AccountZone(ctx, row.AccountID, row.ZoneName)
	if err != nil {
		var de *dns.Error
		return errors.As(err, &de) && de.Class == dns.ClassZoneNotFound
	}
	return strings.EqualFold(z.Name, row.ZoneName) && z.ID != row.ZoneID
}

// stampFailed records a failed delete on the row: one more attempt, and the next try on the backoff
// schedule.
func (e *DNSEngine) stampFailed(ctx context.Context, row DNSRecord, class string) {
	now := e.clock.Now()
	attempts := row.Attempts + 1
	next := now.Add(BackoffDelay(attempts, 0, e.jitter()))
	if err := e.ledger.MarkDNSRecordFailed(ctx, row.ID, attempts, now, next, class); err != nil {
		e.log.Error("record a failed DNS record delete", "name", row.Name, "error", err)
	}
}
