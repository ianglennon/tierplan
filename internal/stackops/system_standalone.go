// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// SystemDeployInput is TierPlan's own project to bring up on a standalone host: the ingress proxy's (D-29). It is not
// a stack: nothing of it is stored as one.
type SystemDeployInput struct {
	// EnvID is the environment and NodeID the node of its one engine, by the ID the write is routed by.
	EnvID, NodeID string
	// Project is the typed project, named ingress.ProjectName. Its Environment carries the values the services read,
	// among them the join token, which no text of the job shows.
	Project *types.Project
	// Network is the key, in Project.Networks, of the network that is given a subnet; empty for none.
	Network string
	// Prefix is the size of the block the network is given when the host holds none of it yet; zero is a /24.
	Prefix int
	// Subnet is a block to give the network when the host holds none of it yet, already checked by the caller; empty
	// has the lowest free block of the host's pools chosen.
	Subnet string
	// NoWait has the up return once the containers are started: the proxy cannot be healthy before it has enrolled.
	NoWait bool
	// OnDone, when set, is called once with the result before the job's last item is finished.
	OnDone func(DeployResult)
}

// SystemItemSpecs are the items a system deploy job starts with, in run order. They have the IDs of a standalone
// deploy's, so the failure and progress code of the two is one; only the words differ.
func SystemItemSpecs() []jobs.ItemSpec {
	return []jobs.ItemSpec{
		{ID: ItemCheck, Label: "Check the host", Kind: KindStep},
		{ID: ItemLookup, Label: "Look up the proxy image", Kind: KindStep},
		{ID: ItemNetworks, Label: "Network", Kind: KindGroup},
		{ID: ItemVolumes, Label: "Volume", Kind: KindGroup},
		{ID: ItemServices, Label: "Start the proxy", Kind: KindStep},
		{ID: ItemCleanup, Label: "Remove containers no longer in the project", Kind: KindStep},
	}
}

// systemOwnership is the rule that says what is the system project's own once the host is known to hold nothing of
// its name that is not: everything under its Compose project, as for a project found on the host.
func systemOwnership() Stack {
	return Stack{Name: ingress.ProjectName, Target: stacks.TargetStandalone, Discovered: true}
}

// systemHost is what a standalone host holds under the system project's names.
type systemHost struct {
	standaloneLive
	// Named are the networks of the reserved name and Volume the volume of the reserved name, whether or not they carry
	// the project's label: an object of the name that is not TierPlan's is what the check refuses.
	Named  []dockerx.NetworkRow
	Volume *dockerx.VolumeDetail
}

// readSystemHost reads the host under the project and under the reserved names, from one node. It is a read.
func (d Deps) readSystemHost(ctx context.Context, envID, nodeID string) (systemHost, error) {
	var out systemHost
	err := d.Aggregator.OnNode(ctx, envID, nodeID, func(c *dockerx.Client) error {
		var e error
		if out.Containers, e = c.ComposeContainers(ctx, ingress.ProjectName); e != nil {
			return e
		}
		all, e := c.Networks(ctx)
		if e != nil {
			return e
		}
		for _, n := range all {
			if n.Name == ingress.NetworkName {
				out.Named = append(out.Named, n)
			}
		}
		out.Networks = slices.Clone(out.Named)
		if out.Volumes, e = c.VolumesWithLabel(ctx, stacks.LabelComposeProject, ingress.ProjectName); e != nil {
			return e
		}
		det, _, e := c.VolumeDetail(ctx, ingress.VolumeName)
		switch {
		case e == nil:
			out.Volume = &det
		case dockerx.IsNotFound(e):
		default:
			return e
		}
		return nil
	})
	return out, err
}

// foreignSystem reports whether the host holds, under the project or the reserved names, an object that is not
// TierPlan's: a container of the project that is not the proxy, a network or the cache volume of the reserved name
// without the component label (D-29).
func foreignSystem(h systemHost) bool {
	for _, c := range h.Containers {
		if !ingress.OwnsProxyContainer(c.Labels) {
			return true
		}
	}
	for _, n := range h.Named {
		if !ingress.OwnsNetwork(n.Name, n.Labels) {
			return true
		}
	}
	return h.Volume != nil && !ingress.OwnsVolume(h.Volume.Name, h.Volume.Labels)
}

// systemMasker hides every value of the project's environment in the job's text: the token reaches the engine in it,
// and an engine's error can echo what it was sent (D-08).
func systemMasker(p *types.Project) stacks.Masker {
	if p == nil {
		return stacks.NewMasker(nil, nil)
	}
	vars := make([]stacks.Var, 0, len(p.Environment))
	for name, value := range p.Environment {
		vars = append(vars, stacks.Var{Name: name, Value: value, Secret: true})
	}
	return stacks.NewMasker(vars, nil)
}

