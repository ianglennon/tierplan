// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"crypto"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// ReadyMargin is how long before it expires a certificate stops being ready: one that has less than this left is
// not served any more (D-20), so a renewal that is late is noticed before a visitor is.
const ReadyMargin = 24 * time.Hour

// Ready reports whether certificate c may be served now (CRT-09, D-20): it holds a chain, it is a production
// certificate or an upload, its validity has begun, it has more than ReadyMargin left and its key opens
// (keyOpens, which the caller works out, so a pure rule can be tested without a vault). A staging certificate
// is never ready, whatever its dates: no live route may serve it (PROH-20-02).
func Ready(c Cert, now time.Time, keyOpens bool) bool {
	if !keyOpens || len(c.ChainPEM) == 0 {
		return false
	}
	switch {
	case c.Source == SourceUpload:
	case c.Source == SourceACME && c.Issuer == IssuerProduction:
	default:
		return false
	}
	if c.NotBefore.IsZero() || c.NotAfter.IsZero() || now.Before(c.NotBefore) {
		return false
	}
	return c.NotAfter.Sub(now) > ReadyMargin
}

// The states of a certificate, as StateOf names them.
const (
	StateQueued             = "queued"
	StateOrdering           = "ordering"
	StateValid              = "valid"
	StateUploaded           = "uploaded"
	StateDNSProvenOnStaging = "dns_proven_on_staging"
	StateRenewalFailed      = "renewal_failed"
	StateFailed             = "failed"
	StateExpired            = "expired"
	StateStopped            = "stopped"
	StateRetired            = "retired"
)

// StateOf is the one word for where certificate c stands at now. A retired row is retired; a chain whose end has
// passed is expired; an upload is uploaded; a stopped row is stopped. A staging certificate is
// dns_proven_on_staging, never valid: DNS-01 was proven against the staging directory and nothing may serve it
// (CRT-09). A row with a chain and failed attempts is renewal_failed, one without a chain is failed, and the
// rest are ordering, queued or valid.
func StateOf(c Cert, now time.Time) string {
	hasChain := len(c.ChainPEM) > 0
	switch {
	case !c.RetiredAt.IsZero():
		return StateRetired
	case hasChain && !c.NotAfter.IsZero() && !now.Before(c.NotAfter):
		return StateExpired
	case c.Source == SourceUpload:
		return StateUploaded
	case c.Phase == PhaseStopped:
		return StateStopped
	case hasChain && c.Issuer == IssuerStaging:
		return StateDNSProvenOnStaging
	case c.Phase == PhaseOrdering:
		return StateOrdering
	case hasChain && c.ConsecutiveFailures > 0:
		return StateRenewalFailed
	case hasChain:
		return StateValid
	case c.ConsecutiveFailures > 0:
		return StateFailed
	default:
		return StateQueued
	}
}

// StateText is the words a person reads for a state.
func StateText(state string) string {
	switch state {
	case StateQueued:
		return "Queued"
	case StateOrdering:
		return "Ordering"
	case StateValid:
		return "Valid"
	case StateUploaded:
		return "Uploaded"
	case StateDNSProvenOnStaging:
		return "DNS proven on staging"
	case StateRenewalFailed:
		return "Renewal failed"
	case StateFailed:
		return "Failed"
	case StateExpired:
		return "Expired"
	case StateStopped:
		return "Stopped"
	case StateRetired:
		return "Retired"
	}
	return state
}

