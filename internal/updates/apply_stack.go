// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// Judging the update of the services of a stack TierPlan manages (plan 12-18, IMG-06, D-06, D-15). The stack is updated
// by a revision of its own, which the API layer makes and deploys; this file decides which services it changes: the ones
// with an update available, leaving out the skipped and ignored, and, for a click, asks the registry again and refuses what
// moved since the person confirmed. It writes nothing.

// StackRequest asks which services of a stack an update would change.
type StackRequest struct {
	EnvID string
	// Stack is the stack's name: its swarm namespace or its Compose project.
	Stack string
	// Services, when not empty, limits the update to those services, by the name the stack's pins use: the swarm service name
	// of a swarm stack, the compose service name of a standalone one.
	Services []string
	// Expect maps a service name to the digest the confirm showed. It is only compared with the digest the registry gives
	// now; it is never deployed.
	Expect map[string]string
	// Recheck asks the registries again for the images of the stack's services, bypassing the stored checks, as a click does.
	// A preview leaves it off and reads the stored checks.
	Recheck bool
	// Force is a person's explicit request to try again a version of one service that failed before (D-14): with exactly one service
	// named in Services, that service is a target although it reads rolled_back or rollback_failed. It means nothing for a request
	// that names no service or several, so an update of a whole stack never retries a failure. Without it, such a service is left out
	// of a stack's update, and a request for it alone is refused with *PreviouslyFailedError.
	Force bool
}

// StackTarget is a service of a stack whose image an update would change.
type StackTarget struct {
	// Key is the service as the stack's pins and the engine's update set name it: the swarm service name of a swarm stack, the
	// compose service name of a standalone one.
	Key string
	// Short is the service's name in the compose file.
	Short string
	// Name is the workload the judgement and the memory of a failed update file the service under: the swarm service's name, or the
	// name of the container a standalone host runs it in.
	Name string
	// Swarm is true for a swarm service and false for a service of a standalone host.
	Swarm bool
	// Image is the image the service follows, without a digest (repository:tag), and Repo its repository as the registry knows it.
	Image, Repo string
	// From is the digest the service runs and To the one the registry gave.
	From, To string
	// Platforms are the platforms of the image To, from the check, for the deploy to send without asking a registry again.
	// RunsOn are the platforms the service runs on (os/arch words) and ChangedPlatforms those of them whose image changed.
	Platforms        []swarm.Platform
	RunsOn           []string
	ChangedPlatforms []string
	// ServiceID is the swarm service's ID, empty on a standalone host.
	ServiceID string
	// Failure is the earlier failed update this target retries, nil for a target that has none. It is only set for a forced one.
	Failure *Failure
}

// StackLeftOut is a service of a stack that an update leaves out, and why: it is skipped or ignored.
type StackLeftOut struct {
	Name  string
	State State
}

// StackPlan is what an update of a stack would do.
type StackPlan struct {
	Targets []StackTarget
	LeftOut []StackLeftOut
}

// stackMember is one service of the stack with its verdict, and the name it goes by.
type stackMember struct {
	key, short string
	sv         ServiceVerdict
}

