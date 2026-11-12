// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dnscheck

import "github.com/ianglennon/tierplan/internal/acme/resolvers"

// The resolver list parser lives in internal/acme/resolvers, a leaf package that internal/config
// can import without an import cycle. These names keep dnscheck's API unchanged for its callers.

// MaxResolvers is the most resolvers an admin may list.
const MaxResolvers = resolvers.MaxResolvers

// Reasons a ResolverError carries.
const (
	// ReasonInvalid is an entry that is not host or host:port.
	ReasonInvalid = resolvers.ReasonInvalid
	// ReasonTooMany is a list with more than MaxResolvers distinct entries.
	ReasonTooMany = resolvers.ReasonTooMany
)

// ResolverError names the resolver entry that was refused.
type ResolverError = resolvers.ResolverError

// ParseResolvers validates and normalises the admin's resolver list; see resolvers.ParseResolvers.
func ParseResolvers(entries []string) ([]string, error) { return resolvers.ParseResolvers(entries) }
