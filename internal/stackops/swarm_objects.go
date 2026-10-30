// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import (
	"context"
	"encoding/json"
	"errors"
	"maps"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/jobs"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/convert"
	"github.com/ianglennon/coaming/internal/stacks/rebuild"
)

// stepError is a failure of one step of a swarm deploy: the deploy code to end the item with, the
// name of the network, secret, config or service it names, and the cause, which is logged and
// never put in a job (the engine's words can echo a value).
type stepError struct {
	code, name string
	err        error
}

func (e *stepError) Error() string { return e.code + " " + e.name + ": " + e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// stepOf reads the deploy code and the name out of an error from a step, or classifies a plain
// engine error with codeOf.
func stepOf(ctx context.Context, err error) (code, name string) {
	var se *stepError
	if errors.As(err, &se) {
		return se.code, se.name
	}
	return codeOf(ctx, err), ""
}

// ensureNetworks makes the networks of the stack exist on the swarm, once each. plans are the
// planned networks; live holds the networks already on the swarm: the stack's own, and every
// network when the stack names an external one. An external network is only looked up by name and
// must be a swarm network (external_missing when it is absent, external_not_swarm when only an
// engine-local one has the name); it is never created, changed or removed (T-07-31). A network the
// stack owns is left alone even if its options differ from the file's: the review (plan 07-14)
// reports the difference, and a network cannot be changed in place. A name another owner holds is
// network_conflict. Every other network is created with all its pools, its IPAM driver, its driver
// options, its flags and the stack's labels, through WriteOnManager, once. The first failure ends
// the call with a *stepError; the networks created before it stay in created.
func (d Deps) ensureNetworks(ctx context.Context, envID string, stack Stack, plans []convert.NetworkPlan, live []dockerx.NetworkRow, prog *progress) (created []string, err error) {
	own := OwnershipOf(stack)
	for _, n := range plans {
		if n.External {
			found, swarmScope := externalNetwork(live, n.Name)
			switch {
			case !found:
				return created, &stepError{code: CodeExternalMissing, name: n.Name, err: errors.New("the external network does not exist")}
			case !swarmScope:
				return created, &stepError{code: CodeExternalNotSwarm, name: n.Name, err: errors.New("the external network is not a swarm network")}
			}
			continue
		}
		if row, ok := findNetwork(live, n.Name); ok {
			if !own.OwnsNetwork(row.Name, row.Labels) && !own.Knows(rebuild.KindNetwork, row.Name) {
				return created, &stepError{code: CodeNetworkConflict, name: n.Name, err: errors.New("the network belongs to another owner")}
			}
			continue
		}
		if ctx.Err() != nil {
			return created, &stepError{code: CodeCancelled, name: n.Name, err: ctx.Err()}
		}
		spec := networkSpec(n)
		prog.start(idNetwork + n.Name)
		werr := d.write(ctx, envID, func(wctx context.Context, c *dockerx.Client) error {
			_, _, e := c.NetworkCreate(wctx, spec)
			return e
		})
		if werr != nil {
			d.log().Warn("could not create a stack network", "environment", envID, "stack", stack.Name, "network", n.Name, "error", werr)
			code := codeOf(ctx, werr)
			if code == CodeEngineError {
				code = CodeNetworkFailed
				if isConflict(werr) {
					code = CodeNetworkConflict
				}
			}
			return created, &stepError{code: code, name: n.Name, err: werr}
		}
		created = append(created, n.Name)
		prog.line("Created network " + n.Name)
		prog.done(idNetwork+n.Name, nil)
	}
	return created, nil
}

// externalNetwork reports whether a network of that name exists and whether one of them has swarm
// scope.
func externalNetwork(live []dockerx.NetworkRow, name string) (found, swarmScope bool) {
	for _, n := range live {
		if n.Name == name {
			found = true
			swarmScope = swarmScope || n.Scope == dockerx.ScopeSwarm
		}
	}
	return found, swarmScope
}

func findNetwork(live []dockerx.NetworkRow, name string) (dockerx.NetworkRow, bool) {
	for _, n := range live {
		if n.Name == name {
			return n, true
		}
	}
	return dockerx.NetworkRow{}, false
}

// networkSpec turns a planned network into the spec dockerx creates it from: all its pools, its
// IPAM driver, driver options, flags and labels.
func networkSpec(n convert.NetworkPlan) dockerx.NetworkCreateSpec {
	spec := dockerx.NetworkCreateSpec{
		Name: n.Name, Driver: n.Driver, Scope: dockerx.NetworkScope(n.Driver),
		Attachable: n.Attachable, Internal: n.Internal, Labels: n.Labels, Options: n.DriverOpts,
		IPAMDriver: n.IPAMDriver,
	}
	for _, p := range n.Pools {
		spec.Pools = append(spec.Pools, dockerx.NetworkPool{Subnet: p.Subnet, Gateway: p.Gateway, IPRange: p.IPRange})
	}
	return spec
}

// pruneServices removes the services the swarm has under this stack that the file no longer
// declares, once each. live are the services read before the deploy and declared the full names of
// the services the file declares. Only a service that carries this stack's namespace and this
// stack's ID is removed, whatever live holds, so a service of another stack, or of nobody's, is
// never touched. The caller runs it only after every declared service was created or updated. The
// first failure ends the call with a *stepError; removed names the services removed before it.
func (d Deps) pruneServices(ctx context.Context, envID string, stack Stack, declared map[string]bool, live []dockerx.ServiceDetail, prog *progress) (removed []string, err error) {
	own := OwnershipOf(stack)
	for _, s := range live {
		if declared[s.Name] || !own.OwnsWorkload(s.Labels) {
			continue
		}
		if ctx.Err() != nil {
			return removed, &stepError{code: CodeCancelled, name: s.Name, err: ctx.Err()}
		}
		werr := d.write(ctx, envID, func(wctx context.Context, c *dockerx.Client) error { return c.ServiceRemove(wctx, s.ID) })
		switch {
		case werr == nil:
			removed = append(removed, s.Name)
			prog.line("Removed service " + s.Name)
		case errors.Is(werr, dockerx.ErrServiceNotFound):
			// Already gone: nothing was left behind.
		default:
			d.log().Warn("could not remove a service the stack no longer declares", "environment", envID, "stack", stack.Name, "service", s.Name, "error", werr)
			return removed, &stepError{code: codeOf(ctx, werr), name: s.Name, err: werr}
		}
	}
	return removed, nil
}

// objectRow is a swarm secret or config as the deploy sees it. The manager never returns a
// secret's value and the config's content is dropped at the dockerx edge, so a row holds neither.
type objectRow struct {
	Kind     stacks.ObjectKind
	ID, Name string
	Labels   map[string]string
}

func secretRows(rows []dockerx.SecretRow) []objectRow {
	out := make([]objectRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, objectRow{Kind: stacks.KindSecret, ID: r.ID, Name: r.Name, Labels: r.Labels})
	}
	return out
}