// refreshReady keeps the stored ready flag of every certificate that holds a chain current with the clock
// (D-41). A certificate that crosses ReadyMargin by time alone changes nothing else, so this is the write that
// raises certs_version for it. The key is opened only when the flag would turn true, and a flag that is true and
// still holds is left alone.
func (m *Manager) refreshReady(ctx context.Context) {
	now := m.clock.Now()
	rows, err := m.d.Store.st.RQ.ListReadyCandidateIngressCerts(ctx, toNanos(now))
	if err != nil {
		m.log.Error("the certificate manager could not list the certificates to check their readiness", "error", err)
		return
	}
	changes := map[string][]readyChange{}
	for _, row := range rows {
		c, err := certOf(row)
		if err != nil {
			m.log.Error("the certificate manager could not read a certificate", "error", err)
			continue
		}
		due := Ready(c, now, true)
		switch {
		case c.Ready && !due:
			changes[c.EnvID] = append(changes[c.EnvID], readyChange{id: c.ID, ready: false})
		case !c.Ready && due:
			if _, err := m.d.Store.OpenKey(c); err != nil {
				m.log.Warn("a certificate is not ready because its key does not open", "host", c.Hostname, "error", err)
				continue
			}
			changes[c.EnvID] = append(changes[c.EnvID], readyChange{id: c.ID, ready: true})
		}
	}
	for env, list := range changes {
		if ctx.Err() != nil {
			return
		}
		if err := m.d.Store.setReady(ctx, env, list, now); err != nil {
			m.log.Error("the certificate manager could not store the readiness of the certificates", "env", env, "error", err)
		}
	}
}

// readyChange is one certificate's new ready flag.
type readyChange struct {
	id    string
	ready bool
}

// setReady stores the new flags of certificates of environment envID in one transaction, which raises the
// environment's certs_version once.
func (s *Store) setReady(ctx context.Context, envID string, list []readyChange, now time.Time) error {
	return s.write(ctx, []string{envID}, func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		for _, ch := range list {
			ready := int64(0)
			if ch.ready {
				ready = 1
			}
			if err := q.SetIngressCertReady(ctx, sqlc.SetIngressCertReadyParams{Ready: ready, UpdatedAt: toNanos(now), ID: ch.id}); err != nil {
				return false, fmt.Errorf("ingresscert: set the readiness of certificate %q: %w", ch.id, err)
			}
		}
		return len(list) > 0, nil
	})
}

// ServableCert is a certificate Phase 21 may put on a live route: its chain and its opened key.
type ServableCert struct {
	Hostname string
	ChainPEM []byte
	Key      crypto.Signer
	NotAfter time.Time
	Source   string
}

// Servable returns, for each hostname of environment envID, the certificate that may be served now, ordered by
// hostname. That is the live row when it is ready; otherwise the newest retired production or uploaded row that
// still is (a certificate an issuer change or a switch back to ACME retired keeps serving until its replacement
// is ready). A staging row is never returned (PROH-20-02), and neither is one whose key does not open. A
// hostname with nothing ready is left out.
func (s *Store) Servable(ctx context.Context, envID string) ([]ServableCert, error) {
	rows, err := s.EnvCerts(ctx, envID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	byHost := map[string][]Cert{}
	var hosts []string
	for _, c := range rows {
		if _, seen := byHost[c.Hostname]; !seen {
			hosts = append(hosts, c.Hostname)
		}
		byHost[c.Hostname] = append(byHost[c.Hostname], c)
	}
	var out []ServableCert
	for _, host := range hosts {
		cands := byHost[host]
		// The live row first, then the retired ones, newest retirement first.
		slices.SortStableFunc(cands, func(a, b Cert) int {
			switch {
			case a.RetiredAt.IsZero() && !b.RetiredAt.IsZero():
				return -1
			case !a.RetiredAt.IsZero() && b.RetiredAt.IsZero():
				return 1
			}
			return b.RetiredAt.Compare(a.RetiredAt)
		})
		for _, c := range cands {
			if !Ready(c, now, true) {
				continue
			}
			key, err := s.OpenKey(c)
			if err != nil {
				continue
			}
			out = append(out, ServableCert{Hostname: host, ChainPEM: c.ChainPEM, Key: key, NotAfter: c.NotAfter, Source: c.Source})
			break
		}
	}
	return out, nil
}
