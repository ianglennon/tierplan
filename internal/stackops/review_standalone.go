// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// CodeExternalFileObject is the code of the blocking problem for an external secret or config on a
// standalone host. The check reports it at the reference; the review repeats it only when the check did not.
const CodeExternalFileObject = stacks.CodeExternalFileObject

// reviewStandalone fills rv with what a deploy of the saved draft would do to a standalone host (STK-06):
// the host's containers under the stack's Compose project are read, the images are pinned the way a swarm
// deploy pins them (the same table, STK-09), the project is prepared as the deploy prepares it, and each
// service is compared with the containers that run it (compareStandalone): a service with no container is
// added; one whose containers carry another config hash, or whose running configuration is not what the file
// says, is changed, with the field-level difference of the prepared service against the service rebuilt from
// the container's inspect document; a service of the host's project that the file no longer declares is
// removed. It never writes.
//
// res is the check of the draft, which loaded.
func (d Deps) reviewStandalone(ctx context.Context, in ReviewInput, res stacks.CheckResult, rv *Review) error {
	files, vars := stacks.NewFileSet(in.Copy.Files), varTable(in.Copy.Vars)
	project := res.Loaded.Project
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, project, files, vars)
	if err != nil {
		rv.Problems = append(rv.Problems, objectErrorProblem(err)).Sorted()
		return nil
	}
	node, err := d.StandaloneNode(ctx, in.EnvID)
	if err != nil {
		return err
	}
	live, err := d.readStandalone(ctx, in.EnvID, node, in.Stack.Name)
	if err != nil {
		return err
	}
	if foreignLive(live, in.Stack.ID) {
		rv.Problems = append(rv.Problems, stacks.Problem{
			Severity: stacks.SeverityError, Code: CodeNameClash, Message: NameClashMessage(in.Stack.Name), File: stacks.ComposeFile,
			Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		})
	}
	// The compose rules judge the file; there is no converted swarm spec to judge on a standalone host.
	pol, err := d.Policy(ctx, in.Caller, in.EnvID, in.Copy, convert.Plan{})
	if err != nil {
		return err
	}
	rv.Problems = append(rv.Problems, pol.Problems...)
	if pol.Kinds != nil {
		rv.NeedsAdmin = pol.Kinds
	}

	items := standaloneItems(project)
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
	pinned := d.resolvePins(ctx, in.EnvID, items, standaloneLivePins(live.Containers), stored, update, d.nodeReader(in.EnvID, node))
	rv.Problems = append(rv.Problems, placeProblems(files, pinned.Problems)...)
	if pinned.Code != "" && len(pinned.Problems) == 0 {
		// A failure that is not about one image (the host is not reachable, the agent refused).
		rv.Problems = append(rv.Problems, stacks.Problem{
			Severity: stacks.SeverityError, Code: pinned.Code, Message: CodeMessage(pinned.Code), File: stacks.ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		})
	}
	rv.Pins = pinned.Refs()
	rv.Images = imageRows(items, pinned, standaloneCurrent(live.Containers), update != nil)
	for _, r := range rv.Images {
		switch r.State {
		case ImageNew:
			rv.Summary.ImagesNew++
		case ImagePinned, ImageAsWritten, ImageLocal:
			rv.Summary.ImagesPinned++
		}
	}

	if err := stacks.PrepareStandalone(project, in.Stack.ID, standalonePins(items, pinned), objs); err != nil {
		if p := prepareProblem(err); !slices.ContainsFunc(rv.Problems, func(have stacks.Problem) bool { return have.Code == p.Code }) {
			rv.Problems = append(rv.Problems, p)
		}
		rv.Problems = rv.Problems.Sorted()
		return nil
	}
	diff, err := diffStandalone(project, live.Containers)
	if err != nil {
		return err
	}
	cmp, err := d.compareOnNode(ctx, in, node, project, live.Containers, diff)
	if err != nil {
		return err
	}
	for _, c := range cmp.Changes {
		rv.Services = append(rv.Services, c.Row)
		bump(&rv.Summary.Services, c.Row.Change)
	}
	for _, name := range diff.Removed {
		rv.Services = append(rv.Services, stacks.ServiceChange{Name: name, Short: name, Change: stacks.ChangeRemoved})
		rv.Summary.Services.Removed++
	}
	for _, name := range missingNetworks(project, live.Networks) {
		rv.Objects = append(rv.Objects, ObjectChange{Change: stacks.ChangeAdded, Kind: "network", Name: name})
		rv.Summary.Networks.Added++
	}
	for _, name := range missingVolumes(project, live.Volumes) {
		rv.Objects = append(rv.Objects, ObjectChange{Change: stacks.ChangeAdded, Kind: "volume", Name: name})
		rv.Summary.Volumes.Added++
	}
	rv.Problems = rv.Problems.Sorted()
	return nil
}

