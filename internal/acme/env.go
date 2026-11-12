// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

// The TIERPLAN_ACME* variables override what is stored (D-02): a variable that is set decides, and one
// that is unset leaves the stored setting in force.
//
// The rule that keeps this safe is that an environment value is NEVER written to the database, so
// removing the variable always brings back exactly the value that was stored. That is why the overlay is
// this set of pure accessors, used only where a decision is made (what is served, whether a renewal
// starts, which challenge a run answers, whether an action is refused), and is never folded into the
// State that Store.State returns. Every read-modify-write site (SetProfile, Stop, renewalRound, Resume,
// reuse) keeps saving the raw state it read; a State that had the overlay applied and was then saved
// would persist the variable (RESEARCH F3).

// envOn reports whether TIERPLAN_ACME=on.
func (m *Manager) envOn() bool {
	e := m.d.Env.Enabled
	return e != nil && *e
}

// envOff reports whether TIERPLAN_ACME=off.
func (m *Manager) envOff() bool {
	e := m.d.Env.Enabled
	return e != nil && !*e
}

// mode is the mode that decides what happens now: ModeOff while TIERPLAN_ACME=off, whatever is stored,
// otherwise the stored mode. The stored mode itself is untouched.
func (m *Manager) mode(st State) string {
	if m.envOff() {
		return ModeOff
	}
	return st.Mode
}

// challenge is the challenge type a run answers: TIERPLAN_ACME_CHALLENGE when it is set, otherwise the
// saved one.
func (m *Manager) challenge(st State) string {
	if c := m.d.Env.Challenge; c != "" {
		return c
	}
	return challengeOf(st)
}

// ready reports whether the listener is bound, so an order placed now can be validated: the CA reaches
// the HTTP-01 answer only once the server accepts connections. A manager with no Ready channel is ready.
func (m *Manager) ready() bool {
	if m.d.Ready == nil {
		return true
	}
	select {
	case <-m.d.Ready:
		return true
	default:
		return false
	}
}