// prepareSystem puts on every service of the system project what the Compose SDK and the lookups of a later deploy
// read: the image as written, and the labels the SDK's own loader would have set. Nothing is derived from a stack, an ID
// or the content of a secret: the project belongs to no stack (Pitfall 9).
func prepareSystem(p *types.Project) {
	if p.Environment == nil {
		p.Environment = types.Mapping{}
	}
	for name, svc := range p.Services {
		labels := types.Labels{}
		for k, v := range svc.CustomLabels {
			labels[k] = v
		}
		if svc.Image != "" {
			labels[stacks.LabelStackImage] = svc.Image
		}
		labels[stacks.LabelComposeProject] = p.Name
		labels[stacks.LabelComposeService] = name
		labels[stacks.LabelComposeOneoff] = "False"
		labels[stacks.LabelComposeWorkingDir] = stacks.NeutralWorkingDir
		labels[stacks.LabelComposeConfigFiles] = stacks.NeutralConfigFiles
		svc.CustomLabels = labels
		p.Services[name] = svc
	}
}

// pinSystem sends each service the image its pin names.
func pinSystem(p *types.Project, pins map[string]string) error {
	for name, svc := range p.Services {
		if svc.Image == "" {
			continue
		}
		ref := pins[name]
		if ref == "" {
			return stacks.ErrPinMissing
		}
		svc.Image = ref
		p.Services[name] = svc
	}
	return nil
}

// defaultSystemPrefix is the size of the block the proxy's network is given when nothing asks for another.
const defaultSystemPrefix = 24

// systemSubnet decides the subnet the project's network is created with, and holds the allocation lock for as long as
// the caller keeps release: the network the host holds already keeps the subnet it has (the SDK would recreate it
// otherwise); a new one takes the requested block, else the lowest free block of the host's pools (D-29). code is a
// refusal of the check, err a failure to read the host, and sub empty when the network is left as the project has it.
func (d Deps) systemSubnet(ctx context.Context, in SystemDeployInput, host systemHost) (sub string, release func(), code, msg string, err error) {
	release = func() {}
	key := in.Network
	n, ok := in.Project.Networks[key]
	if !ok {
		return "", release, CodeStackInvalid, "", nil
	}
	for _, row := range host.Named {
		if ingress.OwnsNetwork(row.Name, row.Labels) && len(row.Subnets) > 0 {
			return row.Subnets[0], release, "", "", nil
		}
	}
	if len(host.Named) > 0 {
		return "", release, "", "", nil // the host holds the network and says no subnet: leave it
	}
	if d.AddressSpaces == nil {
		if in.Subnet != "" {
			return in.Subnet, release, "", "", nil
		}
		return "", release, CodeNoFreeSubnet, CodeMessage(CodeNoFreeSubnet), nil
	}
	unlock := d.AddressSpaces.LockAllocation(in.EnvID)
	release = sync.OnceFunc(unlock)
	if in.Subnet != "" {
		return in.Subnet, release, "", "", nil
	}
	prefix := in.Prefix
	if prefix <= 0 {
		prefix = defaultSystemPrefix
	}
	inputs, ierr := d.AddressSpaces.AllocationInputs(ctx, in.EnvID, true)
	if ierr == nil && len(inputs.Pools) == 0 {
		return "", release, CodeNoFreeSubnet, NoFreeSubnetMessage(n.Name, prefix, "the host's address pools"), nil
	}
	if ierr != nil {
		release()
		return "", func() {}, "", "", ierr
	}
	block, _, lerr := netspace.LowestFree(inputs.Pools, inputs.Occupied, inputs.Avoid, prefix)
	if errors.Is(lerr, netspace.ErrNoFreeBlock) {
		return "", release, CodeNoFreeSubnet, NoFreeSubnetMessage(n.Name, prefix, envs.PoolList(inputs.Pools)), nil
	}
	if lerr != nil {
		release()
		return "", func() {}, "", "", lerr
	}
	return block.String(), release, "", "", nil
}

