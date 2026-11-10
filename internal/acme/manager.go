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
	mrand "math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v5/acme/api"

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
// live swap); this package never touches a listener.
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
	Log       *slog.Logger
	// Rand picks in [0, n), for the scheduler's choice inside an ARI window; nil is math/rand/v2.
	Rand func(n int64) int64
	// Jitter is the backoff jitter in [-0.1, 0.1]; nil is math/rand/v2.
	Jitter func() float64
	// RunEnded is a test seam: it is told when a run has ended, after its outcome is stored and
	// before its last job item is finished.
	RunEnded func(runID, outcome string)
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
}

// Manager is the admin's way into ACME: register, issue, renew now, stop, resume and read the status.
// It runs one order at a time, in the background, and applies the ledger around every run.
type Manager struct {
	d       Deps
	clock   stackops.Clock
	log     *slog.Logger
	orderer *Orderer

	// opMu serialises the admin's mutations, so two requests cannot both pass the guards.
	opMu sync.Mutex

	// mu guards active, unrecorded, inSync and installErr.
	mu     sync.Mutex
	active *activeRun
	// unrecorded is the end of a run that could not be written when the run ended. Until it is, the
	// stored ledger does not know about that run, so no run starts and every admin change writes it
	// first.
	unrecorded *runEnd
	// inSync is whether the Installer last served the stored Let's Encrypt certificate, and installErr
	// the last refusal that was logged (scheduler.go).
	inSync     bool
	installErr string
}

