// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/convert"
	"github.com/ianglennon/coaming/internal/stacks/rebuild"
)

// Adoption of a stack Coaming did not make (plan 08-13, STK-13, D-14). Pressing Adopt changes nothing on the
// engine: the commit reads what runs, rebuilds the compose definition itself and stores it as the stack's draft
// and as its first revision. The browser never supplies the definition; it only says which state of the engine it
// looked at, by the fingerprint, so a stack that changed after it was shown is refused (T-08-46).

// ErrAdoptStale means what runs on the environment is not what the page showed: the commit stores nothing.
var ErrAdoptStale = errors.New("stackops: what runs changed since the stack was checked")

// ErrUnmanagedGone means no stack of that name runs on the environment any more.
var ErrUnmanagedGone = errors.New("stackops: no stack of that name runs here that Coaming does not manage")

// ErrNotAdoptable is the cause of a *NotAdoptableError.
var ErrNotAdoptable = errors.New("stackops: this stack cannot be adopted")

// The sources an adoption starts from (plan 08-15, D-12). Rebuild is the default and is always available: the
// definition Coaming writes from what runs. Paste is the person's own compose file, checked against what runs and
// kept as written.
const (
	AdoptSourceRebuild = "rebuild"
	AdoptSourcePaste   = "paste"
)

// PasteError is the refusal of a pasted compose file that does not pass the check, at the commit. Reason is the
// first problem the check found, masked as every check answer is.
type PasteError struct{ Reason string }

func (e *PasteError) Error() string {
	return "stackops: the pasted compose file does not pass the check: " + e.Reason
}

// NotAdoptableError is a refusal to adopt a stack that is Coaming's own or its agent's, or whose name its target
// cannot hold (D-48, D-49, D-53). Found is the stack as discovery read it, whose Owner and NameReason the caller
// words.
type NotAdoptableError struct{ Found Discovered }

func (e *NotAdoptableError) Error() string {
	return "stackops: " + e.Found.Name + " cannot be adopted"
}

// Unwrap makes errors.Is(err, ErrNotAdoptable) true.
func (e *NotAdoptableError) Unwrap() error { return ErrNotAdoptable }

// fingerprintOf is the digest of the entries that say what the engine held: sorted, so the order a read came in
// does not matter, and hashed, so it is short enough to send to a browser and back.
func fingerprintOf(entries []string) string {
	slices.Sort(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(sum[:])
}

// swarmFingerprint is the fingerprint of the services of a namespace: each one's ID and version, which the
// engine raises on every update (T-08-46).
func swarmFingerprint(services []dockerx.ServiceDetail) string {
	entries := make([]string, 0, len(services))
	for _, s := range services {
		entries = append(entries, "service:"+s.ID+":"+strconv.FormatUint(s.Version, 10))
	}
	return fingerprintOf(entries)
}

// standaloneFingerprint is the fingerprint of the containers of a project: each one's ID, config hash and image
// ID, so a container that was recreated, or whose image was replaced, reads as a change.
func standaloneFingerprint(containers []dockerx.ComposeContainer) string {
	entries := make([]string, 0, len(containers))
	for _, c := range containers {
		entries = append(entries, "container:"+c.ID+":"+c.ConfigHash+":"+c.ImageID)
	}
	return fingerprintOf(entries)
}

// AdoptFingerprint reads, now, what the engine holds for the stack of that name and answers its fingerprint: the
// state a page shows and the commit must find again. It is a read. A swarm is read through a manager and a
// standalone host from its containers, the same reads RebuildLive makes its file from.
func (d Deps) AdoptFingerprint(ctx context.Context, envID string, target stacks.Target, name string) (string, error) {
	switch target {
	case stacks.TargetSwarm:
		if d.Clusters == nil {
			return "", errors.New("stackops: no cluster reader to read the swarm with")
		}
		var services []dockerx.ServiceDetail
		if err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
			var e error
			services, e = c.ServicesWithLabel(ctx, stacks.LabelSwarmNamespace, name)
			return e
		}); err != nil {
			return "", err
		}
		return swarmFingerprint(services), nil
	case stacks.TargetStandalone:
		node, err := d.StandaloneNode(ctx, envID)
		if err != nil {
			return "", err
		}
		var containers []dockerx.ComposeContainer
		if err := d.Aggregator.OnNode(ctx, envID, node, func(c *dockerx.Client) error {
			var e error
			containers, e = c.ComposeContainers(ctx, name)
			return e
		}); err != nil {
			return "", err
		}
		return standaloneFingerprint(containers), nil
	}
	return "", errors.New("stackops: unknown stack target " + string(target))
}

