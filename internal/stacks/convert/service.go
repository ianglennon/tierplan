// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package convert

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// convertService converts one service and returns the keys of the networks it joins.
//
// The shape follows Service in docker/cli's cli/compose/convert/service.go (Apache-2.0), ported
// onto the compose-go v2 types: the same fields land in the same places of the swarm spec, so a
// stack deployed here matches what docker stack deploy would send.
func convertService(svc composetypes.ServiceConfig, p *composetypes.Project, o Options) (ServicePlan, []string, error) {
	if unconverted := unconvertedFields(svc); len(unconverted) > 0 {
		return ServicePlan{}, nil, &NotConvertedError{Service: svc.Name, Fields: unconverted}
	}
	if svc.Image == "" {
		return ServicePlan{}, nil, fmt.Errorf("%w: %s", ErrNoImage, svc.Name)
	}

	labels := map[string]string{}
	if svc.Deploy != nil {
		maps.Copy(labels, svc.Deploy.Labels)
	}
	labels[stacks.LabelSwarmNamespace] = o.StackName
	labels[stacks.LabelStackID] = o.StackID
	labels[stacks.LabelSwarmImage] = svc.Image

	containerLabels := map[string]string{}
	maps.Copy(containerLabels, svc.Labels)
	containerLabels[stacks.LabelSwarmNamespace] = o.StackName
	containerLabels[stacks.LabelStackID] = o.StackID

	health, err := healthcheck(svc)
	if err != nil {
		return ServicePlan{}, nil, err
	}
	endpoint, err := endpointSpec(svc)
	if err != nil {
		return ServicePlan{}, nil, err
	}
	mode, err := serviceMode(svc)
	if err != nil {
		return ServicePlan{}, nil, err
	}
	attachments, keys, err := serviceNetworks(svc, p, o)
	if err != nil {
		return ServicePlan{}, nil, err
	}

	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: o.StackName + "_" + svc.Name, Labels: labels},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:       svc.Image,
				Labels:      containerLabels,
				Command:     slices.Clone(svc.Entrypoint),
				Args:        slices.Clone(svc.Command),
				Healthcheck: health,
				Env:         environment(svc.Environment),
			},
			Networks: attachments,
		},
		EndpointSpec: endpoint,
		Mode:         mode,
	}
	return ServicePlan{Name: spec.Name, Short: svc.Name, ImageAsWritten: svc.Image, Spec: spec}, keys, nil
}

// serviceMode converts deploy.mode and deploy.replicas. A replicated service defaults to one
// replica; a global service has no count.
func serviceMode(svc composetypes.ServiceConfig) (swarm.ServiceMode, error) {
	mode, replicas := "", uint64(1)
	if svc.Deploy != nil {
		mode = svc.Deploy.Mode
		if svc.Deploy.Replicas != nil {
			if *svc.Deploy.Replicas < 0 {
				return swarm.ServiceMode{}, fmt.Errorf("convert: service %s has %d replicas", svc.Name, *svc.Deploy.Replicas)
			}
			replicas = uint64(*svc.Deploy.Replicas)
		}
	}
	switch mode {
	case "", "replicated":
		return swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}}, nil
	case "global":
		return swarm.ServiceMode{Global: &swarm.GlobalService{}}, nil
	}
	return swarm.ServiceMode{}, &NotConvertedError{Service: svc.Name, Fields: []string{"deploy.mode: " + mode}}
}

// environment converts the explicit environment of a service to KEY=value strings, sorted. A
// variable the file names without a value is left out: Bayplan resolves values from the stack's
// table only (D-01), so there is no host environment to pass it through from. (docker stack
// deploy would pass the bare name through; with no host environment that sets nothing either.)
func environment(env composetypes.MappingWithEquals) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		if v == nil {
			continue
		}
		out = append(out, k+"="+*v)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// healthcheck converts a service's healthcheck. disable: true becomes the test NONE, and a
// disable that also names a test is an error (ported from convertHealthcheck in docker/cli's
// cli/compose/convert/service.go, Apache-2.0).
func healthcheck(svc composetypes.ServiceConfig) (*container.HealthConfig, error) {
	hc := svc.HealthCheck
	if hc == nil {
		return nil, nil
	}
	if hc.Disable {
		if len(hc.Test) != 0 {
			return nil, fmt.Errorf("convert: service %s: healthcheck test and disable cannot be set together", svc.Name)
		}
		return &container.HealthConfig{Test: []string{"NONE"}}, nil
	}
	out := &container.HealthConfig{Test: slices.Clone([]string(hc.Test))}
	if hc.Timeout != nil {
		out.Timeout = time.Duration(*hc.Timeout)
	}
	if hc.Interval != nil {
		out.Interval = time.Duration(*hc.Interval)
	}
	if hc.StartPeriod != nil {
		out.StartPeriod = time.Duration(*hc.StartPeriod)
	}
	if hc.StartInterval != nil {
		out.StartInterval = time.Duration(*hc.StartInterval)
	}
	if hc.Retries != nil {
		out.Retries = int(min(*hc.Retries, math.MaxInt32))
	}
	return out, nil
}

// endpointSpec converts deploy.endpoint_mode and the published ports. Like docker stack deploy
// it always writes an endpoint spec, with the ports in the engine's own order.
func endpointSpec(svc composetypes.ServiceConfig) (*swarm.EndpointSpec, error) {
	mode := ""
	if svc.Deploy != nil {
		mode = strings.ToLower(svc.Deploy.EndpointMode)
	}
	switch mode {
	case "", string(swarm.ResolutionModeVIP), string(swarm.ResolutionModeDNSRR):
	default:
		return nil, fmt.Errorf("convert: service %s: endpoint_mode %q is not vip or dnsrr", svc.Name, svc.Deploy.EndpointMode)
	}
	ports, err := ports(svc)
	if err != nil {
		return nil, err
	}
	return &swarm.EndpointSpec{Mode: swarm.ResolutionMode(mode), Ports: ports}, nil
}

// ports converts published ports (the conversion of convertEndpointSpec in docker/cli's
// cli/compose/convert/service.go, Apache-2.0). A port with no published number lets the swarm
// choose one; a published range is refused, because a swarm port is one number.
func ports(svc composetypes.ServiceConfig) ([]swarm.PortConfig, error) {
	var out []swarm.PortConfig
	for _, p := range svc.Ports {
		pc := swarm.PortConfig{
			Name:        p.Name,
			Protocol:    network.IPProtocol(cmp.Or(p.Protocol, "tcp")),
			TargetPort:  p.Target,
			PublishMode: swarm.PortConfigPublishModeIngress,
		}
		if p.Mode == "host" {
			pc.PublishMode = swarm.PortConfigPublishModeHost
		}
		if p.Published != "" {
			n, err := strconv.ParseUint(p.Published, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("convert: service %s: published port %q is not one port number", svc.Name, p.Published)
			}
			pc.PublishedPort = uint32(n)
		}
		out = append(out, pc)
	}
	slices.SortFunc(out, swarm.PortConfig.Compare)
	return out, nil
}

// errNoSuchNetwork is wrapped with the service and the network key.
var errNoSuchNetwork = errors.New("convert: the service uses a network the file does not declare")
