// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package swarmops

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/redact"
)

// A swarm secret or config can't be changed: a new value is a new object. A rotate (D-26) makes that
// painless: it creates the new object, points every standalone service that uses the old one at it
// through the same update steps an edit takes, and removes the old one once nothing uses it. Services a
// stack owns are left alone, because the stack's next deploy would put the old object back.

// The states of a service in a rotate.
const (
	// RotateUpdated is a service that now uses the new object.
	RotateUpdated = "updated"
	// RotateSkipped is a service a stack owns, which a rotate does not change.
	RotateSkipped = "skipped"
	// RotateFailed is the service the run stopped at.
	RotateFailed = "failed"
	// RotatePending is a service the run stopped before reaching.
	RotatePending = "pending"
	// RotateUnchanged is a service that, when it was read, no longer used the old object.
	RotateUnchanged = "unchanged"
)

// ErrRotateStopped means a rotate created the new object and moved some services, then stopped at one it
// could not update. The error is a *RotateStopError and wraps the reason.
var ErrRotateStopped = errors.New("swarmops: the rotate stopped at a service")

// RotateStopError names the service a rotate stopped at and wraps why.
type RotateStopError struct {
	Service string
	Err     error
}

func (e *RotateStopError) Error() string {
	return "swarmops: the rotate stopped at " + e.Service + ": " + e.Err.Error()
}
func (e *RotateStopError) Unwrap() error        { return e.Err }
func (e *RotateStopError) Is(target error) bool { return target == ErrRotateStopped }

// RotatePreviewInput is an object to preview the rotate of. NewName is the name to use; empty means the
// next version of the old one's name.
type RotatePreviewInput struct {
	EnvID   string
	Client  *dockerx.Client
	Kind    string
	OldID   string
	NewName string
	Caller  Caller
}

// RotatePreviewUser is one service that uses the object: where it is mounted and, for a standalone
// service, its spec as it is and as it would be, both masked for a caller who may not reveal secrets.
type RotatePreviewUser struct {
	ServiceID, Name string
	// Stack is the stack that owns the service, empty for a standalone one; Skipped is true when it has one.
	Stack   string
	Skipped bool
	// File, UID, GID and Mode are how the service mounts the object.
	File, UID, GID string
	Mode           uint32
	// Current and Next are the service's spec and what it would be after the rotate. They are zero for a
	// skipped service. Next names the new object without an ID, which it does not have yet.
	Current, Next swarm.ServiceSpec
}

// RotatePreviewResult is what a rotate of an object would do.
type RotatePreviewResult struct {
	Kind, OldID, OldName string
	// NewName is the name given, or the next version of OldName; NameTaken says the swarm has it already.
	NewName   string
	NameTaken bool
	// Templating is a config's templating driver (golang, or empty for none); Labels are the old object's.
	Templating string
	Labels     map[string]string
	// Users are the services that use the object, in name order; Skipped counts those a stack owns.
	Users   []RotatePreviewUser
	Skipped int
}

// RotateInput is a rotate to make: the object to replace, the name and value of the new one and whether to
// remove the old one when nothing uses it. Data is the value, which crosses Coaming once, on its way to the
// manager. Templating is a config's driver (golang, or empty). NewName empty means the next version of the
// old name.
type RotateInput struct {
	EnvID      string
	Client     *dockerx.Client
	Kind       string
	OldID      string
	NewName    string
	Data       []byte
	Templating string
	RemoveOld  bool
	Caller     Caller
}

// RotateService is what a rotate did to one service.
type RotateService struct {
	ServiceID, Name string
	// Stack is the stack that owns a skipped service.
	Stack string
	// State is one of the Rotate* states; Code says why a failed service failed.
	State, Code string
	// Image and Digest are what the update was sent with; Version is the service's version it was sent at;
	// Findings counts the host-escalation settings of its spec. They are zero for a service that was not updated.
	Image, Digest string
	Version       uint64
	Findings      int
}

