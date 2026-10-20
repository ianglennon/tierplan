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
// Every compose service field is accounted for. FieldClasses says, for each, whether it is
// converted, ignored on a swarm (the converter lists it in ServicePlan.Ignored and the lint warns)
// or blocking (Convert returns *NotConvertedError), so a setting is never dropped silently.
package convert

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// ErrNotConverted means a service uses a compose setting a swarm stack cannot deploy.
var ErrNotConverted = errors.New("convert: the compose file uses a setting that a swarm stack cannot deploy")

// NotConvertedError names the service and the compose keys that were refused. Fields are compose
// YAML keys (volumes_from, deploy.mode: x).
type NotConvertedError struct {
	Service string
	Fields  []string
}

func (e *NotConvertedError) Error() string {
	return fmt.Sprintf("convert: service %s uses %s, which a swarm stack cannot deploy", e.Service, strings.Join(e.Fields, ", "))
}

// Is makes errors.Is(err, ErrNotConverted) true.
func (e *NotConvertedError) Is(target error) bool { return target == ErrNotConverted }

// FieldError is a conversion refused because of one field's value. Path is the dotted compose path
// from the top of the file (services.web.restart), the same path stacks.SecretUse judges. Error and
// Unwrap give the wrapped error's own text and chain, so errors.Is and errors.As see what they saw
// before. A refusal that carries no value (a keys-only NotConvertedError, ErrNoImage) is not wrapped.
type FieldError struct {
	Path string
	Err  error
}

func (e *FieldError) Error() string { return e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

// SecretFieldError replaces a FieldError, or any other converter error, that could quote a secret
// variable (D-02). Its text is stacks.SecretValueMessage(Path), with Path empty when no field is
// named.
//
// It deliberately has no Unwrap. A builder that walks a refusal with errors.As (the NotConvertedError
// field list of the API's problem) would otherwise read the cause's fields and values. Is answers
// for the cause's sentinels, so a caller that only needs the class (errors.Is(err, ErrNotConverted))
// still gets it.
type SecretFieldError struct {
	Path  string
	cause error
}

func (e *SecretFieldError) Error() string { return stacks.SecretValueMessage(e.Path) }

// Is reports whether the hidden cause is target.
func (e *SecretFieldError) Is(target error) bool { return errors.Is(e.cause, target) }

// GuardSecrets withholds the words of a converter error that could quote a value from a secret
// variable. The decision is stacks.SecretUse.Withholds, the one the load stage uses:
//
//   - a *FieldError is judged with its own Path, and its text, so a field misjudged plain whose
//     text still quotes a secret is withheld too;
//   - a keys-only *NotConvertedError and ErrNoImage carry compose keys and service names, never a
//     value, and come back as they are;
//   - any other error is judged by its text alone (the fail-closed case for a refusal site added
//     without a FieldError), and becomes the general sentence.
//
// A plain value keeps the converter's words, even in a file that uses a secret elsewhere.
func GuardSecrets(err error, s stacks.SecretUse) error {
	if err == nil {
		return nil
	}
	var fe *FieldError
	if errors.As(err, &fe) {
		if s.Withholds(fe.Path, err.Error()) {
			return &SecretFieldError{Path: fe.Path, cause: err}
		}
		return err
	}
	var nc *NotConvertedError
	if errors.As(err, &nc) || errors.Is(err, ErrNoImage) {
		return err
	}
	if s.Withholds("", err.Error()) {
		return &SecretFieldError{cause: err}
	}
	return err
}

// ErrRelativeBind means a bind mount's source is not an absolute path. Bayplan never resolves a
// path against its own directory, so a relative source could only mean a path on the server.
var ErrRelativeBind = errors.New("convert: a bind mount source must be an absolute path on the swarm nodes")

// ErrUnknownObject means a service references a secret or config the resolver does not know.
var ErrUnknownObject = errors.New("convert: the service references a secret or config that does not exist")

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
	// Ignored lists the compose keys the service sets that have no swarm equivalent, sorted. The
	// converter does not apply them; the lint turns each into a warning.
	Ignored []string `json:",omitempty"`
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