// AdoptInput is what a commit of an adoption needs. Fingerprint is the one the page read (AdoptFingerprint),
// Managed the stacks Coaming holds on the environment, and Self says which objects are Coaming's own.
//
// Source is AdoptSourceRebuild (or empty) or AdoptSourcePaste. With paste, Pasted is the compose file and variables the
// person typed or pasted: untrusted text, loaded only through stacks.Check (T-08-56), stored as written when it
// passes. The browser sends text, never a verdict: the commit checks the pasted file again (T-08-57).
type AdoptInput struct {
	EnvID, Name string
	Target      stacks.Target
	Fingerprint string
	Caller      Caller
	Managed     []Stack
	Self        SelfMarks
	Source      string
	Pasted      *stacks.WorkingCopy
}

// isPaste reports whether the adoption starts from a pasted file, and refuses a source nobody defined.
func (in AdoptInput) isPaste() (bool, error) {
	switch in.Source {
	case "", AdoptSourceRebuild:
		return false, nil
	case AdoptSourcePaste:
		return true, nil
	}
	return false, errors.New("stackops: unknown adopt source " + strconv.Quote(in.Source))
}

// pastedCopy is the working copy a pasted adoption starts from: never nil files, so an empty paste is checked as the
// empty file it is.
func (in AdoptInput) pastedCopy() stacks.WorkingCopy {
	if in.Pasted == nil {
		return stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte{}}}}
	}
	return *in.Pasted
}

// checkPasted runs the pasted copy through the one check pipeline every draft goes through (no include, no extends,
// no file of the server read, at most 1 MiB). A copy that does not load, or has an error, is blocked: nothing is
// reviewed or stored for it.
func checkPasted(ctx context.Context, in AdoptInput) (stacks.CheckResult, stacks.WorkingCopy, bool) {
	copy := in.pastedCopy()
	res := stacks.Check(ctx, stacks.CheckInput{Name: in.Name, Target: in.Target, Copy: copy})
	return res, copy, res.Loaded == nil || res.Problems.Errors() > 0
}

// withPasted is the prepared adoption with the pasted copy as the definition to adopt. The pins are the running
// digests of the services that run the image the file names; any other service is pinned to its image as written
// (a service that does not run, or runs another image than the file says, so its difference shows in the review).
// A service keeps the platforms it runs with only where it keeps the running pin (D-12).
func (p adoptPrepared) withPasted(copy stacks.WorkingCopy, loaded *stacks.Loaded, target stacks.Target) adoptPrepared {
	pins := make(map[string]string, len(loaded.Project.Services))
	platforms := make(map[string][]swarm.Platform, len(loaded.Project.Services))
	for key, svc := range loaded.Project.Services {
		if svc.Image == "" {
			continue
		}
		engine := key
		if target == stacks.TargetSwarm {
			engine = p.stack.Name + "_" + key
		}
		if live, ok := p.res.Pins[engine]; ok {
			text := live
			if i := strings.LastIndex(live, "@"); i > 0 {
				text = live[:i]
			}
			if stacks.SameImageText(text, svc.Image) {
				pins[engine] = live
				if pl := p.res.Platforms[engine]; len(pl) > 0 {
					platforms[engine] = pl
				}
				continue
			}
		}
		pins[engine] = svc.Image
	}
	p.copy = copy
	p.res.Pins = pins
	p.res.Platforms = platforms
	p.res.Notes = nil
	return p
}

