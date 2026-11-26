// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// JobKindEnable is the kind of the job that enables an environment's ingress.
const JobKindEnable = "ingress_enable"

// The items of the enable job, in the order they run.
const (
	ItemToken   = "token"
	ItemNetwork = "network"
	ItemService = "service"
	ItemSwitch  = "switch"
)

// The codes an item of the enable job ends with when it fails. Each is a stable word; the engine's own
// words go on the item as a detail where they cannot carry a secret.
const (
	CodeEnableTokenFailed   = "enable_token_failed"
	CodeEnableNetworkFailed = "enable_network_failed"
	CodeEnableServiceFailed = "enable_service_failed"
	CodeEnableSwitchFailed  = "enable_switch_failed"
	// CodeEnableHostFailed ends the first item of a standalone enable when the host could not be read.
	CodeEnableHostFailed = "enable_host_failed"
	// CodeNotRun ends the items after the one that failed.
	CodeNotRun = "not_run"
)

// codeMessages are the sentences of the codes. They are built here and never from an engine's answer.
var codeMessages = map[string]string{
	CodeNameShadowed:        "A container is joined to a network by a name that is the start of the new ingress network's ID, so after a reboot it could join the ingress network under a name of its own. Disconnect it from that network or remove it, then switch ingress on again; or switch ingress off and on again, which makes the network with a new ID.",
	CodeEnableTokenFailed:   "The join token could not be made or stored as a swarm secret.",
	CodeEnableNetworkFailed: "The ingress network could not be created.",
	CodeEnableServiceFailed: "The proxy service could not be created or updated.",
	CodeEnableSwitchFailed:  "Ingress could not be switched on.",
	CodeEnableHostFailed:    "The host could not be read, so the proxy was not changed.",
	CodeNotRun:              "Not run because an earlier step failed.",
}

// itemLabels are the lines of the job's steps.
var itemLabels = map[string]string{
	ItemToken:   "Make the join token",
	ItemNetwork: "Create the ingress network",
	ItemService: "Create the proxy service",
	ItemSwitch:  "Switch ingress on",
}

// enableItems are the items of the job in order.
var enableItems = []string{ItemToken, ItemNetwork, ItemService, ItemSwitch}

// auditUserAgent is what the join token the job makes is audited under.
const auditUserAgent = "tierplan-ingress-deploy"

// EnableRequest is what a person asks for when they enable ingress.
type EnableRequest struct {
	// Subnet is the CIDR block of the ingress network; empty lets TierPlan choose a free /22.
	Subnet string
}

// Plan is what an enable would create or reuse.
type Plan struct {
	Network struct{ Name, Subnet string }
	Service struct{ Name, Image string }
	// Secret is the swarm secret that carries the join token. Name is empty while the token is yet to be
	// made, because the name carries the token's ID.
	Secret struct {
		Name string
		// New is true when the enable makes a new token and secret.
		New bool
	}
	Volume struct{ Name string }
	// Standalone is true when the plan is a standalone host's: the proxy is one container of a compose project and not a
	// global service, its network is a bridge and its join token is no swarm secret.
	Standalone bool
}

// PreviewResult is what Preview tells a person before they enable.
type PreviewResult struct {
	Findings []Finding
	Plan     Plan
	// Redeploy lists the stacks that hold routes and have to be redeployed to join the ingress network. The
	// enable never changes a stack (D-14); it only lists them.
	Redeploy []StackRef
}

// LockedError means the environment's operation lock is held, so the enable was refused and not queued.
type LockedError struct {
	Holder stackops.Holder
}

// Error says who holds the lock.
func (e *LockedError) Error() string {
	if e.Holder.Kind == "" {
		return "the environment is busy with another operation"
	}
	return "the environment is busy with another operation: " + e.Holder.Kind
}

// stepError is a step that failed: the item's code and the cause, which is logged and not put in a job.
type stepError struct {
	code string
	// detail is the engine's own words, safe to show; empty for the token step.
	detail string
	err    error
}

