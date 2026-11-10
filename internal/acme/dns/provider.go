// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package dns is the seam between the ACME order code and a DNS provider (CHAL-03). A provider
// finds the zone that holds a name and creates, reads and deletes one TXT record by the
// provider's own record ID; everything ACME-specific (the order, the challenge, the ledger of
// records TierPlan created) sits above it and everything provider-specific (Cloudflare's API)
// sits below it. A second provider is one more Factory, with no change to the ACME core.
//
// The package imports nothing from internal/acme, so the ACME core can depend on it and a provider
// package can depend on it without a cycle.
package dns

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The classes a DNS failure maps to. The values are stored and sent to the browser, so they are a
// contract; the English copy and the fix each one gets live in the API and the SPA.
const (
	// ClassTokenInvalid is a token the provider does not accept at all: malformed, unknown,
	// expired or revoked.
	ClassTokenInvalid = "dns_token_invalid" //nolint:gosec // G101: an error class name, not a credential
	// ClassTokenScope is a token the provider accepts but that may not do what TierPlan needs:
	// it lacks the permission to list zones or to edit DNS records, or it sees no zone at all.
	ClassTokenScope = "dns_token_scope" //nolint:gosec // G101: an error class name, not a credential
	// ClassZoneNotFound is a name no zone the token can see holds.
	ClassZoneNotFound = "dns_zone_not_found"
	// ClassDelegationTarget is a name whose _acme-challenge record is delegated to a place the
	// token cannot edit.
	ClassDelegationTarget = "dns_delegation_target"
	// ClassNotVisible is a record that was written but could not be seen through the resolvers
	// in time.
	ClassNotVisible = "dns_not_visible"
	// ClassRateLimited is the provider asking the caller to slow down.
	ClassRateLimited = "dns_rate_limited"
	// ClassUnavailable is the provider not answering, or answering with something unreadable.
	ClassUnavailable = "dns_unavailable"
)

// MaxCommentLen is the longest record comment Cloudflare accepts on its Free plan.
const MaxCommentLen = 100

// ErrIdentical is the provider refusing a record identical to one that exists. It is a sentinel a
// caller can match with errors.Is; the caller holds no ID for that record, so it must not adopt it.
var ErrIdentical = errors.New("an identical record already exists")

// Zone is a DNS zone a token can edit.
type Zone struct {
	ID   string
	Name string
	// NameServers are the zone's authoritative name servers as the provider reports them, which
	// gives a propagation check its servers without a DNS lookup.
	NameServers []string
}

// RecordRef is a record by the provider's own identifiers. It is all a caller needs to read or
// delete the record, and it is what a caller stores to clean up after a restart.
type RecordRef struct {
	ZoneID   string
	RecordID string
}

// Record is a TXT record as the provider holds it.
type Record struct {
	Name string
	// Content is the TXT value with the one pair of double quotes some providers add removed.
	Content string
	Comment string
}

// Provider is one DNS provider used with one token. It is safe to use from one goroutine at a
// time; the token it was built with never outlives it.
type Provider interface {
	// Name is the provider id stored with every ledger row, such as "cloudflare".
	Name() string
	// Zone finds the zone that holds fqdn: the longest match, never above the registrable
	// domain. A failure is a *Error.
	Zone(ctx context.Context, fqdn string) (Zone, error)
	// CreateTXT writes one TXT record in z and returns the provider's reference to it. comment is
	// informational text the provider keeps with the record. A refused duplicate is ErrIdentical.
	CreateTXT(ctx context.Context, z Zone, fqdn, value, comment string) (RecordRef, error)
	// GetTXT reads a record back by its reference.
	GetTXT(ctx context.Context, ref RecordRef) (Record, error)
	// DeleteTXT deletes one record by its reference and never by name or value. A record that is
	// already gone is success.
	DeleteTXT(ctx context.Context, ref RecordRef) error
}

// Factory builds a Provider for one use from a token. The token is held by the Provider it returns
// and by nothing else.
type Factory func(token string) Provider

// Error is a classified DNS failure. It carries the data its copy needs and the status and code the
// provider answered with, and never the request, a header, a body or the token.
type Error struct {
	// Class is one of the Class constants, or empty for a failure that has no class of its own.
	Class string
	// Host is the name the failure is about.
	Host string
	// Zone is the zone the failure is about.
	Zone string
	// Target is the delegated name a CNAME points to, when that is what failed.
	Target string
	// Resolvers are the resolvers a propagation check asked.
	Resolvers []string
	// RetryAfter is the provider's Retry-After for a rate limit.
	RetryAfter time.Duration
	// Status and Code are the HTTP status and the provider's own error code.
	Status, Code int
	// Provider and Op name the call that failed, such as "cloudflare" and "create record".
	Provider, Op string
	// Err is the underlying cause, such as ErrIdentical or a network error.
	Err error
}

// Error is built from the provider, the operation, the status, the code and the class only.
func (e *Error) Error() string {
	var parts []string
	if where := strings.TrimSpace(e.Provider + " " + e.Op); where != "" {
		parts = append(parts, where+":")
	}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.Status))
		if e.Code != 0 {
			parts = append(parts, fmt.Sprintf("code %d", e.Code))
		}
	}
	if e.Class != "" {
		parts = append(parts, "("+e.Class+")")
	}
	return strings.Join(parts, " ")
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// TXTValue is the TXT value that answers a DNS-01 challenge: the unpadded base64url of the SHA-256
// of the key authorization (RFC 8555 section 8.4).
func TXTValue(keyAuth string) string {
	sum := sha256.Sum256([]byte(keyAuth))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ChallengeFQDN is the name the challenge record is written at: _acme-challenge. in front of the
// name, lower-cased and without a trailing dot.
func ChallengeFQDN(name string) string {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	return "_acme-challenge." + name
}

// RecordComment is the comment kept with a record TierPlan created: tierplan:<install-id>:<owner>.
// The owner is an order UUID or test-<8 hex>. It is ASCII with no newline and at most
// MaxCommentLen bytes; anything else in the inputs is dropped.
func RecordComment(installID, owner string) string {
	c := "tierplan:" + installID + ":" + owner
	var b strings.Builder
	for i := 0; i < len(c) && b.Len() < MaxCommentLen; i++ {
		if ch := c[i]; ch >= 0x20 && ch < 0x7f {
			b.WriteByte(ch)
		}
	}
	return b.String()
}
