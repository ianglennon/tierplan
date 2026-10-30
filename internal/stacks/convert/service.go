// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package convert

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/stacks"
)

// convertService converts one service and returns the keys of the networks it joins.
//
// The shape follows Service in docker/cli's cli/compose/convert/service.go (Apache-2.0), ported
// onto the compose-go v2 types: the same fields land in the same places of the swarm spec, so a
// stack deployed here matches what docker stack deploy would send. Where Arcane's
// libarcane/swarm (BSD-3-Clause) differs, docker/cli wins.
//
// Every field of the service is accounted for. FieldClasses says what each is: a Converted one
// is carried below, an IgnoredOnSwarm one is listed in ServicePlan.Ignored and not applied, and a
// Blocking one (or one FieldClasses does not know) refuses the service.
func convertService(svc composetypes.ServiceConfig, p *composetypes.Project, o Options) (ServicePlan, []string, error) {
	ignored, err := classifyFields(svc)
	if err != nil {
		return ServicePlan{}, nil, err
	}
	if svc.Image == "" {
		if slices.Contains(ignored, "build") {
			return ServicePlan{}, nil, &NotConvertedError{Service: svc.Name, Fields: []string{"build"}}
		}
		return ServicePlan{}, nil, fmt.Errorf("%w: %s", ErrNoImage, svc.Name)
	}

	d := deployOf(svc)
	labels := map[string]string{}
	maps.Copy(labels, d.Labels)
	labels[stacks.LabelSwarmNamespace] = o.StackName
	labels[stacks.LabelStackID] = o.StackID
	labels[stacks.LabelSwarmImage] = svc.Image

	containerLabels := map[string]string{}
	maps.Copy(containerLabels, svc.Labels)
	containerLabels[stacks.LabelSwarmNamespace] = o.StackName
	containerLabels[stacks.LabelStackID] = o.StackID

	health, err := healthcheck(svc)
	if err != nil {
		return ServicePlan{}, nil, atField(svc, "healthcheck", err)
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
		return ServicePlan{}, nil, atField(svc, "networks", err)
	}
	mounts, err := serviceMounts(svc, p, o)
	if err != nil {
		return ServicePlan{}, nil, atField(svc, "volumes", err)
	}
	res, err := resources(svc)
	if err != nil {
		return ServicePlan{}, nil, err
	}
	restart, err := restartPolicy(svc)
	if err != nil {
		return ServicePlan{}, nil, err
	}
	dns, err := dnsConfig(svc)
	if err != nil {
		return ServicePlan{}, nil, atField(svc, "dns", err)
	}
	secrets, err := secretRefs(svc, o)
	if err != nil {
		return ServicePlan{}, nil, atField(svc, "secrets", err)
	}
	configs, err := configRefs(svc, o)
	if err != nil {
		return ServicePlan{}, nil, atField(svc, "configs", err)
	}
	cred, configs, err := credentialSpec(svc, o, configs)
	if err != nil {
		return ServicePlan{}, nil, atField(svc, "credential_spec", err)
	}

	var logDriver *swarm.Driver
	if svc.Logging != nil {
		logDriver = &swarm.Driver{Name: svc.Logging.Driver, Options: maps.Clone(svc.Logging.Options)}
	}
	capAdd, capDrop := effectiveCapAddCapDrop(svc.CapAdd, svc.CapDrop)

	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: o.StackName + "_" + svc.Name, Labels: labels},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:           svc.Image,
				Labels:          containerLabels,
				Command:         slices.Clone(svc.Entrypoint),
				Args:            slices.Clone(svc.Command),
				Hostname:        svc.Hostname,
				Hosts:           extraHosts(svc.ExtraHosts),
				DNSConfig:       dns,
				Healthcheck:     health,
				Env:             environment(svc.Environment),
				Dir:             svc.WorkingDir,
				User:            svc.User,
				Groups:          slices.Clone(svc.GroupAdd),
				Mounts:          mounts,
				StopGracePeriod: durationPtr(svc.StopGracePeriod),
				StopSignal:      svc.StopSignal,
				TTY:             svc.Tty,
				OpenStdin:       svc.StdinOpen,
				Secrets:         secrets,
				Configs:         configs,
				ReadOnly:        svc.ReadOnly,
				Privileges:      &swarm.Privileges{CredentialSpec: cred},
				Isolation:       container.Isolation(svc.Isolation),
				Init:            svc.Init,
				Sysctls:         maps.Clone(svc.Sysctls),
				CapabilityAdd:   capAdd,
				CapabilityDrop:  capDrop,
				Ulimits:         ulimits(svc.Ulimits),
				OomScoreAdj:     svc.OomScoreAdj,
			},
			LogDriver:     logDriver,
			Resources:     res,
			RestartPolicy: restart,
			Placement:     placement(svc),
			Networks:      attachments,
		},
		EndpointSpec:   endpoint,
		Mode:           mode,
		UpdateConfig:   updateConfig(d.UpdateConfig),
		RollbackConfig: updateConfig(d.RollbackConfig),
	}
	return ServicePlan{Name: spec.Name, Short: svc.Name, ImageAsWritten: svc.Image, Spec: spec, Ignored: ignored}, keys, nil
}

