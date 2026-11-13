// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	mrand "math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v5/acme/api"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/cloudflare"
	"github.com/ianglennon/tierplan/internal/acme/dnscheck"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
)

const (
	// JobKind is the kind of the jobs.Hub job an issue or a renewal runs as.
	JobKind = "acme"
	// OwnerScheduler is the job owner of a run no person started.
	OwnerScheduler = "system:acme"
)

// What started a run.
const (
	TriggerAdmin    = "admin"
	TriggerSchedule = "schedule"
	TriggerResume   = "resume"
)

// The outcomes of a run, as the run history records them.
const (
	OutcomeRunning   = "running"
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	// OutcomeCancelled is a run a person cancelled through the jobs API. It is not a failure: the
	// ledger is left as it was, and the order stays open for the next run to pick up.
	OutcomeCancelled = "cancelled"
)

var (
	// ErrRunActive means a run is in progress. Only one run exists at a time, whoever started it.
	ErrRunActive = errors.New("a certificate run is already in progress")
	// ErrPaused means Let's Encrypt is paused after a restore until an admin resumes it. No request
	// is made to the CA while it is.
	ErrPaused = errors.New("Let's Encrypt is paused after a restore") //nolint:staticcheck // ST1005: Let\'s Encrypt is a proper name, and this is the text the screens show
	// ErrCertFiles means TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE are set: the files are
	// served and the ACME setup is disabled (D-12, D-13).
	ErrCertFiles = errors.New("certificate files are configured, so Let's Encrypt is disabled")
	// ErrHSTSOn means HSTS is on, so Let's Encrypt cannot be stopped: browsers hold a policy for the host
	// and would refuse the self-signed certificate that replaces it. Turn HSTS off first (D-26).
	ErrHSTSOn = errors.New("HSTS is on, so Let's Encrypt cannot be stopped") //nolint:staticcheck // ST1005: Let's Encrypt is a proper name, and this is the text the screens show
	// ErrNotActive means there is no Let's Encrypt certificate in use to renew or stop.
	ErrNotActive = errors.New("Let's Encrypt is not in use") //nolint:staticcheck // ST1005: Let\'s Encrypt is a proper name, and this is the text the screens show
	// ErrNotPaused means there is nothing to resume.
	ErrNotPaused = errors.New("Let's Encrypt is not paused") //nolint:staticcheck // ST1005: Let\'s Encrypt is a proper name, and this is the text the screens show
	// ErrProfileUnknown means the directory does not offer the profile.
	ErrProfileUnknown = errors.New("the certificate authority does not offer that profile")
	// errRunUnrecorded means the end of the last run is not stored yet, so the ledger cannot be trusted
	// to say whether another may start.
	errRunUnrecorded = errors.New("the end of the last certificate run is not recorded yet")
)

// RefusedError is returned by Issue when a preflight check refuses to order: every refusal at once,
// and no request was made to the CA.
type RefusedError struct{ Refusals []Refusal }

func (e *RefusedError) Error() string {
	codes := make([]string, len(e.Refusals))
	for i, r := range e.Refusals {
		codes[i] = r.Code
	}
	return "the certificate cannot be ordered: " + strings.Join(codes, ", ")
}

// BlockedError is returned by Issue and RenewNow while the ledger forbids a manual attempt: the CA
// told us to wait (Reason BlockToldToWait) or three runs failed within the hour (BlockFailureGuard).
// Until is the instant the attempt is allowed again.
type BlockedError struct {
	Until  time.Time
	Reason string
	// Wait is how long from the moment of the refusal until Until, by the manager's own clock; the
	// API sends it as Retry-After.
	Wait time.Duration
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("a certificate attempt is blocked until %s (%s)", e.Until.UTC().Format(time.RFC3339), e.Reason)
}

// Installer swaps the certificate the server serves. The server implements it (mirror first, then the
// live swap); this package never touches a listener. A swap that returns an error has changed nothing:
// the certificate served before it is still the one served.
type Installer interface {
	// ServeACME serves the Let's Encrypt chain and key (PEM) from now on.
	ServeACME(chainPEM, keyPEM []byte) error
	// ServeSelfSigned serves the install's self-signed certificate again.
	ServeSelfSigned() error
}

// Deps is what a Manager works with.
type Deps struct {
	Store      *Store
	Client     *Client
	Challenges *Challenges
	Jobs       *jobs.Hub
	Installer  Installer
	// Clock is the only source of time; nil is the wall clock.
	Clock stackops.Clock
	// PublicURL, AgentURL and BasePath are read on every use, so a change of URL is seen at once.
	PublicURL, AgentURL, BasePath func() string
	// DialHosts are the hosts agents have dialed.
	DialHosts func() []string
	// CertFiles is whether TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE are set.
	CertFiles bool
	// HSTSOn is the effective HSTS setting. Stop using Let's Encrypt is refused while it reports true,
	// because serving the self-signed certificate under a policy browsers hold would lock them out
	// (D-26). Nil is off. The expiry fallback does not ask it: that path withdraws the header itself.
	HSTSOn func() bool
	// HSTSHold runs fn while the HSTS setting cannot change, so the check of HSTSOn and the swap to the
	// self-signed certificate in Stop are one step and a toggle cannot slip in between them (D-26). Nil
	// runs fn at once.
	HSTSHold func(fn func() error) error
	// Env is what the TIERPLAN_ACME* variables say. They override the stored settings at the decision
	// sites in env.go and are never saved (D-02).
	Env config.ACMEEnv
	// Ready is closed once the listener is bound, so an order placed at start can be validated; nil
	// means ready.
	Ready <-chan struct{}
	// DNS is what DNS-01 works with; its nil fields take the production defaults.
	DNS DNSDeps
	Log *slog.Logger
	// Rand picks in [0, n), for the scheduler's choice inside an ARI window; nil is math/rand/v2.
	Rand func(n int64) int64
	// Jitter is the backoff jitter in [-0.1, 0.1]; nil is math/rand/v2.
	Jitter func() float64
	// RunEnded is a test seam: it is told when a run has ended, after its outcome is stored and
	// before its last job item is finished.
	RunEnded func(runID, outcome string)
	// Audit writes one audit row; the server sets it to an audit.Writer's Record. Every run that ends in a
	// certificate or in an error writes one row as system:acme before RunEnded is told (Phase 17, SET-04). Nil writes
	// nothing, so a Manager built without it works as before. A failed write is logged at error level and never fails
	// the run.
	Audit func(ctx context.Context, e audit.Entry) error
}