// RotateResult is what a rotate did. It is filled as far as the work got, also when Rotate returns an
// error, so the caller can audit the attempt.
type RotateResult struct {
	Kind, OldID, OldName string
	// NewID and NewName are the new object; Created is true once it was made.
	NewID, NewName string
	Created        bool
	// Services are the users of the old object in name order, each as far as the run got.
	Services []RotateService
	// Updated and Skipped count the services moved and those a stack owns.
	Updated, Skipped int
	// FailedAt is the service the run stopped at, empty for a run that did not stop.
	FailedAt string
	// OldRemoved is true when the old object was removed. RemoveCode says why it was not when removing it was
	// asked for and it stayed (object_in_use, remove_failed).
	OldRemoved bool
	RemoveCode string
}

// oldObject is the object being replaced, as far as a rotate needs it.
type oldObject struct {
	id, name, templating string
	labels               map[string]string
}

// readOld reads the object to replace by ID or name. An object the manager does not know is
// dockerx.ErrObjectNotFound. The content of a config is dropped at once.
func readOld(ctx context.Context, cli *dockerx.Client, kind, id string) (oldObject, error) {
	switch kind {
	case KindSecret:
		row, _, err := cli.SecretInspect(ctx, id)
		if err != nil {
			return oldObject{}, err
		}
		return oldObject{id: row.ID, name: row.Name, labels: maps.Clone(row.Labels)}, nil
	case KindConfig:
		detail, _, err := cli.ConfigInspect(ctx, id)
		if err != nil {
			return oldObject{}, err
		}
		clear(detail.Data)
		return oldObject{id: detail.ID, name: detail.Name, labels: maps.Clone(detail.Labels), templating: detail.Templating}, nil
	}
	return oldObject{}, fmt.Errorf("swarmops: a rotate is of a secret or a config, not %q", kind)
}

// takenNames lists the names of the objects of the kind the swarm has.
func takenNames(ctx context.Context, cli *dockerx.Client, kind string) (func(string) bool, error) {
	names := map[string]bool{}
	switch kind {
	case KindSecret:
		rows, err := cli.SecretList(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			names[r.Name] = true
		}
	case KindConfig:
		rows, err := cli.ConfigList(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			names[r.Name] = true
		}
	}
	return func(n string) bool { return names[n] }, nil
}

// usesObject lists the references of a service to the object, in the order the service has them.
func usesObject(ref dockerx.ServiceRef, kind, id string) []dockerx.ServiceObjectRef {
	list := ref.Secrets
	if kind == KindConfig {
		list = ref.Configs
	}
	var out []dockerx.ServiceObjectRef
	for _, o := range list {
		if o.ID == id {
			out = append(out, o)
		}
	}
	return out
}

