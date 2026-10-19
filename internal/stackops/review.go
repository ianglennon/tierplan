// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// The modes of a review: a deploy of the draft, or an update of the images of some services (D-16).
const (
	ReviewModeDeploy = "deploy"
	ReviewModeUpdate = "update"
)

// The states of an image row of a review.
const (
	// ImagePinned is a service that keeps the digest it runs.
	ImagePinned = "pinned"
	// ImageNew is an image the review just looked up, for a new service or a changed reference.
	ImageNew = "new"
	// ImageAsWritten is a reference that carries its own digest in the compose file.
	ImageAsWritten = "as_written"
	// ImageLocal is an image no registry has, pinned to the image ID on the manager.
	ImageLocal = "local"
	// ImageUnreachable is a registry that could not be reached for an image that needs a lookup.
	ImageUnreachable = "unreachable"
	// ImageUnresolved is an image a registry did not give, for a reference that needs a lookup.
	ImageUnresolved = "unresolved"
)

// ChangeExternal is the change of an object the stack only names: Bayplan never touches it.
const ChangeExternal = "external"

// The sentences of the Other resources table of a review (UI-SPEC, Review).
const (
	detailNewVersion = "New version. The old one is removed after the deploy."
	detailExternal   = "External. Bayplan doesn't touch it."
	detailRemoved    = "Removed from the stack. Deleted with the deploy."
	detailNetwork    = "Differs from the file. A network can't be changed in place, so the deploy leaves it as it is."
)

// The codes of the problems a review adds to what the check found.
const (
	CodeExternalMissingProblem = "external_missing"
	CodeObjectInvalidProblem   = "object_invalid"
	CodeConvertProblem         = "convert_failed"
)

// ReviewInput is a stack to review: the environment, the stack, its saved working copy and the
// version of the draft it came from, what to review (a deploy, or an update of the images of
// Services) and who asks.
type ReviewInput struct {
	EnvID string
	Stack Stack
	Copy  stacks.WorkingCopy
	// Previous is the working copy of the stack's last good revision, when it has one. Its secret
	// values are hidden too, so a secret that has since changed is not shown as the old value
	// of the running service.
	Previous     *stacks.WorkingCopy
	DraftVersion int64
	Mode         string
	Services     []string
	// Pins is where the pins of the stack's last good revision are read from. Nil means none.
	Pins   PinStore
	Caller Caller
}

// Counts is how many of a kind a deploy would add, change and remove.
type Counts struct{ Added, Changed, Removed int }

// Summary is the counts a review opens with. Volumes are always zero on a swarm: a stack's named
// volumes are made by the engine of each node when a task first uses one, so there is nothing for
// a deploy to create, change or remove.
type Summary struct {
	Services, Networks, Volumes, Secrets, Configs Counts
	// ImagesPinned counts the services that keep or carry a digest, ImagesNew the ones whose
	// digest was just looked up.
	ImagesPinned, ImagesNew int
}

// ImageRow is one service's image: the reference as written, the digest it will run with and the
// digest it runs with now, and what decided it.
type ImageRow struct {
	Service, Image, Digest, Current, State, Note string
}

// ObjectChange is a network, secret or config a deploy would add, change or remove.
type ObjectChange struct {
	Change, Kind, Name, Detail string
}

// Review is what a deploy of a stack would do, read against what is running. Nothing was changed
// to make it.
type Review struct {
	// Problems are what the check found and what the review added: the policy, a name clash, an
	// image that cannot be looked up. A problem that is an error stops a deploy.
	Problems stacks.Problems
	Summary  Summary
	Images   []ImageRow
	Services []stacks.ServiceChange
	Objects  []ObjectChange
	// DraftVersion is the draft the review was made of.
	DraftVersion int64
	// Pins are the images to deploy with, by swarm service name: the digests the review looked up
	// and the ones services keep. A deploy that carries them uses exactly these.
	Pins map[string]string
	// NeedsAdmin names the kinds of host access the stack uses that the caller may not deploy.
	NeedsAdmin []string
}