func (e *stepError) Error() string { return e.code + ": " + e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// live is what the swarm holds of the ingress objects.
type live struct {
	secrets  []dockerx.SecretRow
	networks []dockerx.NetworkRow
	service  *dockerx.ServiceDetail
}

func (l live) secret(name string) (dockerx.SecretRow, bool) {
	for _, r := range l.secrets {
		if r.Name == name {
			return r, true
		}
	}
	return dockerx.SecretRow{}, false
}

func (l live) network() (dockerx.NetworkRow, bool) {
	for _, r := range l.networks {
		if r.Name == ingress.NetworkName {
			return r, true
		}
	}
	return dockerx.NetworkRow{}, false
}

// readLive reads the secrets, the networks and the proxy service of the swarm through c.
func readLive(ctx context.Context, c *dockerx.Client) (live, error) {
	var l live
	var err error
	if l.secrets, err = c.SecretList(ctx); err != nil {
		return live{}, err
	}
	if l.networks, err = c.Networks(ctx); err != nil {
		return live{}, err
	}
	svc, _, err := c.ServiceInspect(ctx, ingress.ServiceName)
	switch {
	case err == nil:
		l.service = &svc
	case errors.Is(err, dockerx.ErrServiceNotFound):
	default:
		return live{}, err
	}
	return l, nil
}

// Preview says what an enable would do and what would stop it, and writes nothing.
func (s *Service) Preview(ctx context.Context, envID string, req EnableRequest) (PreviewResult, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return PreviewResult{}, err
	}
	if !info.Swarm {
		return s.previewStandalone(ctx, info, req)
	}
	findings, redeploy, err := s.preflightRun(ctx, info, req)
	if err != nil {
		return PreviewResult{}, err
	}
	res := PreviewResult{Findings: findings, Redeploy: redeploy}
	res.Plan.Network.Name = ingress.NetworkName
	res.Plan.Service.Name = ingress.ServiceName
	res.Plan.Service.Image = IngressImage(s.d.ServerVersion)
	res.Plan.Volume.Name = ingress.VolumeName
	res.Plan.Secret.New = true
	res.Plan.Network.Subnet = req.Subnet
	if !info.ManagerConnected {
		return res, nil
	}
	st, err := s.d.Tokens.TokenStatus(ctx, envID)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("read the join token: %w", err)
	}
	var l live
	if err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) (err error) {
		l, err = readLive(ctx, c)
		return err
	}); err != nil {
		s.log().Warn("could not read the ingress objects for a preview", "environment", envID, "error", err)
		return res, nil
	}
	if st.Exists && !st.Disabled {
		name := SecretName(envID, st.Generation, st.ID)
		if row, ok := l.secret(name); ok && ingress.OwnsSecret(row.Name, row.Labels) {
			res.Plan.Secret.Name, res.Plan.Secret.New = name, false
		}
	}
	if row, ok := l.network(); ok && ingress.OwnsNetwork(row.Name, row.Labels) && len(row.Subnets) > 0 {
		res.Plan.Network.Subnet = row.Subnets[0]
	} else if req.Subnet == "" {
		if block, err := s.chooseSubnet(ctx, envID, false); err == nil {
			res.Plan.Network.Subnet = block
		}
	}
	return res, nil
}

// chooseSubnet is the lowest free block of the ingress network's size in the swarm's pools.
func (s *Service) chooseSubnet(ctx context.Context, envID string, fresh bool) (string, error) {
	if s.d.AddressSpaces == nil {
		return "", errors.New("no address spaces to choose a subnet from")
	}
	in, err := s.d.AddressSpaces.AllocationInputs(ctx, envID, fresh)
	if err != nil {
		return "", err
	}
	block, _, err := netspace.LowestFree(in.Pools, in.Occupied, in.Avoid, NetworkPrefix)
	if err != nil {
		return "", err
	}
	return block.String(), nil
}

