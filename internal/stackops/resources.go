// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// The ownership of an object on the Resources tab. An object is the stack's only when it carries the
// stack's own labels; one the compose file refers to that was made outside the stack is external, and
// Bayplan never changes or removes it (STK-10, D-22).
const (
	OwnerStack    = "stack"
	OwnerExternal = "external"
)

// The codes a card's Error holds when its read failed. The page words them; the engine's own text is
// logged on the server and never sent.
const (
	// ReadNoManager means no manager of the swarm is connected.
	ReadNoManager = "no_manager"
	// ReadNoQuorum means the swarm has lost its quorum.
	ReadNoQuorum = "no_quorum"
	// ReadNodeUnavailable means the standalone host is not connected.
	ReadNodeUnavailable = "node_unavailable"
	// ReadFailed is any other read that failed.
	ReadFailed = "read_failed"
)

// ResourceCard is one card of the Resources tab: its rows, or the code of the read that failed. Each
// card is read on its own, so one that fails never takes the others with it.
type ResourceCard[T any] struct {
	Items []T
	Error string
}

// TaskResource is the current task of one slot of a swarm stack's service (or of one node, for a global
// service), with the node it is on.
type TaskResource struct {
	Task dockerx.TaskRow
	// ServiceID is the swarm service ID, Service its name in the compose file (without the stack prefix).
	ServiceID, Service string
	// Global is true for a global service, whose rows are nodes rather than slots.
	Global bool
	// Node is the swarm's row for the task's node, NodeListed whether the swarm lists it (a removed node's
	// ID is kept on its tasks).
	Node       dockerx.NodeRow
	NodeListed bool
}

// ContainerResource is one container of a standalone stack.
type ContainerResource struct {
	ID, Name, Service, State, Health, Image string
	// Ports are the published ports as published->target/protocol, without the host address.
	Ports   []string
	Created time.Time
}

// NetworkResource is one network the stack owns or uses.
type NetworkResource struct {
	ID, Name, Driver, Scope string
	// Ownership is OwnerStack or OwnerExternal.
	Ownership string
}

// VolumeResource is one volume the stack owns or uses, on the node that holds it: a volume is local to a
// node, so a swarm stack's volume has one row for each node that has it.
type VolumeResource struct {
	Name, Driver string
	// Node is the display name of the node and NodeID its ID, which the volume's page takes.
	Node, NodeID string
	// Ownership is OwnerStack or OwnerExternal.
	Ownership string
}

// ObjectUser is a service that uses a secret or a config: its ID on a swarm (the service page), its name in the
// compose file.
type ObjectUser struct{ ID, Name string }

// ObjectResource is a secret or a config of the stack. On a swarm it is an object of the cluster; on a standalone
// host it is a file in the containers, listed from the deployed definition, with no ID.
type ObjectResource struct {
	ID string
	// Name is the name as the compose file writes it, FullName the name of the object on the swarm (the stack,
	// the name and the content hash joined) and Hash that content hash, empty for an object Bayplan did not hash.
	Name, FullName, Hash string
	// UsedBy are the services of the stack that use it, by name.
	UsedBy []ObjectUser
	// Ownership is OwnerStack or OwnerExternal.
	Ownership string
}

// Resources is everything a stack owns on its environment, in cards: the swarm's tasks or the
// standalone host's containers, the networks, the volumes, the secrets and the configs. A card is left empty
// (not nil) when nothing is there.
type Resources struct {
	Tasks      ResourceCard[TaskResource]
	Containers ResourceCard[ContainerResource]
	Networks   ResourceCard[NetworkResource]
	Volumes    ResourceCard[VolumeResource]
	// NotReporting are the nodes that should have answered the volumes read and did not: their volumes are left out.
	NotReporting []string
	Secrets      ResourceCard[ObjectResource]
	Configs      ResourceCard[ObjectResource]
}

// DeclaredObject is a network, volume, secret or config that a service of the deployed revision uses.
type DeclaredObject struct {
	// Key is the name in the compose file. Name is the object's own name on the engine for an external one.
	Key, Name string
	External  bool
	// UsedBy are the services that use it, by name in the compose file, sorted.
	UsedBy []string
}

