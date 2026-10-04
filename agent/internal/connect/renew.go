// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package connect

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/proto/wire"
)

const (
	// renewRetry is how long after a failed renewal the agent asks again, give or
	// take a quarter, so a fleet that failed together does not ask together.
	renewRetry = time.Hour
	// renewAnswerTimeout is how long the agent waits for the server's answer to a
	// renew before it counts the attempt as failed and asks again later.
	renewAnswerTimeout = 2 * time.Minute
)

// nextRenewal is when the agent should next ask for a new certificate (D-04). With
// no failures it is two thirds of the way through the certificate's life, about day
// 20 of 30, whatever now is: a certificate that is already past that point is due at
// that point, in the past. After a failure it is about renewRetry after now (the
// time of the failure), jittered between three quarters and five quarters of it by
// rnd, which returns a value in [0, 1) and may be nil for the process's random source.
func nextRenewal(notBefore, notAfter, now time.Time, failures int, rnd func() float64) time.Time {
	if failures <= 0 {
		return notBefore.Add(notAfter.Sub(notBefore) * 2 / 3)
	}
	if rnd == nil {
		rnd = rand.Float64 //nolint:gosec // jitter, not a security decision
	}
	return now.Add(time.Duration(float64(renewRetry) * (0.75 + 0.5*rnd())))
}

// renewer is the agent's side of certificate renewal: it decides when to ask, and
// checks and keeps what the server answers. The control loop calls poll on every
// tick and the control reader calls handle, so it has a lock.
type renewer struct {
	now func() time.Time
	rnd func() float64
	log *slog.Logger
	// agentID and pub are what a renewed certificate must name: the same agent and
	// the same key. A renewal never rotates the key.
	agentID string
	pub     ed25519.PublicKey
	save    func([]byte) error

	mu                  sync.Mutex
	notBefore, notAfter time.Time
	failures            int
	next                time.Time
	// sentAt is when the renew in flight was sent, zero when none is.
	sentAt time.Time
}

// newRenewer reads the certificate the agent holds and schedules its renewal. It is
// nil when the certificate cannot be read, and then the agent does not renew; it
// logs why. save keeps a renewed certificate: it must replace the stored one
// atomically and update the identity the next connection presents.
func newRenewer(cert []byte, agentID string, key ed25519.PrivateKey, now func() time.Time, rnd func() float64, log *slog.Logger, save func([]byte) error) *renewer {
	leaf, err := x509.ParseCertificate(cert)
	if err != nil {
		log.Warn("the agent cannot read its own certificate, so it will not renew it; enrol it again if it stops connecting", "error", err)
		return nil
	}
	if now == nil {
		now = time.Now
	}
	rn := &renewer{
		now: now, rnd: rnd, log: log, agentID: agentID, pub: publicOf(key), save: save,
		notBefore: leaf.NotBefore, notAfter: leaf.NotAfter,
	}
	rn.next = nextRenewal(rn.notBefore, rn.notAfter, now(), 0, rnd)
	return rn
}

// poll reports whether a renew should be sent now, and if so counts one as in
// flight until the answer arrives or renewAnswerTimeout passes.
func (rn *renewer) poll() bool {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	now := rn.now()
	if !rn.sentAt.IsZero() {
		if now.Sub(rn.sentAt) < renewAnswerTimeout {
			return false
		}
		rn.failLocked(now, "the server did not answer")
		return false
	}
	if now.Before(rn.next) {
		return false
	}
	rn.sentAt = now
	return true
}

// failLocked counts a failed attempt and schedules the next about an hour on.
func (rn *renewer) failLocked(now time.Time, why string) {
	rn.failures++
	rn.sentAt = time.Time{}
	rn.next = nextRenewal(rn.notBefore, rn.notAfter, now, rn.failures, rn.rnd)
	left := rn.notAfter.Sub(now).Round(time.Minute)
	rn.log.Warn("the agent could not renew its certificate; it will ask again", "why", why,
		"certificate_expires_in", left.String(), "retry_in", rn.next.Sub(now).Round(time.Minute).String())
}

// handle takes the server's answer to a renew: typ is the message type and raw the
// whole line. An answer to a question that was not asked is ignored.
func (rn *renewer) handle(typ string, raw []byte) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	now := rn.now()
	if rn.sentAt.IsZero() {
		rn.log.Debug("ignored a renewal answer nobody asked for", "type", typ)
		return
	}
	switch typ {
	case wire.TypeRenewRefused:
		var m wire.RenewRefused
		_ = json.Unmarshal(raw, &m)
		rn.failLocked(now, "the server refused: "+clipCode(m.Code))
	case wire.TypeRenewed:
		var m wire.Renewed
		if err := json.Unmarshal(raw, &m); err != nil {
			rn.failLocked(now, "the answer was malformed")
			return
		}
		leaf, why := rn.check(m.Cert)
		if why != "" {
			rn.failLocked(now, why)
			return
		}
		if err := rn.save(m.Cert); err != nil {
			// The old certificate keeps working until it expires, so this is a failure
			// to try again, not the end of the agent. Said loudly: a volume that cannot
			// be written will stop the agent connecting when the old one expires.
			rn.log.Error("the agent could not save its renewed certificate", "error", err)
			rn.failLocked(now, "the renewed certificate could not be saved")
			return
		}
		rn.notBefore, rn.notAfter = leaf.NotBefore, leaf.NotAfter
		rn.failures, rn.sentAt = 0, time.Time{}
		rn.next = nextRenewal(rn.notBefore, rn.notAfter, now, 0, rn.rnd)
		rn.log.Info("the agent renewed its certificate", "expires", rn.notAfter.UTC().Format(time.RFC3339))
	}
}

// check reads a renewed certificate and judges it. It must be for this agent and
// this key and must outlive the one in use, so a server cannot push the agent back
// to a certificate it already has or on to someone else's. It returns the reason
// for refusing it, or "".
func (rn *renewer) check(der []byte) (*x509.Certificate, string) {
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, "the renewed certificate could not be read"
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || !bytes.Equal(pub, rn.pub) {
		return nil, "the renewed certificate is for another key"
	}
	if leaf.Subject.CommonName != rn.agentID {
		return nil, "the renewed certificate is for another agent"
	}
	if !leaf.NotAfter.After(rn.notAfter) {
		return nil, "the renewed certificate does not last longer than the current one"
	}
	return leaf, ""
}

// clipCode cuts a code the server sent to a length that is safe to log.
func clipCode(s string) string {
	return capString(s, 32)
}
