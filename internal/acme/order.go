// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	legoacme "github.com/go-acme/lego/v5/acme"
	"github.com/go-acme/lego/v5/acme/api"
	"github.com/go-acme/lego/v5/certcrypto"
	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/stackops"
)

// The six steps of an order, in the order they are reported. They are the item keys of the live job
// the UI shows, so they are a contract.
const (
	StepAccount   = "account"
	StepOrder     = "order"
	StepChallenge = "challenge"
	StepValidate  = "validate"
	StepFinalize  = "finalize"
	StepStored    = "stored"
)

// The kinds of order.
const (
	KindIssue = "issue"
	KindRenew = "renew"
)

const (
	// PollInterval is how long an order waits between asking the CA where it stands.
	PollInterval = 2 * time.Second
	// ValidationTimeout bounds the wait for the CA to validate the challenges.
	ValidationTimeout = 2 * time.Minute
	// FinalizeTimeout bounds the wait for the CA to issue after it accepted the CSR.
	FinalizeTimeout = 2 * time.Minute
	// MaxResumes is how many times one order is resumed before it is given up on. A crash loop must
	// not be able to resume the same order for ever.
	MaxResumes = 5
)

// StepReporter is told, as they happen, how the steps of an order go. The messages it receives are
// fixed hints written here: they never carry a token, a key authorization or text from the CA. The
// error given to Failed is the real error, for the caller to classify; it is not for display.
type StepReporter interface {
	Running(step, hint string)
	Done(step string)
	Skipped(step, message string)
	Failed(step string, err error)
}

// quietSteps is the reporter used when the caller passes none.
type quietSteps struct{}

func (quietSteps) Running(string, string) {}
func (quietSteps) Done(string)            {}
func (quietSteps) Skipped(string, string) {}
func (quietSteps) Failed(string, error)   {}

// OrderRequest is what one run asks the CA for.
type OrderRequest struct {
	// Kind is KindIssue or KindRenew.
	Kind  string
	Names []string
	// Profile is the certificate profile to ask for; empty means the CA's default.
	Profile string
	// Replaces is the ARI certificate ID of the certificate a renewal replaces (ACME-05).
	Replaces string
}

// Issued is a certificate that has been checked and stored.
type Issued struct {
	OrderID  string
	Kind     string
	ChainPEM []byte
	Key      *ecdsa.PrivateKey
	Leaf     *x509.Certificate
}

// Orderer drives one ACME order at a time from newOrder to a stored certificate. It works on lego's
// low-level api.Core, not on its high-level certificate package: that one keeps the order in a local
// variable, so it cannot be resumed after a restart, and it deactivates authorizations on failure.
type Orderer struct {
	client *Client
	store  *Store
	solver Solver
	clock  stackops.Clock
}

// NewOrderer is an Orderer that orders through c, keeps its state in s, answers challenges with solver
// and judges time by clock.
func NewOrderer(c *Client, s *Store, solver Solver, clock stackops.Clock) *Orderer {
	return &Orderer{client: c, store: s, solver: solver, clock: clock}
}

// presented is a challenge answer a run made available and owes a clean-up for.
type presented struct{ domain, token, keyAuth string }

// orderRun is the state of one call to Run.
type orderRun struct {
	o     *Orderer
	req   OrderRequest
	steps StepReporter
	core  *api.Core

	row  Order
	key  *ecdsa.PrivateKey
	made []presented
}

// Run orders a certificate for req.Names and returns it once it has been checked and stored.
func (o *Orderer) Run(ctx context.Context, req OrderRequest, steps StepReporter) (Issued, error) {
	if steps == nil {
		steps = quietSteps{}
	}
	if err := checkRequest(req); err != nil {
		return Issued{}, err
	}
	r := &orderRun{o: o, req: req, steps: steps}
	// The tokens come down whatever happened, and even if ctx was cancelled to stop the run.
	defer r.cleanUp(context.WithoutCancel(ctx))
	return r.run(ctx)
}

func checkRequest(req OrderRequest) error {
	if req.Kind != KindIssue && req.Kind != KindRenew {
		return fmt.Errorf("an order is %q or %q, not %q", KindIssue, KindRenew, req.Kind)
	}
	if len(req.Names) == 0 {
		return errors.New("an order needs at least one name")
	}
	return nil
}

func (r *orderRun) fail(step string, err error) error {
	r.steps.Failed(step, err)
	return err
}

