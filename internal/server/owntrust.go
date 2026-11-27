// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"strings"
	"sync"
	"time"
)

// ownTrustGrace is how long the chain TierPlan served before a change stays in the trust set. A
// proxy that has not yet fetched the new set still verifies TierPlan against the old one for this
// long (A-03); the self-signed anchor never leaves, because it is always a current member.
const ownTrustGrace = time.Hour

// ownTrust is the set of certificates a proxy must trust to verify TierPlan itself, as one PEM
// with a version (D-14, A-02). The members are, in a fixed order: TierPlan's own self-signed leaf
// (the stable anchor), the chain served now (leaf first), then any chain served within the grace
// period. The version is a fingerprint of the PEM content, not a counter, so it is equal across
// restarts for the same certificates and changes on every certificate change: a Let's Encrypt
// install, the fallback to self-signed, a self-signed re-sign for new names (the old leaf does not
// trust the new one) and a replaced supplied pair. The proxy then trusts a set, never one
// certificate, so a swap is a change of members and never a moment with no trusted member.
//
// The PEM holds certificates only and is built from parsed certificates the runtime serves, never
// from request input.
type ownTrust struct {
	// served is the runtime's servedChain: the installed kind and the chain, leaf first.
	served func() (tlsKind, []*x509.Certificate)
	// anchor is the self-signed source's current leaf, nil when TierPlan has none.
	anchor func() *x509.Certificate
	now    func() time.Time

	mu      sync.Mutex
	started bool
	// cur is the current members, anchor first, de-duplicated.
	cur []*x509.Certificate
	// prev is the members that left cur, each kept until its own time.
	prev []ownTrustPrev
	sig  string
	pem  []byte
	ver  int64
}

type ownTrustPrev struct {
	cert  *x509.Certificate
	until time.Time
}

// newOwnTrust returns the trust set read from served and anchor, on the clock now. A nil now is
// the wall clock.
func newOwnTrust(served func() (tlsKind, []*x509.Certificate), anchor func() *x509.Certificate, now func() time.Time) *ownTrust {
	if now == nil {
		now = time.Now
	}
	return &ownTrust{served: served, anchor: anchor, now: now}
}

// PEM is the trust set as PEM and its version, after looking at what is served now. It is cheap when
// nothing changed: the members' DER fingerprints are compared first and the PEM is encoded only on
// a change.
func (t *ownTrust) PEM() ([]byte, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.observeLocked()
	return t.pem, t.ver
}

// Observe looks at what is served now and reports whether the version changed. The install hook
// calls it so an ACME install or a fallback is seen at once; the re-sign and the supplied-file
// reload do not install, so the reconciler's poll of PEM is what catches those.
func (t *ownTrust) Observe() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.observeLocked()
}

func (t *ownTrust) observeLocked() bool {
	now := t.now()
	next := t.members()

	if t.started && fingerprints(next) != fingerprints(t.cur) {
		// What left the set is kept for the grace period; what came back is current again.
		kept := t.prev[:0:0]
		for _, p := range t.prev {
			if !containsCert(next, p.cert) {
				kept = append(kept, p)
			}
		}
		for _, c := range t.cur {
			if !containsCert(next, c) && !containsPrev(kept, c) {
				kept = append(kept, ownTrustPrev{cert: c, until: now.Add(ownTrustGrace)})
			}
		}
		t.prev = kept
	}
	t.cur = next

	// A chain whose grace is over drops out, which is a change of version.
	live := t.prev[:0:0]
	for _, p := range t.prev {
		if now.Before(p.until) {
			live = append(live, p)
		}
	}
	t.prev = live

	sig := fingerprints(t.cur) + "|" + fingerprints(prevCerts(t.prev))
	if t.started && sig == t.sig {
		return false
	}
	t.started = true
	t.sig = sig

	var out []byte
	for _, c := range t.cur {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	for _, p := range t.prev {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.cert.Raw})...)
	}
	sum := sha256.Sum256(out)
	// A zero byte first keeps the 63-bit value positive.
	ver := int64(binary.BigEndian.Uint64(append([]byte{0}, sum[:7]...))) //nolint:gosec // seven bytes under a zero byte always fit an int64
	changed := ver != t.ver
	t.pem, t.ver = out, ver
	return changed
}

// members is the current set: the anchor, then the served chain, each certificate once.
func (t *ownTrust) members() []*x509.Certificate {
	var all []*x509.Certificate
	if t.anchor != nil {
		if a := t.anchor(); a != nil {
			all = append(all, a)
		}
	}
	if t.served != nil {
		_, chain := t.served()
		all = append(all, chain...)
	}
	out := make([]*x509.Certificate, 0, len(all))
	for _, c := range all {
		if c != nil && !containsCert(out, c) {
			out = append(out, c)
		}
	}
	return out
}

func containsCert(set []*x509.Certificate, c *x509.Certificate) bool {
	for _, o := range set {
		if sha256.Sum256(o.Raw) == sha256.Sum256(c.Raw) {
			return true
		}
	}
	return false
}

func containsPrev(set []ownTrustPrev, c *x509.Certificate) bool {
	for _, p := range set {
		if sha256.Sum256(p.cert.Raw) == sha256.Sum256(c.Raw) {
			return true
		}
	}
	return false
}

func prevCerts(p []ownTrustPrev) []*x509.Certificate {
	out := make([]*x509.Certificate, len(p))
	for i, e := range p {
		out[i] = e.cert
	}
	return out
}

// fingerprints is the DER SHA-256 of each certificate, in order, as one string.
func fingerprints(set []*x509.Certificate) string {
	var b strings.Builder
	for _, c := range set {
		sum := sha256.Sum256(c.Raw)
		b.WriteString(hex.EncodeToString(sum[:]))
		b.WriteByte(',')
	}
	return b.String()
}
