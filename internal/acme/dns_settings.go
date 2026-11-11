// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/tierplan/internal/acme/dnscheck"
)

// The most names tested at once. A certificate carries two names at most, so this is the whole list.
const testParallel = 2

var (
	// ErrDNSTokenRequired means a DNS API token is needed and none was typed or saved.
	ErrDNSTokenRequired = errors.New("a DNS API token is required")
	// ErrDNSTokenInUse means the token cannot be removed because DNS-01 is the saved validation method
	// and renewals would silently stop.
	ErrDNSTokenInUse = errors.New("the DNS API token is in use: DNS-01 is the saved validation method")
	// ErrChallengeUnknown means the validation method is neither http-01 nor dns-01.
	ErrChallengeUnknown = errors.New("the validation method is not http-01 or dns-01")
)

// ValidationInput is one save of the validation settings.
type ValidationInput struct {
	// ChallengeType is ChallengeHTTP01 or ChallengeDNS01.
	ChallengeType string
	// Token is the DNS API token the admin typed, empty to keep the saved one. A typed token is tested
	// before anything is stored.
	Token string
	// Resolvers is the admin's resolver list, nil to keep the saved one. It is validated and normalised.
	Resolvers *[]string
}

// SaveResult says whether a save stored anything. Test is the result of the token test a save ran, and
// is nil when no token was typed.
type SaveResult struct {
	Saved bool
	Test  *TokenTest
}

// TestRequest is one Test token press.
type TestRequest struct {
	// Token is the typed token; empty tests the saved one.
	Token string
	// Resolvers is the draft resolver list, nil for the saved one.
	Resolvers *[]string
}

// claimForDNS runs the guards of an action that holds the DNS slot and takes the slot. opMu is held
// only for that: the network work that follows can take half a minute, and every other admin change is
// refused with ErrRunActive while the slot is held, so nothing slips in. The returned func gives the
// slot back.
func (m *Manager) claimForDNS(ctx context.Context) (State, func(), error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	st, err := m.mutable(ctx)
	if err != nil {
		return State{}, nil, err
	}
	if !m.claimDNS() {
		return State{}, nil, ErrRunActive
	}
	return st, m.releaseDNS, nil
}

// dnsProviderOf is the provider id the saved settings use.
func dnsProviderOf(st State) string {
	if st.DNSProvider == "" {
		return defaultDNSProvider
	}
	return st.DNSProvider
}

// resolversOf are the normalised resolvers a request tests with and a save stores: the draft list when
// there is one, else the saved list. A draft that cannot be parsed is returned as the *dnscheck.ResolverError
// that names the entry.
func resolversOf(st State, draft *[]string) ([]string, error) {
	if draft == nil {
		return st.DNSResolvers, nil
	}
	return dnscheck.ParseResolvers(*draft)
}

