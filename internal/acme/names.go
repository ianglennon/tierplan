// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

// NameSet is the names a certificate would carry and the reasons it cannot.
type NameSet struct {
	Names    []string
	Refusals []Refusal
}

// WantedNames is not implemented yet.
func WantedNames(publicURL, agentURL string, checkPublic bool) NameSet { return NameSet{} }

// SameNames is not implemented yet.
func SameNames(a, b []string) bool { return false }