func configRows(rows []dockerx.ConfigRow) []objectRow {
	out := make([]objectRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, objectRow{Kind: stacks.KindConfig, ID: r.ID, Name: r.Name, Labels: r.Labels})
	}
	return out
}

func findObject(rows []objectRow, kind stacks.ObjectKind, name string) (objectRow, bool) {
	for _, r := range rows {
		if r.Kind == kind && r.Name == name {
			return r, true
		}
	}
	return objectRow{}, false
}

// objectRef is the swarm object a compose secret or config became: its ID and its name.
type objectRef struct{ ID, Name string }

// objectRefs maps the name the compose file uses for a secret or config to the swarm object a
// deploy made or found for it. It is the convert.ObjectResolver of the deploy.
type objectRefs struct {
	secrets, configs map[string]objectRef
}

func newObjectRefs() objectRefs {
	return objectRefs{secrets: map[string]objectRef{}, configs: map[string]objectRef{}}
}

var _ interface {
	Secret(name string) (id, objectName string, ok bool)
	Config(name string) (id, objectName string, ok bool)
} = objectRefs{}

// Secret resolves a compose secret.
func (o objectRefs) Secret(name string) (id, objectName string, ok bool) {
	r, ok := o.secrets[name]
	return r.ID, r.Name, ok
}

