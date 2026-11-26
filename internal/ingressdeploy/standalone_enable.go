// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// errSystemFailed is the standalone enable's own word for a system deploy that failed: the deploy has finished its
// items with the codes of its own, so nothing is left for the enable to end but the switch.
var errSystemFailed = errors.New("the proxy's project did not come up")

// standaloneSpecs are the items of the standalone enable job, in the order they run: the token, the items of the system
// deploy, the switch.
func standaloneSpecs() []jobs.ItemSpec {
	specs := []jobs.ItemSpec{{ID: ItemToken, Label: itemLabels[ItemToken], Kind: "step"}}
	specs = append(specs, stackops.SystemItemSpecs()...)
	return append(specs, jobs.ItemSpec{ID: ItemSwitch, Label: itemLabels[ItemSwitch], Kind: "step"})
}

// previewStandalone says what an enable on a standalone host would do and what would stop it, and writes nothing. The
// host is read only when it is connected.
func (s *Service) previewStandalone(ctx context.Context, info EnvInfo, req EnableRequest) (PreviewResult, error) {
	findings, redeploy, err := s.preflightStandalone(ctx, info, req)
	if err != nil {
		return PreviewResult{}, err
	}
	res := PreviewResult{Findings: findings, Redeploy: redeploy}
	res.Plan.Standalone = true
	res.Plan.Network.Name = ingress.NetworkName
	res.Plan.Service.Name = ingress.ProjectName
	res.Plan.Service.Image = IngressImage(s.d.ServerVersion)
	res.Plan.Volume.Name = ingress.VolumeName
	res.Plan.Secret.New = true
	res.Plan.Network.Subnet = req.Subnet
	if len(info.Nodes) == 0 || !info.Nodes[0].Connected {
		return res, nil
	}
	l, err := s.readStandaloneLive(ctx, info)
	if err != nil {
		s.log().Warn("could not read the host for an ingress preview", "environment", info.ID, "error", err)
		return res, nil
	}
	if l.network != nil && len(l.network.Subnets) > 0 {
		res.Plan.Network.Subnet = l.network.Subnets[0]
	}
	if plan, err := s.planFor(ctx, info.ID, l); err == nil {
		res.Plan.Secret.New = plan.Up && plan.Token
	}
	return res, nil
}

// planFor decides what the host needs from what it holds and what the server knows.
func (s *Service) planFor(ctx context.Context, envID string, l standaloneLive) (standalonePlan, error) {
	tok, err := s.d.Tokens.TokenStatus(ctx, envID)
	if err != nil {
		return standalonePlan{}, err
	}
	state, err := s.d.Store.DeployState(ctx, envID)
	if err != nil {
		return standalonePlan{}, err
	}
	enrolled, err := s.enrolled(ctx, envID, l)
	if err != nil {
		return standalonePlan{}, err
	}
	return planStandalone(l, IngressImage(s.d.ServerVersion), s.url(), strings.Join(s.pins(), ","), state.AccessLogs, enrolled, tok), nil
}

// enableStandalone is Enable for a standalone host: the lock, the preflight and a job of the token, the proxy's project
// and the switch.
func (s *Service) enableStandalone(ctx context.Context, actor audit.Actor, owner string, info EnvInfo, req EnableRequest) (string, error) {
	pre := func(ctx context.Context, info EnvInfo, req EnableRequest) ([]Finding, error) {
		f, _, err := s.preflightStandalone(ctx, info, req)
		if err == nil && !hasErrors(f) && s.d.System == nil {
			// A wiring fault, found after the preflight so a person is told what is wrong with their host first.
			return nil, errors.New("ingress: no deployer of the standalone proxy's project")
		}
		return f, err
	}
	return s.startEnable(ctx, JobKindEnable, owner, info, req, pre, standaloneSpecs(), func(jctx context.Context, r *jobs.Reporter, release func()) {
		s.runStandaloneEnable(jctx, r, release, actor, info, req)
	})
}

// standaloneEnable is the state the steps of one standalone enable share.
type standaloneEnable struct {
	*enableRun
	info EnvInfo
	node string
	plan standalonePlan
	// tok is the join token made for the up. Its clear text lives in this field and in the project built from it, and
	// in no item, line, log or audit detail (D-08).
	tok *StandaloneToken
}