// RunStart says what an Issue or a renewal did: started a job, or found nothing to do.
type RunStart struct {
	// JobID is the jobs.Hub job of the run; empty when Reused.
	JobID string
	// Reused is true when the stored certificate already covers the wanted names and is not yet due
	// for renewal, so no order was placed.
	Reused bool
	// ValidUntil is the end of that certificate when Reused.
	ValidUntil time.Time
	// hold keeps an admin-started run from beginning until the caller's WithRunStarted hook has run; nil for any
	// other run (audit.go).
	hold *runHold
}

// Manager is the admin's way into ACME: register, issue, renew now, stop, resume and read the status.
// It runs one order at a time, in the background, and applies the ledger around every run.
type Manager struct {
	d     Deps
	clock stackops.Clock
	log   *slog.Logger
	// checker asks DNS servers whether a TXT record shows and whether a name is delegated.
	checker *dnscheck.Checker

	// opMu serialises the admin's mutations, so two requests cannot both pass the guards.
	opMu sync.Mutex

	// mu guards active, dnsBusy, unrecorded, wire and installErr.
	mu     sync.Mutex
	active *activeRun
	// dnsBusy is true while a Test token or a validation save holds the DNS slot. It makes runActive
	// true, so no run starts and no other admin change is accepted while a test writes
	// _acme-challenge, but it is not a run: Status.ActiveRun follows active alone.
	dnsBusy bool
	// unrecorded is the end of a run that could not be written when the run ended. Until it is, the
	// stored ledger does not know about that run, so no run starts and every admin change writes it
	// first.
	unrecorded *runEnd
	// wire is what the Installer serves, as far as this manager has seen, and installErr the last
	// refusal that was logged (scheduler.go).
	wire       servedCert
	installErr string
	// envNote is the last refusal of the start-time issue that was logged, so one that lasts is logged
	// once and not every tick (envissue.go).
	envNote string

	// suggest is the last answer of the DNS-01 suggestion's host lookup (dns_status.go).
	suggest suggestCache
}

// activeRun is the one run in progress.
type activeRun struct {
	jobID, kind, trigger string
	// challenge is the challenge type the run answers.
	challenge string
	startedAt time.Time
}

// NewManager is a Manager over d. Nil optional fields take their defaults.
func NewManager(d Deps) *Manager {
	if d.Clock == nil {
		d.Clock = stackops.RealClock()
	}
	if d.Challenges == nil {
		d.Challenges = NewChallenges()
	}
	if d.Rand == nil {
		d.Rand = mrand.Int64N
	}
	if d.Jitter == nil {
		d.Jitter = func() float64 { return (mrand.Float64()*2 - 1) * maxJitter } //nolint:gosec // G404: spreads the renewal time of one install; it is not a secret and needs no cryptographic randomness
	}
	if d.DNS.Providers == nil {
		d.DNS.Providers = map[string]dns.Factory{defaultDNSProvider: cloudflare.New(cloudflare.Options{UserAgent: "TierPlan"})}
	}
	if d.DNS.Querier == nil {
		d.DNS.Querier = dnscheck.NewQuerier()
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		d: d, clock: d.Clock, log: log.With("component", "acme"),
		checker: &dnscheck.Checker{
			Q: d.DNS.Querier, Clock: d.Clock, LookupIP: d.DNS.LookupIP,
			NameserverAddrs: d.DNS.NameserverAddrs, ResolvConf: d.DNS.ResolvConf,
		},
	}
	if d.Installer != nil {
		// Every swap goes through the tracker, so the scheduler knows what is being served.
		m.d.Installer = trackedInstaller{Installer: d.Installer, m: m}
	}
	return m
}

// Challenges is the HTTP-01 token answerer the plain listener serves.
func (m *Manager) Challenges() *Challenges { return m.d.Challenges }

func (m *Manager) publicURL() string { return call(m.d.PublicURL) }
func (m *Manager) agentURL() string  { return call(m.d.AgentURL) }
func (m *Manager) basePath() string  { return call(m.d.BasePath) }

func call(f func() string) string {
	if f == nil {
		return ""
	}
	return f()
}

func (m *Manager) dialHosts() []string {
	if m.d.DialHosts == nil {
		return nil
	}
	return m.d.DialHosts()
}

// preflight decides the names and every refusal, from the URLs as they are now.
func (m *Manager) preflight(st State) (NameSet, []Refusal) {
	return Preflight(PreflightInput{
		PublicURL: m.publicURL(), AgentURL: m.agentURL(), BasePath: m.basePath(),
		ChallengeType: m.challenge(st), DirectoryOverridden: m.d.Client.Directory().Overridden,
		DialHosts: m.dialHosts(),
	})
}

// challengeOf is the challenge type the saved state asks for; a state that names none is HTTP-01.
func challengeOf(st State) string {
	if st.ChallengeType == "" {
		return ChallengeHTTP01
	}
	return st.ChallengeType
}

// mutable reads the state for an action that changes something or asks the CA: it is refused when
// certificate files are set or after a restore, before anything else is looked at.
func (m *Manager) mutable(ctx context.Context) (State, error) {
	if m.d.CertFiles {
		return State{}, ErrCertFiles
	}
	if err := m.settle(ctx); err != nil {
		return State{}, err
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		return State{}, err
	}
	if st.PausedReason != "" {
		return State{}, ErrPaused
	}
	return st, nil
}