// Config resolves a compose config.
func (o objectRefs) Config(name string) (id, objectName string, ok bool) {
	r, ok := o.configs[name]
	return r.ID, r.Name, ok
}

func (o objectRefs) set(kind stacks.ObjectKind, name string, ref objectRef) {
	if kind == stacks.KindSecret {
		o.secrets[name] = ref
		return
	}
	o.configs[name] = ref
}

// keeps reports whether the deploy still uses the object: its ID is one the refs hold.
func (o objectRefs) keeps(kind stacks.ObjectKind, id string) bool {
	m := o.configs
	if kind == stacks.KindSecret {
		m = o.secrets
	}
	for _, r := range m {
		if r.ID == id {
			return true
		}
	}
	return false
}

// PlannedObjects is the resolver for a conversion made before anything is created, to check a
// stack or to show what a deploy would do: each secret and config of objs has the name a deploy
// would give it (its hashed name, or the external name) and no ID. A plan converted with it has the
// right shape; the deploy converts again with the IDs of the objects it creates.
func PlannedObjects(stack string, objs []stacks.ObjectContent) convert.ObjectResolver {
	return plannedRefs(stack, objs)
}

// plannedRefs are the refs a conversion gets before anything is created: the names the objects
// will have, and no IDs. A plan converted with them has the right shape for the networks and the
// service list; the services are converted again with the real IDs.
func plannedRefs(stack string, objs []stacks.ObjectContent) objectRefs {
	refs := newObjectRefs()
	for _, o := range objs {
		name := o.ExternalName
		if !o.External {
			name, _ = stacks.ObjectName(stack, o.Name, o.Content)
		}
		refs.set(o.Kind, o.Name, objectRef{Name: name})
	}
	return refs
}

// hasExternalObject reports whether any object of the kind is external.
func hasExternalObject(objs []stacks.ObjectContent, kind stacks.ObjectKind) bool {
	for _, o := range objs {
		if o.External && o.Kind == kind {
			return true
		}
	}
	return false
}