// classifyFields sorts the compose keys the service sets by FieldClasses. It returns the ignored
// keys, sorted, and an error naming every blocking one; a key FieldClasses does not know is
// blocking, so a field compose-go adds can never pass through unseen.
func classifyFields(svc composetypes.ServiceConfig) ([]string, error) {
	var ignored, blocking []string
	for _, key := range serviceFieldKeys(reflect.ValueOf(svc)) {
		switch FieldClasses[key] {
		case Converted:
		case IgnoredOnSwarm:
			ignored = append(ignored, key)
		default:
			blocking = append(blocking, key)
		}
	}
	if len(blocking) > 0 {
		return nil, &NotConvertedError{Service: svc.Name, Fields: blocking}
	}
	return ignored, nil
}

// environment converts the explicit environment of a service to KEY=value strings, sorted. A
// variable the file names without a value is left out: Coaming resolves values from the stack's
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
	mode := strings.ToLower(deployOf(svc).EndpointMode)
	switch mode {
	case "", string(swarm.ResolutionModeVIP), string(swarm.ResolutionModeDNSRR):
	default:
		return nil, &FieldError{Path: fieldPath(svc, "deploy.endpoint_mode"), Err: fmt.Errorf("convert: service %s: endpoint_mode %q is not vip or dnsrr", svc.Name, deployOf(svc).EndpointMode)}
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
				return nil, &FieldError{Path: fieldPath(svc, "ports"), Err: fmt.Errorf("convert: service %s: published port %q is not one port number", svc.Name, p.Published)}
			}
			pc.PublishedPort = uint32(n)
		}
		out = append(out, pc)
	}
	slices.SortFunc(out, swarm.PortConfig.Compare)
	return out, nil
}

// extraHosts converts extra_hosts to swarm's "IP-address hostname" notation (convertExtraHosts in
// docker/cli, Apache-2.0). compose-go keeps a map, so the entries are in host order.
func extraHosts(hosts composetypes.HostsList) []string {
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		for _, ip := range hosts[name] {
			out = append(out, ip+" "+name)
		}
	}
	return out
}

// dnsConfig converts dns, dns_search and dns_opt. docker stack deploy skips a nameserver that is
// not an IP address; dropping one silently would leave a service resolving through the wrong
// servers, so it is an error here.
func dnsConfig(svc composetypes.ServiceConfig) (*swarm.DNSConfig, error) {
	if len(svc.DNS) == 0 && len(svc.DNSSearch) == 0 && len(svc.DNSOpts) == 0 {
		return nil, nil
	}
	out := &swarm.DNSConfig{Search: slices.Clone([]string(svc.DNSSearch)), Options: slices.Clone(svc.DNSOpts)}
	for _, ns := range svc.DNS {
		addr, err := netip.ParseAddr(ns)
		if err != nil {
			return nil, fmt.Errorf("convert: service %s: dns %q is not an IP address", svc.Name, ns)
		}
		out.Nameservers = append(out.Nameservers, addr)
	}
	return out, nil
}

// ulimits converts ulimits, sorted by name; a single value is both the soft and the hard limit
// (convertUlimits in docker/cli, Apache-2.0).
func ulimits(in map[string]*composetypes.UlimitsConfig) []*container.Ulimit {
	out := make([]*container.Ulimit, 0, len(in))
	for name, u := range in {
		if u == nil {
			continue
		}
		soft, hard := int64(u.Soft), int64(u.Hard)
		if u.Single != 0 {
			soft, hard = int64(u.Single), int64(u.Single)
		}
		out = append(out, &container.Ulimit{Name: name, Soft: soft, Hard: hard})
	}
	slices.SortFunc(out, func(a, b *container.Ulimit) int { return cmp.Compare(a.Name, b.Name) })
	if len(out) == 0 {
		return nil
	}
	return out
}

// errUnknownObject builds the error for a reference the resolver cannot resolve.
func errUnknownObject(service, kind, name string) error {
	return fmt.Errorf("%w: service %s references the %s %q", ErrUnknownObject, service, kind, name)
}

