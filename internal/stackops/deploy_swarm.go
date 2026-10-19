// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// The items of a swarm deploy job, in order.
const (
	ItemCheck    = "check"
	ItemNetworks = "networks"
	ItemServices = "services"
)

// The codes a swarm deploy ends a failed item with. Each has a sentence in codeMessages; the
// manager's own words are never put in a job.
const (
	CodeHostReadOnly       = "host_read_only"
	CodeAgentPolicyDenied  = "agent_policy_denied"
	CodeNoManagerConnected = "no_manager_connected"
	CodeServiceRejected    = "service_rejected"
	CodeServiceChanged     = "service_changed"
	CodeServiceNameTaken   = "service_name_taken"
	CodeNetworkConflict    = "network_conflict"
	CodeNetworkFailed      = "network_failed"
	CodeExternalMissing    = "external_network_missing"
	CodeNameClash          = "stack_name_clash"
	CodeRegistryUnresolved = "registry_unresolved"
	CodeEngineError        = "engine_error"
	CodeCancelled          = "cancelled"
	CodeNotRun             = "not_run"
)

var codeMessages = map[string]string{
	CodeHostReadOnly:       "This host is read-only, so Bayplan cannot change it.",
	CodeAgentPolicyDenied:  "The agent refused the request.",
	CodeNoManagerConnected: "No manager of this swarm is connected to Bayplan.",
	CodeServiceRejected:    "The swarm rejected the service.",
	CodeServiceChanged:     "The service was changed by someone else while the deploy ran.",
	CodeServiceNameTaken:   "The swarm already has a service of that name.",
	CodeNetworkConflict:    "The swarm already has a network of that name that this stack does not own.",
	CodeNetworkFailed:      "The swarm could not create the network.",
	CodeExternalMissing:    "A network the stack names as external does not exist.",
	CodeNameClash:          "A stack of that name already exists on this environment that Bayplan does not manage.",
	CodeRegistryUnresolved: "The registry credential for an image could not be read.",
	CodeEngineError:        "The engine could not complete the request.",
	CodeCancelled:          "The deploy was cancelled.",
	CodeNotRun:             "Not run because an earlier step failed.",
}

// CodeMessage is the sentence for a deploy code, empty for a code it does not know.
func CodeMessage(code string) string { return codeMessages[code] }

// SwarmDeployInput is a stack to deploy to a swarm: the environment, the stack, the converted
// plan and who asks.
type SwarmDeployInput struct {
	EnvID  string
	Stack  Stack
	Plan   convert.Plan
	Caller Caller
	// OnDone, when set, is called once with the result before the job's last item is finished,
	// so a job that reads as finished has already been recorded by the caller.
	OnDone func(DeployResult)
}

// DeployResult is what a deploy did, as far as it got. FailedService and Code are empty when it
// succeeded. A cancelled deploy has Code CodeCancelled.
type DeployResult struct {
	// Networks and Services count the networks and services created; Updated counts the
	// services that were updated in place.
	Networks, Services, Updated int
	FailedService, Code         string
}

// OK reports whether the deploy ended without a failure.
func (r DeployResult) OK() bool { return r.Code == "" }