// Enable switches ingress on: it takes the environment's operation lock, runs the preflight, and starts a job. On a
// swarm the job makes the join token and its secret, the network and the proxy service; on a standalone host it makes the
// join token when the proxy has to enrol and brings the proxy's compose project up. Either way it then switches ingress
// on and audits it. It returns the job's ID. A held lock is a *LockedError and a failed preflight a *PreflightError; in
// each case nothing was written. The job changes nothing of any stack (D-14).
func (s *Service) Enable(ctx context.Context, actor audit.Actor, owner, envID string, req EnableRequest) (string, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return "", err
	}
	if !info.Swarm {
		return s.enableStandalone(ctx, actor, owner, info, req)
	}
	specs := make([]jobs.ItemSpec, 0, len(enableItems))
	for _, id := range enableItems {
		specs = append(specs, jobs.ItemSpec{ID: id, Label: itemLabels[id], Kind: "step"})
	}
	return s.startEnable(ctx, JobKindEnable, owner, info, req, s.preflight, specs, func(jctx context.Context, r *jobs.Reporter, release func()) {
		s.runEnable(jctx, r, release, actor, info.ID, req)
	})
}

// startEnable is the part of an enable the two kinds of environment share: the operation lock, the preflight and the
// job of the given kind. The job starts only after the lock names it, so the holder a refused operation is told about is always the whole
// of it. run is the job's body; it is handed release, which frees the lock and is safe to repeat.
func (s *Service) startEnable(ctx context.Context, kind, owner string, info EnvInfo, req EnableRequest,
	pre func(context.Context, EnvInfo, EnableRequest) ([]Finding, error), items []jobs.ItemSpec,
	run func(ctx context.Context, r *jobs.Reporter, release func())) (string, error) {
	envID := info.ID
	release, held, ok := s.d.Locks.TryAcquire(envID, stackops.Holder{
		Kind: stackops.HolderKindIngress, By: owner, Workload: "ingress", Started: s.d.Clock.Now(),
	})
	if !ok {
		return "", &LockedError{Holder: held}
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
	findings, err := pre(ctx, info, req)
	if err != nil {
		return "", err
	}
	if hasErrors(findings) {
		return "", &PreflightError{Findings: findings}
	}
	attached := make(chan struct{})
	job, err := s.d.Jobs.Start(jobs.Spec{Kind: kind, EnvID: envID, Owner: owner, Items: items},
		func(jctx context.Context, r *jobs.Reporter) {
			defer release()
			select {
			case <-attached:
			case <-jctx.Done():
			}
			run(jctx, r, release)
		})
	if err != nil {
		return "", err
	}
	started = true
	s.d.Locks.Attach(envID, job.ID)
	close(attached)
	return job.ID, nil
}

// enableRun is the state the steps of one enable share.
type enableRun struct {
	s     *Service
	actor audit.Actor
	envID string
	req   EnableRequest
	r     *jobs.Reporter
	image string
	// release frees the environment's operation lock. It is called before the item that ends the job is
	// finished, so a reader that sees the job done finds the lock free; freeing is safe to repeat.
	release func()

	secretID, secretName string
	subnet               string
	// items are the items of the job this run ends, in order.
	items []string
}

// runEnable runs the steps, one at a time. The first failure ends its item with a code and the items after
// it as not run; nothing is retried and nothing is rolled back.
func (s *Service) runEnable(ctx context.Context, r *jobs.Reporter, release func(), actor audit.Actor, envID string, req EnableRequest) {
	run := &enableRun{s: s, actor: actor, envID: envID, req: req, r: r, release: release, image: IngressImage(s.d.ServerVersion), items: enableItems}
	steps := []struct {
		item string
		do   func(context.Context) error
	}{
		{ItemToken, run.token},
		{ItemNetwork, run.network},
		{ItemService, run.service},
		{ItemSwitch, run.enableSwitch},
	}
	for i, st := range steps {
		if ctx.Err() != nil {
			// The row is written before the items that end the job, so a reader that sees the job over finds it.
			s.auditJobFailed(ctx, actor, audit.ActionIngressEnabled, envID, steps[i].item, jobs.CodeCancelled)
			run.stop(steps[i].item, jobs.ItemCancelled, jobs.CodeCancelled, "")
			run.skipFrom(i+1, jobs.ItemCancelled, jobs.CodeCancelled)
			return
		}
		r.Update(st.item, func(it *jobs.Item) { it.State = jobs.ItemRunning })
		if err := st.do(ctx); err != nil {
			code, detail := CodeNotRun, ""
			var se *stepError
			if errors.As(err, &se) {
				code, detail = se.code, se.detail
			}
			s.log().Warn("an ingress enable step failed", "environment", envID, "step", st.item, "code", code, "error", err)
			s.noteEnableFailed(ctx, envID)
			r.Output("Ingress is still off. Switch it on again to carry on from what was made, or switch it off to remove it.")
			s.auditJobFailed(ctx, actor, audit.ActionIngressEnabled, envID, st.item, code)
			run.stop(st.item, jobs.ItemFailed, code, detail)
			run.skipFrom(i+1, jobs.ItemSkipped, CodeNotRun)
			return
		}
	}
}

// noteEnableFailed records that an enable failed on an environment whose ingress is still off, so the status does not read the
// objects it left as an incomplete switch-off (IN-11). An environment whose ingress is on keeps the loop's own record.
func (s *Service) noteEnableFailed(ctx context.Context, envID string) {
	state, err := s.d.Store.DeployState(ctx, envID)
	if err != nil || state.Enabled {
		return
	}
	if err := s.d.Store.RecordEnsure(ctx, envID, s.d.Clock.Now(), EnsureEnableFailed); err != nil {
		s.log().Warn("could not record a failed ingress enable", "environment", envID, "error", err)
	}
}

// clearEnableFailed takes the failed-enable record away, with no pass time: a successful enable and a switch-off both end it.
func (s *Service) clearEnableFailed(ctx context.Context, envID string) {
	state, err := s.d.Store.DeployState(ctx, envID)
	if err != nil || state.EnsureError != EnsureEnableFailed {
		return
	}
	if err := s.d.Store.RecordEnsure(ctx, envID, time.Time{}, EnsureOK); err != nil {
		s.log().Warn("could not clear a failed ingress enable", "environment", envID, "error", err)
	}
}

// stop ends the item that failed or was cancelled.
func (e *enableRun) stop(item, state, code, detail string) {
	e.release()
	if detail != "" {
		e.r.Detail(item, detail)
	}
	msg := codeMessages[code]
	if code == jobs.CodeCancelled {
		msg = ""
	}
	e.r.Finish(item, state, code, msg, nil)
}

// skipFrom ends the items from index i on without running them.
func (e *enableRun) skipFrom(i int, state, code string) {
	for _, id := range e.items[i:] {
		e.r.Finish(id, state, code, codeMessages[code], nil)
	}
}

// done ends an item that went well.
func (e *enableRun) done(item string, result map[string]string) {
	e.r.Finish(item, jobs.ItemDone, "", "", result)
}

// fail builds a step's failure.
func fail(code string, err error, detail ...string) error {
	se := &stepError{code: code, err: err}
	if len(detail) > 0 {
		se.detail = detail[0]
	}
	return se
}

// token makes sure the current join token's secret exists on the swarm, through the step the ensure loop shares
// (ensureSecret). A token that is missing, disabled, or whose secret is gone is replaced by the next generation
// (Pitfall 10).
func (e *enableRun) token(ctx context.Context) error {
	s := e.s
	err := s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) (err error) {
		e.secretID, e.secretName, err = s.ensureSecret(ctx, c, e.actor, e.envID, true)
		return err
	})
	if err != nil {
		// The engine's words are not shown: an error that touched the token step is never put in a job.
		return fail(CodeEnableTokenFailed, err)
	}
	e.done(ItemToken, map[string]string{"secret": e.secretName})
	return nil
}