func (r *orderRun) run(ctx context.Context) (Issued, error) {
	core, _, _, err := r.o.client.core(ctx)
	if err != nil {
		return Issued{}, r.fail(StepAccount, err)
	}
	r.core = core
	r.steps.Skipped(StepAccount, "Account already registered.")

	r.steps.Running(StepOrder, "Asking the certificate authority for an order.")
	ord, err := r.create(ctx)
	if err != nil {
		return Issued{}, r.fail(StepOrder, err)
	}
	r.steps.Done(StepOrder)

	r.steps.Running(StepChallenge, "Making the challenge answerable.")
	if err := r.challenges(ctx, ord); err != nil {
		return Issued{}, r.fail(StepChallenge, err)
	}
	if err := r.o.store.UpdateOrder(ctx, r.row.ID, StageChallengeSubmitted, r.row.ResumeCount, r.o.clock.Now()); err != nil {
		return Issued{}, r.fail(StepChallenge, err)
	}
	r.steps.Done(StepChallenge)

	r.steps.Running(StepValidate, "Waiting for the certificate authority to check the challenge.")
	ord, err = r.awaitValidation(ctx)
	if err != nil {
		return Issued{}, r.fail(StepValidate, err)
	}
	if err := r.o.store.UpdateOrder(ctx, r.row.ID, StageReady, r.row.ResumeCount, r.o.clock.Now()); err != nil {
		return Issued{}, r.fail(StepValidate, err)
	}
	r.steps.Done(StepValidate)

	r.steps.Running(StepFinalize, "Sending the certificate request.")
	ord, err = r.finalize(ctx, ord)
	if err != nil {
		return Issued{}, r.fail(StepFinalize, err)
	}
	r.steps.Done(StepFinalize)

	r.steps.Running(StepStored, "Checking and saving the certificate.")
	issued, err := r.download(ctx, ord)
	if err != nil {
		return Issued{}, r.fail(StepStored, err)
	}
	r.steps.Done(StepStored)
	return issued, nil
}

// create opens a new order at the CA and stores it with its fresh certificate key before anything
// else can happen to it (RESEARCH Pitfall 2).
func (r *orderRun) create(ctx context.Context) (legoacme.ExtendedOrder, error) {
	key, err := newKey()
	if err != nil {
		return legoacme.ExtendedOrder{}, err
	}
	ord, err := r.core.Orders.New(ctx, r.req.Names, &api.OrderOptions{Profile: r.req.Profile, ReplacesCertID: r.req.Replaces})
	if err != nil {
		return legoacme.ExtendedOrder{}, fmt.Errorf("create an ACME order: %w", err)
	}
	if ord.Location == "" {
		return legoacme.ExtendedOrder{}, errors.New("create an ACME order: the CA gave no order URL")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return legoacme.ExtendedOrder{}, fmt.Errorf("make an order ID: %w", err)
	}
	now := r.o.clock.Now()
	row := Order{
		ID: id.String(), DirectoryURL: r.o.client.dir.URL, URL: ord.Location, Kind: r.req.Kind,
		Names: slices.Clone(r.req.Names), Profile: r.req.Profile, Replaces: r.req.Replaces,
		Stage: StageCreated, ExpiresAt: parseExpiry(ord.Expires), CreatedAt: now, UpdatedAt: now,
	}
	if err := r.o.store.InsertOrder(ctx, row, key); err != nil {
		return legoacme.ExtendedOrder{}, err
	}
	r.row, r.key = row, key
	return ord, nil
}