// fileTarget converts the target, ownership and mode of a secret or config reference. A target
// the file does not give is the reference's own name; the default compose-go writes for a secret
// (/run/secrets/<name>) is the same file, and is written as the bare name docker stack deploy
// writes. Ownership defaults to root and the mode to 0444 (convertFileObject in docker/cli,
// Apache-2.0).
func fileTarget(ref composetypes.FileReferenceConfig, defaultTarget string) (name, uid, gid string, mode os.FileMode) {
	name = ref.Target
	if name == "" || name == defaultTarget {
		name = ref.Source
	}
	mode = 0o444
	if ref.Mode != nil {
		mode = os.FileMode(*ref.Mode) //nolint:gosec // a file mode is at most 0o7777, which fits
	}
	return name, cmp.Or(ref.UID, "0"), cmp.Or(ref.GID, "0"), mode
}

// secretRefs converts a service's secrets, sorted by the swarm secret's name so the order of the
// file does not restart the service.
func secretRefs(svc composetypes.ServiceConfig, o Options) ([]*swarm.SecretReference, error) {
	var out []*swarm.SecretReference
	for _, s := range svc.Secrets {
		if o.Objects == nil {
			return nil, errUnknownObject(svc.Name, "secret", s.Source)
		}
		id, objName, ok := o.Objects.Secret(s.Source)
		if !ok {
			return nil, errUnknownObject(svc.Name, "secret", s.Source)
		}
		name, uid, gid, mode := fileTarget(composetypes.FileReferenceConfig(s), "/run/secrets/"+s.Source)
		out = append(out, &swarm.SecretReference{
			File:       &swarm.SecretReferenceFileTarget{Name: name, UID: uid, GID: gid, Mode: mode},
			SecretID:   id,
			SecretName: objName,
		})
	}
	slices.SortStableFunc(out, func(a, b *swarm.SecretReference) int { return cmp.Compare(a.SecretName, b.SecretName) })
	return out, nil
}

// configRefs converts a service's configs the same way.
func configRefs(svc composetypes.ServiceConfig, o Options) ([]*swarm.ConfigReference, error) {
	var out []*swarm.ConfigReference
	for _, c := range svc.Configs {
		if o.Objects == nil {
			return nil, errUnknownObject(svc.Name, "config", c.Source)
		}
		id, objName, ok := o.Objects.Config(c.Source)
		if !ok {
			return nil, errUnknownObject(svc.Name, "config", c.Source)
		}
		name, uid, gid, mode := fileTarget(composetypes.FileReferenceConfig(c), "")
		out = append(out, &swarm.ConfigReference{
			File:       &swarm.ConfigReferenceFileTarget{Name: name, UID: uid, GID: gid, Mode: mode},
			ConfigID:   id,
			ConfigName: objName,
		})
	}
	slices.SortStableFunc(out, func(a, b *swarm.ConfigReference) int { return cmp.Compare(a.ConfigName, b.ConfigName) })
	return out, nil
}

// credentialSpec converts credential_spec (convertCredentialSpec and the runtime-config part of
// convertServiceConfigObjs in docker/cli, Apache-2.0). A spec read from a config needs that
// config attached to the service as a runtime reference, and names it by its ID.
func credentialSpec(svc composetypes.ServiceConfig, o Options, configs []*swarm.ConfigReference) (*swarm.CredentialSpec, []*swarm.ConfigReference, error) {
	cs := svc.CredentialSpec
	if cs == nil {
		return nil, configs, nil
	}
	var set []string
	for _, kv := range [][2]string{{"Config", cs.Config}, {"File", cs.File}, {"Registry", cs.Registry}} {
		if kv[1] != "" {
			set = append(set, `"`+kv[0]+`"`)
		}
	}
	switch {
	case len(set) == 0:
		return nil, configs, nil
	case len(set) > 1:
		return nil, nil, fmt.Errorf("convert: service %s: invalid credential spec: cannot specify both %s", svc.Name, strings.Join(set, " and "))
	}
	out := &swarm.CredentialSpec{Config: cs.Config, File: cs.File, Registry: cs.Registry}
	if cs.Config == "" {
		return out, configs, nil
	}
	if o.Objects == nil {
		return nil, nil, errUnknownObject(svc.Name, "config", cs.Config)
	}
	id, objName, ok := o.Objects.Config(cs.Config)
	if !ok {
		return nil, nil, errUnknownObject(svc.Name, "config", cs.Config)
	}
	out.Config = id
	configs = append(configs, &swarm.ConfigReference{ConfigID: id, ConfigName: objName, Runtime: &swarm.ConfigReferenceRuntimeTarget{}})
	slices.SortStableFunc(configs, func(a, b *swarm.ConfigReference) int { return cmp.Compare(a.ConfigName, b.ConfigName) })
	return out, configs, nil
}