// Declared is what the deployed revision of a stack's compose file refers to. The Resources tab needs it for what
// the engine cannot say: the externals a stack uses but did not make, and the secrets and configs of a standalone
// host, which are files in containers and not objects of the engine. The zero value is a stack with no deployed revision.
type Declared struct {
	// Known is true when a deployed revision was read and loaded.
	Known bool
	// Err is set when a deployed revision exists and could not be read or loaded.
	Err                                 error
	Networks, Volumes, Secrets, Configs []DeclaredObject
}

// addUser records that a service uses the object of that key.
func addUser(used map[string][]string, key, service string) {
	if key == "" || slices.Contains(used[key], service) {
		return
	}
	used[key] = append(used[key], service)
}

// declaredObjects turns the services that use each key into DeclaredObjects, by key. external says whether the
// compose file declares the object external and under what name; a key it does not declare (the implicit
// default network) is the stack's own.
func declaredObjects(used map[string][]string, external func(key string) (bool, string)) []DeclaredObject {
	out := make([]DeclaredObject, 0, len(used))
	for key, users := range used {
		ext, name := external(key)
		slices.Sort(users)
		out = append(out, DeclaredObject{Key: key, Name: cmp.Or(name, key), External: ext, UsedBy: users})
	}
	slices.SortFunc(out, func(a, b DeclaredObject) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

// DeclaredOf reads what a loaded project's services use: the networks they join, the named volumes they mount, the
// secrets and the configs they refer to (a credential_spec's config included). An object the file declares and no
// service uses is left out: it is not part of what the stack runs, and a deploy does not create it.
func DeclaredOf(p *types.Project) Declared {
	nets, vols, secrets, configs := map[string][]string{}, map[string][]string{}, map[string][]string{}, map[string][]string{}
	for name, svc := range p.Services {
		for key := range svc.Networks {
			addUser(nets, key, name)
		}
		for _, v := range svc.Volumes {
			if v.Type == types.VolumeTypeVolume {
				addUser(vols, v.Source, name)
			}
		}
		for _, ref := range svc.Secrets {
			addUser(secrets, ref.Source, name)
		}
		for _, ref := range svc.Configs {
			addUser(configs, ref.Source, name)
		}
		if cs := svc.CredentialSpec; cs != nil {
			addUser(configs, cs.Config, name)
		}
	}
	return Declared{
		Known: true,
		Networks: declaredObjects(nets, func(key string) (bool, string) {
			n, ok := p.Networks[key]
			return ok && bool(n.External), n.Name
		}),
		Volumes: declaredObjects(vols, func(key string) (bool, string) {
			v, ok := p.Volumes[key]
			return ok && bool(v.External), v.Name
		}),
		Secrets: declaredObjects(secrets, func(key string) (bool, string) {
			o, ok := p.Secrets[key]
			return ok && bool(o.External), o.Name
		}),
		Configs: declaredObjects(configs, func(key string) (bool, string) {
			o, ok := p.Configs[key]
			return ok && bool(o.External), o.Name
		}),
	}
}

// Declared reads what the stack's newest good revision uses. A stack with none is the zero Declared. A revision
// that is stored and does not load comes back as Declared{Err}: the page then says what it cannot, and keeps
// the cards the engine can answer.
func (r Repo) Declared(ctx context.Context, stack Stack) (Declared, error) {
	snap, ok, err := r.LatestGoodRevision(ctx, stack.ID)
	if err != nil {
		return Declared{}, err
	}
	if !ok {
		return Declared{}, nil
	}
	loaded, err := stacks.Load(ctx, stacks.LoadInput{Name: stack.Name, Target: stack.Target, Copy: snap.Copy})
	if err != nil {
		return Declared{Err: err}, nil
	}
	return DeclaredOf(loaded.Project), nil
}

// VolumesFrom picks the volumes of a stack out of what the nodes of an environment hold, one row for each node
// that has one: those that carry the stack's own labels, and those the compose file declares external, found
// by the name it gives. A volume is the stack's only by its labels. The nodes that did not answer are named, so
// the page can say their volumes are left out.
func VolumesFrom(stack Stack, decl Declared, agg envs.Aggregate) ([]VolumeResource, []string) {
	external := map[string]bool{}
	for _, v := range decl.Volumes {
		if v.External {
			external[v.Name] = true
		}
	}
	out := []VolumeResource{}
	for _, v := range agg.Volumes {
		owner := ""
		switch {
		case ownsLabels(stack, v.Labels):
			owner = OwnerStack
		case external[v.Name]:
			owner = OwnerExternal
		default:
			continue
		}
		out = append(out, VolumeResource{Name: v.Name, Driver: v.Driver, Node: v.Node, NodeID: v.NodeID, Ownership: owner})
	}
	slices.SortFunc(out, func(a, b VolumeResource) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Node, b.Node), cmp.Compare(a.NodeID, b.NodeID))
	})
	return out, append([]string{}, agg.NotReporting...)
}

