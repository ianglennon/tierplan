// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// ownTrustGrace is how long the chain TierPlan served before a change stays in the trust set. A
// proxy that has not yet fetched the new set still verifies TierPlan against the old one for this
// long (A-03); the self-signed anchor never leaves, because it is always a current member.
const ownTrustGrace = time.Hour

// ownTrustStageWait bounds how long a certificate change waits for the proxies to apply the trust set that holds the
// incoming certificate before TierPlan serves it (A-03). A proxy that is stuck or slow must not hold a renewal for ever:
// once ownTrustStageWait has passed the certificate is served anyway and the wait is logged.
const ownTrustStageWait = 15 * time.Second

// ownTrustStagePoll is how often the wait asks whether the proxies have applied the staged trust.
const ownTrustStagePoll = 500 * time.Millisecond

// errOwnTrustStageTimeout is waitApplied's answer when ownTrustStageWait passed before the proxies applied the trust.
var errOwnTrustStageTimeout = errors.New("the proxies did not apply the staged trust set in time")

// ownTrust is the set of certificates a proxy must trust to verify TierPlan itself, as one PEM
// with a version (D-14, A-02). The members are, in a fixed order: TierPlan's own self-signed leaf
// (the stable anchor), the leaf served now, any leaf staged to be served next (Stage), then any leaf
// served within the grace period. Only leaves, never an issuer of the served chain (WR-06): a proxy
// trusts every member as an anchor, and an issuer as an anchor would make it accept any certificate
// that issuer gives out for TierPlan's name. A leaf in the roots is accepted as it is (Go's verifier,
// which Traefik's rootCAs uses, ends the chain at a root that is the leaf itself). The version is a fingerprint of the PEM content, not a counter, so it is equal across
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
	// after is the clock the staged wait polls on (ownTrustStagePoll), the same clock as now.
	after func(time.Duration) <-chan time.Time

	mu      sync.Mutex
	started bool
	// cur is the current members, anchor first, de-duplicated.
	cur []*x509.Certificate
	// prev is the members that left cur, each kept until its own time.
	prev []ownTrustPrev
	// incoming is the certificates staged before they are served (Stage), each kept until it is served or until its
	// own time, whichever comes first. A staged certificate that is never served (a failed install) leaves at its time.
	incoming []ownTrustPrev
	sig      string
	pem      []byte
	ver      int64
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
	return &ownTrust{served: served, anchor: anchor, now: now, after: time.After}
}

// useClock puts the trust set and its staged wait on c. The server calls it once, before anything reads the set.
func (t *ownTrust) useClock(c stackops.Clock) *ownTrust {
	if c != nil {
		t.now, t.after = c.Now, c.After
	}
	return t
}

// Stage adds the leaf of chain, a certificate TierPlan is about to serve (leaf first), to the trust set as incoming and
// returns the set's version with it (A-03). The rest of the chain is never added (WR-06). Once the leaf is served it is an
// ordinary member and the served/previous rule applies; a leaf that is never served leaves after ownTrustGrace. Staging a
// certificate the set already holds changes nothing.
func (t *ownTrust) Stage(chain []*x509.Certificate) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	until := t.now().Add(ownTrustGrace)
	for _, c := range servedLeafOf(chain) {
		if c == nil {
			continue
		}
		if i := slices.IndexFunc(t.incoming, func(p ownTrustPrev) bool { return c.Equal(p.cert) }); i >= 0 {
			t.incoming[i].until = until
			continue
		}
		t.incoming = append(t.incoming, ownTrustPrev{cert: c, until: until})
	}
	t.observeLocked()
	return t.ver
}

// waitApplied waits until applied(version) is true, asking every ownTrustStagePoll on the trust set's clock, for at most
// ownTrustStageWait. It answers nil once applied, errOwnTrustStageTimeout when the bound passed first and ctx's error when
// ctx ended first. A nil applied is applied at once.
func (t *ownTrust) waitApplied(ctx context.Context, version int64, applied func(version int64) bool) error {
	if applied == nil || applied(version) {
		return nil
	}
	deadline := t.now().Add(ownTrustStageWait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.after(ownTrustStagePoll):
		}
		if applied(version) {
			return nil
		}
		if !t.now().Before(deadline) {
			return errOwnTrustStageTimeout
		}
	}
}