// activeRun is the one run in progress.
type activeRun struct {
	jobID, kind, trigger string
	startedAt            time.Time
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
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		d: d, clock: d.Clock, log: log.With("component", "acme"),
		orderer: NewOrderer(d.Client, d.Store, d.Challenges, d.Clock),
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
	ct := st.ChallengeType
	if ct == "" {
		ct = ChallengeHTTP01
	}
	return Preflight(PreflightInput{
		PublicURL: m.publicURL(), AgentURL: m.agentURL(), BasePath: m.basePath(),
		ChallengeType: ct, DirectoryOverridden: m.d.Client.Directory().Overridden,
		DialHosts: m.dialHosts(),
	})
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

func (m *Manager) runActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active != nil
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
	if m.d.CertFiles {
		return nil
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		return err
	}
	if st.Mode != ModeActive {
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

// Register creates the Let's Encrypt account (D-06).
func (m *Manager) Register(ctx context.Context, email string, agreeTOS bool) (Account, error) {
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
func (m *Manager) SetProfile(ctx context.Context, profile string) error {
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
// new names are ordered (D-05): a URL change, a save or a scheduled round never starts one.
//
// The guards run in this order, each before anything that could reach the CA: certificate files,
// pause, account, a run in progress, every preflight refusal, the ledger's manual block, and reuse of
// a stored certificate that already covers the names and is not yet due.
func (m *Manager) Issue(ctx context.Context, actor string) (RunStart, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	st, err := m.mutable(ctx)
	if err != nil {
		return RunStart{}, err
	}
	if err := m.needAccount(ctx); err != nil {
		return RunStart{}, err
	}
	if m.runActive() {
		return RunStart{}, ErrRunActive
	}
	ns, refusals := m.preflight(st)
	if len(refusals) > 0 {
		return RunStart{}, &RefusedError{Refusals: refusals}
	}
	if err := m.manualBlock(ctx, st); err != nil {
		return RunStart{}, err
	}
	if rs, reused, err := m.reuse(ctx, st, ns.Names); err != nil || reused {
		return rs, err
	}
	return m.startRun(runParams{
		trigger: TriggerAdmin, actor: actor,
		req: OrderRequest{Kind: KindIssue, Names: slices.Clone(ns.Names), Profile: st.Profile},
	})
}

// RenewNow renews the certificate now, for the names it already carries: never the names the URLs
// want today (D-05). It names the certificate it replaces when the CA offers renewal information.
func (m *Manager) RenewNow(ctx context.Context, actor string) (RunStart, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	st, err := m.mutable(ctx)
	if err != nil {
		return RunStart{}, err
	}
	if m.runActive() {
		return RunStart{}, ErrRunActive
	}
	if st.Mode != ModeActive {
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
	return m.startRun(runParams{
		trigger: TriggerAdmin, actor: actor,
		req: m.renewRequest(ctx, st, leaf),
	})
}

// renewRequest is the order that renews leaf: its own names, the saved profile, and the certificate
// it replaces when the directory offers renewalInfo.
func (m *Manager) renewRequest(ctx context.Context, st State, leaf *x509.Certificate) OrderRequest {
	req := OrderRequest{Kind: KindRenew, Names: slices.Clone(leaf.DNSNames), Profile: st.Profile}
	if meta, err := m.d.Client.Meta(ctx); err == nil && meta.RenewalInfo {
		if id, err := api.MakeARICertID(leaf); err == nil {
			req.Replaces = id
		}
	}
	return req
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
// a restore is not lifted by it.
func (m *Manager) Stop(ctx context.Context, actor string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
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
	Detail   string `json:"detail,omitempty"`
}

func encodeError(f Failure) string {
	b, err := json.Marshal(errorDoc{V: 1, Host: f.Host, Profile: f.Profile, TermsURL: f.TermsURL, Detail: f.Detail})
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
)

// jobSteps maps an Orderer's step reports onto the items of a job. The last step, stored, is held
// back: its end is reported by the run itself, once the outcome is stored and the certificate is
// installed, so a job that has finished is a run that has finished. What it reports is fixed text and
// a class code, never anything the CA said and never key material.
type jobSteps struct {
	r  *jobs.Reporter
	in ClassifyInput

	mu   sync.Mutex
	held string // what the stored step ended as: "", ItemDone, ItemFailed or ItemSkipped
	code string // the class code of a failed stored step
}

func (s *jobSteps) Running(step, hint string) {
	s.r.Update(step, func(it *jobs.Item) {
		it.State = jobs.ItemRunning
		it.Message = hint
	})
}

func (s *jobSteps) Done(step string) {
	if step == StepStored {
		s.hold(jobs.ItemDone, "")
		return
	}
	s.r.Finish(step, jobs.ItemDone, "", "", nil)
}

func (s *jobSteps) Skipped(step, message string) {
	if step == StepStored {
		s.hold(jobs.ItemSkipped, "")
		return
	}
	s.r.Finish(step, jobs.ItemSkipped, "", message, nil)
}

// Failed ends the step as failed with the error's class and every step that was not reached as
// skipped. A cancelled run reports nothing: the hub ends its unfinished steps as cancelled.
func (s *jobSteps) Failed(step string, err error) {
	if errors.Is(err, context.Canceled) {
		// The run was cancelled, not defeated: the hub ends the unfinished steps as cancelled.
		return
	}
	class := Classify(err, s.in).Class
	if step == StepStored {
		s.hold(jobs.ItemFailed, class)
	} else {
		s.r.Finish(step, jobs.ItemFailed, class, messageFailed, nil)
		s.hold(jobs.ItemSkipped, "")
	}
	for _, id := range stepItems {
		if id.ID != step && id.ID != StepStored {
			s.r.Finish(id.ID, jobs.ItemSkipped, "", messageNotReached, nil)
		}
	}
}

func (s *jobSteps) hold(state, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held, s.code = state, code
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
}

// runParams is what starts a run.
type runParams struct {
	trigger, actor string
	req            OrderRequest
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
	if m.active != nil {
		return RunStart{}, ErrRunActive
	}
	if m.unrecorded != nil {
		return RunStart{}, errRunUnrecorded
	}
	ar := &activeRun{kind: p.req.Kind, trigger: p.trigger, startedAt: m.clock.Now()}
	job, err := m.d.Jobs.Start(jobs.Spec{
		Kind: JobKind, Owner: owner,
		Meta:  map[string]string{"kind": p.req.Kind, "trigger": p.trigger},
		Items: slices.Clone(stepItems),
	}, func(ctx context.Context, r *jobs.Reporter) { m.runBody(ctx, r, ar, p) })
	if err != nil {
		return RunStart{}, fmt.Errorf("start the certificate run: %w", err)
	}
	ar.jobID = job.ID
	m.active = ar
	return RunStart{JobID: job.ID}, nil
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
	steps := &jobSteps{r: r, in: ClassifyInput{ChallengeType: ChallengeHTTP01, Profile: p.req.Profile}}

	if err := m.d.Store.InsertRun(bg, Run{
		ID: x.id, Kind: p.req.Kind, Trigger: p.trigger, Actor: p.actor, Names: p.req.Names,
		StartedAt: m.clock.Now(), Outcome: OutcomeRunning,
	}); err != nil {
		m.log.Error("record the start of a certificate run", "error", err)
		steps.Failed(StepAccount, err)
		x.end(OutcomeFailed)
		steps.finishStored(nil)
		return
	}

	issued, err := m.orderer.Run(ctx, p.req, steps)
	if err != nil {
		m.failed(ctx, bg, x, steps, err, p)
		return
	}
	m.succeeded(bg, x, steps, issued, p)
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
	x.end(OutcomeFailed)
	steps.finishStored(nil)
}

// succeeded stores the outcome and the ledger, and only then asks the Installer to serve the new
// certificate.
func (m *Manager) succeeded(bg context.Context, x *run, steps *jobSteps, issued Issued, p runParams) {
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