// AdoptCommit adopts the unmanaged stack of that name: it reads what runs and rebuilds the definition from it
// itself, reveal included, because the stored draft holds the real environment values (D-15), and refuses with
// ErrAdoptStale when what it read is not what the caller's fingerprint names. It stores the stack, its draft and
// revision 1 in one transaction (Repo.Adopt) and sends nothing to the engine (D-14). It never judges the policy:
// adoption only records what already runs, and a later deploy is judged as any deploy (D-17).
//
// A name one of the managed stacks holds is ErrNameTaken, and so is the loser of two adoptions that race. A
// stack that is Coaming's own, its agent's, or whose name the target cannot hold is a *NotAdoptableError. A
// definition that cannot be written, or does not pass the check, is the *RebuildError of RebuildLive.
func (d Deps) AdoptCommit(ctx context.Context, repo Repo, in AdoptInput) (Stack, error) {
	a, err := d.AdoptWithOutcome(ctx, repo, in)
	return a.Stack, err
}

// Adopted is the outcome of an adoption: the stack, the number of its first revision and how many services it
// has, which the audit row records.
type Adopted struct {
	Stack    Stack
	Revision int64
	Services int
}

// adoptPrepared is a stack read, and rebuilt from what runs it, ready to be reviewed and adopted.
type adoptPrepared struct {
	found Discovered
	res   rebuild.Result
	copy  stacks.WorkingCopy
	// stack is the stack as it would be adopted: pending its first deploy, under the placeholder ID of the check.
	stack Stack
}

// prepareAdoption reads what runs and rebuilds the definition from it, reveal included because equality is decided on
// the real values (D-15, D-47). It refuses a name Coaming already holds (ErrNameTaken), a name nothing carries
// (ErrUnmanagedGone) and a stack that is not adoptable (*NotAdoptableError), and sends nothing to the engine.
func (d Deps) prepareAdoption(ctx context.Context, in AdoptInput) (adoptPrepared, error) {
	if _, taken := HoldingName(in.Managed, in.Target, in.Name); taken {
		return adoptPrepared{}, ErrNameTaken
	}
	found, ok, err := d.DiscoverNamed(ctx, in.EnvID, in.Target, in.Name, in.Managed, in.Self)
	if err != nil {
		return adoptPrepared{}, err
	}
	if !ok {
		return adoptPrepared{}, ErrUnmanagedGone
	}
	if !found.Adoptable() {
		return adoptPrepared{}, &NotAdoptableError{Found: found}
	}
	res, err := d.RebuildLive(ctx, in.EnvID, DiscoveredStack(in.EnvID, in.Name, in.Target), RebuildOptions{Reveal: true})
	if err != nil {
		return adoptPrepared{}, err
	}
	return adoptPrepared{
		found: found, res: res,
		copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: res.YAML}}},
		stack: Stack{
			ID: AdoptCheckID, EnvID: in.EnvID, Name: in.Name, Target: in.Target,
			Source: stacks.Source{Kind: stacks.SourceAdopted, Ref: in.Name}, Adoption: adoptionRecordOf(found, res),
		},
	}, nil
}

// review is the adopt review of the prepared stack against what runs, and the plan it converted.
func (p adoptPrepared) review(ctx context.Context, d Deps) (Review, convert.Plan, error) {
	rv := Review{Pins: map[string]string{}, NeedsAdmin: []string{}}
	plan, err := d.reviewAdoptPlan(ctx, ReviewInput{
		EnvID: p.stack.EnvID, Stack: p.stack, Copy: p.copy, Mode: ReviewModeAdopt, Rebuilt: &p.res,
	}, &rv)
	return rv, plan, err
}