// DeploySwarm creates the stack's networks and services on the swarm, once each and in order,
// reporting through r on the items ItemCheck, ItemNetworks and ItemServices. It reads what the
// stack already owns from a manager (one read), creates each missing network, then creates or
// updates each service with the registry credential chosen for its image. It stops at the first
// failure and ends the item that failed with a code; items after it end cancelled with CodeNotRun.
// Every write goes through Aggregator.WriteOnManager and is never retried (P2 D-42).
//
// The last item is finished only after in.OnDone returns.
func (d Deps) DeploySwarm(ctx context.Context, in SwarmDeployInput, r *jobs.Reporter) (res DeployResult) {
	final := itemEnd{state: jobs.ItemDone}
	defer func() {
		if in.OnDone != nil {
			in.OnDone(res)
		}
		r.Finish(ItemServices, final.state, final.code, final.message, final.result)
	}()
	// fail ends the item that failed with its code and the items after it as not run. The last
	// item is held in final, to be finished after OnDone.
	fail := func(item, code, service string) DeployResult {
		res.Code, res.FailedService = code, service
		state := jobs.ItemFailed
		if code == CodeCancelled {
			state = jobs.ItemCancelled
		}
		for _, id := range []string{ItemCheck, ItemNetworks, ItemServices} {
			var end itemEnd
			switch {
			case id == item:
				end = itemEnd{state: state, code: code, message: CodeMessage(code)}
			case order(id) > order(item):
				end = itemEnd{state: jobs.ItemCancelled, code: CodeNotRun, message: CodeMessage(CodeNotRun)}
			default:
				continue // finished before the failure
			}
			if id == ItemServices {
				final = end
			} else {
				r.Finish(id, end.state, end.code, end.message, nil)
			}
		}
		return res
	}

	// 1. Check: read what the stack owns, once, from a manager.
	r.Update(ItemCheck, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	var existingNets []dockerx.NetworkRow
	var existingSvcs []dockerx.ServiceDetail
	var allNets []dockerx.NetworkRow
	err := d.Clusters.ClusterRead(ctx, in.EnvID, func(c *dockerx.Client) error {
		var e error
		if existingNets, e = c.NetworksWithLabel(ctx, stacks.LabelSwarmNamespace, in.Stack.Name); e != nil {
			return e
		}
		if existingSvcs, e = c.ServicesWithLabel(ctx, stacks.LabelSwarmNamespace, in.Stack.Name); e != nil {
			return e
		}
		if hasExternal(in.Plan) {
			allNets, e = c.Networks(ctx)
		}
		return e
	})
	if err != nil {
		d.log().Warn("could not read a stack's objects before a deploy", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		return fail(ItemCheck, codeOf(ctx, err), "")
	}
	if clash(in.Stack.ID, existingNets, existingSvcs) {
		return fail(ItemCheck, CodeNameClash, "")
	}
	for _, n := range in.Plan.Networks {
		if n.External && !hasNetwork(allNets, n.Name) {
			return fail(ItemCheck, CodeExternalMissing, n.Name)
		}
	}
	r.Finish(ItemCheck, jobs.ItemDone, "", "", nil)

	// 2. Networks: create each one the stack uses that does not exist yet.
	r.Update(ItemNetworks, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	for _, n := range in.Plan.Networks {
		if n.External || hasNetwork(existingNets, n.Name) {
			continue
		}
		if ctx.Err() != nil {
			return fail(ItemNetworks, CodeCancelled, "")
		}
		spec, specErr := networkSpec(n)
		if specErr != nil {
			return fail(ItemNetworks, CodeNetworkFailed, n.Name)
		}
		err := d.Aggregator.WriteOnManager(ctx, in.EnvID, func(c *dockerx.Client) error {
			_, _, e := c.NetworkCreate(ctx, spec)
			return e
		})
		if err != nil {
			d.log().Warn("could not create a stack network", "environment", in.EnvID, "stack", in.Stack.Name, "network", n.Name, "error", err)
			code := codeOf(ctx, err)
			if code == CodeEngineError {
				code = CodeNetworkFailed
				if isConflict(err) {
					code = CodeNetworkConflict
				}
			}
			return fail(ItemNetworks, code, n.Name)
		}
		res.Networks++
	}
	r.Finish(ItemNetworks, jobs.ItemDone, "", "", nil)

	// 3. Services: update the ones that exist at the version just read, create the rest.
	r.Update(ItemServices, func(it *jobs.Item) {
		it.State, it.Unit, it.Total = jobs.ItemRunning, "services", int64(len(in.Plan.Services))
	})
	byName := make(map[string]dockerx.ServiceDetail, len(existingSvcs))
	for _, s := range existingSvcs {
		byName[s.Name] = s
	}
	for i, sp := range in.Plan.Services {
		if ctx.Err() != nil {
			return fail(ItemServices, CodeCancelled, sp.Name)
		}
		raw, err := json.Marshal(sp.Spec)
		if err != nil {
			return fail(ItemServices, CodeServiceRejected, sp.Name)
		}
		auth, err := d.authFor(ctx, in.EnvID, sp.ImageAsWritten)
		if err != nil {
			d.log().Error("could not resolve the registry credential of a stack service", "environment", in.EnvID, "stack", in.Stack.Name, "service", sp.Name, "error", err)
			return fail(ItemServices, CodeRegistryUnresolved, sp.Name)
		}
		existing, update := byName[sp.Name]
		err = d.Aggregator.WriteOnManager(ctx, in.EnvID, func(c *dockerx.Client) error {
			if update {
				_, e := c.ServiceUpdate(ctx, existing.ID, existing.Version, raw, auth)
				return e
			}
			_, _, e := c.ServiceCreate(ctx, raw, auth)
			return e
		})
		if err != nil {
			d.log().Warn("could not write a stack service", "environment", in.EnvID, "stack", in.Stack.Name, "service", sp.Name, "update", update, "error", err)
			return fail(ItemServices, codeOf(ctx, err), sp.Name)
		}
		if update {
			res.Updated++
		} else {
			res.Services++
		}
		r.Update(ItemServices, func(it *jobs.Item) { it.Current = int64(i + 1) })
	}
	final = itemEnd{state: jobs.ItemDone, result: map[string]string{
		"services": strconv.Itoa(res.Services), "updated": strconv.Itoa(res.Updated), "networks": strconv.Itoa(res.Networks),
	}}
	return res
}

// itemEnd is how an item will be finished.
type itemEnd struct {
	state, code, message string
	result               map[string]string
}

func order(item string) int {
	switch item {
	case ItemCheck:
		return 0
	case ItemNetworks:
		return 1
	default:
		return 2
	}
}

// authFor chooses the registry credential an image is sent with, "" for none or no resolver.
func (d Deps) authFor(ctx context.Context, envID, image string) (string, error) {
	if d.Resolver == nil {
		return "", nil
	}
	ref, err := registry.ParseRef(image)
	if err != nil {
		// The engine will reject an unparsable reference with its own words; there is no
		// credential to choose for it.
		return "", nil //nolint:nilerr // see above
	}
	header, _, err := d.Resolver.AuthFor(ctx, envID, ref)
	return header, err
}

// clash reports whether the swarm holds an object under this stack's namespace that another
// stack, or none of Bayplan's, owns: a stack of that name that Bayplan does not manage (D-12).
func clash(stackID string, nets []dockerx.NetworkRow, svcs []dockerx.ServiceDetail) bool {
	for _, n := range nets {
		if n.Labels[stacks.LabelStackID] != stackID {
			return true
		}
	}
	for _, s := range svcs {
		if s.Labels[stacks.LabelStackID] != stackID {
			return true
		}
	}
	return false
}

func hasNetwork(nets []dockerx.NetworkRow, name string) bool {
	for _, n := range nets {
		if n.Name == name {
			return true
		}
	}
	return false
}

func hasExternal(p convert.Plan) bool {
	for _, n := range p.Networks {
		if n.External {
			return true
		}
	}
	return false
}

// networkSpec turns a planned network into the spec dockerx creates it from. dockerx expresses
// one address pool, so a plan with more is refused.
func networkSpec(n convert.NetworkPlan) (dockerx.NetworkCreateSpec, error) {
	spec := dockerx.NetworkCreateSpec{
		Name: n.Name, Driver: n.Driver, Scope: dockerx.NetworkScope(n.Driver),
		Attachable: n.Attachable, Internal: n.Internal, Labels: n.Labels, Options: n.DriverOpts,
	}
	switch len(n.Pools) {
	case 0:
	case 1:
		spec.Subnet, spec.Gateway, spec.IPRange = n.Pools[0].Subnet, n.Pools[0].Gateway, n.Pools[0].IPRange
	default:
		return dockerx.NetworkCreateSpec{}, errors.New("a network with more than one address pool")
	}
	return spec, nil
}

// codeOf classifies the error of one engine call into a deploy code.
func codeOf(ctx context.Context, err error) string {
	err = envs.ClassifyDockerError(err)
	var denied *envs.AgentPolicyDeniedError
	var rejected *dockerx.SpecRejectedError
	switch {
	case ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		return CodeCancelled
	case errors.Is(err, envs.ErrHostReadOnly):
		return CodeHostReadOnly
	case errors.Is(err, envs.ErrNodeUnavailable):
		return CodeNoManagerConnected
	case errors.As(err, &denied):
		return CodeAgentPolicyDenied
	case errors.Is(err, dockerx.ErrServiceChanged):
		return CodeServiceChanged
	case errors.Is(err, dockerx.ErrServiceNameTaken):
		return CodeServiceNameTaken
	case errors.As(err, &rejected), errors.Is(err, dockerx.ErrServiceSpecInvalid):
		return CodeServiceRejected
	}
	return CodeEngineError
}

// isConflict reports whether a network create was refused because the name exists.
func isConflict(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "already exists") || strings.Contains(text, "conflict")
}
