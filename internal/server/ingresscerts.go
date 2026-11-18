// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/buildinfo"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/vault"
)

// quietable is a test clock that can hand out a view of itself whose timers announce nothing (acmetest.Clock). The
// certificate manager waits on its own timers; on a clock whose arming a test counts, they would be mistaken for the
// Let's Encrypt manager's and the test would move the clock before that manager is waiting. A wall clock is not one.
type quietable interface {
	Quiet() interface {
		Now() time.Time
		After(d time.Duration) <-chan time.Time
	}
}

// wireIngressCerts builds the per-hostname certificate store and manager over the server's parts (Phase 20). It does
// not start the manager: Build calls Start before the server listens and runs it in the background, as it does the
// Let's Encrypt manager.
//
// The manager shares the Let's Encrypt manager's DNS provider map and clock, so the one WithACMEDNS option redirects
// the tokens of both to a fake and the one WithACMEClock option moves both. A lazily registered ingress account is
// given the own account's contact for the own directory, when there is one (D-36). The clock may be nil, which is the
// wall clock.
func wireIngressCerts(cfg config.Config, log *slog.Logger, st *store.Store, vlt *vault.Vault, jobHub *jobs.Hub,
	ingressStore *ingressstore.Store, notifier *ingress.Notifier, clock stackops.Clock, dnsDeps acme.DNSDeps) (*ingresscert.Manager, *ingresscert.Store, error) {
	if q, ok := clock.(quietable); ok {
		clock = q.Quiet()
	}
	var now func() time.Time
	if clock != nil {
		now = clock.Now
	}
	certStore := ingresscert.NewStore(ingresscert.StoreDeps{
		St: st, Vault: vlt, Now: now, Notifier: notifier, Audit: audit.NewWriter(st, vlt, log, nil),
	})
	// The own account's contact, read when an order needs an account: it is stored as a mailto: URI and the manager
	// wants the bare address. The directory is the own one; none (or no account) is no contact.
	ownStore := acme.NewStore(st, vlt)
	contact := func(ctx context.Context) string {
		dir, err := acme.NewDirectory(cfg)
		if err != nil {
			return ""
		}
		acct, found, err := ownStore.AccountInfo(ctx, dir.URL)
		if err != nil || !found {
			return ""
		}
		return strings.TrimPrefix(acct.Contact, "mailto:")
	}
	mgr, err := ingresscert.New(ingresscert.Deps{
		Store: certStore, Domains: ingressStore, Notifier: notifier, Jobs: jobHub, Clock: clock, Config: cfg, DNS: dnsDeps,
		Ledger: ownStore, Contact: contact, Log: log, UserAgent: "TierPlan/" + buildinfo.Get().Version,
	})
	if err != nil {
		return nil, nil, err
	}
	return mgr, certStore, nil
}
