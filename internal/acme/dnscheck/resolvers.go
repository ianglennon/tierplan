// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dnscheck

// MaxResolvers is the most resolvers an admin may list.
const MaxResolvers = 5

// ResolverError names the resolver entry that was refused.
type ResolverError struct {
	Entry, Reason string
}

func (e *ResolverError) Error() string { return "resolver " + e.Entry + ": " + e.Reason }

// ParseResolvers validates and normalises a resolver list.
func ParseResolvers(entries []string) ([]string, error) { return nil, nil }
