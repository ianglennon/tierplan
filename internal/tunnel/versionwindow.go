// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ianglennon/bayplan/proto/wire"
)

const (
	// MaxClockOffset is the skew beyond which an agent is refused (D-45).
	MaxClockOffset = 5 * time.Minute
	// ClockWarnOffset is the skew beyond which the session carries a warning.
	ClockWarnOffset = 30 * time.Second
)

// Compatible decides whether a server of serverVersion accepts an agent of
// agentVersion that speaks protocol proto (D-25, Pitfall 23). The rule lives here
// and nowhere else, so it can be read, tested and changed in one place:
//
//   - The protocol integer must equal wire.ProtoVersion. Another one is refused by
//     every server, as too old when it is lower.
//   - Versions are vMAJOR.MINOR.PATCH[-pre]. Between two such versions the agent's
//     major must be the server's, and its minor the server's or the one before:
//     a v0.3 server accepts v0.2 and v0.3 agents, whatever their patch level or
//     prerelease suffix.
//   - Anything else, such as "dev (abc123)" or an edge build, cannot be placed in
//     a window and is accepted, on either side, as long as the protocol matches.
//
// When ok is false, tooOld says whether the agent is behind (true) or ahead
// (false), and detail is a sentence for the agent's operator.
func Compatible(serverVersion, agentVersion string, proto int) (ok bool, tooOld bool, detail string) {
	if proto != wire.ProtoVersion {
		return false, proto < wire.ProtoVersion,
			fmt.Sprintf("this server speaks protocol %d, the agent sent %d", wire.ProtoVersion, proto)
	}
	sMaj, sMin, sok := parseRelease(serverVersion)
	aMaj, aMin, aok := parseRelease(agentVersion)
	if !sok || !aok {
		return true, false, ""
	}
	window := fmt.Sprintf("the server is %d.%d and accepts agents of %d.%d and %d.%d", sMaj, sMin, sMaj, max(sMin-1, 0), sMaj, sMin)
	switch {
	case aMaj < sMaj || (aMaj == sMaj && aMin < sMin-1):
		return false, true, fmt.Sprintf("the agent (%d.%d) is too old: %s", aMaj, aMin, window)
	case aMaj > sMaj || aMin > sMin:
		return false, false, fmt.Sprintf("the agent (%d.%d) is too new: %s", aMaj, aMin, window)
	}
	return true, false, ""
}

// parseRelease reads vMAJOR.MINOR.PATCH[-pre][+build], with or without the v. It
// reports ok false for anything else.
func parseRelease(v string) (major, minor int, ok bool) {
	n, ok := parseVersion(v)
	return n[0], n[1], ok
}

// ParseVersion reads vMAJOR.MINOR.PATCH[-pre][+build], with or without the v, into
// its three numbers. It reports ok false for anything else, such as "edge".
func ParseVersion(v string) (n [3]int, ok bool) { return parseVersion(v) }

// parseVersion is parseRelease with the patch level as well.
func parseVersion(v string) (n [3]int, ok bool) {
	core := strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, _, _ = strings.Cut(core, "+")
	core, _, _ = strings.Cut(core, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	for i, p := range parts {
		// Atoi accepts a sign and surrounding nothing else; only digits are allowed.
		if p == "" || len(p) > 9 || strings.Trim(p, "0123456789") != "" {
			return [3]int{}, false
		}
		n[i], _ = strconv.Atoi(p)
	}
	return n, true
}

// Behind reports whether an agent of agentVersion is older than a server of
// serverVersion by a minor or a patch: the agent could be updated. Both must be
// releases, vMAJOR.MINOR.PATCH with an optional prerelease or build suffix; a
// suffix is ignored, so v0.3.0-rc.1 is v0.3.0. Anything else, and an agent that is
// the same or newer, is not behind.
func Behind(serverVersion, agentVersion string) bool {
	s, sok := parseVersion(serverVersion)
	a, aok := parseVersion(agentVersion)
	if !sok || !aok {
		return false
	}
	for i := range s {
		if a[i] != s[i] {
			return a[i] < s[i]
		}
	}
	return false
}

// CheckClock refuses a clock that is further from the server's than
// MaxClockOffset, with the code clock_skew. offset is how far the agent's clock is
// ahead of the server's. A Verifier calls it at the point of its own checks where
// it belongs, which for an enrolment is before a certificate is issued.
func CheckClock(offset time.Duration) error {
	if abs(offset) <= MaxClockOffset {
		return nil
	}
	dir := "ahead of"
	if offset < 0 {
		dir = "behind"
	}
	return &Refusal{
		Code: wire.ReasonClockSkew,
		Detail: fmt.Sprintf("this host's clock is %s %s the server's; set it correctly, for example with NTP",
			abs(offset).Round(time.Second), dir),
		AuditReason: wire.ReasonClockSkew,
	}
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// WithServerVersion tells the hub which version this server is. The handshake
// checks every agent against it with Compatible, and the challenge carries it. An
// empty or non-release version accepts any agent of the same protocol. Call it
// before the hub serves.
func (h *Hub) WithServerVersion(v string) *Hub {
	h.serverVersion = v
	return h
}

// WithUpdateHint sets text added to a version_incompatible refusal: the manual
// command that brings the agent up to date. tooOld says which side is behind, and
// envType is "swarm" when the agent reported an active swarm and "standalone"
// otherwise, so the hint can name the command for that kind of host (D-25). Nil
// adds nothing. Call it before the hub serves.
func (h *Hub) WithUpdateHint(f func(tooOld bool, envType string) string) *Hub {
	h.updateHint = f
	return h
}