// readCode names the failure of a read for a card.
func readCode(err error) string {
	switch {
	case errors.Is(err, envs.ErrNoManager):
		return ReadNoManager
	case envs.ReasonFor(err) == envs.ReasonNoQuorum:
		return ReadNoQuorum
	case errors.Is(err, envs.ErrNodeUnavailable):
		return ReadNodeUnavailable
	}
	return ReadFailed
}

// cardOf turns the result of one read into a card. The engine's text is logged and the card carries a code.
func cardOf[T any](d Deps, envID string, stack Stack, card string, items []T, err error) ResourceCard[T] {
	if err != nil {
		d.log().Warn("could not read a card of a stack's resources", "environment", envID, "stack", stack.Name, "card", card, "error", err)
		return ResourceCard[T]{Items: []T{}, Error: readCode(err)}
	}
	if items == nil {
		items = []T{}
	}
	return ResourceCard[T]{Items: items}
}

// ownsLabels reports whether an object's labels are the stack's own: its swarm namespace or Compose
// project and the ID of its Bayplan record. A name alone is never ownership: another stack, or a person,
// can make an object under the same namespace.
func ownsLabels(stack Stack, labels map[string]string) bool {
	if labels[stacks.LabelStackID] != stack.ID {
		return false
	}
	if stack.Target == stacks.TargetSwarm {
		return labels[stacks.LabelSwarmNamespace] == stack.Name
	}
	return labels[stacks.LabelComposeProject] == stack.Name
}

// Resources reads the cards of a stack's Resources tab now. Each card keeps its own error, so a secrets list the
// engine refuses leaves the tasks and the networks in place. decl is what the deployed revision refers to
// (Repo.Declared); the zero value is a stack with none. It is a read.
func (d Deps) Resources(ctx context.Context, envID string, stack Stack, decl Declared) Resources {
	var out Resources
	vols, notReporting, verr := d.volumesOf(ctx, envID, stack, decl)
	out.Volumes = cardOf(d, envID, stack, "volumes", vols, verr)
	if verr == nil {
		out.NotReporting = notReporting
	}
	if out.NotReporting == nil {
		out.NotReporting = []string{}
	}
	nets, nerr := d.networksOf(ctx, envID, stack, decl)
	out.Networks = cardOf(d, envID, stack, "networks", nets, nerr)
	if stack.Target == stacks.TargetSwarm {
		cc, err := d.clusterCards(ctx, envID, stack, decl)
		if err != nil {
			cc.tasksErr, cc.secretsErr, cc.configsErr = err, err, err
		}
		out.Tasks = cardOf(d, envID, stack, "tasks", cc.tasks, cc.tasksErr)
		out.Containers = ResourceCard[ContainerResource]{Items: []ContainerResource{}}
		out.Secrets = cardOf(d, envID, stack, "secrets", cc.secrets, cc.secretsErr)
		out.Configs = cardOf(d, envID, stack, "configs", cc.configs, cc.configsErr)
		return out
	}
	containers, err := d.standaloneContainers(ctx, envID, stack)
	out.Tasks = ResourceCard[TaskResource]{Items: []TaskResource{}}
	out.Containers = cardOf(d, envID, stack, "containers", containers, err)
	out.Secrets = d.standaloneObjectsCard(envID, stack, decl, decl.Secrets)
	out.Configs = d.standaloneObjectsCard(envID, stack, decl, decl.Configs)
	return out
}