// runActive reports whether a run is in progress or a Test token holds the DNS slot: either way the
// admin's changes and the scheduler leave things alone.
func (m *Manager) runActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active != nil || m.dnsBusy
}

// claimDNS takes the DNS slot unless a run or another test holds it.
func (m *Manager) claimDNS() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil || m.dnsBusy {
		return false
	}
	m.dnsBusy = true
	return true
}

// releaseDNS gives the DNS slot back.
func (m *Manager) releaseDNS() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dnsBusy = false
}

// Start puts the manager in its state at process start (D-15): runs a crash left running are closed,
// and a stored, unexpired Let's Encrypt certificate is served when Let's Encrypt is in use and no
// certificate files are set. An expired certificate is never installed. It returns an error only for
// a store failure; a stored certificate that cannot be opened or installed is logged and recorded as
// the last error, and start-up goes on with the self-signed certificate.
func (m *Manager) Start(ctx context.Context) error {
	now := m.clock.Now()
	if err := m.d.Store.InterruptRunning(ctx, now); err != nil {
		return err
	}
	// Records an earlier process could not delete are not tried here: a sweep makes one provider call
	// for each row, so it would hold up start-up, and the stored certificate with it, for as long as
	// the provider takes to answer. The scheduler's first round deletes every row whose next try has
	// come, and a row never tried has none (D-17).
	if m.d.CertFiles {
		return nil
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		return err
	}
	if m.mode(st) != ModeActive {
		return nil
	}
	chain, key, found, err := m.d.Store.Certificate(ctx)
	if err != nil {
		m.log.Error("the stored Let's Encrypt certificate cannot be opened, so it is not served", "error", err)
		return m.recordInternal(ctx, "open the stored certificate: "+err.Error())
	}
	if !found {
		return nil
	}
	leaf, err := leafOfChain(chain)
	if err != nil {
		m.log.Error("the stored Let's Encrypt certificate cannot be read, so it is not served", "error", err)
		return m.recordInternal(ctx, "read the stored certificate: "+err.Error())
	}
	if !now.Before(leaf.NotAfter) {
		m.log.Warn("the stored Let's Encrypt certificate has expired, so it is not served", "expired_at", leaf.NotAfter)
		return nil
	}
	keyPEM, err := encodeKey(key)
	if err == nil {
		err = m.d.Installer.ServeACME(chain, keyPEM)
	}
	if err != nil {
		m.log.Error("the stored Let's Encrypt certificate could not be served", "error", err)
		return m.recordInternal(ctx, "serve the stored certificate: "+err.Error())
	}
	return nil
}

