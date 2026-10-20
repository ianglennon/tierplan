// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package convert

import (
	"fmt"
	"slices"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/swarm"
)

// The conversions in this file are ported from cli/compose/convert/service.go in docker/cli
// (Apache-2.0) onto the compose-go v2 types: convertDeployMode, convertResources,
// convertRestartPolicy and convertUpdateConfig, plus the Placement block of Service.

// fieldPath is the dotted compose path of one of the service's fields.
func fieldPath(svc composetypes.ServiceConfig, field string) string {
	return "services." + svc.Name + "." + field
}

// deployOf returns the service's deploy block, or an empty one.
func deployOf(svc composetypes.ServiceConfig) composetypes.DeployConfig {
	if svc.Deploy == nil {
		return composetypes.DeployConfig{}
	}
	return *svc.Deploy
}

// serviceMode converts deploy.mode and deploy.replicas. A replicated service defaults to one
// replica; a job takes replicas as both its concurrency and its completions; a global service or
// job has no count, and a count on one is an error.
func serviceMode(svc composetypes.ServiceConfig) (swarm.ServiceMode, error) {
	d := deployOf(svc)
	var replicas *uint64
	if d.Replicas != nil {
		if *d.Replicas < 0 {
			return swarm.ServiceMode{}, fmt.Errorf("convert: service %s has %d replicas", svc.Name, *d.Replicas)
		}
		replicas = new(uint64(*d.Replicas))
	}
	switch d.Mode {
	case "", "replicated":
		if replicas == nil {
			replicas = new(uint64(1))
		}
		return swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: replicas}}, nil
	case "replicated-job":
		var total *uint64
		if replicas != nil {
			total = new(*replicas)
		}
		return swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{MaxConcurrent: replicas, TotalCompletions: total}}, nil
	case "global", "global-job":
		if replicas != nil {
			return swarm.ServiceMode{}, fmt.Errorf("convert: service %s: replicas can only be used with replicated or replicated-job mode, not %s", svc.Name, d.Mode)
		}
		if d.Mode == "global" {
			return swarm.ServiceMode{Global: &swarm.GlobalService{}}, nil
		}
		return swarm.ServiceMode{GlobalJob: &swarm.GlobalJob{}}, nil
	}
	return swarm.ServiceMode{}, &NotConvertedError{Service: svc.Name, Fields: []string{"deploy.mode: " + d.Mode}}
}

// resources converts deploy.resources. Like docker stack deploy it always returns a value, empty
// when the file sets no limits or reservations. A device request is refused: a swarm reserves
// generic resources, not devices, and dropping a GPU request would deploy a service that cannot
// run.
func resources(svc composetypes.ServiceConfig) (*swarm.ResourceRequirements, error) {
	src := deployOf(svc).Resources
	out := &swarm.ResourceRequirements{}
	if l := src.Limits; l != nil {
		if len(l.Devices) > 0 {
			return nil, &NotConvertedError{Service: svc.Name, Fields: []string{"deploy.resources.limits.devices"}}
		}
		cpus, err := nanoCPUs(l.NanoCPUs)
		if err != nil {
			return nil, fmt.Errorf("convert: service %s: deploy.resources.limits: %w", svc.Name, err)
		}
		out.Limits = &swarm.Limit{NanoCPUs: cpus, MemoryBytes: int64(l.MemoryBytes), Pids: l.Pids}
	}
	if r := src.Reservations; r != nil {
		if len(r.Devices) > 0 {
			return nil, &NotConvertedError{Service: svc.Name, Fields: []string{"deploy.resources.reservations.devices"}}
		}
		cpus, err := nanoCPUs(r.NanoCPUs)
		if err != nil {
			return nil, fmt.Errorf("convert: service %s: deploy.resources.reservations: %w", svc.Name, err)
		}
		var generic []swarm.GenericResource
		for _, g := range r.GenericResources {
			if g.DiscreteResourceSpec == nil {
				continue
			}
			generic = append(generic, swarm.GenericResource{DiscreteResourceSpec: &swarm.DiscreteGenericResource{
				Kind: g.DiscreteResourceSpec.Kind, Value: g.DiscreteResourceSpec.Value,
			}})
		}
		out.Reservations = &swarm.Resources{NanoCPUs: cpus, MemoryBytes: int64(r.MemoryBytes), GenericResources: generic}
	}
	return out, nil
}

// restartPolicy converts deploy.restart_policy, or the legacy restart key when the file sets no
// restart_policy.
func restartPolicy(svc composetypes.ServiceConfig) (*swarm.RestartPolicy, error) {
	if rp := deployOf(svc).RestartPolicy; rp != nil {
		return &swarm.RestartPolicy{
			Condition:   swarm.RestartPolicyCondition(rp.Condition),
			Delay:       durationPtr(rp.Delay),
			MaxAttempts: rp.MaxAttempts,
			Window:      durationPtr(rp.Window),
		}, nil
	}
	out, err := restartPolicyFromRestart(svc.Restart)
	if err != nil {
		return nil, &FieldError{Path: fieldPath(svc, "restart"), Err: fmt.Errorf("convert: service %s: %w", svc.Name, err)}
	}
	return out, nil
}

// updateConfig converts update_config or rollback_config; nil when the file sets none.
// Parallelism defaults to one.
func updateConfig(src *composetypes.UpdateConfig) *swarm.UpdateConfig {
	if src == nil {
		return nil
	}
	parallelism := uint64(1)
	if src.Parallelism != nil {
		parallelism = *src.Parallelism
	}
	return &swarm.UpdateConfig{
		Parallelism:     parallelism,
		Delay:           time.Duration(src.Delay),
		FailureAction:   swarm.FailureAction(src.FailureAction),
		Monitor:         time.Duration(src.Monitor),
		MaxFailureRatio: src.MaxFailureRatio,
		Order:           swarm.UpdateOrder(src.Order),
	}
}

// placement converts deploy.placement. It is written even when the file sets none, as docker
// stack deploy writes it.
func placement(svc composetypes.ServiceConfig) *swarm.Placement {
	src := deployOf(svc).Placement
	out := &swarm.Placement{Constraints: slices.Clone(src.Constraints), MaxReplicas: src.MaxReplicas}
	for _, pref := range src.Preferences {
		out.Preferences = append(out.Preferences, swarm.PlacementPreference{
			Spread: &swarm.SpreadOver{SpreadDescriptor: pref.Spread},
		})
	}
	return out
}

func durationPtr(d *composetypes.Duration) *time.Duration {
	if d == nil {
		return nil
	}
	return new(time.Duration(*d))
}
