// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The kinds of object an apply creates for a service, in the order they are created: a network
// first, then the secrets and configs that are mounted into its tasks.
const (
	KindNetwork = "network"
	KindSecret  = "secret"
	KindConfig  = "config"
)

// Sources an audit row of an object made or removed by an apply carries.
const (
	SourceApply        = "service_apply"
	SourceApplyCleanup = "service_apply_cleanup"
	// SourceRotate is the source of the rows of the new object and of the old one a rotate removes.
	SourceRotate = "rotate"
)

// The states and codes of an ApplyStep and the codes of an audit row of an object.
const (
	StepDone    = "done"
	StepFailed  = "failed"
	StepSkipped = "skipped"

	StepImage   = "image"
	StepObject  = "object"
	StepService = "service"

	codeCreated      = "created"
	codeRemoved      = "removed"
	codeNameTaken    = "name_taken"
	codeCreateFailed = "create_failed"
	codeRemoveFailed = "remove_failed"
	codeInUse        = "object_in_use"
	codeUpdateFailed = "update_failed"
	codeNameTakenSvc = "service_name_taken"
)

// cleanupTimeout bounds the removal of the objects of a failed apply. It has a clock of its own
// because the failure may well be the request's clock running out.
const cleanupTimeout = 30 * time.Second

// ErrObjectPermission means the caller may not create one of the kinds of object the apply needs,
// so nothing was created. The error is an *ObjectPermissionError.
var ErrObjectPermission = errors.New("swarmops: the caller may not create the objects the spec needs")

// ObjectPermissionError names the kinds the caller may not create, in creation order.
type ObjectPermissionError struct{ Kinds []string }

func (e *ObjectPermissionError) Error() string {
	return "swarmops: the caller may not create: " + strings.Join(e.Kinds, ", ")
}
func (e *ObjectPermissionError) Is(target error) bool { return target == ErrObjectPermission }

// ObjectCreateError is the creation of one object failing: which, and why. A name the swarm has
// already is dockerx.ErrNameTaken through Unwrap.
type ObjectCreateError struct {
	Kind, Name string
	Err        error
}

func (e *ObjectCreateError) Error() string {
	return "swarmops: could not create " + e.Kind + " " + e.Name + ": " + e.Err.Error()
}
func (e *ObjectCreateError) Unwrap() error { return e.Err }

// StagedObject is an object the caller made in the editor, which does not exist yet. Data is the
// value of a secret or the content of a config; it crosses Bayplan once, on its way to the manager.
type StagedObject struct {
	Kind, Name string
	Data       []byte
	Templating string
	// Subnet is the optional addressing of a network, in CIDR form.
	Subnet string
	Labels map[string]string
}

// CreatedObject is an object an apply made.
type CreatedObject struct{ Kind, Name, ID string }

// CleanupReport is what removing the objects of a failed apply did: Removed in the order they were
// removed (the reverse of the order they were made), NotRemoved those that are still there.
type CleanupReport struct{ Removed, NotRemoved []CreatedObject }

// ApplyStep is one step of an apply: the image, an object, the service. State is done, failed or
// skipped (not reached, or nothing to do); Code says why a step failed.
type ApplyStep struct{ Step, Kind, Name, State, Code string }

// Recorder takes the audit row of an object made or removed. The caller fills in who and where.
type Recorder func(audit.Entry)

func (r Recorder) record(e audit.Entry) {
	if r != nil {
		r(e)
	}
}

// kindRank orders the kinds as they are created.
func kindRank(kind string) int {
	switch kind {
	case KindNetwork:
		return 0
	case KindSecret:
		return 1
	}
	return 2
}

// objectRef is one place a spec names a network, secret or config. id and name are what it says;
// a network says only a target, which is a name or an ID, in name.
type objectRef struct {
	kind, id, name string
	setID          func(id string)
}