// PlanStack works out which services of a stack an update changes. ErrWorkloadGone is returned when the environment has no
// workload of that stack, or none of the services asked for. A service that is skipped or ignored is left out and named; a
// service in any other state than update_available changes nothing. When Services names exactly one service and it has no
// update, the answer says why (*NotUpdatableError, *CheckFailedError, ErrNothingToUpdate), as for a service on its own. With
// Recheck, a *MovedError carries the services whose digest is not the one Expect names.
func (a *Applier) PlanStack(ctx context.Context, req StackRequest) (StackPlan, error) {
	if a.Service == nil {
		return StackPlan{}, errors.New("updates: the applier is not wired")
	}
	if req.Recheck {
		if err := a.recheckStack(ctx, req); err != nil {
			return StackPlan{}, err
		}
	}
	_, per, err := a.Service.StackVerdicts(ctx, req.EnvID)
	if err != nil {
		return StackPlan{}, err
	}
	members := stackMembers(req.Stack, per[req.Stack])
	if len(members) == 0 {
		return StackPlan{}, ErrWorkloadGone
	}
	if len(req.Services) > 0 {
		kept := members[:0:0]
		for _, m := range members {
			if slices.Contains(req.Services, m.key) || slices.Contains(req.Services, m.short) {
				kept = append(kept, m)
			}
		}
		if len(kept) == 0 {
			return StackPlan{}, ErrWorkloadGone
		}
		members = kept
	}

	// A forced retry is one person's choice of one service (D-14); a request for more than one never forces.
	force := req.Force && len(req.Services) == 1 && len(members) == 1
	var plan StackPlan
	var moved []MovedItem
	for _, m := range members {
		v, w := m.sv.Verdict, m.sv.Workload
		var retried *Failure
		switch v.State {
		case StateSkipped, StateIgnored:
			plan.LeftOut = append(plan.LeftOut, StackLeftOut{Name: m.key, State: v.State})
			continue
		case StateUpdateAvailable:
		case StateRolledBack, StateRollbackFailed:
			// The version failed before: only a person's forced request for this one service tries it again.
			if !force {
				continue
			}
			retried = v.Failure
		default:
			continue
		}
		remote := v.RemoteDigest
		if remote == "" || remote == v.RunningDigest {
			continue
		}
		if req.Recheck {
			if want, ok := req.Expect[m.key]; ok && want != remote {
				moved = append(moved, MovedItem{Service: m.key, FromDigest: v.RunningDigest, ToDigest: remote})
				continue
			}
			if want, ok := req.Expect[m.short]; ok && want != remote {
				moved = append(moved, MovedItem{Service: m.key, FromDigest: v.RunningDigest, ToDigest: remote})
				continue
			}
		}
		if !digestGrammar.MatchString(remote) {
			return StackPlan{}, fmt.Errorf("the registry answered %s with something that is not a sha256 digest", w.Ref.Repo())
		}
		t := StackTarget{
			Key: m.key, Short: m.short, Name: strings.TrimPrefix(w.Name, "/"), Swarm: w.Kind == "service", Image: imageOf(w), Repo: w.Ref.Repo(), From: v.RunningDigest, To: remote,
			RunsOn: append([]string{}, w.Platforms...), ChangedPlatforms: append([]string{}, v.ChangedPlatforms...),
		}
		if t.Swarm {
			t.ServiceID = w.ID
		}
		t.Failure = retried
		t.Platforms = a.checkedPlatforms(ctx, req.EnvID, w, remote)
		plan.Targets = append(plan.Targets, t)
	}
	if len(moved) > 0 {
		return StackPlan{}, &MovedError{Items: moved}
	}
	if len(plan.Targets) == 0 {
		if len(req.Services) == 1 && len(members) == 1 {
			return StackPlan{}, singleServiceRefusal(members[0].sv)
		}
		return plan, ErrNothingToUpdate
	}
	return plan, nil
}

// singleServiceRefusal is why one service of a stack, asked for on its own, has no update.
func singleServiceRefusal(sv ServiceVerdict) error {
	v, w := sv.Verdict, sv.Workload
	switch {
	case w.PinnedInFile() || v.State == StatePinned:
		return &NotUpdatableError{Code: NotUpdatablePinned}
	case !w.Checkable() || v.State == StateNotFromRegistry:
		return &NotUpdatableError{Code: NotUpdatableNoSource}
	case v.State == StateCheckFailed:
		return &CheckFailedError{Reason: v.Reason}
	case v.State == StateNotChecked:
		return &NotUpdatableError{Code: NotUpdatableNotChecked}
	case v.State == StateRolledBack || v.State == StateRollbackFailed:
		return &PreviouslyFailedError{Failure: v.Failure}
	}
	return ErrNothingToUpdate
}

// stackMembers names the services of a stack as the pins do, in a fixed order: a swarm service by its name, a service of a
// standalone host by its compose name. The map is the one StackVerdicts answers: by swarm service ID or compose service name.
func stackMembers(stack string, per map[string]ServiceVerdict) []stackMember {
	out := make([]stackMember, 0, len(per))
	for id, sv := range per {
		m := stackMember{sv: sv}
		if sv.Workload.Kind == "service" {
			m.key = sv.Workload.Name
			m.short = strings.TrimPrefix(m.key, stack+"_")
		} else {
			m.key = id
			m.short = id
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b stackMember) int { return strings.Compare(a.key, b.key) })
	return out
}

// recheckStack asks the registries again for the images of the stack's services and saves the answers, so the verdicts that
// follow are the registry's now. It is the check at the click, as recheck is for one service.
func (a *Applier) recheckStack(ctx context.Context, req StackRequest) error {
	if a.Service.Checker == nil {
		return errors.New("updates: the applier has no checker")
	}
	ws, err := a.Service.Workloads(ctx, req.EnvID)
	if err != nil {
		return err
	}
	var members []Workload
	for _, w := range ws {
		if w.Stack != req.Stack || w.Task || !w.Checkable() {
			continue
		}
		if w.Kind == "container" && w.Labels[stacks.LabelComposeService] == "" {
			continue
		}
		members = append(members, w)
	}
	if len(members) == 0 {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	if _, err := a.Service.Checker.CheckEnv(cctx, req.EnvID, members); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("check the images of stack %s: %w", req.Stack, err)
	}
	return nil
}