// Review reads what a deploy of the saved draft would do. It runs the check, reads what the swarm
// holds under the stack's namespace (one read of a manager), looks up the digests of the images
// that need one, converts the stack the way a deploy does and compares each service with the one
// running. It never writes: not to an engine, and not to the store.
//
// A stack that does not load is answered with the check's problems and nothing else. A problem
// that blocks a deploy is an error in Problems; the review still lists what it can, so the person
// sees the change beside the reason. A failure to read the swarm is the error returned.
func (d Deps) Review(ctx context.Context, in ReviewInput) (Review, error) {
	rv := Review{DraftVersion: in.DraftVersion, Pins: map[string]string{}, NeedsAdmin: []string{}}
	// A review judges what the caller may deploy, so it cannot be made without a way to judge it.
	if d.Checker == nil || in.Caller.Principal == nil {
		return rv, ErrNoPolicy
	}
	if err := d.CheckTarget(ctx, in.EnvID, in.Stack); err != nil {
		return rv, err
	}
	res := stacks.Check(ctx, stacks.CheckInput{Name: in.Stack.Name, Target: in.Stack.Target, Copy: in.Copy})
	rv.Problems = res.Problems
	if res.Loaded == nil {
		return rv, nil
	}
	files, vars := stacks.NewFileSet(in.Copy.Files), varTable(in.Copy.Vars)
	project := res.Loaded.Project
	objs, err := stacks.ResolveObjects(project, files, vars)
	if err != nil {
		rv.Problems = append(rv.Problems, objectErrorProblem(err)).Sorted()
		return rv, nil
	}

	live, err := d.readLive(ctx, in.EnvID, in.Stack.Name, liveWant{
		allNetworks: true,
		allSecrets:  hasExternalObject(objs, stacks.KindSecret),
		allConfigs:  hasExternalObject(objs, stacks.KindConfig),
	})
	if err != nil {
		return rv, err
	}
	if clash(in.Stack.ID, in.Stack.Name, live.Networks, live.Services, live.Objects) {
		rv.Problems = append(rv.Problems, stacks.Problem{
			Severity: stacks.SeverityError, Code: CodeNameClash, Message: NameClashMessage(in.Stack.Name), File: stacks.ComposeFile,
			Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		})
	}
	plan, err := convert.Convert(project, convert.Options{StackName: in.Stack.Name, StackID: in.Stack.ID, Objects: reviewRefs(in.Stack, objs, live.Objects)})
	if err != nil {
		rv.Problems = append(rv.Problems, ConvertProblem(err)).Sorted()
		return rv, nil
	}
	pol, err := d.Policy(ctx, in.Caller, in.EnvID, in.Copy, plan)
	if err != nil {
		return rv, err
	}
	rv.Problems = append(rv.Problems, pol.Problems...)
	if pol.Kinds != nil {
		rv.NeedsAdmin = pol.Kinds
	}

	byName := make(map[string]dockerx.ServiceDetail, len(live.Services))
	for _, s := range live.Services {
		byName[s.Name] = s
	}
	var stored map[string]stacks.LivePin
	if in.Pins != nil {
		var perr error
		if stored, perr = in.Pins.GoodPins(ctx, in.Stack.ID); perr != nil {
			d.log().Warn("could not read the pins of a stack's last good revision", "environment", in.EnvID, "stack", in.Stack.Name, "error", perr)
		}
	}
	var update map[string]bool
	if in.Mode == ReviewModeUpdate {
		update = make(map[string]bool, len(in.Services))
		for _, s := range in.Services {
			update[s] = true
		}
	}
	pinned := d.ResolvePins(ctx, in.EnvID, plan.Services, byName, stored, update)
	rv.Problems = append(rv.Problems, placeProblems(files, pinned.Problems)...)
	if pinned.Code != "" && len(pinned.Problems) == 0 {
		// A failure that is not about one image (no manager is connected, the agent refused).
		rv.Problems = append(rv.Problems, stacks.Problem{
			Severity: stacks.SeverityError, Code: pinned.Code, Message: CodeMessage(pinned.Code), File: stacks.ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		})
	}
	rv.Pins = pinned.Refs()
	rv.Images = imageRows(plan.Services, pinned, byName, update != nil)
	for _, r := range rv.Images {
		switch r.State {
		case ImageNew:
			rv.Summary.ImagesNew++
		case ImagePinned, ImageAsWritten, ImageLocal:
			rv.Summary.ImagesPinned++
		}
	}

	desired := make(map[string]swarm.ServiceSpec, len(plan.Services))
	for _, sp := range plan.Services {
		spec := sp.Spec
		if p, ok := pinned.Pins[sp.Name]; ok && spec.TaskTemplate.ContainerSpec != nil {
			cs := *spec.TaskTemplate.ContainerSpec
			cs.Image = p.Ref
			spec.TaskTemplate.ContainerSpec = &cs
		}
		if existing, ok := byName[sp.Name]; ok {
			// The counter that makes the swarm replace tasks is carried over, as a deploy does.
			spec.TaskTemplate.ForceUpdate = liveForceUpdate(existing.Spec)
		}
		desired[sp.Name] = spec
	}
	running, err := liveSpecs(live, in.Stack)
	if err != nil {
		return rv, err
	}
	rv.Services = stacks.DiffServices(desired, running, reviewMasker(in))
	for _, c := range rv.Services {
		bump(&rv.Summary.Services, c.Change)
	}

	var problems stacks.Problems
	rv.Objects, problems = resourceChanges(in.Stack, plan, objs, live)
	rv.Problems = append(rv.Problems, placeProblems(files, problems)...)
	for _, o := range rv.Objects {
		switch o.Kind {
		case "network":
			bump(&rv.Summary.Networks, o.Change)
		case string(stacks.KindSecret):
			bump(&rv.Summary.Secrets, o.Change)
		case string(stacks.KindConfig):
			bump(&rv.Summary.Configs, o.Change)
		}
	}
	rv.Problems = rv.Problems.Sorted()
	return rv, nil
}