// refs lists every network, secret and config the spec names. The setters write into spec, which
// the caller owns. A config that is a runtime target names no object.
func refs(spec *swarm.ServiceSpec) []objectRef {
	var out []objectRef
	for i := range spec.TaskTemplate.Networks {
		n := &spec.TaskTemplate.Networks[i]
		out = append(out, objectRef{kind: KindNetwork, name: n.Target, setID: func(id string) { n.Target = id }})
	}
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		for _, s := range cs.Secrets {
			if s != nil {
				out = append(out, objectRef{kind: KindSecret, id: s.SecretID, name: s.SecretName, setID: func(id string) { s.SecretID = id }})
			}
		}
		for _, c := range cs.Configs {
			if c != nil && c.Runtime == nil {
				out = append(out, objectRef{kind: KindConfig, id: c.ConfigID, name: c.ConfigName, setID: func(id string) { c.ConfigID = id }})
			}
		}
	}
	return out
}

// shown is how a person is told about a reference: its name, else its ID.
func (r objectRef) shown() string {
	if r.name != "" {
		return r.name
	}
	return r.id
}

// existing is what the environment has, by kind.
type existing struct {
	networks []dockerx.NetworkRow
	secrets  []dockerx.SecretRow
	configs  []dockerx.ConfigRow
}

// find says whether the reference names an object that exists and, for a secret or config named
// without an ID, the ID of the object its name is.
func (e existing) find(r objectRef) (id string, ok bool) {
	switch r.kind {
	case KindNetwork:
		_, ok = findNetwork(e.networks, r.name)
		return "", ok
	case KindSecret:
		for _, s := range e.secrets {
			if (r.id != "" && s.ID == r.id) || (r.id == "" && s.Name == r.name) {
				return s.ID, true
			}
		}
	case KindConfig:
		for _, c := range e.configs {
			if (r.id != "" && c.ID == r.id) || (r.id == "" && c.Name == r.name) {
				return c.ID, true
			}
		}
	}
	return "", false
}

// objectKey names a staged object.
func objectKey(kind, name string) string { return kind + "\x00" + name }

// fill writes the ID of an object an apply made into the reference that named it.
type fill struct {
	key   string
	setID func(id string)
}

// objectPlan is what a spec needs done about the objects it names: what is missing (named, not
// staged), what to create (named, staged) and where to write the new IDs.
type objectPlan struct {
	missing Missing
	create  []StagedObject
	fills   []fill
}

// planObjects works out the objects a spec names against what exists and what is staged. A
// reference that names an existing secret or config by name only is given its ID in spec. A
// reference whose name was staged is to be created, whatever the environment has by that name now:
// the editor chose to make a new one, so somebody else's that appeared meanwhile is a name_taken
// and never silently used. spec must be a copy the caller owns.
func planObjects(spec *swarm.ServiceSpec, have existing, staged []StagedObject) objectPlan {
	plan := objectPlan{missing: Missing{Networks: []string{}, Secrets: []string{}, Configs: []string{}}}
	byKey := map[string]StagedObject{}
	for _, s := range staged {
		if _, dup := byKey[objectKey(s.Kind, s.Name)]; !dup {
			byKey[objectKey(s.Kind, s.Name)] = s
		}
	}
	needed := map[string]bool{}
	for _, r := range refs(spec) {
		key := objectKey(r.kind, r.name)
		id, ok := have.find(r)
		if _, isStaged := byKey[key]; isStaged && r.name != "" && (r.kind == KindNetwork || !ok || r.id == "") {
			if !needed[key] {
				needed[key] = true
				plan.create = append(plan.create, byKey[key])
			}
			plan.fills = append(plan.fills, fill{key: key, setID: r.setID})
			continue
		}
		switch {
		case ok:
			if r.kind != KindNetwork && r.id == "" {
				r.setID(id)
			}
		default:
			plan.missing.add(r.kind, r.shown())
		}
	}
	slices.SortStableFunc(plan.create, func(a, b StagedObject) int { return kindRank(a.Kind) - kindRank(b.Kind) })
	return plan
}

// add notes a name once.
func (m *Missing) add(kind, name string) {
	list := &m.Configs
	switch kind {
	case KindNetwork:
		list = &m.Networks
	case KindSecret:
		list = &m.Secrets
	}
	if !containsString(*list, name) {
		*list = append(*list, name)
	}
}