// ensureObjects makes the secrets and configs of the stack exist on the swarm, once each: an
// object whose hashed name the stack already owns is reused, one whose content is new is created
// under <stack>_<name>_<hash> with the stack's labels, and an external object is only looked up by
// its name and never created (T-07-31). live holds the rows already on the swarm: the stack's own,
// and every row of a kind that has an external object. refs resolves each compose name to the
// object it became, for the conversion of the services; created names what this call made.
//
// The first failure ends the call with a *stepError: external_missing for an external object that
// is not there, object_name_taken for a name another owner holds, object_failed or the engine's
// own class for a create that failed. Objects created before it stay in created, so the caller can
// count them. Every create goes through WriteOnManager, once, and the content goes only into that
// create (T-07-33).
func (d Deps) ensureObjects(ctx context.Context, envID string, stack Stack, objs []stacks.ObjectContent, live []objectRow, prog *progress) (refs objectRefs, created []string, err error) {
	refs = newObjectRefs()
	own := OwnershipOf(stack)
	for _, o := range objs {
		if o.External {
			row, ok := findObject(live, o.Kind, o.ExternalName)
			if !ok {
				return refs, created, &stepError{code: CodeExternalMissing, name: o.ExternalName, err: errors.New("the external " + string(o.Kind) + " does not exist")}
			}
			refs.set(o.Kind, o.Name, objectRef{ID: row.ID, Name: row.Name})
			continue
		}
		full, hash := stacks.ObjectName(stack.Name, o.Name, o.Content)
		if row, ok := findObject(live, o.Kind, full); ok {
			if !own.OwnsObject(row.Labels) {
				return refs, created, &stepError{code: CodeObjectNameTaken, name: full, err: dockerx.ErrNameTaken}
			}
			refs.set(o.Kind, o.Name, objectRef{ID: row.ID, Name: full})
			continue
		}
		if ctx.Err() != nil {
			return refs, created, &stepError{code: CodeCancelled, name: full, err: ctx.Err()}
		}
		labels := maps.Clone(o.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		labels[stacks.LabelSwarmNamespace] = stack.Name
		labels[stacks.LabelStackID] = stack.ID
		labels[stacks.LabelObject] = o.Name
		labels[stacks.LabelObjectHash] = hash

		itemID := idConfig + full
		if o.Kind == stacks.KindSecret {
			itemID = idSecret + full
		}
		prog.start(itemID)
		var id string
		werr := d.write(ctx, envID, func(wctx context.Context, c *dockerx.Client) error {
			var e error
			if o.Kind == stacks.KindSecret {
				id, e = c.SecretCreate(wctx, dockerx.SecretSpec{Name: full, Labels: labels, Data: o.Content})
			} else {
				id, e = c.ConfigCreate(wctx, dockerx.ConfigSpec{Name: full, Labels: labels, Data: o.Content})
			}
			return e
		})
		if werr != nil {
			d.log().Warn("could not create a stack object", "environment", envID, "stack", stack.Name, "kind", o.Kind, "object", full, "error", werr)
			code := codeOf(ctx, werr)
			switch {
			case errors.Is(werr, dockerx.ErrNameTaken):
				code = CodeObjectNameTaken
			case code == CodeEngineError:
				code = CodeObjectFailed
			}
			return refs, created, &stepError{code: code, name: full, err: werr}
		}
		created = append(created, full)
		prog.line("Created " + string(o.Kind) + " " + full)
		prog.done(itemID, nil)
		refs.set(o.Kind, o.Name, objectRef{ID: id, Name: full})
	}
	return refs, created, nil
}

// removeReplacedObjects removes the secrets and configs of the stack that no deploy uses any more:
// those that carry this stack's ID and the coaming.stack.object label (so a made-from-a-file
// object, never one the user added by hand and never another stack's) and are not in keep. It
// reads the stack's services first and removes an object only when no service spec still
// references it (T-07-32); the manager refuses one a service of another owner references, and
// that is left in place too. removed names what was removed and leftInPlace what could not be:
// referenced, refused or failed. A removal that fails is not an error of the deploy, because the
// next deploy finds the same objects by their labels; err is only a read that failed, when
// nothing could be judged.
func (d Deps) removeReplacedObjects(ctx context.Context, envID string, stack Stack, keep objectRefs, prog *progress) (removed, leftInPlace []string, err error) {
	own := OwnershipOf(stack)
	var svcs []dockerx.ServiceDetail
	var secrets, configs []objectRow
	err = d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		secrets, configs = nil, nil // a read that is tried on another manager starts again
		if svcs, e = c.ServicesWithLabel(ctx, stacks.LabelSwarmNamespace, stack.Name); e != nil {
			return e
		}
		srows, e := c.SecretsWithLabel(ctx, own.labelKey(), own.Name)
		if e != nil {
			return e
		}
		crows, e := c.ConfigsWithLabel(ctx, own.labelKey(), own.Name)
		if e != nil {
			return e
		}
		for _, r := range secretRows(srows) {
			if own.OwnsObject(r.Labels) {
				secrets = append(secrets, r)
			}
		}
		for _, r := range configRows(crows) {
			if own.OwnsObject(r.Labels) {
				configs = append(configs, r)
			}
		}
		return nil
	})
	if err != nil {
		d.log().Warn("could not read a stack's objects after a deploy", "environment", envID, "stack", stack.Name, "error", err)
		return nil, nil, err
	}
	used := referencedObjects(svcs)

	var candidates []objectRow
	for _, r := range append(secrets, configs...) {
		if r.Labels[stacks.LabelObject] == "" || keep.keeps(r.Kind, r.ID) {
			continue
		}
		candidates = append(candidates, r)
	}
	for _, r := range candidates {
		label := r.Labels[stacks.LabelObject]
		prog.addRemoval(r.Kind, label, r.Name)
	}
	for i, r := range candidates {
		switch {
		case ctx.Err() != nil:
			for _, rest := range candidates[i:] {
				leftInPlace = append(leftInPlace, rest.Name)
				prog.failed(ItemCleanup, rest.Name, jobs.ItemCancelled, CodeCancelled, CodeMessage(CodeCancelled), "")
			}
			return removed, leftInPlace, nil
		case used.has(r):
			leftInPlace = append(leftInPlace, r.Name)
			prog.done(removalID(r), map[string]string{"left": "true"})
			continue
		}
		prog.start(removalID(r))
		rerr := d.write(ctx, envID, func(wctx context.Context, c *dockerx.Client) error {
			if r.Kind == stacks.KindSecret {
				return c.SecretRemove(wctx, r.ID)
			}
			return c.ConfigRemove(wctx, r.ID)
		})
		switch {
		case rerr == nil:
			removed = append(removed, r.Name)
			prog.line("Removed old " + string(r.Kind) + " " + r.Name)
			prog.done(removalID(r), nil)
		case errors.Is(rerr, dockerx.ErrObjectNotFound):
			// Already gone: nothing was left behind.
			prog.done(removalID(r), nil)
		default:
			if !errors.Is(rerr, dockerx.ErrObjectInUse) {
				d.log().Warn("could not remove a replaced stack object", "environment", envID, "stack", stack.Name, "kind", r.Kind, "object", r.Name, "error", rerr)
			}
			leftInPlace = append(leftInPlace, r.Name)
			prog.done(removalID(r), map[string]string{"left": "true"})
		}
	}
	return removed, leftInPlace, nil
}