// standaloneObjectsCard lists the secrets or configs of a standalone stack from its deployed definition. They are
// files written into the containers, not objects of the engine, so the definition is the only place that knows
// them, and each is the stack's own. A stack with no deployed definition has none, and one whose definition
// could not be read says so.
func (d Deps) standaloneObjectsCard(envID string, stack Stack, decl Declared, declared []DeclaredObject) ResourceCard[ObjectResource] {
	if decl.Err != nil {
		return cardOf[ObjectResource](d, envID, stack, "definition", nil, decl.Err)
	}
	out := make([]ObjectResource, 0, len(declared))
	for _, o := range declared {
		if o.External {
			continue // a standalone host cannot use one (stacks.ErrExternalFileObject)
		}
		users := make([]ObjectUser, 0, len(o.UsedBy))
		for _, name := range o.UsedBy {
			users = append(users, ObjectUser{Name: name})
		}
		out = append(out, ObjectResource{Name: o.Key, FullName: o.Key, UsedBy: users, Ownership: OwnerStack})
	}
	return ResourceCard[ObjectResource]{Items: out}
}

// volumesOf reads the stack's volumes from the environment's aggregate, which holds the volumes of every node that
// answered.
func (d Deps) volumesOf(ctx context.Context, envID string, stack Stack, decl Declared) ([]VolumeResource, []string, error) {
	if d.Aggregator == nil {
		return nil, nil, errors.New("stackops: no aggregator to read the volumes with")
	}
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return nil, nil, err
	}
	vols, notReporting := VolumesFrom(stack, decl, agg)
	return vols, notReporting, nil
}

// clusterCards are the cards of a swarm stack that one read of the cluster answers.
type clusterCards struct {
	tasks                []TaskResource
	secrets, configs     []ObjectResource
	tasksErr, secretsErr error
	configsErr           error
}

// clusterCards reads the tasks, the secrets and the configs of a swarm stack on one manager, from one read of the
// cluster. If that read fails, none of the three has an answer; once it has succeeded each card is read and keeps its
// own error.
func (d Deps) clusterCards(ctx context.Context, envID string, stack Stack, decl Declared) (clusterCards, error) {
	if d.Clusters == nil {
		return clusterCards{}, errors.New("stackops: no cluster reader to read the swarm with")
	}
	var cc clusterCards
	err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		cc = clusterCards{}
		read, e := c.Cluster(ctx)
		if e != nil {
			return e
		}
		svcs := SwarmRows(stack, read.Services)
		cc.tasks, cc.tasksErr = swarmTasks(ctx, c, svcs, read.Nodes)
		users := objectUsers(svcs, read.ServiceRefs)
		secrets, e := c.SecretList(ctx)
		if e != nil {
			cc.secretsErr = e
		} else {
			cc.secrets = swarmObjects(stack, decl.Secrets, users, secretObjects(secrets), func(r dockerx.ServiceRef) []dockerx.ServiceObjectRef { return r.Secrets })
		}
		configs, e := c.ConfigList(ctx)
		if e != nil {
			cc.configsErr = e
		} else {
			cc.configs = swarmObjects(stack, decl.Configs, users, configObjects(configs), func(r dockerx.ServiceRef) []dockerx.ServiceObjectRef { return r.Configs })
		}
		return nil
	})
	return cc, err
}

// currentDesired are the desired states of a task that still belongs on the page: one that is wanted
// running, one the swarm holds ready for its restart delay, and one a job completed.
func currentDesired(state string) bool {
	switch state {
	case "running", "ready", "completed":
		return true
	}
	return false
}

// currentTasks picks the current task of each slot (a replicated service) or each node (a global one) from
// tasks that are newest first: the newest the swarm still wants, so the page shows what runs and not the
// history Docker keeps (the service's Tasks tab has that). A slot with no task the swarm wants, as after
// a scale down, has no row.
func currentTasks(rows []dockerx.TaskRow, services map[string]ServiceRow, nodes map[string]dockerx.NodeRow) []TaskResource {
	seen := map[string]bool{}
	out := make([]TaskResource, 0)
	for _, r := range rows {
		svc, ok := services[r.ServiceID]
		if !ok || !currentDesired(r.DesiredState) {
			continue
		}
		global := svc.Mode == "global" || svc.Mode == "global-job"
		key := r.ServiceID + "|slot|" + strconv.Itoa(r.Slot)
		if global {
			key = r.ServiceID + "|node|" + r.NodeID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		n, listed := nodes[r.NodeID]
		out = append(out, TaskResource{Task: r, ServiceID: svc.ID, Service: svc.Short, Global: global, Node: n, NodeListed: listed})
	}
	slices.SortStableFunc(out, func(a, b TaskResource) int {
		return cmp.Or(
			cmp.Compare(a.Service, b.Service),
			cmp.Compare(a.Task.Slot, b.Task.Slot),
			cmp.Compare(a.Node.Hostname, b.Node.Hostname),
			cmp.Compare(a.Task.NodeID, b.Task.NodeID),
			cmp.Compare(a.Task.ID, b.Task.ID),
		)
	})
	return out
}

// swarmTasks reads the current tasks of the stack's services.
func swarmTasks(ctx context.Context, c *dockerx.Client, svcs []ServiceRow, nodeRows []dockerx.NodeRow) ([]TaskResource, error) {
	if len(svcs) == 0 {
		return []TaskResource{}, nil
	}
	byID := make(map[string]ServiceRow, len(svcs))
	ids := make([]string, 0, len(svcs))
	for _, s := range svcs {
		byID[s.ID] = s
		ids = append(ids, s.ID)
	}
	tasks, err := c.Tasks(ctx, dockerx.TaskFilter{ServiceIDs: ids})
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]dockerx.NodeRow, len(nodeRows))
	for _, n := range nodeRows {
		nodes[n.ID] = n
	}
	return currentTasks(tasks, byID, nodes), nil
}