// prepareProblem is the blocking problem for a project the Compose SDK cannot be given.
func prepareProblem(err error) stacks.Problem {
	msg := "The stack can't be prepared for a standalone host."
	code := CodeConvertProblem
	if errors.Is(err, stacks.ErrUseAPISocket) {
		code = stacks.CodeUseAPISocket
		msg = "use_api_socket can't be used on a standalone host: it would mount the host's Docker socket into the container. Mount the socket as a bind volume if the service needs it; that needs Admin."
	}
	if errors.Is(err, stacks.ErrUnsupportedKey) {
		code = stacks.CodeStandaloneUnsupported
		msg = "The stack uses a provider, a model or a lifecycle hook, which a standalone deploy doesn't run."
	}
	if errors.Is(err, stacks.ErrExternalFileObject) {
		code = CodeExternalFileObject
		msg = "A standalone host can't use an external secret or config: it writes the content of each into the container, and an external one has none. Declare it with a file, an environment variable or content instead."
	}
	return stacks.Problem{
		Severity: stacks.SeverityError, Code: code, Message: msg, File: stacks.ComposeFile, Line: 1, Col: 1, EndLine: 1, EndCol: 2,
	}
}

// StoredRevision is a revision of a stack as it was deployed: its number, its files and variables, and the
// image each service ran with, by service name.
type StoredRevision struct {
	Seq  int64
	Copy stacks.WorkingCopy
	Pins map[string]string
}

// RevisionStore reads the revisions a standalone review prepares again to find what the engine does not
// record about a running container (its depends_on, secrets, configs, profiles and pull policy).
type RevisionStore interface {
	// LatestRevisions are the stack's newest good revision and its newest revision whatever became of it
	// (one entry when they are the same), the good one first.
	LatestRevisions(ctx context.Context, stackID string) ([]StoredRevision, error)
}

var _ RevisionStore = Repo{}

