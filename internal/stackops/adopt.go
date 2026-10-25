// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/rebuild"
)

// Adoption of a stack Bayplan did not make (plan 08-13, STK-13, D-14). Pressing Adopt changes nothing on the
// engine: the commit reads what runs, rebuilds the compose definition itself and stores it as the stack's draft
// and as its first revision. The browser never supplies the definition; it only says which state of the engine it
// looked at, by the fingerprint, so a stack that changed after it was shown is refused (T-08-46).

// ErrAdoptStale means what runs on the environment is not what the page showed: the commit stores nothing.
var ErrAdoptStale = errors.New("stackops: what runs changed since the stack was checked")

// ErrUnmanagedGone means no stack of that name runs on the environment any more.
var ErrUnmanagedGone = errors.New("stackops: no stack of that name runs here that Bayplan does not manage")

// ErrNotAdoptable is the cause of a *NotAdoptableError.
var ErrNotAdoptable = errors.New("stackops: this stack cannot be adopted")

// NotAdoptableError is a refusal to adopt a stack that is Bayplan's own or its agent's, or whose name its target
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
// Managed the stacks Bayplan holds on the environment, and Self says which objects are Bayplan's own.
type AdoptInput struct {
	EnvID, Name string
	Target      stacks.Target
	Fingerprint string
	Caller      Caller
	Managed     []Stack
	Self        SelfMarks
}

// AdoptCommit adopts the unmanaged stack of that name: it reads what runs and rebuilds the definition from it
// itself, reveal included, because the stored draft holds the real environment values (D-15), and refuses with
// ErrAdoptStale when what it read is not what the caller's fingerprint names. It stores the stack, its draft and
// revision 1 in one transaction (Repo.Adopt) and sends nothing to the engine (D-14). It never judges the policy:
// adoption only records what already runs, and a later deploy is judged as any deploy (D-17).
//
// A name one of the managed stacks holds is ErrNameTaken, and so is the loser of two adoptions that race. A
// stack that is Bayplan's own, its agent's, or whose name the target cannot hold is a *NotAdoptableError. A
// definition that cannot be written, or does not pass the check, is the *RebuildError of RebuildLive.
func (d Deps) AdoptCommit(ctx context.Context, repo Repo, in AdoptInput) (Stack, error) {
	a, err := d.AdoptCommitWith(ctx, repo, in)
	return a.Stack, err
}

// Adopted is the outcome of an adoption: the stack, the number of its first revision and how many services it
// has, which the audit row records.
type Adopted struct {
	Stack    Stack
	Revision int64
	Services int
}

// AdoptCommitWith is AdoptCommit with the outcome the caller records.
func (d Deps) AdoptCommitWith(ctx context.Context, repo Repo, in AdoptInput) (Adopted, error) {
	if slices.ContainsFunc(in.Managed, func(s Stack) bool { return s.Name == in.Name }) {
		return Adopted{}, ErrNameTaken
	}
	found, ok, err := d.DiscoverNamed(ctx, in.EnvID, in.Target, in.Name, in.Self)
	if err != nil {
		return Adopted{}, err
	}
	if !ok {
		return Adopted{}, ErrUnmanagedGone
	}
	if !found.Adoptable() {
		return Adopted{}, &NotAdoptableError{Found: found}
	}

	res, err := d.RebuildLive(ctx, in.EnvID, DiscoveredStack(in.EnvID, in.Name, in.Target), RebuildOptions{Reveal: true})
	if err != nil {
		return Adopted{}, err
	}
	if res.Fingerprint != in.Fingerprint {
		return Adopted{}, ErrAdoptStale
	}

	copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: res.YAML}}}
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: in.Name, Target: in.Target, Copy: copy})
	if err != nil {
		var refused *stacks.NotDeployableError
		if errors.As(err, &refused) && len(refused.Problems) > 0 {
			return Adopted{}, &RebuildError{Reason: refused.Problems[0].Message}
		}
		return Adopted{}, &RebuildError{Reason: err.Error()}
	}
	rendered, err := stacks.Render(stacks.RenderInput{
		Loaded: loaded, Pins: LivePins(res.Pins), Masker: stacks.NewMasker(copy.Vars, copy.Files),
	})
	if err != nil {
		return Adopted{}, &RebuildError{Reason: err.Error()}
	}

	st, rev, err := repo.Adopt(ctx, AdoptRecord{
		EnvID: in.EnvID, Name: in.Name, Target: in.Target, SourceRef: in.Name, Actor: in.Caller.Actor,
		Copy: copy, Pins: res.Pins, Rendered: rendered, Adoption: adoptionRecordOf(found, res),
	})
	if err != nil {
		return Adopted{}, err
	}
	return Adopted{Stack: st, Revision: rev.Seq, Services: len(loaded.Project.Services)}, nil
}

// adoptionRecordOf is what the adoption writes down about the objects of the stack: the ID some object of it
// carried (a stack Bayplan made once, D-11), the networks and volumes it now owns, and the secrets and configs it
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