// runStandaloneEnable runs the steps, one at a time. The first failure ends its item with a code and the items after it
// as not run; nothing is retried and nothing is rolled back.
func (s *Service) runStandaloneEnable(ctx context.Context, r *jobs.Reporter, release func(), actor audit.Actor, info EnvInfo, req EnableRequest) {
	run := &standaloneEnable{
		enableRun: &enableRun{s: s, actor: actor, envID: info.ID, req: req, r: r, release: release, image: IngressImage(s.d.ServerVersion)},
		info:      info, node: info.Nodes[0].AgentID,
	}
	system := make([]string, 0, 6)
	for _, sp := range stackops.SystemItemSpecs() {
		system = append(system, sp.ID)
	}
	steps := []struct {
		ids []string
		do  func(context.Context) error
	}{
		{[]string{ItemToken}, run.tokenStep},
		{system, run.systemStep},
		{[]string{ItemSwitch}, run.standaloneSwitch},
	}
	for i, st := range steps {
		if ctx.Err() != nil {
			run.release()
			for _, later := range steps[i:] {
				for _, id := range later.ids {
					r.Finish(id, jobs.ItemCancelled, jobs.CodeCancelled, "", nil)
				}
			}
			s.auditJobFailed(ctx, actor, audit.ActionIngressEnabled, info.ID, steps[i].ids[0], jobs.CodeCancelled)
			return
		}
		if len(st.ids) == 1 {
			r.Update(st.ids[0], func(it *jobs.Item) { it.State = jobs.ItemRunning })
		}
		err := st.do(ctx)
		if err == nil {
			continue
		}
		code, detail := CodeNotRun, ""
		var se *stepError
		if errors.As(err, &se) {
			code, detail = se.code, se.detail
		}
		step := st.ids[0]
		if errors.Is(err, errSystemFailed) {
			// The deploy ended its own items with codes of its own; the row names the step as a whole.
			step, code = auditStepSystem, CodeEnableSystemFailed
		}
		s.log().Warn("an ingress enable step failed", "environment", info.ID, "step", st.ids[0], "code", code, "error", err)
		s.noteEnableFailed(ctx, info.ID)
		r.Output("Ingress is still off. Switch it on again to carry on from what was made, or switch it off to remove it.")
		run.release()
		if !errors.Is(err, errSystemFailed) {
			if detail != "" {
				r.Detail(st.ids[0], detail)
			}
			r.Finish(st.ids[0], jobs.ItemFailed, code, codeMessages[code], nil)
			for _, id := range st.ids[1:] {
				r.Finish(id, jobs.ItemSkipped, CodeNotRun, codeMessages[CodeNotRun], nil)
			}
		}
		for _, later := range steps[i+1:] {
			for _, id := range later.ids {
				r.Finish(id, jobs.ItemSkipped, CodeNotRun, codeMessages[CodeNotRun], nil)
			}
		}
		s.auditJobFailed(ctx, actor, audit.ActionIngressEnabled, info.ID, step, code)
		return
	}
}

// tokenStep reads the host, decides what it needs (planStandalone) and makes the join token when the up that follows
// has to carry one. The token is kept in a field of the run and nowhere else: the item says whether one was made and
// which generation it is, never the value (D-08).
func (e *standaloneEnable) tokenStep(ctx context.Context) error {
	s := e.s
	l, err := s.readStandaloneLive(ctx, e.info)
	if err != nil {
		return fail(CodeEnableHostFailed, err)
	}
	e.plan, err = s.planFor(ctx, e.envID, l)
	if err != nil {
		return fail(CodeEnableTokenFailed, err)
	}
	if l.network != nil && len(l.network.Subnets) > 0 {
		e.subnet = l.network.Subnets[0]
	}
	if !e.plan.Up || !e.plan.Token {
		e.done(ItemToken, map[string]string{"issued": "false"})
		return nil
	}
	tok, err := s.d.Tokens.IssueToken(ctx, e.actor, e.envID, "", auditUserAgent)
	if err != nil {
		// The engine's words are not shown: an error that touched the token step is never put in a job.
		return fail(CodeEnableTokenFailed, err)
	}
	e.tok = &StandaloneToken{ID: tok.ID, Value: tok.Token}
	e.done(ItemToken, map[string]string{"issued": "true", "generation": strconv.Itoa(tok.Generation)})
	return nil
}

// systemStep brings the proxy's project up through the standalone pipeline, without waiting for the proxy to enrol
// (D-28): the job ends when the project is up, and the status says whether the proxy has enrolled. A host that holds what
// is wanted is not touched.
func (e *standaloneEnable) systemStep(ctx context.Context) error {
	s := e.s
	if !e.plan.Up {
		for _, sp := range stackops.SystemItemSpecs() {
			e.r.Finish(sp.ID, jobs.ItemDone, "", "", map[string]string{"unchanged": "true"})
		}
		return nil
	}
	state, err := s.d.Store.DeployState(ctx, e.envID)
	if err != nil {
		return fail(CodeEnableHostFailed, err)
	}
	project := StandaloneProject(StandaloneInput{
		Image: e.image, URL: s.url(), Pin: strings.Join(s.pins(), ","), AccessLog: state.AccessLogs, Subnet: e.subnet, Token: e.tok,
	})
	res := s.d.System.DeploySystem(ctx, stackops.SystemDeployInput{
		EnvID: e.envID, NodeID: e.node, Project: project, Network: standaloneNetworkKey, Prefix: StandaloneNetworkPrefix,
		Subnet: e.req.Subnet, NoWait: true,
	}, e.r)
	if !res.OK() {
		return errSystemFailed
	}
	if n, ok := project.Networks[standaloneNetworkKey]; ok && len(n.Ipam.Config) > 0 && n.Ipam.Config[0] != nil && n.Ipam.Config[0].Subnet != "" {
		e.subnet = n.Ipam.Config[0].Subnet
	}
	return nil
}

// standaloneSwitch turns ingress on once the ingress network is made and its ID known, unless a container of the host is
// stored on a network name that is the start of that ID (T-25-47): then ingress stays off and the containers are named.
// A connect made after the network exists is refused by its own check, so nothing can slip in after this read.
func (e *standaloneEnable) standaloneSwitch(ctx context.Context) error {
	var refs []dockerx.ShadowRef
	err := e.s.d.Engine.OnNode(ctx, e.envID, e.node, func(c *dockerx.Client) (err error) {
		refs, err = c.ContainersShadowingIngress(ctx)
		return err
	})
	if err != nil {
		return fail(CodeEnableHostFailed, err)
	}
	if len(refs) > 0 {
		return fail(CodeNameShadowed, errors.New("a container is stored on a network name that is the start of the ingress network's ID"), shadowDetail(refs))
	}
	return e.enableSwitch(ctx)
}