// Register creates the Let's Encrypt account (D-06). While TIERPLAN_ACME_EMAIL or
// TIERPLAN_ACME_ACCEPT_TOS is set the variable decides what is registered (registerTerms), and a request
// that disagrees is refused with ErrLocked before anything is read or sent.
func (m *Manager) Register(ctx context.Context, email string, agreeTOS bool) (Account, error) {
	email, err := m.registerTerms(email, agreeTOS)
	if err != nil {
		return Account{}, err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if _, err := m.mutable(ctx); err != nil {
		return Account{}, err
	}
	return m.d.Client.Register(ctx, email, agreeTOS)
}

// Directory is what the CA says about itself: its terms and the profiles it offers.
func (m *Manager) Directory(ctx context.Context) (DirectoryMeta, error) {
	st, err := m.d.Store.State(ctx)
	if err != nil {
		return DirectoryMeta{}, err
	}
	if st.PausedReason != "" {
		return DirectoryMeta{}, ErrPaused
	}
	return m.d.Client.Meta(ctx)
}

// SetProfile saves the certificate profile the next orders ask for; empty is the CA's default. A name
// the directory does not offer is refused when the directory can be read, and accepted as given when
// it cannot.
//
// While TIERPLAN_ACME_PROFILE is set it decides (D-24): the variable's own value is accepted and changes
// nothing, since the page echoes what it was shown, and any other value is refused with ErrLocked. Both
// answers come before anything is read or stored, so a refused request leaves the saved profile as it was.
func (m *Manager) SetProfile(ctx context.Context, profile string) error {
	if fixed := m.d.Env.Profile; fixed != "" {
		if profile == fixed {
			return nil
		}
		return m.lockedProfile()
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	st, err := m.mutable(ctx)
	if err != nil {
		return err
	}
	if m.runActive() {
		return ErrRunActive
	}
	if profile != "" {
		if meta, err := m.d.Client.Meta(ctx); err == nil && len(meta.Profiles) > 0 {
			if _, ok := meta.Profiles[profile]; !ok {
				return ErrProfileUnknown
			}
		}
	}
	st.Profile = profile
	st.UpdatedAt = m.clock.Now()
	return m.d.Store.SaveState(ctx, st)
}

// Issue orders a certificate for the names the URLs say now, in the background. It is the only way
// new names are ordered by a person (D-05): a URL change, a save or a scheduled round never starts one
// (TIERPLAN_ACME=on is the one exception, envissue.go).
//
// The guards run in this order, each before anything that could reach the CA: certificate files,
// pause, account, a run in progress, every preflight refusal, the ledger's manual block, and reuse of
// a stored certificate that already covers the names and is not yet due (issueLocked).
func (m *Manager) Issue(ctx context.Context, actor string) (RunStart, error) {
	// TIERPLAN_ACME=off fixes Let's Encrypt off; on leaves Issue allowed, as the manual retry (D-04).
	if m.envOff() {
		return RunStart{}, m.lockedACME()
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	st, err := m.mutable(ctx)
	if err != nil {
		return RunStart{}, err
	}
	rs, err := m.issueLocked(ctx, st, actor, TriggerAdmin, nil, false)
	// The caller's hook runs while the new run waits, so a row it writes comes before the run's outcome row.
	announceStarted(runStartedFrom(ctx), rs)
	return rs, err
}

// RenewNow renews the certificate now, for the names it already carries: never the names the URLs
// want today (D-05). It names the certificate it replaces when the CA offers renewal information.
func (m *Manager) RenewNow(ctx context.Context, actor string) (RunStart, error) {
	if m.envOff() {
		return RunStart{}, m.lockedACME()
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	st, err := m.mutable(ctx)
	if err != nil {
		return RunStart{}, err
	}
	if m.runActive() {
		return RunStart{}, ErrRunActive
	}
	if m.mode(st) != ModeActive {
		return RunStart{}, ErrNotActive
	}
	leaf, found, err := m.storedLeaf(ctx)
	if err != nil {
		return RunStart{}, err
	}
	if !found {
		return RunStart{}, ErrNotActive
	}
	if err := m.needAccount(ctx); err != nil {
		return RunStart{}, err
	}
	if err := m.manualBlock(ctx, st); err != nil {
		return RunStart{}, err
	}
	rs, err := m.startRun(runParams{
		trigger: TriggerAdmin, actor: actor, challenge: m.challenge(st),
		req: m.renewRequest(ctx, st, leaf),
	})
	announceStarted(runStartedFrom(ctx), rs)
	return rs, err
}

// renewRequest is the order that renews leaf: its own names, the profile in force (m.profile), and the certificate
// it replaces when the directory offers renewalInfo and leaf was issued to the account that orders the
// renewal. A CA refuses to let one account replace a certificate issued to another, so after the CA
// forgot the account and a new one was registered the renewal names nothing; so does a certificate
// whose account is not known.
func (m *Manager) renewRequest(ctx context.Context, st State, leaf *x509.Certificate) OrderRequest {
	req := OrderRequest{Kind: KindRenew, Names: slices.Clone(leaf.DNSNames), Profile: m.profile(st)}
	if meta, err := m.d.Client.Meta(ctx); err != nil || !meta.RenewalInfo {
		return req
	}
	if !m.issuedToCurrentAccount(ctx) {
		return req
	}
	if id, err := api.MakeARICertID(leaf); err == nil {
		req.Replaces = id
	}
	return req
}

// issuedToCurrentAccount reports whether the stored certificate was issued to the account registered
// with the directory now. A failure to tell is a no.
func (m *Manager) issuedToCurrentAccount(ctx context.Context) bool {
	acct, found, err := m.d.Store.AccountInfo(ctx, m.d.Client.Directory().URL)
	if err != nil || !found || acct.URL == "" {
		return false
	}
	issuedTo, err := m.d.Store.CertAccountURL(ctx)
	if err != nil {
		m.log.Warn("the account of the stored certificate cannot be read, so the renewal names no certificate to replace", "error", err)
		return false
	}
	return issuedTo == acct.URL
}

// Resume clears the pause a restore set (ACME-08). Nothing is ordered by it.
func (m *Manager) Resume(ctx context.Context, actor string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.d.CertFiles {
		return ErrCertFiles
	}
	if err := m.settle(ctx); err != nil {
		return err
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		return err
	}
	if st.PausedReason == "" {
		return ErrNotPaused
	}
	if m.runActive() {
		return ErrRunActive
	}
	st.PausedReason = ""
	st.UpdatedAt = m.clock.Now()
	if err := m.d.Store.SaveState(ctx, st); err != nil {
		return err
	}
	m.log.Info("Let's Encrypt resumed", "actor", actor)
	return nil
}

// Stop serves the self-signed certificate again and stops renewals (D-14). The account and the stored
// certificate are kept, so issuing again with the same names is one click and no order. A pause after
// a restore is not lifted by it. It is refused with ErrHSTSOn while the effective HSTS setting is on.
func (m *Manager) Stop(ctx context.Context, actor string) error {
	// Either value of TIERPLAN_ACME fixes whether Let's Encrypt is stopped. On locks Stop (D-04); off
	// leaves nothing to stop, and a Stop would rewrite the stored mode underneath the variable (D-02).
	if m.d.Env.Enabled != nil {
		return m.lockedACME()
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.d.HSTSHold == nil {
		return m.stopLocked(ctx, actor)
	}
	return m.d.HSTSHold(func() error { return m.stopLocked(ctx, actor) })
}

// stopLocked is Stop with opMu held and, when the HSTS setting is held, the setting unable to change.
func (m *Manager) stopLocked(ctx context.Context, actor string) error {
	// With HSTS on, browsers hold a policy for this host that the self-signed certificate cannot
	// satisfy. The refusal is here and not in the API, so no caller can go around it (D-26).
	if m.d.HSTSOn != nil && m.d.HSTSOn() {
		return ErrHSTSOn
	}
	if m.d.CertFiles {
		return ErrCertFiles
	}
	if m.runActive() {
		return ErrRunActive
	}
	// A success not yet written would make the mode active again after this stop.
	if err := m.settle(ctx); err != nil {
		return err
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		return err
	}
	if st.Mode != ModeActive {
		return ErrNotActive
	}
	now := m.clock.Now()
	if ord, _, found, err := m.d.Store.OpenOrder(ctx); err != nil {
		return err
	} else if found {
		if err := m.d.Store.CloseOrder(ctx, ord.ID, StageFailed, now); err != nil {
			return err
		}
	}
	if err := m.d.Installer.ServeSelfSigned(); err != nil {
		return fmt.Errorf("serve the self-signed certificate: %w", err)
	}
	st.Mode = ModeOff
	st.ConsecutiveFailures = 0
	st.NextAttemptAt = time.Time{}
	st.LastErrorAt, st.LastErrorClass, st.LastErrorDetail = time.Time{}, "", ""
	st.UpdatedAt = now
	if err := m.d.Store.SaveState(ctx, st); err != nil {
		return err
	}
	// With Let's Encrypt stopped there is nothing for the "Renew now to test it" prompt to test.
	if err := m.d.Store.SetValidationUntested(ctx, false, now); err != nil {
		m.log.Warn("clear the untested validation flag", "error", err)
	}
	m.log.Info("stopped using Let's Encrypt", "actor", actor)
	return nil
}

func (m *Manager) needAccount(ctx context.Context) error {
	_, ok, err := m.d.Client.Account(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoAccount
	}
	return nil
}

// manualBlock is the ledger's answer to a person pressing Issue or Renew now.
func (m *Manager) manualBlock(ctx context.Context, st State) error {
	now := m.clock.Now()
	failed, err := m.d.Store.FailedRunsSince(ctx, now.Add(-FailureGuardWindow))
	if err != nil {
		return err
	}
	if until, reason := ManualBlock(ledgerOf(st), failed, now); reason != "" {
		return &BlockedError{Until: until, Reason: reason, Wait: max(until.Sub(now), 0)}
	}
	return nil
}

// reuse is ACME-07's idempotency: a stored certificate that carries exactly the wanted names and is
// not yet due for renewal is the answer, and no order is placed. When Let's Encrypt was stopped it is
// served again.
func (m *Manager) reuse(ctx context.Context, st State, wanted []string) (RunStart, bool, error) {
	chain, key, found, err := m.d.Store.Certificate(ctx)
	if err != nil {
		return RunStart{}, false, err
	}
	if !found {
		return RunStart{}, false, nil
	}
	leaf, err := leafOfChain(chain)
	if err != nil {
		m.log.Warn("the stored Let's Encrypt certificate cannot be read, so it is not reused", "error", err)
		return RunStart{}, false, nil
	}
	now := m.clock.Now()
	due := st.RenewAt
	if due.IsZero() {
		due, _ = FallbackRenewAt(leaf.NotBefore, leaf.NotAfter)
	}
	if !SameNames(leaf.DNSNames, wanted) || !now.Before(leaf.NotAfter) || !now.Before(due) {
		return RunStart{}, false, nil
	}
	rs := RunStart{Reused: true, ValidUntil: leaf.NotAfter}
	if st.Mode == ModeActive {
		return rs, true, nil
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return RunStart{}, false, err
	}
	if err := m.d.Installer.ServeACME(chain, keyPEM); err != nil {
		return RunStart{}, false, fmt.Errorf("serve the stored certificate: %w", err)
	}
	st.Mode = ModeActive
	if st.RenewAt.IsZero() {
		renewAt, basis := FallbackRenewAt(leaf.NotBefore, leaf.NotAfter)
		st.RenewAt, st.RenewBasis = renewAt, string(basis)
		st.AriNextCheckAt = now
	}
	st.UpdatedAt = now
	if err := m.d.Store.SaveState(ctx, st); err != nil {
		return RunStart{}, false, err
	}
	return rs, true, nil
}

// storedLeaf is the first certificate of the stored Let's Encrypt chain.
func (m *Manager) storedLeaf(ctx context.Context) (*x509.Certificate, bool, error) {
	chain, _, found, err := m.d.Store.Certificate(ctx)
	if err != nil || !found {
		return nil, false, err
	}
	leaf, err := leafOfChain(chain)
	if err != nil {
		return nil, false, err
	}
	return leaf, true, nil
}

// leafOfChain parses the first certificate of a PEM chain.
func leafOfChain(chainPEM []byte) (*x509.Certificate, error) {
	rest := chainPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("the chain holds no certificate")
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}

func ledgerOf(st State) Ledger {
	return Ledger{
		ConsecutiveFailures: st.ConsecutiveFailures, LastAttemptAt: st.LastAttemptAt,
		NextAttemptAt: st.NextAttemptAt, RetryAfterUntil: st.RetryAfterUntil,
	}
}

func withLedger(st State, l Ledger) State {
	st.ConsecutiveFailures, st.LastAttemptAt = l.ConsecutiveFailures, l.LastAttemptAt
	st.NextAttemptAt, st.RetryAfterUntil = l.NextAttemptAt, l.RetryAfterUntil
	return st
}

// errorDoc is what LastErrorDetail holds: the raw detail, and the facts about the failure that have no
// column of their own, so the page can still name the host after a restart. A raw problem document has
// no "v", so a value written any other way reads back as plain detail.
type errorDoc struct {
	V        int    `json:"v"`
	Host     string `json:"host,omitempty"`
	Profile  string `json:"profile,omitempty"`
	TermsURL string `json:"terms_url,omitempty"`
	// Zone, Target and Resolvers are the DNS facts a DNS failure's copy needs.
	Zone      string   `json:"zone,omitempty"`
	Target    string   `json:"target,omitempty"`
	Resolvers []string `json:"resolvers,omitempty"`
	Detail    string   `json:"detail,omitempty"`
}

func encodeError(f Failure) string {
	doc := errorDoc{V: 1, Host: f.Host, Profile: f.Profile, TermsURL: f.TermsURL, Zone: jobs.Clean(f.Zone, nil), Target: jobs.Clean(f.Target, nil), Detail: f.Detail}
	for _, r := range f.Resolvers {
		doc.Resolvers = append(doc.Resolvers, jobs.Clean(r, nil))
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return f.Detail
	}
	return string(b)
}

func decodeError(s string) errorDoc {
	var d errorDoc
	if err := json.Unmarshal([]byte(s), &d); err != nil || d.V != 1 {
		return errorDoc{Detail: s}
	}
	return d
}

// recordInternal stores an internal error as the last error, for a failure outside a run.
func (m *Manager) recordInternal(ctx context.Context, detail string) error {
	st, err := m.d.Store.State(ctx)
	if err != nil {
		return err
	}
	now := m.clock.Now()
	st.LastErrorAt, st.LastErrorClass = now, ClassInternal
	st.LastErrorDetail = encodeError(Failure{Class: ClassInternal, Detail: cleanDetail(detail)})
	st.UpdatedAt = now
	return m.d.Store.SaveState(ctx, st)
}

// The steps of a run, with the labels the page shows.
var stepItems = []jobs.ItemSpec{
	{ID: StepAccount, Label: "Account", Kind: "step"},
	{ID: StepOrder, Label: "Order", Kind: "step"},
	{ID: StepChallenge, Label: "Challenge", Kind: "step"},
	{ID: StepValidate, Label: "Validation", Kind: "step"},
	{ID: StepFinalize, Label: "Finalize", Kind: "step"},
	{ID: StepStored, Label: "Stored", Kind: "step"},
}

const (
	messageFailed      = "This step did not finish. The error is shown below."
	messageNotReached  = "Not reached."
	messageNotSwitched = "The certificate was saved, but switching to it failed. It is used at the next start."

	messageNothingToRemove = "Nothing to remove."

	// codeLeftOver is the code of a Clean-up step that ended with records still at the provider.
	codeLeftOver = "left_over"
)

// jobSteps maps an Orderer's step reports onto the items of a job. The last step, stored, is held
// back: its end is reported by the run itself, once the outcome is stored and the certificate is
// installed, so a job that has finished is a run that has finished. What it reports is fixed text and
// a class code, never anything the CA said and never key material.
type jobSteps struct {
	r  *jobs.Reporter
	in ClassifyInput
	// items are the job's own items, in the order they are reported.
	items []jobs.ItemSpec

	mu   sync.Mutex
	held string // what the stored step ended as: "", ItemDone, ItemFailed or ItemSkipped
	code string // the class code of a failed stored step
	// created is how many DNS records the run wrote and leftOver how many of them it could not delete.
	created, leftOver int
}

// dns01 reports whether the run is a DNS-01 run, which has the propagation and clean-up steps.
func (s *jobSteps) dns01() bool { return s.in.ChallengeType == ChallengeDNS01 }

func (s *jobSteps) Running(step, hint string) {
	s.r.Update(step, func(it *jobs.Item) {
		it.State = jobs.ItemRunning
		it.Message = hint
	})
}

// RunningWith is Running that also sets the values the page shows for the step: the host, the record,
// the delegation target and the resolvers. They are names only, never anything the DNS provider said.
func (s *jobSteps) RunningWith(step, hint string, values map[string]string) {
	s.r.Update(step, func(it *jobs.Item) {
		it.State = jobs.ItemRunning
		it.Message = hint
		it.Result = maps.Clone(values)
	})
}

func (s *jobSteps) Done(step string) {
	if step == StepStored {
		s.hold(jobs.ItemDone, "")
		return
	}
	s.r.Finish(step, jobs.ItemDone, "", "", nil)
	if step == StepChallenge && s.dns01() {
		// One propagation step covers every name's record, and the order moves on once they all show.
		s.r.Finish(StepPropagation, jobs.ItemDone, "", "", nil)
	}
}

func (s *jobSteps) Skipped(step, message string) {
	if step == StepStored {
		s.hold(jobs.ItemSkipped, "")
		return
	}
	s.r.Finish(step, jobs.ItemSkipped, "", message, nil)
	if step == StepChallenge && s.dns01() {
		s.r.Finish(StepPropagation, jobs.ItemSkipped, "", message, nil)
	}
}

// Failed ends the step as failed with the error's class and every step that was not reached as
// skipped. A cancelled run reports nothing: the hub ends its unfinished steps as cancelled.
func (s *jobSteps) Failed(step string, err error) {
	if errors.Is(err, context.Canceled) {
		// The run was cancelled, not defeated: the hub ends the unfinished steps as cancelled.
		return
	}
	class := Classify(err, s.in).Class
	switch {
	case step == StepStored:
		s.hold(jobs.ItemFailed, class)
	case s.dns01() && step == StepChallenge && class == dns.ClassNotVisible:
		// The record was written, so the challenge itself is done; the wait for it to show is what failed.
		s.r.Finish(StepChallenge, jobs.ItemDone, "", "", nil)
		s.r.Finish(StepPropagation, jobs.ItemFailed, class, messageFailed, nil)
		s.hold(jobs.ItemSkipped, "")
	default:
		s.r.Finish(step, jobs.ItemFailed, class, messageFailed, nil)
		s.hold(jobs.ItemSkipped, "")
	}
	for _, it := range s.items {
		// Stored ends with the run, and Clean-up reports its own state: neither is a step this failure
		// stopped from being reached.
		if it.ID != step && it.ID != StepStored && it.ID != StepCleanup {
			s.r.Finish(it.ID, jobs.ItemSkipped, "", messageNotReached, nil)
		}
	}
}

func (s *jobSteps) hold(state, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held, s.code = state, code
}

// recordCreated counts a DNS record the run wrote, so the clean-up step knows it has something to do.
func (s *jobSteps) recordCreated() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created++
}

// recordLeftOver counts a DNS record the run could not delete.
func (s *jobSteps) recordLeftOver() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leftOver++
}

// cleanupRunning shows the clean-up step as running.
func (s *jobSteps) cleanupRunning() {
	s.Running(StepCleanup, hintCleanup)
}

// finishStored ends the stored step. installErr is why the new certificate could not be switched to.
func (s *jobSteps) finishStored(installErr error) {
	s.mu.Lock()
	held, code := s.held, s.code
	s.mu.Unlock()
	switch {
	case held == jobs.ItemDone && installErr == nil:
		s.r.Finish(StepStored, jobs.ItemDone, "", "", nil)
	case held == jobs.ItemDone:
		s.r.Finish(StepStored, jobs.ItemFailed, ClassInternal, messageNotSwitched, nil)
	case held == jobs.ItemFailed:
		s.r.Finish(StepStored, jobs.ItemFailed, code, messageFailed, nil)
	default:
		s.r.Finish(StepStored, jobs.ItemSkipped, "", messageNotReached, nil)
	}
	s.finishCleanup()
}

// finishCleanup ends the clean-up step of a run that has one: skipped when the run wrote no record,
// otherwise done.
func (s *jobSteps) finishCleanup() {
	if !s.dns01() {
		return
	}
	s.mu.Lock()
	created, leftOver := s.created, s.leftOver
	s.mu.Unlock()
	switch {
	case created == 0:
		s.r.Finish(StepCleanup, jobs.ItemSkipped, "", messageNothingToRemove, nil)
	case leftOver > 0:
		// Done, not failed: a record that could not be deleted is retried by TierPlan and never changes
		// what the run achieved. A failed step would turn a certificate that issued into a failed run.
		s.r.Finish(StepCleanup, jobs.ItemDone, codeLeftOver,
			fmt.Sprintf("Couldn't remove %d TXT record(s). TierPlan keeps trying.", leftOver),
			map[string]string{"left_over": strconv.Itoa(leftOver)})
	default:
		s.r.Finish(StepCleanup, jobs.ItemDone, "", "", nil)
	}
}

// runParams is what starts a run.
type runParams struct {
	trigger, actor string
	req            OrderRequest
	// challenge is the challenge type the run answers, as saved when the run was started.
	challenge string
	// register, when set, is an account the run registers first: the start-time issue of
	// TIERPLAN_ACME=on has none yet, and registering inside the run charges a failure to the persisted
	// backoff like any other (envissue.go).
	register *registerSpec
	// env marks the run TIERPLAN_ACME=on started. It is not stored: the run's trigger stays schedule, and only the
	// audit rows of the run say env (audit.go).
	env bool
}

// startRun starts the one background run. The caller has checked that none is active; the check is
// repeated under the lock that records the new one.
func (m *Manager) startRun(p runParams) (RunStart, error) {
	owner := p.actor
	if owner == "" {
		owner = OwnerScheduler
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil || m.dnsBusy {
		return RunStart{}, ErrRunActive
	}
	if m.unrecorded != nil {
		return RunStart{}, errRunUnrecorded
	}
	if p.challenge == "" {
		p.challenge = ChallengeHTTP01
	}
	ar := &activeRun{kind: p.req.Kind, trigger: p.trigger, challenge: p.challenge, startedAt: m.clock.Now()}
	// An admin's run waits for its caller's run-started hook, so the caller's request row comes before the run's
	// outcome row (audit.go). Any other run starts at once.
	var hold *runHold
	if p.trigger == TriggerAdmin {
		hold = newRunHold()
	}
	job, err := m.d.Jobs.Start(jobs.Spec{
		Kind: JobKind, Owner: owner,
		Meta:  map[string]string{"kind": p.req.Kind, "trigger": p.trigger, "challenge": p.challenge},
		Items: itemsFor(p.challenge),
	}, func(ctx context.Context, r *jobs.Reporter) {
		hold.wait()
		m.runBody(ctx, r, ar, p)
	})
	if err != nil {
		return RunStart{}, fmt.Errorf("start the certificate run: %w", err)
	}
	ar.jobID = job.ID
	m.active = ar
	return RunStart{JobID: job.ID, hold: hold}, nil
}

// run is one run's own bookkeeping.
type run struct {
	m     *Manager
	ar    *activeRun
	id    string
	ended bool
}

// end releases the single-run slot and tells the test seam. It does both once.
func (x *run) end(outcome string) {
	if x.ended {
		return
	}
	x.ended = true
	x.m.mu.Lock()
	if x.m.active == x.ar {
		x.m.active = nil
	}
	x.m.mu.Unlock()
	if x.m.d.RunEnded != nil {
		x.m.d.RunEnded(x.id, outcome)
	}
}

func (m *Manager) runBody(ctx context.Context, r *jobs.Reporter, ar *activeRun, p runParams) {
	x := &run{m: m, ar: ar, id: r.JobID()}
	defer x.end(OutcomeFailed) // a panic still frees the slot
	bg := context.WithoutCancel(ctx)
	steps := &jobSteps{r: r, in: ClassifyInput{ChallengeType: p.challenge, Profile: p.req.Profile}, items: itemsFor(p.challenge)}

	if err := m.d.Store.InsertRun(bg, Run{
		ID: x.id, Kind: p.req.Kind, Trigger: p.trigger, Actor: p.actor, Names: p.req.Names,
		StartedAt: m.clock.Now(), Outcome: OutcomeRunning, ChallengeType: p.challenge,
	}); err != nil {
		m.log.Error("record the start of a certificate run", "error", err)
		steps.Failed(StepAccount, err)
		x.end(OutcomeFailed)
		steps.finishStored(nil)
		return
	}

	// A run puts the saved validation settings to the test, so they are no longer untested (D-03). A
	// failure to clear the flag only leaves a prompt on the page.
	if err := m.d.Store.SetValidationUntested(bg, false, m.clock.Now()); err != nil {
		m.log.Warn("clear the untested validation flag", "error", err)
	}

	// Whatever an earlier run left at the DNS provider goes first, for any challenge: the saved method
	// may have changed since the record was made (D-17). A failure here never stops the order.
	m.sweepDNS(ctx, false)

	if p.register != nil {
		if err := m.registerAccount(ctx, steps, *p.register); err != nil {
			m.failed(ctx, bg, x, steps, err, p)
			return
		}
	}

	solver, err := m.newSolver(bg, steps, p.challenge)
	if err != nil {
		m.failed(ctx, bg, x, steps, err, p)
		return
	}
	issued, err := NewOrderer(m.d.Client, m.d.Store, solver, m.clock).Run(ctx, p.req, steps)
	if err != nil {
		m.failed(ctx, bg, x, steps, err, p)
		return
	}
	m.succeeded(ctx, bg, x, steps, issued, p)
}

// newSolver is the solver for a run of challenge: the token store for HTTP-01, and for DNS-01 a solver of
// its own over the provider and resolvers saved now. The order code is the same for both.
func (m *Manager) newSolver(ctx context.Context, steps *jobSteps, challenge string) (Solver, error) {
	if challenge != ChallengeDNS01 {
		return m.d.Challenges, nil
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		return nil, err
	}
	return &dnsSolver{m: m, steps: steps, provider: m.provider(st), servers: dnscheck.Resolvers(m.resolvers(st))}, nil
}

// failed records a run that did not end in a certificate. What is served is not touched.
func (m *Manager) failed(ctx, bg context.Context, x *run, steps *jobSteps, runErr error, p runParams) {
	if ctx.Err() != nil {
		// The server is stopping, or a person cancelled the job. Neither is the CA's fault, so the
		// ledger is not charged, and the order stays open to be resumed.
		if jobs.Closing(ctx) {
			// The run row stays running: the next start closes it as interrupted.
			x.end(OutcomeFailed)
			return
		}
		m.finishRun(bg, x.id, OutcomeCancelled, "", func(st State) State { return st })
		x.end(OutcomeCancelled)
		return
	}

	f := Classify(runErr, steps.in)
	lifetime := time.Duration(0)
	if leaf, found, err := m.storedLeaf(bg); err == nil && found {
		lifetime = leaf.NotAfter.Sub(leaf.NotBefore)
	}
	now := m.clock.Now()
	// Drawn once, so a write made again later charges the same backoff.
	jitter := m.d.Jitter()
	m.finishRun(bg, x.id, OutcomeFailed, f.Class, func(st State) State {
		st = withLedger(st, RecordFailure(ledgerOf(st), now, f.RetryAfter, lifetime, jitter))
		st.LastErrorAt, st.LastErrorClass, st.LastErrorDetail = now, f.Class, encodeError(f)
		return st
	})
	if f.Class == ClassAccountGone {
		if err := m.d.Client.Forget(bg); err != nil {
			m.log.Error("forget the account the CA no longer knows", "error", err)
		}
	}
	m.log.Warn("a certificate run failed", "kind", p.req.Kind, "class", f.Class, "host", f.Host)
	// One row per run that ends in an error, before the run is told to have ended (D-11, D-12). The cancel and
	// shutdown returns above write none (amended D-27).
	m.recordRunFailed(bg, x, p, f)
	x.end(OutcomeFailed)
	steps.finishStored(nil)
}

// succeeded stores the outcome and the ledger, and only then asks the Installer to serve the new
// certificate. ctx is the run's own context, read only to tell a person's cancel that landed after the order was
// final; everything the run does here uses bg.
func (m *Manager) succeeded(ctx, bg context.Context, x *run, steps *jobSteps, issued Issued, p runParams) {
	now := m.clock.Now()
	leaf := issued.Leaf
	renewAt, basis := FallbackRenewAt(leaf.NotBefore, leaf.NotAfter)
	m.finishRun(bg, x.id, OutcomeSucceeded, "", func(st State) State {
		l := RecordSuccess(ledgerOf(st))
		l.LastAttemptAt = now
		st = withLedger(st, l)
		st.Mode = ModeActive
		st.RenewAt, st.RenewBasis = renewAt, string(basis)
		// The window belonged to the certificate this one replaces; the scheduler asks again.
		st.AriWindowStart, st.AriWindowEnd, st.AriCheckedAt = time.Time{}, time.Time{}, time.Time{}
		st.AriNextCheckAt = now
		st.LastSuccessAt, st.LastSuccessKind = now, p.req.Kind
		st.LastErrorAt, st.LastErrorClass, st.LastErrorDetail = time.Time{}, "", ""
		return st
	})

	keyPEM, installErr := encodeKey(issued.Key)
	if installErr == nil {
		installErr = m.d.Installer.ServeACME(issued.ChainPEM, keyPEM)
	}
	if installErr != nil {
		m.log.Error("the new certificate is stored but could not be served", "error", installErr)
		if err := m.recordInternal(bg, "switch to the new certificate: "+installErr.Error()); err != nil {
			m.log.Error("record that the new certificate could not be served", "error", err)
		}
	}
	// The row says whether the new certificate is the one being served now, so it follows the install attempt, and
	// it comes before the run is told to have ended, so whoever waits for the end sees it.
	m.recordIssued(ctx, bg, x, p, leaf, installErr == nil)
	x.end(OutcomeSucceeded)
	steps.finishStored(installErr)
}

// runEnd is the end of a run as it is stored: the outcome, and the change it makes to the ledger.
type runEnd struct {
	id, outcome, class string
	at                 time.Time
	change             func(State) State
}

// finishRun reads the ledger, lets change adjust it, and stores it with the run's outcome in one
// transaction. A failure to write is not only logged: the end is kept in memory, no run starts until
// it is written, and the scheduler and every admin change write it again first (settle). Without that
// a failed run would not be charged, so the next tick could try again at once, and a successful
// renewal would leave renew_at in the past, so every tick would issue another certificate.
func (m *Manager) finishRun(bg context.Context, id, outcome, class string, change func(State) State) {
	end := &runEnd{id: id, outcome: outcome, class: class, at: m.clock.Now(), change: change}
	if err := m.writeEnd(bg, end); err != nil {
		m.log.Error("record the end of a certificate run; no run starts until it is recorded", "error", err)
		m.mu.Lock()
		m.unrecorded = end
		m.mu.Unlock()
	}
}

func (m *Manager) writeEnd(ctx context.Context, end *runEnd) error {
	st, err := m.d.Store.State(ctx)
	if err != nil {
		return err
	}
	st = end.change(st)
	st.UpdatedAt = end.at
	return m.d.Store.FinishRun(ctx, end.id, end.outcome, end.class, end.at, st)
}

// settle writes the end of a run that could not be written when the run ended, if there is one. The
// caller holds opMu, so two writes of it never race. An error means it is still not written.
func (m *Manager) settle(ctx context.Context) error {
	m.mu.Lock()
	end := m.unrecorded
	m.mu.Unlock()
	if end == nil {
		return nil
	}
	if err := m.writeEnd(ctx, end); err != nil {
		return fmt.Errorf("record the end of the last certificate run: %w", err)
	}
	m.mu.Lock()
	if m.unrecorded == end {
		m.unrecorded = nil
	}
	m.mu.Unlock()
	m.log.Info("the end of the last certificate run is recorded", "outcome", end.outcome)
	return nil
}