// tokenUsable reports whether token can be a bearer token: 1 to 256 printable ASCII characters, with no
// space or control character in it. Anything else is dns_token_invalid without a request being made.
func tokenUsable(token string) bool {
	if token == "" || len(token) > maxTokenBytes {
		return false
	}
	for i := 0; i < len(token); i++ {
		if c := token[i]; c <= 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// maxTokenBytes is the longest DNS API token that is tried.
const maxTokenBytes = 256

// runTokenTest tests token against the names an order would carry now, with no ACME order placed and
// the CA never contacted. The names are those Issue would use under DNS-01 and the refusals are Issue's;
// each name is tested with a provider of its own, at most testParallel at a time, and the results are
// listed in certificate-name order whichever finishes first. The caller holds the DNS slot.
func (m *Manager) runTokenTest(ctx context.Context, st State, provider, token string, custom []dnscheck.Server) (TokenTest, error) {
	st.ChallengeType = ChallengeDNS01 // a base path is no reason to refuse a DNS-01 name (D-04)
	ns, refusals := m.preflight(st)
	if len(refusals) > 0 {
		return TokenTest{}, &RefusedError{Refusals: refusals}
	}
	factory, ok := m.d.DNS.Providers[provider]
	if !ok {
		return TokenTest{}, fmt.Errorf("no DNS provider %q is built in", provider)
	}
	installID, err := m.d.Store.InstallID(ctx)
	if err != nil {
		return TokenTest{}, err
	}

	usable := tokenUsable(token)
	results := make([]NameTest, len(ns.Names))
	var g errgroup.Group
	g.SetLimit(testParallel)
	for i, name := range ns.Names {
		g.Go(func() error {
			if !usable {
				results[i] = invalidTokenTest(name)
				return nil
			}
			results[i] = m.testName(ctx, factory(token), name, custom, installID)
			return nil
		})
	}
	_ = g.Wait() // the routines return no error: a failure is a row of the result

	t := TokenTest{Passed: true, Names: results}
	for _, n := range results {
		if !n.passed() {
			t.Passed = false
		}
	}
	return t, nil
}

// SaveValidation saves the challenge type, the DNS resolvers and, when one is typed, the DNS token. A
// typed token is tested first against the current certificate names and is stored only when every name
// passed: on a failed name nothing at all is stored, and the result says so (D-07). The token, the
// settings, the closing of an order made under the old method and the reset of a DNS-side backoff are
// written in one transaction, so an interrupted save leaves the previous settings and token whole.
//
// While it runs it holds the DNS slot: a run, another save, a test and the scheduler see a run as
// active, so a test and an order never write _acme-challenge at once.
func (m *Manager) SaveValidation(ctx context.Context, in ValidationInput) (SaveResult, error) {
	st, release, err := m.claimForDNS(ctx)
	if err != nil {
		return SaveResult{}, err
	}
	defer release()

	resolvers, err := resolversOf(st, in.Resolvers)
	if err != nil {
		return SaveResult{}, err
	}
	provider := dnsProviderOf(st)
	token := strings.TrimSpace(in.Token)

	var test *TokenTest
	if token != "" {
		t, err := m.runTokenTest(ctx, st, provider, token, dnscheck.Resolvers(resolvers))
		if err != nil {
			return SaveResult{}, err
		}
		if !t.Passed {
			return SaveResult{Test: &t}, nil
		}
		test = &t
	}

	m.opMu.Lock()
	defer m.opMu.Unlock()
	// The guards again, on the state as it is now: a restore may have paused Let's Encrypt meanwhile.
	if st, err = m.mutable(ctx); err != nil {
		return SaveResult{}, err
	}
	_, hasAccount, err := m.d.Client.Account(ctx)
	if err != nil {
		return SaveResult{}, err
	}
	methodChanged := challengeOf(st) != in.ChallengeType
	changed := methodChanged || token != "" || !slices.Equal(st.DNSResolvers, resolvers)
	w := ValidationWrite{
		ChallengeType: in.ChallengeType, Provider: provider, Resolvers: resolvers,
		// Untested stays as it was unless this save changes something a run has not proved yet.
		Untested:         st.ValidationUntested || (hasAccount && changed),
		CloseOpenOrder:   methodChanged,
		ResetDNSFailures: changed && strings.HasPrefix(st.LastErrorClass, "dns_"),
		Now:              m.clock.Now(),
	}
	if token != "" {
		w.Token = &token
	}
	if err := m.d.Store.SaveValidation(ctx, w); err != nil {
		return SaveResult{}, err
	}
	return SaveResult{Saved: true, Test: test}, nil
}

// TestToken tests a DNS token against the current certificate names without storing anything and
// without placing an order or contacting the CA (CHAL-02). It tests the typed token, or else the saved
// one, with the draft resolvers or else the saved ones. A typed token that cannot be a bearer token, or
// one that is blank and none is saved, is refused or fails every name before any request is made.
//
// It holds the DNS slot while it runs, like SaveValidation does.
func (m *Manager) TestToken(ctx context.Context, req TestRequest) (TokenTest, error) {
	st, release, err := m.claimForDNS(ctx)
	if err != nil {
		return TokenTest{}, err
	}
	defer release()

	resolvers, err := resolversOf(st, req.Resolvers)
	if err != nil {
		return TokenTest{}, err
	}
	provider := dnsProviderOf(st)
	token := strings.TrimSpace(req.Token)
	if token == "" {
		saved, found, err := m.d.Store.DNSToken(ctx, provider)
		if err != nil {
			return TokenTest{}, err
		}
		if !found {
			return TokenTest{}, ErrDNSTokenRequired
		}
		token = saved
	}
	return m.runTokenTest(ctx, st, provider, token, dnscheck.Resolvers(resolvers))
}

// RemoveToken deletes the saved DNS token. It is refused while DNS-01 is the saved method.
func (m *Manager) RemoveToken(ctx context.Context) error {
	return errors.New("not implemented")
}