// bump counts one change; an external object is not a change.
func bump(c *Counts, change string) {
	switch change {
	case stacks.ChangeAdded:
		c.Added++
	case stacks.ChangeChanged:
		c.Changed++
	case stacks.ChangeRemoved:
		c.Removed++
	}
}

// varTable is the table of the stack's variables by name, as a deploy reads it.
func varTable(vars []stacks.Var) map[string]string {
	out := make(map[string]string, len(vars))
	for _, v := range vars {
		out[v.Name] = v.Value
	}
	return out
}

// reviewMasker hides the secrets of the working copy and, when there is one, of the revision that
// is running.
func reviewMasker(in ReviewInput) stacks.Masker {
	vars, files := slices.Clone(in.Copy.Vars), slices.Clone(in.Copy.Files)
	if in.Previous != nil {
		vars, files = append(vars, in.Previous.Vars...), append(files, in.Previous.Files...)
	}
	return stacks.NewMasker(vars, files)
}

// liveStack is what a swarm holds under a stack's namespace, read once through a manager.
type liveStack struct {
	// Networks are the networks that carry the namespace; AllNetworks every network, read only
	// when asked for (an external network is looked up by name among all of them).
	Networks, AllNetworks []dockerx.NetworkRow
	Services              []dockerx.ServiceDetail
	// Objects are the secrets and configs under the namespace, or every one of a kind that has an
	// external object to find.
	Objects []objectRow
}

// liveWant says which lists to read whole.
type liveWant struct{ allNetworks, allSecrets, allConfigs bool }

// readLive reads the services, networks, secrets and configs a stack owns, in one ClusterRead.
func (d Deps) readLive(ctx context.Context, envID, stackName string, want liveWant) (liveStack, error) {
	var out liveStack
	err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		if out.Networks, e = c.NetworksWithLabel(ctx, stacks.LabelSwarmNamespace, stackName); e != nil {
			return e
		}
		if out.Services, e = c.ServicesWithLabel(ctx, stacks.LabelSwarmNamespace, stackName); e != nil {
			return e
		}
		if want.allNetworks {
			if out.AllNetworks, e = c.Networks(ctx); e != nil {
				return e
			}
		}
		var secrets []dockerx.SecretRow
		if want.allSecrets {
			secrets, e = c.SecretList(ctx)
		} else {
			secrets, e = c.SecretsWithLabel(ctx, stacks.LabelSwarmNamespace, stackName)
		}
		if e != nil {
			return e
		}
		var configs []dockerx.ConfigRow
		if want.allConfigs {
			configs, e = c.ConfigList(ctx)
		} else {
			configs, e = c.ConfigsWithLabel(ctx, stacks.LabelSwarmNamespace, stackName)
		}
		if e != nil {
			return e
		}
		out.Objects = append(secretRows(secrets), configRows(configs)...)
		return nil
	})
	return out, err
}