// objectInfo is a secret or a config as the page needs it from the engine: its ID, its name and its labels.
type objectInfo struct {
	ID, Name string
	Labels   map[string]string
}

func secretObjects(rows []dockerx.SecretRow) []objectInfo {
	out := make([]objectInfo, len(rows))
	for i, r := range rows {
		out[i] = objectInfo{ID: r.ID, Name: r.Name, Labels: r.Labels}
	}
	return out
}

func configObjects(rows []dockerx.ConfigRow) []objectInfo {
	out := make([]objectInfo, len(rows))
	for i, r := range rows {
		out[i] = objectInfo{ID: r.ID, Name: r.Name, Labels: r.Labels}
	}
	return out
}

// serviceUse is a service of the stack with what its spec refers to.
type serviceUse struct {
	svc ServiceRow
	ref dockerx.ServiceRef
}

// objectUsers pairs each service the stack owns with the secrets, configs, networks and volumes its spec refers to,
// by the service ID. Only the stack's own services are paired: another stack that uses the same external secret
// is not this stack's user of it.
func objectUsers(svcs []ServiceRow, refs []dockerx.ServiceRef) []serviceUse {
	byID := make(map[string]dockerx.ServiceRef, len(refs))
	for _, r := range refs {
		byID[r.ID] = r
	}
	out := make([]serviceUse, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, serviceUse{svc: s, ref: byID[s.ID]})
	}
	return out
}