// ownStaging is how TierPlan stages a certificate change into the proxies' trust before it serves the certificate
// (A-02, A-03): the Let's Encrypt installer and the certificate sources' beforeSet share it. Every field may be nil: a
// nil trust stages nothing and a nil applied waits for nothing.
type ownStaging struct {
	trust *ownTrust
	// applied reports whether the proxies of TierPlan's own environment hold a bundle with the given own version. It is
	// true at once when no environment carries TierPlan's route or carve-out.
	applied func(version int64) bool
	// env names the environment whose proxies carry TierPlan's trust, "" for none; it is used for the wake and the log.
	env func() string
	// wake asks the environment's reconciler to render now rather than on its next poll.
	wake func(envID string)
	log  *slog.Logger
}

// stage adds chain to the trust set and, when wait is true, waits for the proxies to apply it, bounded by
// ownTrustStageWait. A timeout is logged at warn and the caller serves the certificate anyway; an ended ctx (the server
// is stopping) returns at once. It must be called with no lock held that a handshake, an install hook or the reconciler
// takes.
func (s *ownStaging) stage(ctx context.Context, chain []*x509.Certificate, wait bool) {
	if s == nil || s.trust == nil || len(chain) == 0 {
		return
	}
	ver := s.trust.Stage(chain)
	if !wait || s.applied == nil {
		return
	}
	env := ""
	if s.env != nil {
		env = s.env()
	}
	if env != "" && s.wake != nil {
		s.wake(env)
	}
	log := s.log
	if log == nil {
		log = slog.Default()
	}
	switch err := s.trust.waitApplied(ctx, ver, s.applied); {
	case errors.Is(err, errOwnTrustStageTimeout):
		log.Warn("the proxies did not apply the trust for TierPlan's new certificate in time; serving it anyway",
			"environment", env, "version", ver, "waited", ownTrustStageWait)
	case err != nil:
		log.Debug("stopped waiting for the proxies to trust TierPlan's new certificate", "environment", env, "version", ver, "error", err)
	}
}

// proxiesApplied is whether every connected, active proxy node reports current as its applied bundle: the per-node rule
// the ingress status counts ready nodes by. A node whose schema the server cannot render for keeps its last bundle for
// good, so it is not waited for; a node that is not connected cannot apply anything and is not waited for either.
func proxiesApplied(nodes []ingresshub.Node, current string, live func(nodeID string) bool) bool {
	for _, n := range nodes {
		if n.Status != "active" || n.Incompatible || (live != nil && !live(n.ID)) {
			continue
		}
		if n.AppliedBundleID != current {
			return false
		}
	}
	return true
}

// certChain is the parsed chain of a pair, leaf first, leaving out a certificate that does not parse.
func certChain(cert tls.Certificate) []*x509.Certificate {
	out := make([]*x509.Certificate, 0, len(cert.Certificate))
	for _, der := range cert.Certificate {
		if c, err := x509.ParseCertificate(der); err == nil {
			out = append(out, c)
		}
	}
	return out
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

	// A staged certificate that is served now is an ordinary member; one whose time is over and was never served leaves
	// without a grace period, because no proxy ever verified TierPlan against it.
	var dropped []*x509.Certificate
	staged := t.incoming[:0:0]
	for _, in := range t.incoming {
		switch {
		case containsCert(next, in.cert):
		case !now.Before(in.until):
			dropped = append(dropped, in.cert)
		default:
			staged = append(staged, in)
		}
	}
	t.incoming = staged
	for _, in := range staged {
		if !containsCert(next, in.cert) {
			next = append(next, in.cert)
		}
	}

	if t.started && fingerprints(next) != fingerprints(t.cur) {
		// What left the set is kept for the grace period; what came back is current again.
		kept := t.prev[:0:0]
		for _, p := range t.prev {
			if !containsCert(next, p.cert) {
				kept = append(kept, p)
			}
		}
		for _, c := range t.cur {
			if !containsCert(next, c) && !containsPrev(kept, c) && !containsCert(dropped, c) {
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

// members is the current set without what is staged: the anchor, then the served leaf, each certificate once. The issuers
// of the served chain are never members (WR-06).
func (t *ownTrust) members() []*x509.Certificate {
	var all []*x509.Certificate
	if t.anchor != nil {
		if a := t.anchor(); a != nil {
			all = append(all, a)
		}
	}
	if t.served != nil {
		_, chain := t.served()
		all = append(all, servedLeafOf(chain)...)
	}
	out := make([]*x509.Certificate, 0, len(all))
	for _, c := range all {
		if c != nil && !containsCert(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// leafOf is the leaf of a chain (its first certificate) as a set of at most one.
func servedLeafOf(chain []*x509.Certificate) []*x509.Certificate {
	if len(chain) == 0 || chain[0] == nil {
		return nil
	}
	return chain[:1]
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