// reviewRefs resolves a stack's secrets and configs for a conversion made before anything is
// created: each has the name a deploy would give it, and the ID of the object that already has
// that name, so a service that is unchanged converts to the spec the swarm holds.
func reviewRefs(stack Stack, objs []stacks.ObjectContent, live []objectRow) objectRefs {
	refs := newObjectRefs()
	for _, o := range objs {
		if o.External {
			ref := objectRef{Name: o.ExternalName}
			if row, ok := findObject(live, o.Kind, o.ExternalName); ok {
				ref.ID = row.ID
			}
			refs.set(o.Kind, o.Name, ref)
			continue
		}
		full, _ := stacks.ObjectName(stack.Name, o.Name, o.Content)
		ref := objectRef{Name: full}
		if row, ok := findObject(live, o.Kind, full); ok && row.Labels[stacks.LabelStackID] == stack.ID {
			ref.ID = row.ID
		}
		refs.set(o.Kind, o.Name, ref)
	}
	return refs
}

// liveSpecs reads the spec of each service the swarm holds for this stack, by name, with the
// networks it attaches named as the compose file names them: the swarm stores a network by ID. A
// service of the namespace that Bayplan does not own is left out; the name-clash check refuses
// the stack for it.
func liveSpecs(live liveStack, stack Stack) (map[string]swarm.ServiceSpec, error) {
	idName := make(map[string]string, len(live.AllNetworks))
	for _, n := range live.AllNetworks {
		idName[n.ID] = n.Name
	}
	out := make(map[string]swarm.ServiceSpec, len(live.Services))
	for _, s := range live.Services {
		if s.Labels[stacks.LabelStackID] != stack.ID {
			continue
		}
		var spec swarm.ServiceSpec
		if err := json.Unmarshal(s.Spec, &spec); err != nil {
			return nil, fmt.Errorf("stackops: read the spec of service %s: %w", s.Name, err)
		}
		for i, n := range spec.TaskTemplate.Networks {
			if name, ok := idName[n.Target]; ok {
				spec.TaskTemplate.Networks[i].Target = name
			}
		}
		out[s.Name] = spec
	}
	return out, nil
}

// digestOf is the digest an image reference runs with: what follows the @, or the whole of an
// image ID.
func digestOf(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[i+1:]
	}
	if strings.HasPrefix(ref, "sha256:") {
		return ref
	}
	return ""
}

// untagged reports whether an image reference names neither a tag nor a digest, so it can move.
func untagged(text string) bool {
	named, err := reference.ParseNormalizedNamed(text)
	return err == nil && reference.IsNameOnly(named)
}

// imageRows is one row for each service with an image, ordered by the name in the compose file.
func imageRows(services []convert.ServicePlan, pinned PinOutcome, live map[string]dockerx.ServiceDetail, updating bool) []ImageRow {
	var rows []ImageRow
	for _, sp := range services {
		if sp.ImageAsWritten == "" {
			continue
		}
		row := ImageRow{Service: sp.Short, Image: sp.ImageAsWritten}
		if det, ok := live[sp.Name]; ok {
			row.Current = digestOf(det.Image)
		}
		p, ok := pinned.Pins[sp.Name]
		if !ok {
			row.State, row.Note = ImageUnresolved, "Couldn't look up the image."
			for _, pr := range pinned.Problems {
				if pr.Key == "services."+sp.Short+".image" && pr.Code == CodeRegistryUnreachable {
					row.State, row.Note = ImageUnreachable, "Can't reach the registry"
				}
			}
			rows = append(rows, row)
			continue
		}
		row.Digest = digestOf(p.Ref)
		switch p.Action {
		case stacks.PinKeep:
			row.State = ImagePinned
		case stacks.PinAsWritten:
			row.State, row.Note = ImageAsWritten, "Pinned in the compose file"
		case stacks.PinLocal:
			row.State, row.Note = ImageLocal, "Not from a registry. Pinned to the local image ID."
		default:
			row.State = ImageNew
			if updating && row.Current != "" && row.Current == row.Digest {
				row.State, row.Note = ImagePinned, "Already up to date"
			}
		}
		if row.Note == "" && (row.State == ImageNew || row.State == ImagePinned) && untagged(sp.ImageAsWritten) {
			row.Note = "No tag, so it can move."
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Service < rows[j].Service })
	return rows
}