// AdoptWithOutcome is AdoptCommit with the outcome the caller records.
func (d Deps) AdoptWithOutcome(ctx context.Context, repo Repo, in AdoptInput) (Adopted, error) {
	prep, err := d.prepareAdoption(ctx, in)
	if err != nil {
		return Adopted{}, err
	}
	if prep.res.Fingerprint != in.Fingerprint {
		return Adopted{}, ErrAdoptStale
	}
	paste, err := in.isPaste()
	if err != nil {
		return Adopted{}, err
	}
	if paste {
		// The pasted text is checked again here, whatever the page was told (T-08-57).
		res, copy, blocked := checkPasted(ctx, in)
		if blocked {
			return Adopted{}, &PasteError{Reason: firstErrorMessage(res.Problems)}
		}
		if err := stacks.CheckVars(copy.Vars); err != nil {
			return Adopted{}, err
		}
		prep = prep.withPasted(copy, res.Loaded, in.Target)
	}

	copy := prep.copy
	loaded, err := loadRebuilt(ctx, in.Name, in.Target, copy)
	if err != nil {
		return Adopted{}, err
	}
	rendered, err := stacks.Render(stacks.RenderInput{
		Loaded: loaded, Pins: LivePins(prep.res.Pins), Masker: stacks.NewMasker(copy.Vars, copy.Files),
	})
	if err != nil {
		return Adopted{}, &RebuildError{Reason: err.Error()}
	}
	// The same review the page showed is made again, so the paths of the gaps are stored with the stack and drift can
	// say a difference was there when it was adopted (D-55). The paths only: a value is never stored here.
	rv, _, err := prep.review(ctx, d)
	if err != nil {
		return Adopted{}, err
	}
	adoption := prep.stack.Adoption
	adoption.Gaps = gapPaths(rv.Services)

	st, rev, err := repo.Adopt(ctx, AdoptRecord{
		EnvID: in.EnvID, Name: in.Name, Target: in.Target, SourceRef: in.Name, Actor: in.Caller.Actor,
		Copy: copy, Pins: prep.res.Pins, Platforms: prep.res.Platforms, Rendered: rendered, Adoption: adoption,
	})
	if err != nil {
		return Adopted{}, err
	}
	return Adopted{Stack: st, Revision: rev.Seq, Services: len(loaded.Project.Services)}, nil
}

