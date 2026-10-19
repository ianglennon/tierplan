// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"encoding/json"
	"errors"
	"maps"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
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
func (d Deps) ensureObjects(ctx context.Context, envID string, stack Stack, objs []stacks.ObjectContent, live []objectRow) (refs objectRefs, created []string, err error) {
	refs = newObjectRefs()
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
			if row.Labels[stacks.LabelStackID] != stack.ID {
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

		var id string
		werr := d.Aggregator.WriteOnManager(ctx, envID, func(c *dockerx.Client) error {
			var e error
			if o.Kind == stacks.KindSecret {
				id, e = c.SecretCreate(ctx, dockerx.SecretSpec{Name: full, Labels: labels, Data: o.Content})
			} else {
				id, e = c.ConfigCreate(ctx, dockerx.ConfigSpec{Name: full, Labels: labels, Data: o.Content})
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
		refs.set(o.Kind, o.Name, objectRef{ID: id, Name: full})
	}
	return refs, created, nil
}

// removeReplacedObjects removes the secrets and configs of the stack that no deploy uses any more:
// those that carry this stack's ID and the bayplan.stack.object label (so a made-from-a-file
// object, never one the user added by hand and never another stack's) and are not in keep. It
// reads the stack's services first and removes an object only when no service spec still
// references it (T-07-32); the manager refuses one a service of another owner references, and
// that is left in place too. removed names what was removed and leftInPlace what could not be:
// referenced, refused or failed. A removal that fails is not an error of the deploy, because the
// next deploy finds the same objects by their labels; err is only a read that failed, when
// nothing could be judged.
func (d Deps) removeReplacedObjects(ctx context.Context, envID string, stack Stack, keep objectRefs) (removed, leftInPlace []string, err error) {
	var svcs []dockerx.ServiceDetail
	var secrets, configs []objectRow
	err = d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		if svcs, e = c.ServicesWithLabel(ctx, stacks.LabelSwarmNamespace, stack.Name); e != nil {
			return e
		}
		srows, e := c.SecretsWithLabel(ctx, stacks.LabelStackID, stack.ID)
		if e != nil {
			return e
		}
		crows, e := c.ConfigsWithLabel(ctx, stacks.LabelStackID, stack.ID)
		if e != nil {
			return e
		}
		secrets, configs = secretRows(srows), configRows(crows)
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
	for i, r := range candidates {
		switch {
		case ctx.Err() != nil:
			for _, rest := range candidates[i:] {
				leftInPlace = append(leftInPlace, rest.Name)
			}
			return removed, leftInPlace, nil
		case used.has(r):
			leftInPlace = append(leftInPlace, r.Name)
			continue
		}
		rerr := d.Aggregator.WriteOnManager(ctx, envID, func(c *dockerx.Client) error {
			if r.Kind == stacks.KindSecret {
				return c.SecretRemove(ctx, r.ID)
			}
			return c.ConfigRemove(ctx, r.ID)
		})
		switch {
		case rerr == nil:
			removed = append(removed, r.Name)
		case errors.Is(rerr, dockerx.ErrObjectNotFound):
			// Already gone: nothing was left behind.
		default:
			if !errors.Is(rerr, dockerx.ErrObjectInUse) {
				d.log().Warn("could not remove a replaced stack object", "environment", envID, "stack", stack.Name, "kind", r.Kind, "object", r.Name, "error", rerr)
			}
			leftInPlace = append(leftInPlace, r.Name)
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