// usersOf are the services of the stack whose spec refers to the object, by service name.
func usersOf(o objectInfo, uses []serviceUse, refs func(dockerx.ServiceRef) []dockerx.ServiceObjectRef) []ObjectUser {
	out := []ObjectUser{}
	for _, u := range uses {
		for _, r := range refs(u.ref) {
			if r.ID == o.ID || (r.Name != "" && r.Name == o.Name) {
				out = append(out, ObjectUser{ID: u.svc.ID, Name: u.svc.Short})
				break
			}
		}
	}
	slices.SortFunc(out, func(a, b ObjectUser) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out
}

// swarmObjects lists the secrets or configs of a swarm stack: those that carry the stack's own labels, by the name the
// compose file uses and the content hash Bayplan put in the object's name, and those the compose file declares external,
// found by the name it gives. An object is the stack's only by its labels; an external one never reads Stack.
func swarmObjects(stack Stack, declared []DeclaredObject, uses []serviceUse, all []objectInfo, refs func(dockerx.ServiceRef) []dockerx.ServiceObjectRef) []ObjectResource {
	out := []ObjectResource{}
	owned := map[string]bool{}
	for _, o := range all {
		if !ownsLabels(stack, o.Labels) {
			continue
		}
		owned[o.ID] = true
		out = append(out, ObjectResource{
			ID: o.ID, Name: cmp.Or(o.Labels[stacks.LabelObject], o.Name), FullName: o.Name, Hash: o.Labels[stacks.LabelObjectHash],
			UsedBy: usersOf(o, uses, refs), Ownership: OwnerStack,
		})
	}
	for _, dec := range declared {
		if !dec.External {
			continue
		}
		i := slices.IndexFunc(all, func(o objectInfo) bool { return o.Name == dec.Name && !owned[o.ID] })
		if i < 0 {
			continue
		}
		o := all[i]
		out = append(out, ObjectResource{ID: o.ID, Name: o.Name, FullName: o.Name, UsedBy: usersOf(o, uses, refs), Ownership: OwnerExternal})
	}
	slices.SortFunc(out, func(a, b ObjectResource) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.FullName, b.FullName), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// networksOf lists the networks the stack owns, found by its labels, in one read of the engine that
// holds them: a manager for a swarm (a stack's networks are overlay networks of the cluster), the host's
// own engine for a standalone stack.
func (d Deps) networksOf(ctx context.Context, envID string, stack Stack, decl Declared) ([]NetworkResource, error) {
	var (
		key  string
		read func(fn func(*dockerx.Client) error) error
	)
	if stack.Target == stacks.TargetSwarm {
		if d.Clusters == nil {
			return nil, errors.New("stackops: no cluster reader to read the swarm with")
		}
		key = stacks.LabelSwarmNamespace
		read = func(fn func(*dockerx.Client) error) error { return d.Clusters.ClusterRead(ctx, envID, fn) }
	} else {
		node, err := d.StandaloneNode(ctx, envID)
		if err != nil {
			return nil, err
		}
		key = stacks.LabelComposeProject
		read = func(fn func(*dockerx.Client) error) error { return d.Aggregator.OnNode(ctx, envID, node, fn) }
	}
	var out []NetworkResource
	err := read(func(c *dockerx.Client) error {
		rows, e := c.NetworksWithLabel(ctx, key, stack.Name)
		if e != nil {
			return e
		}
		out = []NetworkResource{}
		for _, r := range rows {
			if !ownsLabels(stack, r.Labels) {
				continue
			}
			out = append(out, NetworkResource{ID: r.ID, Name: r.Name, Driver: r.Driver, Scope: r.Scope, Ownership: OwnerStack})
		}
		// The networks the compose file refers to that were made outside the stack: found by the name it gives, and
		// listed as external. A network that is not there is left out; the deploy would have refused it.
		wantsExternal := slices.ContainsFunc(decl.Networks, func(n DeclaredObject) bool { return n.External })
		if !wantsExternal {
			return nil
		}
		all, e := c.Networks(ctx)
		if e != nil {
			return e
		}
		for _, dn := range decl.Networks {
			if !dn.External {
				continue
			}
			i := slices.IndexFunc(all, func(n dockerx.NetworkRow) bool {
				return n.Name == dn.Name && (stack.Target != stacks.TargetSwarm || n.Scope == dockerx.ScopeSwarm) && !slices.ContainsFunc(out, func(o NetworkResource) bool { return o.ID == n.ID })
			})
			if i < 0 {
				continue
			}
			n := all[i]
			out = append(out, NetworkResource{ID: n.ID, Name: n.Name, Driver: n.Driver, Scope: n.Scope, Ownership: OwnerExternal})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b NetworkResource) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out, nil
}

// standaloneContainers lists the containers of the stack's Compose project, stopped ones too, one-off
// containers (`compose run`) left out.
func (d Deps) standaloneContainers(ctx context.Context, envID string, stack Stack) ([]ContainerResource, error) {
	node, err := d.StandaloneNode(ctx, envID)
	if err != nil {
		return nil, err
	}
	var rows []dockerx.ContainerRow
	if err := d.Aggregator.OnNode(ctx, envID, node, func(c *dockerx.Client) error {
		var e error
		rows, e = c.ContainersWithLabel(ctx, stacks.LabelComposeProject, stack.Name)
		return e
	}); err != nil {
		return nil, err
	}
	out := make([]ContainerResource, 0, len(rows))
	for _, r := range rows {
		if !ownsLabels(stack, r.Labels) || strings.EqualFold(r.Labels[stacks.LabelComposeOneoff], "true") {
			continue
		}
		seen := map[string]bool{}
		ports := []string{}
		for _, p := range r.Ports {
			if h := hostPort(p); h != "" && !seen[h] {
				seen[h] = true
				ports = append(ports, h)
			}
		}
		slices.Sort(ports)
		out = append(out, ContainerResource{
			ID: r.ID, Name: strings.TrimPrefix(r.Name, "/"), Service: r.Labels[stacks.LabelComposeService],
			State: r.State, Health: r.Health, Image: r.Image, Ports: ports, Created: r.Created,
		})
	}
	slices.SortFunc(out, func(a, b ContainerResource) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out, nil
}