// network makes sure the ingress network exists, through the step the ensure loop shares (ensureNetwork).
func (e *enableRun) network(ctx context.Context) error {
	s := e.s
	err := s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) (err error) {
		e.subnet, err = s.ensureNetwork(ctx, c, e.envID, e.req.Subnet)
		return err
	})
	if err != nil {
		var se *stepError
		if errors.As(err, &se) {
			return se
		}
		return fail(CodeEnableNetworkFailed, err, dockerx.DaemonText(err))
	}
	e.done(ItemNetwork, map[string]string{"subnet": e.subnet})
	return nil
}

// service creates the global proxy service, or, when one that is TierPlan's exists, updates it, through the step
// the ensure loop shares (ensureService). An enable always writes the update of an existing service.
func (e *enableRun) service(ctx context.Context) error {
	s := e.s
	state, err := s.d.Store.DeployState(ctx, e.envID)
	if err != nil {
		return fail(CodeEnableServiceFailed, err)
	}
	auth, err := s.authFor(ctx, e.envID, e.image)
	if err != nil {
		return fail(CodeEnableServiceFailed, err)
	}
	in := serviceInput{envID: e.envID, secretID: e.secretID, secretName: e.secretName, accessLog: state.AccessLogs, auth: auth, always: true}
	err = s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) error {
		_, err := s.ensureService(ctx, c, in)
		return err
	})
	if err != nil {
		var se *stepError
		if errors.As(err, &se) {
			return se
		}
		return fail(CodeEnableServiceFailed, err, dockerx.DaemonText(err))
	}
	e.done(ItemService, map[string]string{"image": e.image})
	return nil
}