// missingObjects lists what the spec names that the environment does not have, by the name or ID
// the spec used. It judges a copy: the spec is not changed.
func missingObjects(spec swarm.ServiceSpec, have existing) Missing {
	return planObjects(&spec, have, nil).missing
}

// kinds lists the kinds of the objects to create, once each, in creation order.
func kinds(objects []StagedObject) []string {
	var out []string
	for _, o := range objects {
		if !slices.Contains(out, o.Kind) {
			out = append(out, o.Kind)
		}
	}
	return out
}

// createObjects creates the objects in dependency order (networks, secrets, configs, each in the
// order given), once each and never retried. It stops at the first that fails and returns what it
// made so far beside an *ObjectCreateError. Each attempt is recorded. The values are not kept.
func createObjects(ctx context.Context, cli *dockerx.Client, objects []StagedObject, record Recorder) ([]CreatedObject, error) {
	ordered := slices.Clone(objects)
	slices.SortStableFunc(ordered, func(a, b StagedObject) int { return kindRank(a.Kind) - kindRank(b.Kind) })
	created := make([]CreatedObject, 0, len(ordered))
	for _, o := range ordered {
		id, err := createOne(ctx, cli, o)
		if err != nil {
			code := codeCreateFailed
			if errors.Is(err, dockerx.ErrNameTaken) {
				code = codeNameTaken
			}
			record.record(createdRow(o, o.Name, code))
			return created, &ObjectCreateError{Kind: o.Kind, Name: o.Name, Err: err}
		}
		created = append(created, CreatedObject{Kind: o.Kind, Name: o.Name, ID: id})
		record.record(createdRow(o, id, codeCreated))
	}
	return created, nil
}

// createOne makes one object. A name the environment has already is dockerx.ErrNameTaken.
func createOne(ctx context.Context, cli *dockerx.Client, o StagedObject) (string, error) {
	switch o.Kind {
	case KindNetwork:
		id, _, err := cli.NetworkCreate(ctx, dockerx.NetworkCreateSpec{
			Name: o.Name, Driver: dockerx.DriverOverlay, Scope: dockerx.ScopeSwarm, Attachable: true, Subnet: o.Subnet,
		})
		if err != nil && (cerrdefs.IsConflict(err) || cerrdefs.IsAlreadyExists(err) || strings.Contains(err.Error(), "already exists")) {
			return "", fmt.Errorf("%w: %w", dockerx.ErrNameTaken, err)
		}
		return id, err
	case KindSecret:
		return cli.SecretCreate(ctx, dockerx.SecretSpec{Name: o.Name, Labels: o.Labels, Data: o.Data})
	case KindConfig:
		return cli.ConfigCreate(ctx, dockerx.ConfigSpec{Name: o.Name, Labels: o.Labels, Data: o.Data, Templating: o.Templating})
	}
	return "", fmt.Errorf("swarmops: unknown kind of object %q", o.Kind)
}

// createdRow is the audit row of an attempt to make a staged object: what was asked and the code.
// It never carries the value, only its size.
func createdRow(o StagedObject, targetID, code string) audit.Entry {
	return createdRowFrom(o, targetID, code, SourceApply)
}

// createdRowFrom is createdRow for an object made for another reason than a service apply.
func createdRowFrom(o StagedObject, targetID, code, source string) audit.Entry {
	e := audit.Entry{TargetType: o.Kind, TargetID: targetID, Result: audit.ResultSuccess}
	if code != codeCreated {
		e.Result, e.Reason = audit.ResultFailure, code
	}
	d := map[string]string{"name": o.Name, "source": source, "code": code}
	switch o.Kind {
	case KindNetwork:
		e.Action = audit.ActionNetworkCreated
		d["driver"], d["scope"] = dockerx.DriverOverlay, dockerx.ScopeSwarm
		d["internal"], d["attachable"] = "false", "true"
		if o.Subnet != "" {
			d["subnet"] = o.Subnet
		}
	case KindSecret:
		e.Action = audit.ActionSecretCreated
		d["labels"], d["size"] = strconv.Itoa(len(o.Labels)), strconv.Itoa(len(o.Data))
	case KindConfig:
		e.Action = audit.ActionConfigCreated
		d["labels"], d["size"] = strconv.Itoa(len(o.Labels)), strconv.Itoa(len(o.Data))
		d["templating"] = "none"
		if o.Templating != "" {
			d["templating"] = o.Templating
		}
	}
	e.Details = d
	return e
}