// resourceChanges lists the networks, secrets and configs a deploy would add, change or remove,
// and the problems that stop a deploy for them: an external object that does not exist.
func resourceChanges(stack Stack, plan convert.Plan, objs []stacks.ObjectContent, live liveStack) ([]ObjectChange, stacks.Problems) {
	var out []ObjectChange
	var problems stacks.Problems

	for _, n := range plan.Networks {
		if n.External {
			out = append(out, ObjectChange{Change: ChangeExternal, Kind: "network", Name: n.Name, Detail: detailExternal})
			found, swarmScope := externalNetwork(live.AllNetworks, n.Name)
			if !found || !swarmScope {
				problems = append(problems, externalProblem("network", n.Name, "networks."+n.Key, found))
			}
			continue
		}
		row, ok := findNetwork(live.Networks, n.Name)
		switch {
		case !ok:
			out = append(out, ObjectChange{Change: stacks.ChangeAdded, Kind: "network", Name: n.Name})
		case row.Labels[stacks.LabelStackID] == stack.ID && networkDiffers(n, row):
			out = append(out, ObjectChange{Change: stacks.ChangeChanged, Kind: "network", Name: n.Name, Detail: detailNetwork})
		}
	}

	used := map[string]bool{}
	for _, o := range objs {
		kind := string(o.Kind)
		used[kind+"/"+o.Name] = true
		if o.External {
			out = append(out, ObjectChange{Change: ChangeExternal, Kind: kind, Name: o.ExternalName, Detail: detailExternal})
			if _, found := findObject(live.Objects, o.Kind, o.ExternalName); !found {
				problems = append(problems, externalProblem(kind, o.ExternalName, section(o.Kind)+"."+o.Name, false))
			}
			continue
		}
		full, _ := stacks.ObjectName(stack.Name, o.Name, o.Content)
		if row, ok := findObject(live.Objects, o.Kind, full); ok && row.Labels[stacks.LabelStackID] == stack.ID {
			continue // the same content: the object is reused
		}
		if ownedVersion(live.Objects, o.Kind, stack.ID, o.Name) {
			out = append(out, ObjectChange{Change: stacks.ChangeChanged, Kind: kind, Name: o.Name, Detail: detailNewVersion})
			continue
		}
		out = append(out, ObjectChange{Change: stacks.ChangeAdded, Kind: kind, Name: o.Name})
	}
	removed := map[string]bool{}
	for _, r := range live.Objects {
		name := r.Labels[stacks.LabelObject]
		key := string(r.Kind) + "/" + name
		if r.Labels[stacks.LabelStackID] != stack.ID || name == "" || used[key] || removed[key] {
			continue
		}
		removed[key] = true
		out = append(out, ObjectChange{Change: stacks.ChangeRemoved, Kind: string(r.Kind), Name: name, Detail: detailRemoved})
	}

	kindRank := map[string]int{"network": 0, string(stacks.KindSecret): 1, string(stacks.KindConfig): 2}
	slices.SortStableFunc(out, func(a, b ObjectChange) int {
		return cmp.Or(cmp.Compare(kindRank[a.Kind], kindRank[b.Kind]), cmp.Compare(a.Name, b.Name))
	})
	return out, problems
}

func section(k stacks.ObjectKind) string {
	if k == stacks.KindSecret {
		return "secrets"
	}
	return "configs"
}

// ownedVersion reports whether the stack owns a secret or config of this kind made for the
// compose name, under whatever hash.
func ownedVersion(live []objectRow, kind stacks.ObjectKind, stackID, name string) bool {
	for _, r := range live {
		if r.Kind == kind && r.Labels[stacks.LabelStackID] == stackID && r.Labels[stacks.LabelObject] == name {
			return true
		}
	}
	return false
}