// checkedPlatforms are the platforms the stored check found for the digest of the workload's tag, as the swarm's own type. None when the check stored none (a single-manifest image) or cannot be read; the deploy then asks the
// daemon once, as it always did.
func (a *Applier) checkedPlatforms(ctx context.Context, envID string, w Workload, digest string) []swarm.Platform {
	if a.Service == nil || a.Service.Store == nil || a.Service.Resolver == nil || !w.Checkable() {
		return nil
	}
	cred, err := a.Service.Resolver.CredentialFor(ctx, envID, w.Ref)
	if err != nil {
		return nil
	}
	k := Key{Host: w.Ref.Domain, Repo: w.Ref.Path, Tag: w.Ref.Tag, CredID: cred}
	stored, err := a.Service.Store.Checks(ctx, []Key{k})
	if err != nil {
		return nil
	}
	chk, ok := stored[k]
	if !ok || chk.Digest != digest || !chk.Index || len(chk.Platforms) == 0 {
		return nil
	}
	words := make([]string, 0, len(chk.Platforms))
	for p := range chk.Platforms {
		words = append(words, p)
	}
	slices.Sort(words)
	out := make([]swarm.Platform, 0, len(words))
	for _, p := range words {
		parts := strings.Split(p, "/")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		out = append(out, swarm.Platform{OS: parts[0], Architecture: parts[1]})
	}
	return out
}

// ManagedStackOf answers the ID of the stack TierPlan manages that the service named name belongs to, and "" when the service
// is a plain one (it names no stack, or its stack was only discovered). ErrWorkloadGone is returned when there is no such
// service.
func (a *Applier) ManagedStackOf(ctx context.Context, envID, name string) (string, error) {
	row, found, err := a.serviceRow(ctx, envID, name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrWorkloadGone
	}
	return a.managedStackID(ctx, envID, row)
}

// managedStackID is the ID the row's labels name when the environment manages that stack, else "".
func (a *Applier) managedStackID(ctx context.Context, envID string, row dockerx.ServiceRow) (string, error) {
	return a.managedStackIDOf(ctx, envID, row.Name, row.Labels)
}

// managedStackIDOf is managedStackID for any workload: name is only used to say which one a failed look-up was for.
func (a *Applier) managedStackIDOf(ctx context.Context, envID, name string, labels map[string]string) (string, error) {
	id := labels[stacks.LabelStackID]
	if id == "" {
		return "", nil
	}
	managed := true
	if a.ManagedStack != nil {
		var err error
		if managed, err = a.ManagedStack(ctx, envID, id); err != nil {
			return "", fmt.Errorf("look up the stack of %s: %w", name, err)
		}
	}
	if !managed {
		return "", nil
	}
	return id, nil
}

// StackPreview is what the confirm of an update of a stack shows.
type StackPreview struct {
	Items    []PreviewItem
	Warnings []Warning
	LeftOut  []StackLeftOut
	Lock     *stackops.Holder
	Offered  bool
	Refusal  string
}

// PreviewStack describes the update of the services of a stack from the stored checks, asking no registry: the digests that
// would change, the warnings of running it and the services it leaves out. When nothing would change, Refusal says so. services
// limits it as StackRequest.Services does.
func (a *Applier) PreviewStack(ctx context.Context, envID, stack string, services []string) (StackPreview, error) {
	if a.Service == nil || a.Locks == nil {
		return StackPreview{}, errors.New("updates: the applier is not wired")
	}
	var p StackPreview
	if h, held := a.Locks.Held(envID); held {
		p.Lock = &h
	}
	// The preview of one service describes the update a person's "Update anyway" would make of a version that failed before.
	plan, err := a.PlanStack(ctx, StackRequest{EnvID: envID, Stack: stack, Services: services, Force: len(services) == 1})
	switch {
	case errors.Is(err, ErrNothingToUpdate):
		p.LeftOut, p.Refusal = plan.LeftOut, RefusalNothingToUpdate
		return p, nil
	case err != nil:
		var nu *NotUpdatableError
		var cf *CheckFailedError
		switch {
		case errors.As(err, &nu):
			p.Refusal = nu.Code
			return p, nil
		case errors.As(err, &cf):
			p.Refusal = RefusalCheckFailed
			return p, nil
		}
		return StackPreview{}, err
	}
	p.Offered, p.LeftOut = true, plan.LeftOut
	for _, t := range plan.Targets {
		p.Warnings = append(p.Warnings, failureWarnings(t.Key, t.Failure)...)
		p.Items = append(p.Items, PreviewItem{
			Kind: "service", Name: t.Key, Stack: stack, Image: t.Image, FromDigest: t.From, ToDigest: t.To,
			Platforms: append([]string{}, t.RunsOn...), ChangedPlatforms: append([]string{}, t.ChangedPlatforms...),
		})
		if !t.Swarm {
			p.Warnings = append(p.Warnings, Warning{Workload: t.Key, Code: WarnContainerDowntime})
			continue
		}
		row, found, rerr := a.serviceRow(ctx, envID, t.Key)
		if rerr != nil || !found {
			continue
		}
		p.Warnings = append(p.Warnings, a.serviceWarnings(ctx, envID, row)...)
	}
	return p, nil
}
