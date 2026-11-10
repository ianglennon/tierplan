// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package tunnel

import "time"

// FirstFrameTimeout is the shipped default for how long an agent has to answer the
// challenge, for the black-box tests to pin to its documented 3 s.
const FirstFrameTimeout = firstFrameTimeout

// FirstFrame reports the first-frame deadline this hub applies, so a test can read
// what WithFirstFrameTimeout did without waiting for the deadline to pass.
func (h *Hub) FirstFrame() time.Duration { return h.firstFrame }

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

// DialHostOf exposes dialHost to the black-box tests.
func DialHostOf(aud string) string { return dialHost(aud) }