// enableSwitch turns the environment's ingress on and writes the audit row in the same transaction.
func (e *enableRun) enableSwitch(ctx context.Context) error {
	s := e.s
	hook := func(ctx context.Context, tx *sql.Tx) error {
		return s.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: e.actor, Action: audit.ActionIngressEnabled, TargetType: "environment", TargetID: e.envID,
			EnvID: e.envID, Result: audit.ResultSuccess,
			Details: map[string]string{"subnet": e.subnet, "image": e.image},
		})
	}
	if _, err := s.d.Store.SetEnabled(ctx, e.envID, true, e.actor.ID, hook); err != nil {
		return fail(CodeEnableSwitchFailed, err)
	}
	s.clearEnableFailed(ctx, e.envID)
	e.release()
	e.done(ItemSwitch, nil)
	return nil
}

// authFor chooses the registry credential the proxy image is sent with, "" for none or no resolver.
func (s *Service) authFor(ctx context.Context, envID, image string) (string, error) {
	if s.d.Resolver == nil {
		return "", nil
	}
	ref, err := registry.ParseRef(image)
	if err != nil {
		// The engine rejects an unparsable reference with its own words; there is no credential to choose.
		return "", nil //nolint:nilerr // see above
	}
	header, _, err := s.d.Resolver.AuthFor(ctx, envID, ref)
	return header, err
}

// liveForceUpdate is the force-update counter of a service's spec as the swarm holds it, zero when the spec
// does not say.
func liveForceUpdate(spec json.RawMessage) uint64 {
	var s struct {
		TaskTemplate struct{ ForceUpdate uint64 }
	}
	if json.Unmarshal(spec, &s) != nil {
		return 0
	}
	return s.TaskTemplate.ForceUpdate
}

