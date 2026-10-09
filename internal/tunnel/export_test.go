// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

// ClassifyStreamRequest exposes startsStreamRequest to the black-box tests, naming the
// kind it found.
func ClassifyStreamRequest(p []byte) (kind string, ok bool) {
	k, ok := startsStreamRequest(p)
	switch k {
	case streamEvents:
		return "events", ok
	case streamLogs:
		return "logs", ok
	case streamExec:
		return "exec", ok
	}
	return "other", ok
}
