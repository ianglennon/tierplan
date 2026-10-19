// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package convert turns a compose-go project into the swarm objects a stack deploy creates: one
// service spec per compose service and one network per network the services use. It is pure: no
// HTTP, no store and no Docker client, only the swarm API types, so the same input always gives
// the same plan and every field is covered by a golden test.
//
// The conversion follows docker/cli's stack deploy (cli/compose/convert, Apache-2.0): a service
// is named <stack>_<service>, carries the com.docker.stack.namespace label and joins the
// <stack>_default network unless it names others. Bayplan adds its own ownership labels
// (stacks.LabelStackID) beside the docker ones.
//
// The conversion is built up plan by plan. A compose field it does not convert yet makes Convert
// return *NotConvertedError rather than drop the field, so a service is never deployed with a
// setting silently missing.
package convert

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// ErrNotConverted means a service uses a compose field this version does not convert.
var ErrNotConverted = errors.New("convert: the compose file uses a setting that is not converted yet")

// NotConvertedError names the service and the compose fields that were not converted.
type NotConvertedError struct {
	Service string
	Fields  []string
}

func (e *NotConvertedError) Error() string {
	return fmt.Sprintf("convert: service %s uses %s, which is not converted yet", e.Service, strings.Join(e.Fields, ", "))
}

// Is makes errors.Is(err, ErrNotConverted) true.
func (e *NotConvertedError) Is(target error) bool { return target == ErrNotConverted }

// ErrNoImage means a service names no image: a stack deploys images, it does not build them.
var ErrNoImage = errors.New("convert: the service has no image")

// Options are what a conversion needs besides the project: the stack's name, which prefixes every
// object and is the swarm namespace, and its ID, which is the ownership label.
type Options struct {
	StackName string
	StackID   string
}

// Plan is everything a swarm deploy creates, in the order it creates it.
type Plan struct {
	Networks []NetworkPlan
	Services []ServicePlan
}

// ServicePlan is one service to create or update. Name is <stack>_<service>, Short the service's
// name in the compose file and ImageAsWritten the image reference as the user wrote it.
type ServicePlan struct {
	Name, Short, ImageAsWritten string
	Spec                        swarm.ServiceSpec
}

// NetworkPlan is one network the services use. An external network is only referenced: Name is
// its name on the swarm and a deploy never creates it.
type NetworkPlan struct {
	Key, Name, Driver  string
	Attachable         bool
	Internal           bool
	External           bool
	Labels, DriverOpts map[string]string
	IPAMDriver         string
	Pools              []Pool
}

// Pool is one IPAM address pool of a network.
type Pool struct{ Subnet, Gateway, IPRange string }

// defaultNetwork is the key of the network a service joins when it names none.
const defaultNetwork = "default"

// handledFields are the ServiceConfig fields this version converts. Any other field that is set
// makes Convert refuse the service (see NotConvertedError).
var handledFields = map[string]bool{
	"Name": true, "Image": true, "Environment": true, "Labels": true, "Deploy": true,
	"Networks": true, "Ports": true,
}

// handledDeployFields are the DeployConfig fields this version converts.
var handledDeployFields = map[string]bool{"Mode": true, "Replicas": true, "Labels": true}

// Convert builds the swarm plan for p. The services are in name order and so are the networks,
// so a plan is stable between runs.
func Convert(p *composetypes.Project, o Options) (Plan, error) {
	var plan Plan
	names := make([]string, 0, len(p.Services))
	for name := range p.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	used := map[string]bool{}
	for _, short := range names {
		svc := p.Services[short]
		if svc.Name == "" {
			svc.Name = short
		}
		sp, nets, err := convertService(svc, p, o)
		if err != nil {
			return Plan{}, err
		}
		plan.Services = append(plan.Services, sp)
		for _, key := range nets {
			used[key] = true
		}
	}

	keys := make([]string, 0, len(used))
	for key := range used {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		np := convertNetwork(key, p, o)
		// A stack makes overlay networks only. Another driver (macvlan, ipvlan, a plugin) reaches
		// the host's network interfaces, which creating a network by hand needs Admin for; the
		// policy for it arrives with the review (plan 07-14), and until then it is refused.
		if !np.External && np.Driver != "overlay" {
			return Plan{}, &NotConvertedError{Service: "network " + key, Fields: []string{"driver: " + np.Driver}}
		}
		plan.Networks = append(plan.Networks, np)
	}
	return plan, nil
}