// ensureSecret makes sure the swarm holds the secret of the current join token and returns its ID and name. It is the
// token step of the enable job and of the ensure loop; c is a manager's client, inside one write. A current, enabled
// token whose secret is there is reused; a token that is missing, or whose secret is gone, is replaced by the next
// generation, and a disabled token too when replaceDisabled is set (an enable) (Pitfall 10). With replaceDisabled
// false a disabled token is an admin's decision the loop does not undo: its secret is returned when it exists, and
// otherwise nothing is made and the ID and name come back empty. The token's clear text lives in a local variable
// between the issue and the secret create and is put in no item, line, log or audit detail.
func (s *Service) ensureSecret(ctx context.Context, c *dockerx.Client, actor audit.Actor, envID string, replaceDisabled bool) (id, name string, err error) {
	rows, err := c.SecretList(ctx)
	if err != nil {
		return "", "", err
	}
	st, err := s.d.Tokens.TokenStatus(ctx, envID)
	if err != nil {
		return "", "", err
	}
	if st.Exists && (!st.Disabled || !replaceDisabled) {
		name := SecretName(envID, st.Generation, st.ID)
		for _, row := range rows {
			if row.Name != name {
				continue
			}
			if !ingress.OwnsSecret(row.Name, row.Labels) {
				return "", "", fmt.Errorf("the swarm holds a secret named %s that is not TierPlan's", name)
			}
			return row.ID, name, nil
		}
		if st.Disabled {
			return "", "", nil
		}
	}
	tok, err := s.d.Tokens.IssueToken(ctx, actor, envID, "", auditUserAgent)
	if err != nil {
		return "", "", fmt.Errorf("issue the join token: %w", err)
	}
	name = SecretName(envID, tok.Generation, tok.ID)
	id, err = c.SecretCreate(ctx, SecretSpec(name, []byte(tok.Token)))
	if err != nil {
		return "", "", err
	}
	return id, name, nil
}

// ensureNetwork makes sure the ingress network exists and returns its subnet: an existing one that is TierPlan's is
// reused, a same-named one that is not is refused, and otherwise a /22 (or the requested block) is made from the
// swarm's pool under the allocation lock. It is the network step of the enable job and of the ensure loop; c is a
// manager's client, inside one write. A refusal is a *stepError that carries the sentence to show.
func (s *Service) ensureNetwork(ctx context.Context, c *dockerx.Client, envID, requested string) (string, error) {
	rows, err := c.Networks(ctx)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.Name != ingress.NetworkName {
			continue
		}
		if !ingress.OwnsNetwork(row.Name, row.Labels) {
			return "", &stepError{code: CodeEnableNetworkFailed, err: fmt.Errorf("the swarm holds a network named %s that is not TierPlan's", row.Name),
				detail: "A network named " + row.Name + " already exists and is not TierPlan's. Remove or rename it, then enable ingress again."}
		}
		if len(row.Subnets) > 0 {
			return row.Subnets[0], nil
		}
		return "", nil
	}
	if s.d.AddressSpaces != nil {
		defer s.d.AddressSpaces.LockAllocation(envID)()
	}
	subnet := requested
	if subnet == "" {
		subnet, err = s.chooseSubnet(ctx, envID, true)
		if err != nil {
			return "", &stepError{code: CodeEnableNetworkFailed, err: err,
				detail: "No free " + fmt.Sprintf("/%d", NetworkPrefix) + " block is left in the swarm's address pools."}
		}
	} else if _, err := netip.ParsePrefix(subnet); err != nil {
		return "", &stepError{code: CodeEnableNetworkFailed, err: err, detail: "The subnet is not a valid CIDR block."}
	}
	if _, _, err := c.NetworkCreateAllocated(ctx, NetworkSpec(subnet), s.d.AllocationWait); err != nil {
		return "", &stepError{code: CodeEnableNetworkFailed, err: err, detail: dockerx.DaemonText(err)}
	}
	return subnet, nil
}

// serviceInput is what the service step needs to build the proxy service.
type serviceInput struct {
	// envID names the environment, for the memory of the last spec written to its service.
	envID                string
	secretID, secretName string
	accessLog            bool
	// auth is the registry credential the proxy image is sent with, "" for none.
	auth string
	// always writes the update of an existing service even when it already matches (an enable does, so a repeat
	// brings the service to the spec); false writes it only on a difference (the ensure loop).
	always bool
}