// objectUse is the secrets and configs the specs of some services reference, by ID and by name.
type objectUse struct{ secrets, configs map[string]bool }

func (u objectUse) has(r objectRow) bool {
	m := u.configs
	if r.Kind == stacks.KindSecret {
		m = u.secrets
	}
	return m[r.ID] || m[r.Name]
}

// referencedObjects reads the secrets and configs the current specs of the services reference,
// including the config a credential_spec names. A spec that does not decode counts as referencing
// nothing it can be shown to, so the caller errs toward removing only what the manager allows:
// the manager itself refuses a remove of an object any spec still references.
func referencedObjects(svcs []dockerx.ServiceDetail) objectUse {
	u := objectUse{secrets: map[string]bool{}, configs: map[string]bool{}}
	for _, s := range svcs {
		var spec swarm.ServiceSpec
		if json.Unmarshal(s.Spec, &spec) != nil {
			continue
		}
		cs := spec.TaskTemplate.ContainerSpec
		if cs == nil {
			continue
		}
		for _, r := range cs.Secrets {
			if r != nil {
				u.secrets[r.SecretID], u.secrets[r.SecretName] = true, true
			}
		}
		for _, r := range cs.Configs {
			if r != nil {
				u.configs[r.ConfigID], u.configs[r.ConfigName] = true, true
			}
		}
		if p := cs.Privileges; p != nil && p.CredentialSpec != nil && p.CredentialSpec.Config != "" {
			u.configs[p.CredentialSpec.Config] = true
		}
	}
	return u
}

// removalID is the ID of the item that removes an old secret or config.
func removalID(r objectRow) string {
	if r.Kind == stacks.KindSecret {
		return idRemoveSecret + r.Name
	}
	return idRemoveConfig + r.Name
}
