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
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/swarm"
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
// object and is the swarm namespace, its ID, which is the ownership label, and the resolver of
// the secrets and configs the services reference.
type Options struct {
	StackName string
	StackID   string
	// Objects resolves a service's secret and config references to the swarm objects a deploy
	// created for them. It may be nil for a stack that references none; a reference it cannot
	// resolve is an error.
	Objects ObjectResolver
}

// ObjectResolver turns the name the compose file uses for a secret or config (its key under the
// top-level secrets or configs) into the swarm object a deploy created for it: the object's ID
// and its name on the swarm. ok is false for a name the resolver does not know.
type ObjectResolver interface {
	Secret(name string) (id, objectName string, ok bool)
	Config(name string) (id, objectName string, ok bool)
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

// handledFields are the ServiceConfig fields this version converts. Any other field that is set
// makes Convert refuse the service (see NotConvertedError).
var handledFields = map[string]bool{
	"Name": true, "Image": true, "Environment": true, "Labels": true, "Deploy": true,
	"Networks": true, "Ports": true, "Command": true, "Entrypoint": true, "HealthCheck": true,
}

// handledDeployFields are the DeployConfig fields this version converts.
var handledDeployFields = map[string]bool{"Mode": true, "Replicas": true, "Labels": true, "EndpointMode": true}

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

	nets, err := networkPlans(p, o, used)
	if err != nil {
		return Plan{}, err
	}
	plan.Networks = nets
	return plan, nil
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