// desiredService is the proxy service this server wants, with the ID of its secret filled in.
func (s *Service) desiredService(in serviceInput) swarm.ServiceSpec {
	spec := ProxyServiceSpec(SpecInput{
		Image: IngressImage(s.d.ServerVersion), URL: s.url(), Pin: strings.Join(s.pins(), ","), SecretName: in.secretName, AccessLog: in.accessLog,
	})
	spec.TaskTemplate.ContainerSpec.Secrets[0].SecretID = in.secretID
	return spec
}

// serviceChanges lists how the live spec differs from the desired one. Both go through the swarm's defaults first
// (stacks.NormalizeSpec) and the force-update counter is taken from the live spec; the live spec names its networks
// by ID, so they are put back to names with nets before the two are compared. A spec that cannot be read is an error.
func serviceChanges(desired swarm.ServiceSpec, liveSpec json.RawMessage, nets []dockerx.NetworkRow) ([]stacks.PathChange, error) {
	var cur swarm.ServiceSpec
	if err := json.Unmarshal(liveSpec, &cur); err != nil {
		return nil, fmt.Errorf("read the proxy service's spec: %w", err)
	}
	for i := range cur.TaskTemplate.Networks {
		t := &cur.TaskTemplate.Networks[i]
		for _, n := range nets {
			if n.ID != "" && n.ID == t.Target {
				t.Target = n.Name
			}
		}
	}
	desired.TaskTemplate.ForceUpdate = cur.TaskTemplate.ForceUpdate
	return stacks.ChangedPaths(stacks.NormalizeSpec(cur), stacks.NormalizeSpec(desired)), nil
}

// ensureService creates the global proxy service, or, when one that is TierPlan's exists, updates it at the version
// read, carrying its force-update counter over so the update does not restart the proxy by itself. Unless in.always is
// set it writes only when the live spec differs from the desired one. It reports whether it wrote. It is the service
// step of the enable job and of the ensure loop; c is a manager's client, inside one write. Nothing waits for the
// update to converge.
func (s *Service) ensureService(ctx context.Context, c *dockerx.Client, in serviceInput) (bool, error) {
	spec := s.desiredService(in)
	cur, _, err := c.ServiceInspect(ctx, ingress.ServiceName)
	switch {
	case err == nil:
		if !ingress.OwnsService(cur.Name, cur.Labels) {
			return false, &stepError{code: CodeEnableServiceFailed, err: errors.New("the swarm holds a service named " + cur.Name + " that is not TierPlan's"),
				detail: "A service named " + cur.Name + " already exists and is not TierPlan's. Remove or rename it, then enable ingress again."}
		}
		spec.TaskTemplate.ForceUpdate = liveForceUpdate(cur.Spec)
		if !in.always {
			nets, err := c.Networks(ctx)
			if err != nil {
				return false, err
			}
			changes, err := serviceChanges(spec, cur.Spec, nets)
			if err != nil {
				return false, err
			}
			if len(changes) == 0 {
				return false, nil
			}
			if s.updateHeld(in.envID, &cur, spec) {
				return false, errUpdateHeld
			}
		}
		raw, err := json.Marshal(spec)
		if err != nil {
			return false, err
		}
		if _, err = c.ServiceUpdate(ctx, cur.ID, cur.Version, raw, in.auth); err != nil {
			return false, err
		}
		s.noteWritten(in.envID, spec)
		return true, nil
	case errors.Is(err, dockerx.ErrServiceNotFound):
		raw, err := json.Marshal(spec)
		if err != nil {
			return false, err
		}
		if _, _, err = c.ServiceCreate(ctx, raw, in.auth); err != nil {
			return false, err
		}
		s.noteWritten(in.envID, spec)
		return true, nil
	default:
		return false, err
	}
}
