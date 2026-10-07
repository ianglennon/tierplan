// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package compose is the D-13 privileged-compose checker.
package compose

import "errors"

// MaxSize is the largest compose file Check will read.
const MaxSize = 1 << 20

// ErrTooLarge is returned for a file larger than MaxSize.
var ErrTooLarge = errors.New("compose: file larger than 1 MiB")

// Finding is one host-escalation feature found in a compose file.
type Finding struct {
	Service string
	Key     string
	Value   string
	Line    int
	Column  int
	Rule    string
	Why     string
}

// Check returns every host-escalation finding in src.
func Check(src []byte) ([]Finding, error) { return nil, nil }

// NeedsEnvAdmin reports whether fs holds at least one finding.
func NeedsEnvAdmin(fs []Finding) bool { return false }

// Format renders a finding for an error message.
func Format(f Finding) string { return "" }