// convertService converts one service and returns the keys of the networks it joins.
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

	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: o.StackName + "_" + svc.Name, Labels: labels},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:  svc.Image,
				Labels: containerLabels,
				Env:    environment(svc.Environment),
			},
		},
	}

	mode, err := serviceMode(svc)
	if err != nil {
		return ServicePlan{}, nil, err
	}
	spec.Mode = mode

	keys := networkKeys(svc)
	for _, key := range keys {
		spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{
			Target:  convertNetwork(key, p, o).Name,
			Aliases: aliases(svc, key),
		})
	}
	if ports := ports(svc.Ports); len(ports) > 0 {
		spec.EndpointSpec = &swarm.EndpointSpec{Ports: ports}
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
// table only (D-01), so there is no host environment to pass it through from.
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

// networkKeys returns the keys of the networks a service joins, sorted: its own list, or the
// default network when it names none.
func networkKeys(svc composetypes.ServiceConfig) []string {
	if len(svc.Networks) == 0 {
		return []string{defaultNetwork}
	}
	keys := make([]string, 0, len(svc.Networks))
	for key := range svc.Networks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// aliases are the names a service answers to on a network: its own short name and any it was
// given for that network.
func aliases(svc composetypes.ServiceConfig, key string) []string {
	out := []string{svc.Name}
	if cfg := svc.Networks[key]; cfg != nil {
		for _, a := range cfg.Aliases {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// ports converts published ports. A port with no published number lets the swarm choose one.
func ports(in []composetypes.ServicePortConfig) []swarm.PortConfig {
	out := make([]swarm.PortConfig, 0, len(in))
	for _, p := range in {
		pc := swarm.PortConfig{
			Name:        p.Name,
			Protocol:    network.IPProtocol(cmp.Or(p.Protocol, "tcp")),
			TargetPort:  p.Target,
			PublishMode: swarm.PortConfigPublishModeIngress,
		}
		if p.Mode == "host" {
			pc.PublishMode = swarm.PortConfigPublishModeHost
		}
		if n, err := strconv.ParseUint(p.Published, 10, 32); err == nil {
			pc.PublishedPort = uint32(n)
		}
		out = append(out, pc)
	}
	return out
}

// convertNetwork converts the network the compose file calls key. The default network is
// attachable; one the file declares keeps its own settings. An external network is referenced by
// the name the file gave it and is never created.
func convertNetwork(key string, p *composetypes.Project, o Options) NetworkPlan {
	decl, declared := p.Networks[key]
	np := NetworkPlan{Key: key, Name: o.StackName + "_" + key, Driver: "overlay"}
	if declared {
		if decl.Name != "" {
			np.Name = decl.Name
		}
		np.External = bool(decl.External)
		np.Driver = cmp.Or(decl.Driver, np.Driver)
		np.Internal = decl.Internal
		np.Attachable = decl.Attachable
		np.DriverOpts = maps.Clone(decl.DriverOpts)
		np.IPAMDriver = decl.Ipam.Driver
		for _, pool := range decl.Ipam.Config {
			if pool != nil {
				np.Pools = append(np.Pools, Pool{Subnet: pool.Subnet, Gateway: pool.Gateway, IPRange: pool.IPRange})
			}
		}
	}
	if key == defaultNetwork && !np.External {
		np.Attachable = true
	}
	if np.External {
		return np
	}
	np.Labels = map[string]string{}
	if declared {
		maps.Copy(np.Labels, decl.Labels)
	}
	np.Labels[stacks.LabelSwarmNamespace] = o.StackName
	np.Labels[stacks.LabelStackID] = o.StackID
	return np
}

// unconvertedFields lists the compose fields a service sets that this version does not convert:
// a field of ServiceConfig outside handledFields that is not at its zero value, and a field of
// the deploy block outside handledDeployFields. Names are the compose-go field names.
func unconvertedFields(svc composetypes.ServiceConfig) []string {
	var out []string
	out = append(out, setFields(reflect.ValueOf(svc), handledFields, "")...)
	if svc.Deploy != nil {
		out = append(out, setFields(reflect.ValueOf(*svc.Deploy), handledDeployFields, "deploy.")...)
	}
	sort.Strings(out)
	return out
}

// setFields lists, with prefix, the fields of struct v that are not in skip and are not at their
// zero value. Embedded structs (ContainerSpec, WorkloadSpec) are walked into.
func setFields(v reflect.Value, skip map[string]bool, prefix string) []string {
	var out []string
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() || skip[f.Name] {
			continue
		}
		fv := v.Field(i)
		if f.Anonymous && fv.Kind() == reflect.Struct {
			out = append(out, setFields(fv, skip, prefix)...)
			continue
		}
		if f.Name == "Extensions" || isEmpty(fv) {
			continue
		}
		out = append(out, prefix+f.Name)
	}
	return out
}

// isEmpty reports whether v holds nothing a user set: zero scalars, nil pointers and interfaces,
// empty maps and slices, and structs whose fields are all empty. compose-go fills some blocks
// with empty non-nil maps and slices, which reflect's IsZero would count as set.
func isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Map, reflect.Slice:
		return v.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() && !isEmpty(v.Field(i)) {
				return false
			}
		}
		return true
	default:
		return v.IsZero()
	}
}