// usersOf lists the services whose spec references the object, in name order then ID order.
func usersOf(ctx context.Context, cli *dockerx.Client, kind, id string) ([]dockerx.ServiceRef, error) {
	refs, err := cli.ServiceRefs(ctx)
	if err != nil {
		return nil, err
	}
	var out []dockerx.ServiceRef
	for _, r := range refs {
		if len(usesObject(r, kind, id)) > 0 {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b dockerx.ServiceRef) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func (d Deps) stackOf(labels map[string]string) (string, bool) {
	if d.StackOwner != nil {
		return d.StackOwner(labels)
	}
	v := labels[stackNamespaceLabel]
	return v, v != ""
}

// RotatePreview says what a rotate of the object would do, without changing anything: the name the new
// object would have, whether the swarm has that name, and for each service that uses the object where it
// is mounted and, for a standalone one, its spec as it is and as it would be (masked unless the caller
// may reveal). Services a stack owns are listed and flagged skipped. An object the manager does not know
// is dockerx.ErrObjectNotFound.
func RotatePreview(ctx context.Context, deps Deps, in RotatePreviewInput) (RotatePreviewResult, error) {
	old, err := readOld(ctx, in.Client, in.Kind, in.OldID)
	if err != nil {
		return RotatePreviewResult{}, err
	}
	taken, err := takenNames(ctx, in.Client, in.Kind)
	if err != nil {
		return RotatePreviewResult{}, err
	}
	res := RotatePreviewResult{
		Kind: in.Kind, OldID: old.id, OldName: old.name, NewName: in.NewName, Templating: old.templating, Labels: old.labels,
		Users: []RotatePreviewUser{},
	}
	if res.NewName == "" {
		res.NewName = nextVersionName(old.name, taken)
	}
	res.NameTaken = taken(res.NewName)
	if res.Labels == nil {
		res.Labels = map[string]string{}
	}
	refs, err := usersOf(ctx, in.Client, in.Kind, old.id)
	if err != nil {
		return RotatePreviewResult{}, err
	}
	for _, ref := range refs {
		u := RotatePreviewUser{ServiceID: ref.ID, Name: ref.Name}
		first := usesObject(ref, in.Kind, old.id)[0]
		u.File, u.UID, u.GID, u.Mode = first.File, first.UID, first.GID, first.Mode
		detail, _, err := in.Client.ServiceInspect(ctx, ref.ID)
		if errors.Is(err, dockerx.ErrServiceNotFound) {
			continue // removed since the list was read: it uses nothing now
		}
		if err != nil {
			return RotatePreviewResult{}, err
		}
		if stack, owned := deps.stackOf(detail.Labels); owned {
			u.Stack, u.Skipped = stack, true
			res.Skipped++
			res.Users = append(res.Users, u)
			continue
		}
		var current swarm.ServiceSpec
		if err := json.Unmarshal(detail.Spec, &current); err != nil {
			return RotatePreviewResult{}, fmt.Errorf("read the service's spec: %w", err)
		}
		next, err := cloneSpec(current)
		if err != nil {
			return RotatePreviewResult{}, err
		}
		replaceReference(&next, in.Kind, old.id, "", res.NewName)
		u.Current, _ = redact.MaskServiceSpec(current, in.Caller.Reveal)
		u.Next, _ = redact.MaskServiceSpec(next, in.Caller.Reveal)
		res.Users = append(res.Users, u)
	}
	return res, nil
}

// rotation is a rotate under way.
type rotation struct {
	deps Deps
	in   RotateInput
	old  oldObject
	res  *RotateResult
}

// Rotate replaces an object by a new version of it. In order it: reads the old object and the services that
// use it (dockerx.ErrObjectNotFound when it is not there); creates the new object, once, with the old one's
// labels, the value it was given and a config's templating, and a failure to create it
// (*ObjectCreateError, dockerx.ErrNameTaken for a name the swarm has) changes nothing else; then takes the
// users in name order, one at a time, and for each reads the service fresh, skips one a stack owns, points
// its references to the old object at the new one (same file, owner, group and mode) and sends the update
// through the same steps an edit takes: the host-escalation policy on the whole spec for the caller, the
// registry credential, the version just read, never retried. The first failure stops the run with a
// *RotateStopError wrapping why (dockerx.ErrServiceChanged when somebody edited the service in between),
// leaving the new object and the old one in place and the services after it untouched. When every user
// moved and removing the old object was asked for, it is removed if no service spec names it any more (a
// stack's service still does, then it stays). The value is in no error and no audit row. The result is
// filled as far as the work got.
func Rotate(ctx context.Context, deps Deps, in RotateInput) (RotateResult, error) {
	res := RotateResult{Kind: in.Kind, Services: []RotateService{}}
	old, err := readOld(ctx, in.Client, in.Kind, in.OldID)
	if err != nil {
		return res, err
	}
	res.OldID, res.OldName = old.id, old.name
	refs, err := usersOf(ctx, in.Client, in.Kind, old.id)
	if err != nil {
		return res, err
	}
	res.NewName = in.NewName
	if res.NewName == "" {
		taken, err := takenNames(ctx, in.Client, in.Kind)
		if err != nil {
			return res, err
		}
		res.NewName = nextVersionName(old.name, taken)
	}
	// Who owns each user is found before anything is made, so a person who is told the run stopped is also
	// told how many services it had to move, and a failure to read one changes nothing.
	for _, ref := range refs {
		svc := RotateService{ServiceID: ref.ID, Name: ref.Name, State: RotatePending}
		detail, _, err := in.Client.ServiceInspect(ctx, ref.ID)
		if errors.Is(err, dockerx.ErrServiceNotFound) {
			continue // removed since the list was read: it uses nothing now
		}
		if err != nil {
			return res, err
		}
		if stack, owned := deps.stackOf(detail.Labels); owned {
			svc.State, svc.Stack = RotateSkipped, stack
			res.Skipped++
		}
		res.Services = append(res.Services, svc)
	}

	staged := StagedObject{Kind: in.Kind, Name: res.NewName, Data: in.Data, Templating: in.Templating, Labels: old.labels}
	id, err := createOne(ctx, in.Client, staged)
	if err != nil {
		code := codeCreateFailed
		if errors.Is(err, dockerx.ErrNameTaken) {
			code = codeNameTaken
		}
		deps.Record.record(createdRowFrom(staged, res.NewName, code, SourceRotate))
		return res, &ObjectCreateError{Kind: in.Kind, Name: res.NewName, Err: err}
	}
	deps.Record.record(createdRowFrom(staged, id, codeCreated, SourceRotate))
	res.NewID, res.Created = id, true

	r := &rotation{deps: deps, in: in, old: old, res: &res}
	for i := range res.Services {
		if res.Services[i].State == RotateSkipped {
			continue
		}
		if err := r.service(ctx, i); err != nil {
			res.FailedAt = res.Services[i].Name
			return res, &RotateStopError{Service: res.Services[i].Name, Err: err}
		}
	}
	if in.RemoveOld && res.Updated > 0 {
		res.OldRemoved, res.RemoveCode = r.removeOld(ctx)
	}
	return res, nil
}

// service moves service i of the run to the new object. A service a stack owns by now is skipped; any
// other problem is returned and recorded on the service.
func (r *rotation) service(ctx context.Context, i int) error {
	svc := &r.res.Services[i]
	fail := func(err error) error {
		svc.State, svc.Code = RotateFailed, stepCode(err)
		return err
	}
	cli := r.in.Client
	detail, _, err := cli.ServiceInspect(ctx, svc.ServiceID)
	if err != nil {
		return fail(err)
	}
	svc.Name = detail.Name
	if stack, owned := r.deps.stackOf(detail.Labels); owned {
		svc.State, svc.Stack = RotateSkipped, stack
		r.res.Skipped++
		return nil
	}
	p := &prepared{detail: detail, changed: true}
	if err := json.Unmarshal(detail.Spec, &p.current); err != nil {
		return fail(fmt.Errorf("read the service's spec: %w", err))
	}
	if p.merged, err = cloneSpec(p.current); err != nil {
		return fail(err)
	}
	if replaceReference(&p.merged, r.in.Kind, r.old.id, r.res.NewID, r.res.NewName) == 0 {
		svc.State = RotateUnchanged // somebody else moved it since the list was read
		return nil
	}
	c := common{envID: r.in.EnvID, serviceID: detail.ID, version: detail.Version, caller: r.in.Caller, client: cli}
	if err := p.planObjects(ctx, c); err != nil {
		return fail(err)
	}
	var out Outcome
	err = p.commit(ctx, r.deps, c, &out, false, func(raw []byte, auth string) ([]string, error) {
		return cli.ServiceUpdate(ctx, detail.ID, detail.Version, raw, auth)
	})
	svc.Image, svc.Digest, svc.Version, svc.Findings = out.Image.Ref, out.Image.Digest, detail.Version, len(out.Findings)
	if err != nil {
		return fail(err)
	}
	svc.State = RotateUpdated
	r.res.Updated++
	return nil
}

// removeOld removes the old object when no service spec names it any more. It answers whether it did and,
// when it did not, why: object_in_use for a spec that still names it (a stack's), remove_failed otherwise.
func (r *rotation) removeOld(ctx context.Context) (removed bool, code string) {
	users, err := usersOf(ctx, r.in.Client, r.in.Kind, r.old.id)
	if err != nil {
		return false, codeRemoveFailed
	}
	if len(users) > 0 {
		return false, codeInUse
	}
	switch r.in.Kind {
	case KindSecret:
		err = r.in.Client.SecretRemove(ctx, r.old.id)
	case KindConfig:
		err = r.in.Client.ConfigRemove(ctx, r.old.id)
	}
	gone := CreatedObject{Kind: r.in.Kind, Name: r.old.name, ID: r.old.id}
	switch {
	case err == nil, errors.Is(err, dockerx.ErrObjectNotFound):
		r.deps.Record.record(removedRowFrom(gone, codeRemoved, SourceRotate))
		return true, ""
	case errors.Is(err, dockerx.ErrObjectInUse):
		r.deps.Record.record(removedRowFrom(gone, codeInUse, SourceRotate))
		return false, codeInUse
	}
	r.deps.Record.record(removedRowFrom(gone, codeRemoveFailed, SourceRotate))
	return false, codeRemoveFailed
}
