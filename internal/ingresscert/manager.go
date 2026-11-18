// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
)

const (
	// MaxConcurrent is the most certificate runs at once, install-wide (D-16). Each one is a job of the hub.
	MaxConcurrent = 2
	// Tick is how often the manager looks at the certificates when nothing woke it.
	Tick = time.Minute
	// JobKind is the kind of the hub job a certificate run is.
	JobKind = "ingress-cert"
	// JobOwner is the audit actor that owns those jobs and the rows the reconciler creates.
	JobOwner = "system:ingress"
	// ProfileClassic is the certificate profile an order asks for when the directory offers it (D-09). The
	// short-lived profile is never asked for.
	ProfileClassic = "classic"
	// QueueNoAccount is the queue code of a row that waits for the terms to be acknowledged (D-36). Its
	// sentence is QueueReason's.
	QueueNoAccount = "no_account"
	// StopDomainRemoved is the stopped reason of a row whose domain was removed (D-12).
	StopDomainRemoved = "domain_removed"
)

// How a run ended, as RunEnded reports it.
const (
	OutcomeIssued  = "issued"
	OutcomeRenewed = "renewed"
	OutcomeFailed  = "failed"
	OutcomeQueued  = "queued"
	OutcomeGone    = "gone"
)

// maxJitter is the widest backoff jitter, as a fraction of the wait.
const maxJitter = 0.1

// Deps are what a Manager needs. A nil Clock is the wall clock, and a nil Rand or Jitter takes math/rand/v2,
// as acme.NewManager does.
type Deps struct {
	// Store is the certificates, orders, accounts and the install-wide state.
	Store *Store
	// Domains reads the claims and the domains the claims sit under.
	Domains *ingressstore.Store
	// Notifier wakes Run when an environment's ingress configuration changes; nil means only the tick does.
	Notifier *ingress.Notifier
	// Jobs runs every certificate order as a job.
	Jobs *jobs.Hub
	// Clock is the manager's only source of time.
	Clock stackops.Clock
	// Config decides the directories (the test override of TIERPLAN_ACME_DIRECTORY_URL applies to both issuers).
	Config config.Config
	// DNS carries the DNS providers and the checker's seams.
	DNS acme.DNSDeps
	// Ledger is the record ledger the DNS engine writes the challenge records to.
	Ledger *acme.Store
	// Contact returns the contact email a lazily registered account is given: the own account's, empty when
	// there is none (D-36). Nil means none.
	Contact func(ctx context.Context) string
	// Log is where the manager logs; nil is the default logger.
	Log *slog.Logger
	// Rand returns a number in [0, n); nil is math/rand/v2.
	Rand func(n int64) int64
	// Jitter returns the backoff jitter in [-0.1, 0.1]; nil is math/rand/v2.
	Jitter func() float64
	// UserAgent is sent to the certificate authority.
	UserAgent string
	// Profile, when set, is the profile every order names. It is a seam for Pebble, which picks a profile at
	// random for an order that names none; empty applies the classic-or-default rule (D-09).
	Profile string
	// RunEnded, when set, is told the certificate's id and the outcome (issued, renewed, failed, queued or gone)
	// after a run has written its result and released its slot. It is a test seam.
	RunEnded func(certID, outcome string)
}

// Manager keeps every claimed hostname supplied with a certificate: a reconciler that notices a claim without
// one, and a scheduler that runs the orders, at most MaxConcurrent at once.
type Manager struct {
	d     Deps
	clock stackops.Clock
	log   *slog.Logger

	// clients and dirs are by issuer: one acme.Client per directory, built once.
	clients map[string]*acme.Client
	dirs    map[string]acme.Directory
	engine  *acme.DNSEngine

	// wake is signalled by Wake and by every run that ends, so a queued row does not wait for the tick.
	wake chan struct{}

	mu sync.Mutex
	// active holds the id of every certificate a run is reserved or running for.
	active map[string]struct{}
}

// New returns a Manager. It resolves both directories and builds one ACME client per directory over the
// ingress account store.
func New(d Deps) (*Manager, error) {
	if d.Store == nil || d.Domains == nil || d.Jobs == nil || d.Ledger == nil {
		return nil, errors.New("ingresscert: a manager needs a store, the ingress domains, a job hub and a DNS ledger")
	}
	if d.Clock == nil {
		d.Clock = stackops.RealClock()
	}
	if d.Rand == nil {
		d.Rand = mrand.Int64N
	}
	if d.Jitter == nil {
		d.Jitter = func() float64 { return (mrand.Float64()*2 - 1) * maxJitter } //nolint:gosec // G404: spreads the retry time of one certificate; it is not a secret and needs no cryptographic randomness
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		d: d, clock: d.Clock, log: log.With("component", "ingresscert"),
		clients: map[string]*acme.Client{}, dirs: map[string]acme.Directory{},
		wake: make(chan struct{}, 1), active: map[string]struct{}{},
	}
	for issuer, staging := range map[string]bool{IssuerStaging: true, IssuerProduction: false} {
		dir, err := acme.NewDirectoryFor(d.Config, staging)
		if err != nil {
			return nil, fmt.Errorf("ingresscert: the %s directory: %w", issuer, err)
		}
		m.dirs[issuer] = dir
		m.clients[issuer] = acme.NewClient(dir, d.Store.AccountStore(), d.Clock, d.UserAgent)
	}
	m.engine = acme.NewDNSEngine(acme.DNSEngineDeps{DNS: d.DNS, Ledger: d.Ledger, Clock: d.Clock, Log: m.log, Jitter: d.Jitter})
	return m, nil
}

// client is the ACME client of the issuer's directory and that directory. An issuer that is neither staging nor
// production has none (nil).
func (m *Manager) client(issuer string) (*acme.Client, acme.Directory) {
	c, ok := m.clients[issuer]
	if !ok {
		return nil, acme.Directory{}
	}
	return c, m.dirs[issuer]
}

// Start must run before Run. It puts every row that was ordering back to queued, because a crash must not hold
// a reservation and nothing is ordering in a new process, and it prunes the closed orders that are old enough.
// An open order is kept: the next run resumes it.
func (m *Manager) Start(ctx context.Context) error {
	n, err := m.d.Store.ResetOrdering(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		m.log.Info("certificates that were ordering when the last process stopped are queued again", "count", n)
	}
	if _, err := m.d.Store.PruneClosedOrders(ctx, m.clock.Now().Add(-ClosedOrderRetention)); err != nil {
		return err
	}
	return nil
}

// Wake asks Run for a round soon. It never blocks, and a wake that is already waiting absorbs another.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run is the manager's loop: a round now, then one on every Tick of the injected clock, on every change the
// ingress notifier reports and on every Wake, until ctx ends. Each cycle arms exactly one timer; a wake takes
// its round on the timer already armed.
func (m *Manager) Run(ctx context.Context) {
	var changed <-chan struct{}
	if m.d.Notifier != nil {
		sub := m.d.Notifier.Subscribe()
		defer sub.Close()
		changed = sub.C()
	}
	for {
		m.round(ctx)
		timer := m.clock.After(Tick)
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer:
				break wait
			case <-changed:
				m.round(ctx)
			case <-m.wake:
				m.round(ctx)
			}
		}
	}
}
