// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"fmt"
	"slices"

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

// profile is the certificate profile an order asks for: TIERPLAN_ACME_PROFILE when it is set,
// otherwise the saved profile (D-24). Empty is the CA's default.
func (m *Manager) profile(st State) string {
	if p := m.d.Env.Profile; p != "" {
		return p
	}
	return st.Profile
}

// provider is the DNS provider a DNS-01 run uses: TIERPLAN_DNS_PROVIDER when it is set, otherwise the
// saved provider, otherwise the default (D-05).
func (m *Manager) provider(st State) string {
	if p := m.d.Env.DNSProvider; p != "" {
		return p
	}
	return dnsProviderOf(st)
}

// resolvers are the resolvers the DNS-01 propagation check asks: TIERPLAN_DNS_RESOLVERS when it is set,
// even when it is empty, which means the zone's own nameservers, otherwise the saved list. The result is
// a copy, so a caller cannot change what the environment or the state holds.
func (m *Manager) resolvers(st State) []string {
	if m.d.Env.ResolversSet {
		return slices.Clone(m.d.Env.Resolvers)
	}
	return slices.Clone(st.DNSResolvers)
}

// token is the API token of provider: the one the environment gave when it gave one, otherwise the one
// saved. The environment's token is only ever returned from here, held in memory; it is never sealed,
// stored or logged (D-05), so removing the variable leaves the database exactly as it was.
func (m *Manager) token(ctx context.Context, provider string) (string, bool, error) {
	if t := m.d.Env.CloudflareToken; t != "" && provider == m.envProvider() {
		return t, true, nil
	}
	return m.d.Store.DNSToken(ctx, provider)
}

// tokenSaved reports whether a token is available for provider, without opening a saved one. A token
// given by the environment counts as present.
func (m *Manager) tokenSaved(ctx context.Context, provider string) (bool, error) {
	if m.d.Env.CloudflareToken != "" && provider == m.envProvider() {
		return true, nil
	}
	return m.d.Store.DNSTokenSaved(ctx, provider)
}

// envProvider is the provider the environment's token belongs to: the token and the provider are set
// together (config validation), so the token is the provider's.
func (m *Manager) envProvider() string {
	if p := m.d.Env.DNSProvider; p != "" {
		return p
	}
	return defaultDNSProvider
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

// The field names an ErrLocked carries, one for each setting a variable can fix.
const (
	// LockACME is the switch TIERPLAN_ACME fixes.
	LockACME = "acme"
	// LockEmail is the account contact TIERPLAN_ACME_EMAIL fixes (D-17).
	LockEmail = "email"
	// LockAcceptTOS is the agreement to the CA's terms TIERPLAN_ACME_ACCEPT_TOS fixes (D-17).
	LockAcceptTOS = "acceptTos"
	// LockProfile is the certificate profile TIERPLAN_ACME_PROFILE fixes (D-24).
	LockProfile = "profile"
)

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
	ACME      *Lock
	Email     *Lock
	AcceptTOS *Lock
	Profile   *Lock
}

// lockedACME is the refusal of an action TIERPLAN_ACME fixes.
func (m *Manager) lockedACME() error {
	return &ErrLocked{Field: LockACME, Var: config.EnvACME, Problem: "acme_locked"}
}

// registerTerms settles what a registration sends once the variables have spoken (D-17, UI-SPEC A2,
// A3). TIERPLAN_ACME_EMAIL names the address: none typed takes the variable's, the same one is accepted
// and any other is refused. TIERPLAN_ACME_ACCEPT_TOS=true fixes the terms as agreed, so a request that
// does not agree is refused. It reads no state and calls no one, so a refusal leaves everything as it was,
// and an account that already exists is never touched by it: only a registration reads the variables.
func (m *Manager) registerTerms(email string, agreeTOS bool) (string, error) {
	if fixed := m.d.Env.Email; fixed != "" {
		switch email {
		case "":
			email = fixed
		case fixed:
		default:
			return "", &ErrLocked{Field: LockEmail, Var: config.EnvACMEEmail, Problem: "acme_email_locked"}
		}
	}
	if m.d.Env.AcceptTOS && !agreeTOS {
		return "", &ErrLocked{Field: LockAcceptTOS, Var: config.EnvACMEAcceptTOS, Problem: "acme_tos_locked"}
	}
	return email, nil
}

// lockedProfile is the refusal of a profile TIERPLAN_ACME_PROFILE does not name.
func (m *Manager) lockedProfile() error {
	return &ErrLocked{Field: LockProfile, Var: config.EnvACMEProfile, Problem: "acme_profile_locked"}
}

// locks reports which settings the environment fixes now. st is only read, for the stored values.
func (m *Manager) locks(st State) Locks {
	var l Locks
	if e := m.d.Env.Enabled; e != nil {
		l.ACME = &Lock{Var: config.EnvACME, Value: onOff(*e), Stored: onOff(st.Mode == ModeActive)}
	}
	// The contact and the terms have no stored setting beneath them: an account keeps the contact it
	// was registered with, and the agreement is made when registering, so neither carries a stored value.
	if e := m.d.Env.Email; e != "" {
		l.Email = &Lock{Var: config.EnvACMEEmail, Value: e}
	}
	if m.d.Env.AcceptTOS {
		l.AcceptTOS = &Lock{Var: config.EnvACMEAcceptTOS, Value: "true"}
	}
	if p := m.d.Env.Profile; p != "" {
		l.Profile = &Lock{Var: config.EnvACMEProfile, Value: p, Stored: st.Profile}
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