// gapPaths is the places the rebuilt definition differs from what runs, by service: for a changed service the paths
// of its settings that differ (a setting that differs in a value the masker hides, by its path as well), for a service
// that is added, removed or recreated an empty list. An ownership-only service is no gap. No value is in it.
func gapPaths(rows []stacks.ServiceChange) map[string][]string {
	out := map[string][]string{}
	for _, c := range rows {
		if c.OwnershipOnly {
			continue
		}
		paths := []string{}
		if c.Change == stacks.ChangeChanged {
			for _, p := range stacks.ChangedPaths(c.Current, c.Next) {
				paths = append(paths, p.Path)
			}
			paths = append(paths, c.HiddenChanges...)
			slices.Sort(paths)
			paths = slices.Compact(paths)
		}
		out[c.Name] = paths
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AdoptCheckResult is what adopting a stack would be like, read before anything is stored (plan 08-14, D-16, D-20).
// Gaps are the services whose first deploy would change something that is not an ownership label, with what changes;
// OwnershipOnly the services that differ in nothing else, by their compose names. Clean says there is no gap. Objects
// are the networks, volumes, secrets and configs of the stack with how each is treated (D-13, D-18), Notes what the
// rebuild could not say plainly, and Policy the host-access settings of the definition, listed for whoever asks and
// blocking nothing (D-17). Portainer and Orphan say the stack carries Portainer's marks or the ID of a stack Coaming
// made once (D-19, D-11).
//
// Source says which definition was checked. For a pasted file Problems and Variables are what the check found in it
// (masked, as every check answer is), Blocked says it has an error so nothing was compared, and Matches says it differs
// from what runs in nothing but the ownership labels.
type AdoptCheckResult struct {
	Source        string
	Problems      stacks.Problems
	Variables     []stacks.ResolvedVar
	Matches       bool
	Blocked       bool
	Fingerprint   string
	Gaps          []stacks.ServiceChange
	OwnershipOnly []string
	Clean         bool
	Objects       []rebuild.Object
	Notes         []rebuild.Note
	Policy        PolicyResult
	Portainer     bool
	Orphan        bool
}

// AdoptCheck reads the unmanaged stack of in.Name, rebuilds its definition and reviews it against what runs in adopt
// mode. It writes nothing, to an engine or to the store, and a gap never refuses (D-20). The fingerprint it answers is
// the one the adoption must be sent with: it names the state of the engine the check was made of. The rebuild is made
// with the real values because equality is decided on them; for a caller without secrets.reveal (reveal false) every
// environment value, command, argument, health check command and log option in the gaps is then withheld (D-47).
//
// The refusals are the commit's: ErrNameTaken, ErrUnmanagedGone, *NotAdoptableError and the rebuild's own errors.
func (d Deps) AdoptCheck(ctx context.Context, in AdoptInput, reveal bool) (AdoptCheckResult, error) {
	paste, err := in.isPaste()
	if err != nil {
		return AdoptCheckResult{}, err
	}
	prep, err := d.prepareAdoption(ctx, in)
	if err != nil {
		return AdoptCheckResult{}, err
	}
	out := AdoptCheckResult{
		Fingerprint: prep.res.Fingerprint, Source: AdoptSourceRebuild, Objects: prep.res.Objects, Notes: prep.res.Notes,
		Portainer: prep.found.Portainer, Orphan: prep.found.OrphanID != "",
	}
	if paste {
		res, copy, blocked := checkPasted(ctx, in)
		out.Source, out.Problems, out.Variables, out.Notes = AdoptSourcePaste, res.Problems, res.Variables, nil
		if blocked {
			// Nothing of a file that does not pass the check is compared with what runs.
			out.Blocked = true
			return out, nil
		}
		prep = prep.withPasted(copy, res.Loaded, in.Target)
	}
	rv, plan, err := prep.review(ctx, d)
	if err != nil {
		return AdoptCheckResult{}, err
	}
	pol, err := d.HostAccess(prep.copy, plan)
	if err != nil {
		return AdoptCheckResult{}, err
	}
	out.OwnershipOnly, out.Policy = adoptNames(rv.Services), pol
	for _, c := range rv.Services {
		if !c.OwnershipOnly {
			out.Gaps = append(out.Gaps, c)
		}
	}
	out.Clean = len(out.Gaps) == 0
	out.Matches = paste && out.Clean
	if !reveal {
		out.Gaps = RedactChanges(out.Gaps, in.Target)
	}
	return out, nil
}

// adoptionRecordOf is what the adoption writes down about the objects of the stack: the ID some object of it
// carried (a stack Coaming made once, D-11), the networks and volumes it now owns, and the secrets and configs it
// references as external. Pending is true until the first deploy.
func adoptionRecordOf(found Discovered, res rebuild.Result) stacks.Adoption {
	a := stacks.Adoption{PriorID: found.OrphanID, Pending: true}
	for _, o := range res.Objects {
		switch {
		case o.Treatment == rebuild.TreatmentOwned && o.Kind == rebuild.KindNetwork:
			a.Networks = append(a.Networks, o.Name)
		case o.Treatment == rebuild.TreatmentOwned && o.Kind == rebuild.KindVolume:
			a.Volumes = append(a.Volumes, o.Name)
		case o.Treatment == rebuild.TreatmentExternal && o.Kind == rebuild.KindSecret:
			a.Secrets = append(a.Secrets, o.Name)
		case o.Treatment == rebuild.TreatmentExternal && o.Kind == rebuild.KindConfig:
			a.Configs = append(a.Configs, o.Name)
		}
	}
	for _, names := range []*[]string{&a.Networks, &a.Volumes, &a.Secrets, &a.Configs} {
		slices.Sort(*names)
	}
	return a
}

// firstErrorMessage is the message of the first error among the problems, or a fixed sentence when there is none.
func firstErrorMessage(ps stacks.Problems) string {
	for _, p := range ps {
		if p.Severity == stacks.SeverityError {
			return p.Message
		}
	}
	return "The compose file could not be loaded."
}
