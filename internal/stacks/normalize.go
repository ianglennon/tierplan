// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/swarm"
)

// NormalizeSpec returns the service spec as the swarm will hold it once it has been stored. The
// review compares the spec a deploy would send with the spec the manager returns for the running
// service, and the manager fills in defaults on the way in (and the converter leaves some values
// empty that the manager reads back with a default), so two specs that mean the same thing are
// not byte-equal until both have been through the same defaults. A review made right after a
// deploy therefore shows no change (STK-06), and a real difference is never hidden by one.
//
// It is applied to both sides. The caller's spec is not changed: the result shares nothing with
// it. Every rule below says which engine behaviour it mirrors; a rule is added only for a
// default a real swarm was seen to apply (the swarm shared test StackReviewEmptyAfterDeploy
// lists any it has not learned yet).
func NormalizeSpec(s swarm.ServiceSpec) swarm.ServiceSpec {
	s = cloneSpec(s)

	// Labels: the per-deploy bookkeeping label that remembers a stopped stack's replica count is
	// not part of what a deploy would send.
	delete(s.Labels, LabelStoppedReplicas)

	// A task template with a container spec reads back as the container runtime
	// (daemon/cluster/convert taskSpecFromGRPC sets Runtime from the oneof it holds).
	if s.TaskTemplate.ContainerSpec != nil && s.TaskTemplate.Runtime == "" {
		s.TaskTemplate.Runtime = swarm.RuntimeContainer
	}
	if cs := s.TaskTemplate.ContainerSpec; cs != nil {
		// The isolation of a container spec that names none reads back as "default"
		// (isolationFromGRPC).
		if cs.Isolation == "" {
			cs.Isolation = "default"
		}
		// The order of environment entries only matters between two entries of one name; the
		// converter writes them from a map, so they are compared as a set.
		slices.Sort(cs.Env)
		sortSecretRefs(cs.Secrets)
		sortConfigRefs(cs.Configs)
	}

	// A task template with no placement reads back with an empty one (the manager stores the
	// placement it was given, and the converter always writes one, as docker stack deploy does).
	if s.TaskTemplate.Placement == nil {
		s.TaskTemplate.Placement = &swarm.Placement{}
	}
	// A restart policy with no condition reads back as "any" (restartPolicyToGRPC), and one with no
	// attempt limit reads back with a limit of 0, which is unlimited (the manager keeps the count as
	// a plain number and hands it back as a pointer; seen on Engine 29, StackReviewEmptyAfterDeploy).
	if rp := s.TaskTemplate.RestartPolicy; rp != nil {
		if rp.Condition == "" {
			rp.Condition = swarm.RestartPolicyConditionAny
		}
		if rp.MaxAttempts == nil {
			unlimited := uint64(0)
			rp.MaxAttempts = &unlimited
		}
	}

	// Networks are compared as a set: the manager returns them by network ID, the converter
	// writes them by name, and neither order is meaningful.
	slices.SortStableFunc(s.TaskTemplate.Networks, func(a, b swarm.NetworkAttachmentConfig) int {
		return cmp.Compare(a.Target, b.Target)
	})
	for i := range s.TaskTemplate.Networks {
		slices.Sort(s.TaskTemplate.Networks[i].Aliases)
	}

	// A service with no mode is a replicated service of one task (ServiceSpecToGRPC), and a
	// replicated one with no count has one replica.
	if s.Mode.Replicated == nil && s.Mode.Global == nil && s.Mode.ReplicatedJob == nil && s.Mode.GlobalJob == nil {
		s.Mode.Replicated = &swarm.ReplicatedService{}
	}
	if s.Mode.Replicated != nil && s.Mode.Replicated.Replicas == nil {
		one := uint64(1)
		s.Mode.Replicated.Replicas = &one
	}

	// An update or rollback policy with no failure action or order reads back as pause and
	// stop-first (updateConfigToGRPC).
	for _, uc := range []*swarm.UpdateConfig{s.UpdateConfig, s.RollbackConfig} {
		if uc == nil {
			continue
		}
		if uc.FailureAction == "" {
			uc.FailureAction = swarm.UpdateFailureActionPause
		}
		if uc.Order == "" {
			uc.Order = swarm.UpdateOrderStopFirst
		}
	}

	// A service with no endpoint spec resolves through a virtual IP (the manager reads one back
	// for every service), a spec with no mode does too, and a port with no protocol or publish
	// mode is tcp and ingress (portConfigToGRPC).
	if s.EndpointSpec == nil {
		s.EndpointSpec = &swarm.EndpointSpec{}
	}
	if s.EndpointSpec.Mode == "" {
		s.EndpointSpec.Mode = swarm.ResolutionModeVIP
	}
	for i := range s.EndpointSpec.Ports {
		p := &s.EndpointSpec.Ports[i]
		if p.Protocol == "" {
			p.Protocol = "tcp"
		}
		if p.PublishMode == "" {
			p.PublishMode = swarm.PortConfigPublishModeIngress
		}
	}
	slices.SortStableFunc(s.EndpointSpec.Ports, func(a, b swarm.PortConfig) int {
		return cmp.Or(
			cmp.Compare(a.TargetPort, b.TargetPort),
			cmp.Compare(a.Protocol, b.Protocol),
			cmp.Compare(a.PublishedPort, b.PublishedPort),
			cmp.Compare(a.PublishMode, b.PublishMode),
			strings.Compare(a.Name, b.Name),
		)
	})
	return s
}

// cloneSpec copies a spec through its JSON form, which is also what an engine holds: a nil and an
// empty list, or map, are the same thing in it.
func cloneSpec(s swarm.ServiceSpec) swarm.ServiceSpec {
	raw, err := json.Marshal(s)
	if err != nil {
		return s
	}
	var out swarm.ServiceSpec
	if err := json.Unmarshal(raw, &out); err != nil {
		return s
	}
	return out
}

func sortSecretRefs(refs []*swarm.SecretReference) {
	slices.SortStableFunc(refs, func(a, b *swarm.SecretReference) int {
		return cmp.Compare(refName(a), refName(b))
	})
}

func sortConfigRefs(refs []*swarm.ConfigReference) {
	slices.SortStableFunc(refs, func(a, b *swarm.ConfigReference) int {
		return cmp.Compare(configRefName(a), configRefName(b))
	})
}

func refName(r *swarm.SecretReference) string {
	if r == nil {
		return ""
	}
	return r.SecretName
}

func configRefName(r *swarm.ConfigReference) string {
	if r == nil {
		return ""
	}
	return r.ConfigName
}