// LatestRevisions is the stack's newest good revision and its newest revision of any outcome, with their
// files, variables and pins.
func (r Repo) LatestRevisions(ctx context.Context, stackID string) ([]StoredRevision, error) {
	var out []StoredRevision
	add := func(snap Snapshot) error {
		if slices.ContainsFunc(out, func(s StoredRevision) bool { return s.Seq == snap.Seq }) {
			return nil
		}
		pins, err := r.RevisionPins(ctx, snap.ID)
		if err != nil {
			return err
		}
		out = append(out, StoredRevision{Seq: snap.Seq, Copy: snap.Copy, Pins: pins})
		return nil
	}
	good, ok, err := r.LatestGoodRevision(ctx, stackID)
	if err != nil {
		return nil, err
	}
	if ok {
		if err := add(good); err != nil {
			return nil, err
		}
	}
	latest, err := r.Store.RQ.LatestAttemptSeq(ctx, stackID)
	if err != nil {
		return nil, fmt.Errorf("read the newest revision: %w", err)
	}
	if latest > 0 {
		snap, err := r.Revision(ctx, stackID, latest)
		if err != nil {
			return nil, err
		}
		if err := add(snap); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// standaloneChange is a service the review lists as added or changed.
type standaloneChange struct {
	Name string
	// Added is a service with no container. HashDiffers says a container of it carries another config hash
	// than the service has now, so the SDK recreates it. Forced says every hash is the one the service has
	// now and the running configuration is not what the file says (a change made by hand on the host): the
	// SDK compares labels only and would leave the container as it is, so the deploy forces it.
	Added, HashDiffers, Forced bool
	// Row is the service's row of the review.
	Row stacks.ServiceChange
}

// standaloneComparison is what a standalone deploy would do to the services, read once: the added and
// changed services in name order.
type standaloneComparison struct {
	Changes []standaloneChange
}

// preparedRevisions are the services of the stored revisions as a deploy of each would have prepared them,
// newest good revision first. A revision that no longer loads or prepares is left out.
func (d Deps) preparedRevisions(ctx context.Context, in ReviewInput) ([]map[string]types.ServiceConfig, []stacks.WorkingCopy) {
	if in.Revisions == nil {
		return nil, nil
	}
	revs, err := in.Revisions.LatestRevisions(ctx, in.Stack.ID)
	if err != nil {
		d.log().Warn("could not read the revisions of a standalone stack", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		return nil, nil
	}
	var services []map[string]types.ServiceConfig
	var copies []stacks.WorkingCopy
	for _, rev := range revs {
		checked := stacks.Check(ctx, stacks.CheckInput{Name: in.Stack.Name, Target: stacks.TargetStandalone, Copy: rev.Copy})
		if checked.Loaded == nil {
			continue
		}
		copies = append(copies, rev.Copy)
		p := checked.Loaded.Project
		objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, p, stacks.NewFileSet(rev.Copy.Files), varTable(rev.Copy.Vars))
		if err != nil {
			continue
		}
		pins := map[string]string{}
		for name, svc := range p.Services {
			pins[name] = svc.Image
			if ref := rev.Pins[name]; ref != "" {
				pins[name] = ref
			}
		}
		if err := stacks.PrepareStandalone(p, in.Stack.ID, pins, objs); err != nil {
			continue
		}
		services = append(services, p.Services)
	}
	return services, copies
}

// compareOnNode compares the services with the host's containers through the engine of the node. The
// stored revisions are prepared again only when a hash differs, because that is the only case that needs
// them (recordedFor), and the secrets of those revisions are hidden in what is shown.
func (d Deps) compareOnNode(ctx context.Context, in ReviewInput, node string, project *types.Project, containers []dockerx.ComposeContainer, diff standaloneDiff) (standaloneComparison, error) {
	m := reviewMasker(in)
	var stored []map[string]types.ServiceConfig
	if len(diff.Changed) > 0 {
		var copies []stacks.WorkingCopy
		stored, copies = d.preparedRevisions(ctx, in)
		if len(copies) > 0 {
			vars, files := slices.Clone(in.Copy.Vars), slices.Clone(in.Copy.Files)
			if in.Previous != nil {
				vars, files = append(vars, in.Previous.Vars...), append(files, in.Previous.Files...)
			}
			for _, c := range copies {
				vars, files = append(vars, c.Vars...), append(files, c.Files...)
			}
			m = stacks.NewMasker(vars, files)
		}
	}
	var out standaloneComparison
	err := d.Aggregator.OnNode(ctx, in.EnvID, node, func(c *dockerx.Client) error {
		var e error
		out, e = compareStandalone(ctx, c, project, containers, stored, m)
		return e
	})
	return out, err
}

// hostReader reads the inspect documents of containers and their images from one host, each image once.
type hostReader struct {
	host   *dockerx.Client
	images map[string]*image.InspectResponse
}

// read is the inspect documents of a container and of the image it was created from. ok is false when the
// container or its image is gone, which leaves the service to be judged by its config hash alone.
func (h *hostReader) read(ctx context.Context, c dockerx.ComposeContainer) (container.InspectResponse, image.InspectResponse, bool, error) {
	var insp container.InspectResponse
	_, raw, err := h.host.ContainerDetail(ctx, c.ID)
	if err != nil {
		if dockerx.IsNotFound(err) {
			return insp, image.InspectResponse{}, false, nil
		}
		return insp, image.InspectResponse{}, false, err
	}
	if err := json.Unmarshal(raw, &insp); err != nil {
		return insp, image.InspectResponse{}, false, fmt.Errorf("read the inspect document of container %s: %w", c.Name, err)
	}
	if img, seen := h.images[insp.Image]; seen {
		if img == nil {
			return insp, image.InspectResponse{}, false, nil
		}
		return insp, *img, true, nil
	}
	_, imgRaw, err := h.host.ImageDetail(ctx, insp.Image)
	if err != nil {
		if dockerx.IsNotFound(err) {
			h.images[insp.Image] = nil
			return insp, image.InspectResponse{}, false, nil
		}
		return insp, image.InspectResponse{}, false, err
	}
	var img image.InspectResponse
	if err := json.Unmarshal(imgRaw, &img); err != nil {
		return insp, img, false, fmt.Errorf("read the inspect document of image %s: %w", insp.Image, err)
	}
	h.images[insp.Image] = &img
	return insp, img, true, nil
}

// compareStandalone compares every service of the prepared project with the containers of its Compose
// project on the host, in name order. A service with no container is added. For a service with containers,
// the desired service (references to other services resolved to their containers, as the SDK hashes it) and
// the service rebuilt from the container's inspect document both go through NormalizeComposeService; the
// service is changed when a container carries another config hash than the service has now, or when the two
// differ, and it is Forced when the hashes match and the engine's configuration differs, a difference the
// SDK's label comparison cannot see. The first container that differs supplies the current side. What the
// engine does not record is read from the stored revision whose prepared hash is the container's label
// (stored, newest good revision first); with none, it is taken to be the file's own, because a matching hash
// says the file and the container agree on all that the hash covers, and for a hash that differs there is no
// source of what the container was made from: the review does not invent a difference it cannot know. It
// reads only.
func compareStandalone(ctx context.Context, host *dockerx.Client, project *types.Project, containers []dockerx.ComposeContainer, stored []map[string]types.ServiceConfig, m stacks.Masker) (standaloneComparison, error) {
	byService := containersByService(containers)
	ids := containerIDs(byService)
	peers := map[string]string{}
	for svc, list := range byService {
		for _, c := range list {
			peers[c.ID] = svc
		}
	}
	rc := stacks.RunningContextOf(project, peers)
	reader := &hostReader{host: host, images: map[string]*image.InspectResponse{}}
	var out standaloneComparison
	names := make([]string, 0, len(project.Services))
	for n := range project.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := project.Services[name]
		have := byService[name]
		if len(have) == 0 {
			next := stacks.NormalizeComposeService(svc, stacks.ImageDefaults{}, rc)
			rows := stacks.DiffComposeServices(map[string]types.ServiceConfig{name: next}, nil, m)
			out.Changes = append(out.Changes, standaloneChange{Name: name, Added: true, Row: rows[0]})
			continue
		}
		resolved := stacks.ResolveServiceRefs(svc, ids)
		hash, err := dockerx.ServiceHash(resolved)
		if err != nil {
			return out, fmt.Errorf("hash service %s: %w", name, err)
		}
		type pair struct{ des, run types.ServiceConfig }
		var first, differing *pair
		hashDiffers, forced := false, false
		for _, c := range have {
			differs := c.ConfigHash != hash
			hashDiffers = hashDiffers || differs
			insp, img, ok, err := reader.read(ctx, c)
			if err != nil {
				return out, err
			}
			if !ok {
				continue
			}
			rec := &resolved
			if differs {
				if found := recordedFor(stored, c.ConfigHash, ids); found != nil {
					rec = found
				}
			}
			run := stacks.RunningService(insp, stacks.ImageDefaultsOf(img), rec, rc)
			scale := len(have)
			run.Scale = &scale
			imgDef := stacks.ImageDefaultsOf(img)
			des := stacks.NormalizeComposeService(resolved, imgDef, rc)
			runN := stacks.AlignDaemonDefaults(des, stacks.NormalizeComposeService(run, imgDef, rc))
			p := &pair{des: des, run: runN}
			if !differs && !sameEngine(des, runN) {
				forced = true
			}
			if first == nil {
				first = p
			}
			if differing == nil && (differs || !stacks.SameComposeService(des, runN)) {
				differing = p
			}
		}
		var row stacks.ServiceChange
		switch {
		case differing != nil:
			row = stacks.ChangedComposeService(name, differing.run, differing.des, m)
		case hashDiffers && first != nil:
			row = stacks.ChangedComposeService(name, first.run, first.des, m)
		case hashDiffers:
			row = stacks.ServiceChange{Name: name, Short: name, Change: stacks.ChangeChanged}
		default:
			continue
		}
		out.Changes = append(out.Changes, standaloneChange{Name: name, HashDiffers: hashDiffers, Forced: forced, Row: row})
	}
	return out, nil
}

// recordedFor is the prepared service of a stored revision whose config hash, with the references to other
// services resolved to the host's containers, is the label of a container: the service that container was
// created from. Nil when no stored revision made it.
func recordedFor(stored []map[string]types.ServiceConfig, label string, ids map[string][]string) *types.ServiceConfig {
	for _, services := range stored {
		for _, name := range sortedKeys(services) {
			svc := stacks.ResolveServiceRefs(services[name], ids)
			if h, err := dockerx.ServiceHash(svc); err == nil && h == label {
				return &svc
			}
		}
	}
	return nil
}

// sameEngine compares two normalised services in what the engine records about a container, leaving out
// what it does not (depends_on, secrets, configs, profiles, pull_policy and scale: the SDK sees to those
// itself, and a difference there is no reason to recreate a container).
func sameEngine(a, b types.ServiceConfig) bool {
	for _, s := range []*types.ServiceConfig{&a, &b} {
		s.DependsOn, s.Secrets, s.Configs, s.Profiles, s.PullPolicy, s.Scale = nil, nil, nil, nil, "", nil
	}
	return stacks.SameComposeService(a, b)
}