// parseExpiry reads an order's expiry; an order the CA gave none for never expires on our side.
func parseExpiry(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// challenges makes every authorization that still needs one answerable and submits the challenges
// that are pending. A challenge the CA is already processing or has validated is never submitted
// again: the CA answers that malformed (RESEARCH Pitfall 3).
func (r *orderRun) challenges(ctx context.Context, ord legoacme.ExtendedOrder) error {
	for _, azURL := range ord.Authorizations {
		az, err := r.core.Authorizations.Get(ctx, azURL)
		if err != nil {
			return fmt.Errorf("read an ACME authorization: %w", err)
		}
		if az.Status == legoacme.StatusValid {
			continue
		}
		ch := findChallenge(az, r.o.solver.Type())
		if ch == nil {
			return fmt.Errorf("the CA offered no %s challenge for %s", r.o.solver.Type(), az.Identifier.Value)
		}
		// The key authorization is a function of the account key and the token, so it is re-derived
		// whenever the in-memory answers are needed again.
		keyAuth, err := r.core.GetKeyAuthorization(ch.Token)
		if err != nil {
			return fmt.Errorf("derive the key authorization: %w", err)
		}
		if err := r.o.solver.Present(ctx, az.Identifier.Value, ch.Token, keyAuth); err != nil {
			return fmt.Errorf("present the %s challenge for %s: %w", r.o.solver.Type(), az.Identifier.Value, err)
		}
		r.made = append(r.made, presented{domain: az.Identifier.Value, token: ch.Token, keyAuth: keyAuth})
		if ch.Status == legoacme.StatusPending && az.Status == legoacme.StatusPending {
			if _, err := r.core.Challenges.New(ctx, ch.URL); err != nil {
				return fmt.Errorf("submit the %s challenge for %s: %w", r.o.solver.Type(), az.Identifier.Value, err)
			}
		}
	}
	return nil
}

func findChallenge(az legoacme.Authorization, typ string) *legoacme.Challenge {
	for i := range az.Challenges {
		if az.Challenges[i].Type == typ {
			return &az.Challenges[i]
		}
	}
	return nil
}

// cleanUp withdraws every answer this run made available.
func (r *orderRun) cleanUp(ctx context.Context) {
	for _, p := range r.made {
		_ = r.o.solver.CleanUp(ctx, p.domain, p.token, p.keyAuth)
	}
	r.made = nil
}

// await asks the CA where the order stands until keepWaiting says to stop, waiting PollInterval on
// the clock between asks and giving up once timeout has passed on it. The order is read before the
// first wait, so an order that is already past the wait costs no time.
func (r *orderRun) await(ctx context.Context, timeout time.Duration, keepWaiting func(status string) bool) (legoacme.ExtendedOrder, error) {
	deadline := r.o.clock.Now().Add(timeout)
	for {
		ord, err := r.core.Orders.Get(ctx, r.row.URL)
		if err != nil {
			return legoacme.ExtendedOrder{}, fmt.Errorf("read the ACME order: %w", err)
		}
		if !keepWaiting(ord.Status) {
			return ord, nil
		}
		if !r.o.clock.Now().Before(deadline) {
			return legoacme.ExtendedOrder{}, fmt.Errorf("the order was still %s after %s: %w", ord.Status, timeout, context.DeadlineExceeded)
		}
		wait := r.o.clock.After(PollInterval)
		select {
		case <-ctx.Done():
			return legoacme.ExtendedOrder{}, ctx.Err()
		case <-wait:
		}
	}
}

// awaitValidation waits until the CA has validated the challenges, or says it cannot. A failed
// authorization is terminal: the order is closed as failed and the error names the host.
func (r *orderRun) awaitValidation(ctx context.Context) (legoacme.ExtendedOrder, error) {
	ord, err := r.await(ctx, ValidationTimeout, func(status string) bool { return status == legoacme.StatusPending })
	if err != nil {
		return legoacme.ExtendedOrder{}, err
	}
	if ord.Status == legoacme.StatusInvalid {
		return legoacme.ExtendedOrder{}, r.invalid(ctx, ord)
	}
	return ord, nil
}

// invalid closes the order as failed and returns the reason the CA gave, found in the authorization
// that failed. Authorizations are left as they are: a valid one is reusable, and deactivating one
// costs requests.
func (r *orderRun) invalid(ctx context.Context, ord legoacme.ExtendedOrder) error {
	reason := r.reason(ctx, ord)
	// Recording that the CA ended the order is not undone by the run being cancelled.
	if err := r.o.store.CloseOrder(context.WithoutCancel(ctx), r.row.ID, StageFailed, r.o.clock.Now()); err != nil {
		return fmt.Errorf("%w (and the order could not be closed: %v)", reason, err)
	}
	return reason
}

func (r *orderRun) reason(ctx context.Context, ord legoacme.ExtendedOrder) error {
	for _, azURL := range ord.Authorizations {
		az, err := r.core.Authorizations.Get(ctx, azURL)
		if err != nil || az.Status != legoacme.StatusInvalid {
			continue
		}
		for _, ch := range az.Challenges {
			if ch.Error != nil {
				return &ChallengeError{Host: az.Identifier.Value, Err: ch.Error}
			}
		}
		return &ChallengeError{Host: az.Identifier.Value, Err: errors.New("the authorization became invalid")}
	}
	if ord.Error != nil {
		return fmt.Errorf("the CA ended the order: %w", ord.Error)
	}
	return errors.New("the CA ended the order as invalid")
}

// finalize sends the CSR, built from the key stored with the order, and waits for the certificate.
// The stage is recorded first so that a crash after the CSR left is known to have happened.
func (r *orderRun) finalize(ctx context.Context, ord legoacme.ExtendedOrder) (legoacme.ExtendedOrder, error) {
	if err := ctx.Err(); err != nil {
		return legoacme.ExtendedOrder{}, err
	}
	csr, err := certcrypto.CreateCSR(r.key, certcrypto.CSROptions{SAN: r.row.Names})
	if err != nil {
		return legoacme.ExtendedOrder{}, fmt.Errorf("build the certificate request: %w", err)
	}
	if err := r.o.store.UpdateOrder(ctx, r.row.ID, StageFinalizing, r.row.ResumeCount, r.o.clock.Now()); err != nil {
		return legoacme.ExtendedOrder{}, err
	}
	ord, err = r.core.Orders.UpdateForCSR(ctx, ord.Finalize, csr)
	if err != nil {
		return legoacme.ExtendedOrder{}, fmt.Errorf("finalize the ACME order: %w", err)
	}
	return r.settle(ctx, ord)
}

// settle waits while the CA is issuing, and returns the order once it is valid.
func (r *orderRun) settle(ctx context.Context, ord legoacme.ExtendedOrder) (legoacme.ExtendedOrder, error) {
	if ord.Status == legoacme.StatusProcessing {
		var err error
		ord, err = r.await(ctx, FinalizeTimeout, func(status string) bool { return status == legoacme.StatusProcessing })
		if err != nil {
			return legoacme.ExtendedOrder{}, err
		}
	}
	switch ord.Status {
	case legoacme.StatusValid:
		return ord, nil
	case legoacme.StatusInvalid:
		return legoacme.ExtendedOrder{}, r.invalid(ctx, ord)
	}
	return legoacme.ExtendedOrder{}, fmt.Errorf("the CA left the order %s after the certificate request", ord.Status)
}

// download fetches the issued chain, checks it, and stores it. Nothing the CA sent becomes the served
// certificate before the checks pass (ACME-04).
func (r *orderRun) download(ctx context.Context, ord legoacme.ExtendedOrder) (Issued, error) {
	raw, err := r.core.Certificates.Get(ctx, ord.Certificate, true)
	if err != nil {
		return Issued{}, fmt.Errorf("download the certificate: %w", err)
	}
	now := r.o.clock.Now()
	leaf, chain, err := checkIssued(raw.Cert, r.key, r.row.Names, now)
	if err != nil {
		// The same order would hand out the same certificate again, so it is not resumed.
		_ = r.o.store.CloseOrder(context.WithoutCancel(ctx), r.row.ID, StageFailed, now)
		return Issued{}, err
	}
	if err := r.o.store.StoreIssued(ctx, r.row.ID, chain, r.key, now); err != nil {
		return Issued{}, err
	}
	return Issued{OrderID: r.row.ID, Kind: r.row.Kind, ChainPEM: chain, Key: r.key, Leaf: leaf}, nil
}

// checkIssued parses a chain and returns its leaf and a clean PEM encoding of it. The leaf must carry
// exactly the ordered names, the public half of the key kept with the order, and a lifetime that has
// not ended.
func checkIssued(chainPEM []byte, key *ecdsa.PrivateKey, names []string, now time.Time) (*x509.Certificate, []byte, error) {
	var leaf *x509.Certificate
	var clean []byte
	rest := chainPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, nil, fmt.Errorf("the issued chain holds a %q block, not a certificate", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("the issued chain holds a certificate that does not parse: %w", err)
		}
		if leaf == nil {
			leaf = cert
		}
		clean = append(clean, pem.EncodeToMemory(block)...)
	}
	if leaf == nil {
		return nil, nil, errors.New("the CA sent no certificate")
	}
	if len(strings.TrimSpace(string(rest))) > 0 {
		return nil, nil, errors.New("the issued chain has data after its last certificate")
	}
	got := make([]string, len(leaf.DNSNames))
	for i, n := range leaf.DNSNames {
		got[i] = strings.ToLower(n)
	}
	want := make([]string, len(names))
	for i, n := range names {
		want[i] = strings.ToLower(n)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) || len(leaf.IPAddresses) > 0 {
		return nil, nil, fmt.Errorf("the issued certificate is for %v, not the ordered %v", got, want)
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		return nil, nil, errors.New("the issued certificate is not for the key kept with the order")
	}
	if !leaf.NotAfter.After(now) {
		return nil, nil, fmt.Errorf("the issued certificate expired at %s", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	return leaf, clean, nil
}