// DeploySystem brings the system project up on a standalone host with the Compose SDK, through the tail the
// stack deploy uses: the live read, the pins, the pre-pull, the SDK's up through WriteOnNode, the rows of the job and
// the cleanup when the up fails (D-29). Its head is its own: no text check, no objects from files, no host-access
// policy and no hostname claims. Ownership is the component label and the reserved names, so an object of the project
// or of a reserved name that is not TierPlan's fails the check before anything is written.
//
// It writes no stack, revision, pin, subnet record or claim, and no object it makes carries a stack's ID (PROH-25-02).
// The last item is finished only after in.OnDone returns.
func (d Deps) DeploySystem(ctx context.Context, in SystemDeployInput, r *jobs.Reporter) (res DeployResult) {
	j := newStandaloneJob(r, systemMasker(in.Project), &res)
	fail, failWith, failErr := j.fail, j.failWith, j.failErr
	defer func() {
		if in.OnDone != nil {
			in.OnDone(res)
		}
		j.finishLast()
	}()

	// 1. Check: the project, the host, and what it holds under the reserved names.
	r.Update(ItemCheck, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	if in.Project == nil || in.Project.Name != ingress.ProjectName {
		return fail(ItemCheck, CodeStackInvalid, "")
	}
	project := in.Project
	if err := d.writable(ctx, in.EnvID, in.NodeID); err != nil {
		if errors.Is(err, envs.ErrHostReadOnly) {
			return fail(ItemCheck, CodeHostReadOnly, "")
		}
		d.log().Warn("a standalone host could not be written to before the proxy's deploy", "environment", in.EnvID, "error", err)
		return failErr(ItemCheck, codeOf(ctx, err), "", err)
	}
	host, err := d.readSystemHost(ctx, in.EnvID, in.NodeID)
	if err != nil {
		d.log().Warn("could not read a standalone host before the proxy's deploy", "environment", in.EnvID, "error", err)
		return failErr(ItemCheck, codeOf(ctx, err), "", err)
	}
	if foreignSystem(host) {
		return fail(ItemCheck, CodeNameClash, "")
	}
	live := host.standaloneLive
	res.Total = len(project.Services)
	// The subnet of the network is decided under the allocation lock, which is held until the SDK's up has returned.
	unlock := sync.OnceFunc(func() {})
	defer func() { unlock() }()
	if in.Network != "" {
		sub, release, code, msg, serr := d.systemSubnet(ctx, in, host)
		unlock = release
		switch {
		case serr != nil:
			d.log().Warn("could not plan the subnet of the proxy's network", "environment", in.EnvID, "error", serr)
			return failErr(ItemCheck, codeOf(ctx, serr), "", serr)
		case code != "":
			return failWith(ItemCheck, code, in.Network, msg)
		case sub != "":
			n := project.Networks[in.Network]
			n.Ipam.Config = []*types.IPAMPool{{Subnet: sub}}
			project.Networks[in.Network] = n
		}
	}
	r.Finish(ItemCheck, jobs.ItemDone, "", "", nil)

	// 2. Look up the image: what a container runs and was deployed from is kept, a new or changed text is looked up.
	r.Update(ItemLookup, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	items := standaloneItems(project)
	pinned := d.resolvePins(ctx, in.EnvID, items, standaloneLivePins(live.Containers), nil, nil, d.nodeReader(in.EnvID, in.NodeID))
	res.Pins, res.Problems = pinned.Pins, pinned.Problems
	if pinned.Blocked() {
		msg := ""
		if isImageFailure(pinned.Code) {
			if first, ok := firstError(pinned.Problems); ok {
				msg = first.Message
			}
		}
		return failWith(ItemLookup, pinned.Code, pinned.FailedService, msg)
	}
	pins := standalonePins(items, pinned)
	prepareSystem(project)
	if err := pinSystem(project, pins); err != nil {
		d.log().Warn("the proxy project could not be prepared for a deploy", "environment", in.EnvID, "error", err)
		return fail(ItemLookup, CodeStackInvalid, "")
	}
	diff, err := diffStandalone(project, live.Containers)
	if err != nil {
		d.log().Warn("could not hash the proxy project's services", "environment", in.EnvID, "error", err)
		return fail(ItemLookup, CodeStackInvalid, "")
	}
	// The SDK compares labels, and a container changed by hand keeps them: those the comparison finds changed are recreated.
	if len(live.Containers) > 0 {
		err = d.Aggregator.OnNode(ctx, in.EnvID, in.NodeID, func(c *dockerx.Client) error {
			cmp, e := compareStandalone(ctx, c, project, live.Containers, nil, stacks.NewMasker(nil, nil))
			diff.Forced = cmp.forced()
			for _, row := range cmp.Carried {
				if row.Change == stacks.ChangeRecreated {
					diff.Carried = append(diff.Carried, row.Name)
				}
			}
			return e
		})
		if err != nil {
			d.log().Warn("could not compare the proxy project with its host before a deploy", "environment", in.EnvID, "error", err)
			return failErr(ItemLookup, codeOf(ctx, err), "", err)
		}
	}

	own := systemOwnership()
	return d.standaloneTail(ctx, j, standaloneTail{
		EnvID: in.EnvID, NodeID: in.NodeID, Name: ingress.ProjectName,
		Project: project, Live: live, Diff: diff, Pinned: pinned, Pins: pins, NoWait: in.NoWait,
		Cleanup: func(created []string) {
			d.cleanupStandalone(ctx, in.EnvID, in.NodeID, own, live.Containers, created, &res, j.prog)
		},
		AfterUp: func(error) { unlock() },
		Mask:    systemMasker(project).Mask,
	})
}

// DownSystem runs the Compose SDK's down for the system project on the node, and nothing else: its containers and
// the network the SDK made go, its volume stays (D-20). The write goes through WriteOnNode, so a read-only host is refused.
func (d Deps) DownSystem(ctx context.Context, envID, nodeID string) error {
	return d.Aggregator.WriteOnNode(ctx, envID, nodeID, func(c *dockerx.Client) error {
		return d.downProject(ctx, c, ingress.ProjectName)
	})
}
