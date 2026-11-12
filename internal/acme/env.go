// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"fmt"

	"github.com/ianglennon/tierplan/internal/config"
)

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

// LockACME is the field name an ErrLocked carries for the switch TIERPLAN_ACME fixes.
const LockACME = "acme"

// ErrLocked is returned when an action is refused because an environment variable fixes the setting it
// would change (D-04, D-17). The manager returns it itself, so no caller can go around the lock. Only
// the variable's name is ever shown, never its value.
type ErrLocked struct {
	// Field is the setting that is locked, such as LockACME.
	Field string
	// Var is the environment variable that locks it.
	Var string
	// Problem is the code the API answers with.
	Problem string
}

func (e *ErrLocked) Error() string {
	return fmt.Sprintf("%s is set by the %s environment variable", e.Field, e.Var)
}

// Lock says that a variable fixes a setting: what it sets, and what is stored and comes back when the
// variable is removed. Both are in the page's vocabulary, so the stored mode reads "on" or "off".
type Lock struct {
	Var, Value, Stored string
}

// Locks are the settings the environment currently fixes; a nil field is a setting that is free.
type Locks struct {
	ACME *Lock
}

// lockedACME is the refusal of an action TIERPLAN_ACME fixes.
func (m *Manager) lockedACME() error {
	return &ErrLocked{Field: LockACME, Var: config.EnvACME, Problem: "acme_locked"}
}

// locks reports which settings the environment fixes now. st is only read, for the stored values.
func (m *Manager) locks(st State) Locks {
	var l Locks
	if e := m.d.Env.Enabled; e != nil {
		l.ACME = &Lock{Var: config.EnvACME, Value: onOff(*e), Stored: onOff(st.Mode == ModeActive)}
	}
	return l
}

// onOff is a switch in the vocabulary of the variable and the page.
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