// networkDiffers reports whether a network the stack owns is not what the file asks for. Only what
// the engine reports in a list is compared: the driver, the flags and, when the file gives pools,
// their subnets.
func networkDiffers(plan convert.NetworkPlan, row dockerx.NetworkRow) bool {
	if plan.Driver != "" && plan.Driver != row.Driver {
		return true
	}
	if plan.Internal != row.Internal || plan.Attachable != row.Attachable {
		return true
	}
	if len(plan.Pools) == 0 {
		return false
	}
	want := make([]string, 0, len(plan.Pools))
	for _, p := range plan.Pools {
		if p.Subnet != "" {
			want = append(want, p.Subnet)
		}
	}
	got := slices.Clone(row.Subnets)
	slices.Sort(want)
	slices.Sort(got)
	return !slices.Equal(want, got)
}

// externalProblem is the blocking problem for an external network, secret or config the swarm does
// not have (or, for a network, has only on one node).
func externalProblem(kind, name, key string, found bool) stacks.Problem {
	msg := fmt.Sprintf("The external %s “%s” doesn't exist on this environment. Create it first, or remove external from the file.", kind, name)
	if found {
		msg = fmt.Sprintf("The external network “%s” isn't a swarm network. Use an overlay network that exists on the swarm.", name)
	}
	return stacks.Problem{
		Severity: stacks.SeverityError, Code: CodeExternalMissingProblem, Message: msg, File: stacks.ComposeFile, Key: key,
		Line: 1, Col: 1, EndLine: 1, EndCol: 2,
	}
}

// ObjectErrorDetail is the sentence for a secret or config that cannot be resolved.
func ObjectErrorDetail(err error) string {
	reason := "it cannot be read"
	switch {
	case errors.Is(err, stacks.ErrObjectSource):
		reason = "it needs exactly one of file, environment or content"
	case errors.Is(err, stacks.ErrObjectFileMissing):
		reason = "its file is not a file of the stack"
	case errors.Is(err, stacks.ErrObjectVarMissing):
		reason = "its variable is not in the stack's table"
	case errors.Is(err, stacks.ErrObjectEmpty):
		reason = "it has no content"
	case errors.Is(err, stacks.ErrObjectTooLarge):
		reason = "it is over the size the swarm takes"
	case errors.Is(err, stacks.ErrObjectUnsupported):
		reason = "it sets a driver, which a stack deploy does not apply"
	}
	var oe *stacks.ObjectError
	if errors.As(err, &oe) {
		return "The " + string(oe.Kind) + " " + oe.Name + " cannot be created: " + reason + "."
	}
	return "A secret or config cannot be created: " + reason + "."
}

// ConvertDetail is the sentence for a compose file the converter cannot turn into swarm objects.
func ConvertDetail(err error) string {
	var nc *convert.NotConvertedError
	switch {
	case errors.As(err, &nc):
		return "Service " + nc.Service + " uses " + strings.Join(nc.Fields, ", ") + ", which this version of Bayplan cannot deploy yet."
	case errors.Is(err, convert.ErrNoImage):
		return "A service has no image. Bayplan deploys images, it does not build them."
	}
	return strings.TrimPrefix(err.Error(), "convert: ")
}

// objectErrorProblem is the blocking problem for a secret or config that cannot be resolved.
func objectErrorProblem(err error) stacks.Problem {
	return stacks.Problem{
		Severity: stacks.SeverityError, Code: CodeObjectInvalidProblem, Message: ObjectErrorDetail(err), File: stacks.ComposeFile,
		Line: 1, Col: 1, EndLine: 1, EndCol: 2,
	}
}

// ConvertProblem is the blocking problem for a compose file the converter cannot turn into swarm
// objects.
func ConvertProblem(err error) stacks.Problem {
	return stacks.Problem{
		Severity: stacks.SeverityError, Code: CodeConvertProblem, Message: ConvertDetail(err), File: stacks.ComposeFile,
		Line: 1, Col: 1, EndLine: 1, EndCol: 2,
	}
}