// cleanupObjects removes the objects an apply made, in the reverse of the order it made them, once
// each. A removal that fails does not stop the others: it is in NotRemoved, to be named to the
// person. An object that is already gone counts as removed. Each removal is recorded. It has its
// own deadline, because the failure that led here may be the request's.
func cleanupObjects(ctx context.Context, cli *dockerx.Client, created []CreatedObject, record Recorder) CleanupReport {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	var rep CleanupReport
	for _, o := range slices.Backward(created) {
		var err error
		switch o.Kind {
		case KindNetwork:
			err = cli.NetworkRemove(ctx, o.ID)
		case KindSecret:
			err = cli.SecretRemove(ctx, o.ID)
		case KindConfig:
			err = cli.ConfigRemove(ctx, o.ID)
		}
		code := codeRemoved
		switch {
		case err == nil, errors.Is(err, dockerx.ErrObjectNotFound):
			rep.Removed = append(rep.Removed, o)
		case errors.Is(err, dockerx.ErrObjectInUse):
			code = codeInUse
			rep.NotRemoved = append(rep.NotRemoved, o)
		default:
			code = codeRemoveFailed
			rep.NotRemoved = append(rep.NotRemoved, o)
		}
		record.record(removedRow(o, code))
	}
	return rep
}

// removedRow is the audit row of removing an object the apply made.
func removedRow(o CreatedObject, code string) audit.Entry {
	return removedRowFrom(o, code, SourceApplyCleanup)
}

// removedRowFrom is removedRow for an object removed for another reason than the cleanup of an apply.
func removedRowFrom(o CreatedObject, code, source string) audit.Entry {
	e := audit.Entry{TargetType: o.Kind, TargetID: o.ID, Result: audit.ResultSuccess}
	if code != codeRemoved {
		e.Result, e.Reason = audit.ResultFailure, code
	}
	d := map[string]string{"name": o.Name, "source": source, "code": code}
	switch o.Kind {
	case KindNetwork:
		e.Action = audit.ActionNetworkRemoved
		d["driver"], d["scope"] = dockerx.DriverOverlay, dockerx.ScopeSwarm
	case KindSecret:
		e.Action = audit.ActionSecretRemoved
	case KindConfig:
		e.Action = audit.ActionConfigRemoved
	}
	e.Details = d
	return e
}

// stepCode is the code a failed step carries.
func stepCode(err error) string {
	var rej *RejectedError
	switch {
	case errors.Is(err, dockerx.ErrNameTaken):
		return codeNameTaken
	case errors.Is(err, dockerx.ErrServiceNameTaken):
		return codeNameTakenSvc
	case errors.Is(err, dockerx.ErrServiceChanged):
		return "service_changed"
	case errors.As(err, &rej):
		return "spec_rejected"
	case errors.Is(err, ErrRefused):
		return "privileged_service"
	case errors.Is(err, ErrImageUnresolved):
		return "image_unresolved"
	case errors.Is(err, ErrRegistryUnresolved):
		return "registry_unresolved"
	case errors.Is(err, ErrObjectsMissing):
		return "objects_missing"
	case errors.Is(err, ErrRevealRequired):
		return "secret_reveal_required"
	case errors.Is(err, ErrSpecInvalid):
		return "spec_invalid"
	case errors.Is(err, dockerx.ErrServiceNotFound):
		return "service_not_found"
	}
	var ce *ObjectCreateError
	if errors.As(err, &ce) {
		return codeCreateFailed
	}
	return codeUpdateFailed
}
