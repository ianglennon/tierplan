// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/x509"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// The audit target of every HTTPS row (Phase 17, SET-04): the instance's HTTPS setup, not a resource.
const auditTarget = "https"

// record writes one audit row as the system principal that runs certificate work. A nil Deps.Audit writes nothing.
// A failed write is logged at error level and never fails the run: the certificate change has happened either way,
// and a run that failed because its log row could not be written would hide the real outcome.
//
// The details are a closed set of facts the callers build (class, host, zone, names, issuer, serial, dates, job,
// trigger). No error text and no CA answer is ever put in a row (T-17-04).
func (m *Manager) record(ctx context.Context, action string, result audit.Result, reason string, details map[string]string) {
	if m.d.Audit == nil {
		return
	}
	err := m.d.Audit(ctx, audit.Entry{
		Actor: audit.SystemACME, Action: action, TargetType: auditTarget, TargetID: auditTarget,
		Result: result, Reason: reason, Details: details,
	})
	if err != nil {
		m.log.Error("write the audit entry of a certificate run", "action", action, "error", err)
	}
}

// runDetails are the facts every outcome row of a run shares: what started it, who asked, and which job it was. A run
// an admin started names that admin only here, as started_by; the row's actor stays the system principal (T-17-05).
func runDetails(x *run, p runParams) map[string]string {
	d := map[string]string{"trigger": p.trigger, "job": x.id}
	if p.actor != "" {
		d["started_by"] = p.actor
	}
	if p.challenge != "" {
		d["challenge"] = p.challenge
	}
	return d
}

// recordIssued writes the row of a run that obtained a certificate: https.issued, or https.renewed for a renewal. It
// is called after the install attempt, so served says whether the new certificate is the one being served now. The row
// stays a success when the swap failed: the certificate was issued and stored, and the failed swap is reported on the
// HTTPS page. cancelled is set when a person's cancel landed after the order was final (D-27 exception): a certificate
// TierPlan obtained and tried to serve is always audited.
func (m *Manager) recordIssued(ctx, bg context.Context, x *run, p runParams, leaf *x509.Certificate, served bool) {
	d := runDetails(x, p)
	if p.req.Profile != "" {
		d["profile"] = p.req.Profile
	}
	d["names"] = strings.Join(leaf.DNSNames, ",")
	d["issuer"] = issuerName(leaf)
	d["serial"] = leaf.SerialNumber.Text(16)
	d["not_after"] = leaf.NotAfter.UTC().Format(time.RFC3339)
	d["served"] = "false"
	if served {
		d["served"] = "true"
	}
	// A person's Cancel leaves the run's context cancelled with a cause other than the hub closing. A shutdown at the
	// same point leaves the normal row, without the key.
	if ctx.Err() != nil && !jobs.Closing(ctx) {
		d["cancelled"] = "true"
	}
	action := audit.ActionHTTPSIssued
	if p.req.Kind == KindRenew {
		action = audit.ActionHTTPSRenewed
	}
	m.record(bg, action, audit.ResultSuccess, "", d)
}

// recordRunFailed writes the row of a run that ended in an error, with the error's class as the reason and in the
// details, and the host and zone it was about. Never the CA's words (T-17-04).
func (m *Manager) recordRunFailed(bg context.Context, x *run, p runParams, f Failure) {
	d := runDetails(x, p)
	d["kind"] = p.req.Kind
	d["class"] = f.Class
	if f.Host != "" {
		d["host"] = f.Host
	}
	if f.Zone != "" {
		d["zone"] = f.Zone
	}
	m.record(bg, audit.ActionHTTPSRunFailed, audit.ResultFailure, f.Class, d)
}

// issuerName is the name of the CA that signed the leaf: its common name, else its first organization.
func issuerName(leaf *x509.Certificate) string {
	if leaf.Issuer.CommonName != "" {
		return leaf.Issuer.CommonName
	}
	if len(leaf.Issuer.Organization) > 0 {
		return leaf.Issuer.Organization[0]
	}
	return ""
}

type runStartedKey struct{}

// WithRunStarted returns a context that makes Issue and RenewNow call fn when they have started a run. The call is
// made exactly once, synchronously, with the started run's RunStart, after the run is accepted and before its body
// does anything: so a row fn writes comes before the run's own outcome row, even when the run fails at its first call
// to the CA. fn never runs for a refusal or for a reused certificate, which start no run.
//
// The run waits only for that one call. fn must be quick and must not call back into the Manager: Issue and RenewNow
// hold the operation lock while it runs.
func WithRunStarted(ctx context.Context, fn func(RunStart)) context.Context {
	return context.WithValue(ctx, runStartedKey{}, fn)
}

func runStartedFrom(ctx context.Context) func(RunStart) {
	fn, _ := ctx.Value(runStartedKey{}).(func(RunStart))
	return fn
}

// runHold keeps an admin-started run from beginning its body until the caller's run-started hook has returned. It
// lasts for one synchronous hook call, made while the operation lock is held. The scheduler's and the start-time
// issue's runs have no hold.
type runHold struct {
	ready chan struct{}
	once  sync.Once
}

func newRunHold() *runHold { return &runHold{ready: make(chan struct{})} }

// wait blocks until the hold is released. A nil hold does not block.
func (h *runHold) wait() {
	if h != nil {
		<-h.ready
	}
}

// release lets the run begin. It is safe to call more than once, and on a nil hold.
func (h *runHold) release() {
	if h != nil {
		h.once.Do(func() { close(h.ready) })
	}
}

// announceStarted runs the caller's run-started hook (nil for none) for a run that was just started, then releases the
// run. The release is deferred so a hook that panics still lets the run go.
func announceStarted(fn func(RunStart), rs RunStart) {
	defer rs.hold.release()
	if rs.hold == nil || rs.JobID == "" || fn == nil {
		return
	}
	fn(rs)
}
