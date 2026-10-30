// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stacks

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// SecretEnvPrefix starts the name of the project variable that carries a stack secret's value to the
// Compose SDK. The SDK delivers a secret from an environment variable by writing it into the container
// (CopyToContainer) and by that alone; a secret that names a file is bind-mounted from the server's disk
// (RESEARCH Pitfall 4).
const SecretEnvPrefix = "COAMING_SECRET_"

var (
	// ErrPinMissing means a service that has an image was given no pin: a standalone deploy never sends
	// an image tag that nobody decided on.
	ErrPinMissing = errors.New("stacks: a service has no pinned image")
	// ErrUseAPISocket means a service asks for use_api_socket. The SDK would bind-mount the Docker socket
	// of the host into the container and put the registry logins of its configuration beside it, past the
	// host-access rules, which judge a socket that is written as a bind.
	ErrUseAPISocket = errors.New("stacks: use_api_socket is not supported on a standalone host")
	// ErrUnsupportedKey means a service asks for something the Compose SDK would do by running a program
	// of the Coaming server (a provider service, a model) or a command in the container through exec (a
	// lifecycle hook). A standalone deploy does none of those.
	ErrUnsupportedKey = errors.New("stacks: the stack uses a key a standalone deploy does not run")
	// ErrExternalFileObject means a service uses an external secret or config. A standalone host writes
	// the content of a secret or config into the container, and an external one has none to write.
	ErrExternalFileObject = errors.New("stacks: an external secret or config cannot be used on a standalone host")
)

// PrepareStandalone makes a loaded project safe and complete for the Compose SDK (RESEARCH Q2, Pitfalls
// 4, 5 and 8). It changes p in place, so a caller passes the project of one check or one deploy:
//
//   - every service's image becomes its pin (pins, by the name in the compose file), and the image as
//     written, the stack's ID, the project, the service and the neutral working directory and config file
//     labels go on the service as custom labels, which the SDK keeps out of its config hash (the labels
//     the SDK's own loader would have set, and coaming.stack.id and coaming.stack.image beside them);
//   - every secret and config a service uses puts the short hash of its content on the service as a label
//     (LabelSecretPrefix, LabelConfigPrefix), which is in the SDK's config hash: new content recreates the
//     container, unchanged content does not;
//   - every secret the services use gets no file: it names the variable COAMING_SECRET_<name> and the
//     value goes in the project's environment, from where the SDK writes it into the container; every
//     config the services use carries its content. An object no service uses is dropped;
//   - the networks and volumes the project creates carry the stack's ID, so a later look for a project
//     that Coaming does not manage finds them to be its own.
//
// Nothing here is per deploy: no time, no job and no ID of one reaches a service, so a redeploy of a stack
// nobody edited builds the same services and the SDK recreates nothing (Pitfall 5).
//
// It refuses what the SDK cannot deliver or what would read the server: a service with an image and no
// pin (ErrPinMissing), a service with use_api_socket (ErrUseAPISocket), a provider, a model or a lifecycle hook
// (ErrUnsupportedKey: the SDK would run a program of the Coaming server, or a command through exec), an external secret or config
// (ErrExternalFileObject), and, after the work, a
// build, a file-backed secret or config, an env_file or a relative bind mount that is still there
// (ErrNotSandboxed). The first error is returned.
func PrepareStandalone(p *types.Project, stackID string, pins map[string]string, objects []ObjectContent) error {
	if p.Environment == nil {
		p.Environment = types.Mapping{}
	}
	if len(p.Models) > 0 {
		return fmt.Errorf("%w: models", ErrUnsupportedKey)
	}
	content := make(map[string]ObjectContent, len(objects))
	for _, o := range objects {
		content[string(o.Kind)+"/"+o.Name] = o
	}

	for _, name := range sortedServices(p) {
		svc := p.Services[name]
		if svc.UseAPISocket {
			return fmt.Errorf("%w: service %s", ErrUseAPISocket, name)
		}
		if svc.Provider != nil || len(svc.Models) > 0 || len(svc.PreStart) > 0 || len(svc.PostStart) > 0 || len(svc.PreStop) > 0 {
			return fmt.Errorf("%w: service %s", ErrUnsupportedKey, name)
		}
		if svc.Image != "" {
			ref, ok := pins[name]
			if !ok || ref == "" {
				return fmt.Errorf("%w: %s", ErrPinMissing, name)
			}
			svc.CustomLabels = withLabels(svc.CustomLabels, map[string]string{LabelStackImage: svc.Image})
			svc.Image = ref
		}
		svc.CustomLabels = withLabels(svc.CustomLabels, map[string]string{
			LabelStackID:            stackID,
			LabelComposeProject:     p.Name,
			LabelComposeService:     name,
			LabelComposeOneoff:      "False",
			LabelComposeWorkingDir:  NeutralWorkingDir,
			LabelComposeConfigFiles: NeutralConfigFiles,
		})
		p.Services[name] = svc
	}

	// The content of each secret and config a service uses is part of the service, as the hash of it in a
	// label, so that changed content recreates the container (see LabelSecretPrefix).
	for _, name := range sortedServices(p) {
		svc := p.Services[name]
		add := map[string]string{}
		for _, ref := range svc.Secrets {
			if o, ok := content[string(KindSecret)+"/"+ref.Source]; ok && !o.External {
				add[LabelSecretPrefix+ref.Source] = ObjectHash(ref.Source, o.Content)
			}
		}
		for _, ref := range svc.Configs {
			if o, ok := content[string(KindConfig)+"/"+ref.Source]; ok && !o.External {
				add[LabelConfigPrefix+ref.Source] = ObjectHash(ref.Source, o.Content)
			}
		}
		if len(add) > 0 {
			svc.Labels = withLabels(svc.Labels, add)
			p.Services[name] = svc
		}
	}

	usedSecrets, usedConfigs := usedFileObjects(p)
	for _, name := range sortedNames(p.Secrets) {
		decl := p.Secrets[name]
		if !usedSecrets[name] {
			delete(p.Secrets, name)
			continue
		}
		o, ok := content[string(KindSecret)+"/"+name]
		if bool(decl.External) || !ok || o.External {
			return fmt.Errorf("%w: secret %s", ErrExternalFileObject, name)
		}
		env := SecretEnvPrefix + name
		decl.File, decl.Content, decl.Environment = "", "", env
		p.Secrets[name] = decl
		p.Environment[env] = string(o.Content)
	}
	for _, name := range sortedNames(p.Configs) {
		decl := p.Configs[name]
		if !usedConfigs[name] {
			delete(p.Configs, name)
			continue
		}
		o, ok := content[string(KindConfig)+"/"+name]
		if bool(decl.External) || !ok || o.External {
			return fmt.Errorf("%w: config %s", ErrExternalFileObject, name)
		}
		decl.File, decl.Environment, decl.Content = "", "", string(o.Content)
		p.Configs[name] = decl
	}

	for name, n := range p.Networks {
		if n.External {
			continue
		}
		n.CustomLabels = withLabels(n.CustomLabels, map[string]string{LabelStackID: stackID})
		p.Networks[name] = n
	}
	for name, v := range p.Volumes {
		if v.External {
			continue
		}
		v.CustomLabels = withLabels(v.CustomLabels, map[string]string{LabelStackID: stackID})
		p.Volumes[name] = v
	}
	return assertStandaloneSandboxed(p)
}

// assertStandaloneSandboxed is the check after the work: nothing the SDK would read from the server's disk
// or build is left in the project.
func assertStandaloneSandboxed(p *types.Project) error {
	for _, name := range sortedServices(p) {
		svc := p.Services[name]
		switch {
		case svc.Build != nil:
			return fmt.Errorf("%w: service %s has a build", ErrNotSandboxed, name)
		case len(svc.EnvFiles) > 0:
			return fmt.Errorf("%w: service %s still has an env_file", ErrNotSandboxed, name)
		case svc.Extends != nil:
			return fmt.Errorf("%w: service %s extends another", ErrNotSandboxed, name)
		}
		for _, v := range svc.Volumes {
			if v.Type == types.VolumeTypeBind && v.Source != "" && !strings.HasPrefix(v.Source, "/") {
				return fmt.Errorf("%w: service %s has a relative bind mount", ErrNotSandboxed, name)
			}
		}
	}
	for name, s := range p.Secrets {
		if s.File != "" {
			return fmt.Errorf("%w: secret %s still has a file", ErrNotSandboxed, name)
		}
	}
	for name, c := range p.Configs {
		if c.File != "" {
			return fmt.Errorf("%w: config %s still has a file", ErrNotSandboxed, name)
		}
	}
	return nil
}

// usedFileObjects are the secrets and configs at least one service uses.
func usedFileObjects(p *types.Project) (secrets, configs map[string]bool) {
	secrets, configs = map[string]bool{}, map[string]bool{}
	for _, svc := range p.Services {
		for _, s := range svc.Secrets {
			secrets[s.Source] = true
		}
		for _, c := range svc.Configs {
			configs[c.Source] = true
		}
	}
	return secrets, configs
}

// withLabels is labels with the pairs added over it, never changing the map it was given.
func withLabels(labels types.Labels, add map[string]string) types.Labels {
	out := make(types.Labels, len(labels)+len(add))
	for k, v := range labels {
		out[k] = v
	}
	for k, v := range add {
		out[k] = v
	}
	return out
}

func sortedServices(p *types.Project) []string {
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func sortedNames[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ServiceJSON is the service as JSON in a stable key order (the fields of the struct in their order, the keys
// of a map sorted), without its custom labels, which the configuration hash leaves out too.
func ServiceJSON(svc types.ServiceConfig) (json.RawMessage, error) {
	svc.CustomLabels = nil
	raw, err := json.Marshal(svc)
	if err != nil {
		return nil, fmt.Errorf("encode service %s: %w", svc.Name, err)
	}
	return raw, nil
}

// ResolveServiceRefs is the service as the Compose SDK hashes it when it creates a container for it: a
// reference to another service of the project is replaced by the ID of that service's first container, by
// container name (resolveServiceReferences in the SDK, as serviceHashWithResolvedRefs applies it). ids holds
// each service's container IDs in the order of the containers' names. network_mode, ipc and pid
// "service:X" become "container:<ID>", and each volumes_from entry becomes the bare ID of X's container (the
// access mode is dropped, as the SDK drops it); "container:<name>" in volumes_from loses its prefix. A
// reference that cannot be resolved stays as written, and a failing volumes_from stops the resolution of
// the rest, as in the SDK. The result shares nothing with svc.
func ResolveServiceRefs(svc types.ServiceConfig, ids map[string][]string) types.ServiceConfig {
	out := svc
	out.VolumesFrom = slices.Clone(svc.VolumesFrom)
	for i, vol := range out.VolumesFrom {
		spec := strings.Split(vol, ":")
		if spec[0] == "container" {
			if len(spec) > 1 {
				out.VolumesFrom[i] = spec[1]
			}
			continue
		}
		first := ids[spec[0]]
		if len(first) == 0 {
			return out
		}
		out.VolumesFrom[i] = first[0]
	}
	for _, mode := range []*string{&out.NetworkMode, &out.Ipc, &out.Pid} {
		name, ok := strings.CutPrefix(*mode, types.NetworkModeServicePrefix)
		if !ok {
			continue
		}
		first := ids[name]
		if len(first) == 0 {
			return out
		}
		*mode = types.ContainerPrefix + first[0]
	}
	return out
}

// RecreateCascade lists the services a deploy carries along when it recreates the services in recreated,
// as the Compose SDK's reconciler does (v5.6.0, parentNamespaceRecreated and planStopDependents), so that
// the review can name them before the deploy (STK-06). running says which services have a container. Under
// the diverged policy the unchanged services follow:
//
//   - a service with a container whose network_mode, ipc or pid is service:X, or whose volumes_from names X,
//     with X recreated, is recreated too, and the same goes for a service that shares what that one shares,
//     until nothing more joins; it keeps its pinned image;
//   - a service with a container that depends_on X with restart: true, X recreated and the service not
//     itself recreated, keeps its container and is stopped and started again;
//   - a plain depends_on carries nothing: the SDK derives the cascade from what is shared, not from depends_on.
//
// The rows are the recreated services in name order and then the restarted ones; Because is the recreated
// service that carries it (the first by name), and no row has a Current, a Next or a count of settings.
func RecreateCascade(p *types.Project, recreated []string, running map[string]bool) []ServiceChange {
	recreatedSet := map[string]bool{}
	for _, n := range recreated {
		recreatedSet[n] = true
	}
	names := sortedServices(p)
	because := map[string]string{}
	for changed := true; changed; {
		changed = false
		for _, name := range names {
			if recreatedSet[name] || !running[name] {
				continue
			}
			for _, parent := range sharedParents(p.Services[name]) {
				if recreatedSet[parent] {
					recreatedSet[name], because[name], changed = true, parent, true
					break
				}
			}
		}
	}
	var out []ServiceChange
	for _, name := range names {
		if c, ok := because[name]; ok {
			out = append(out, ServiceChange{Name: name, Short: name, Change: ChangeRecreated, Because: c})
		}
	}
	for _, name := range names {
		if recreatedSet[name] || !running[name] {
			continue
		}
		deps := p.Services[name].DependsOn
		for _, parent := range sortedNames(deps) {
			if deps[parent].Restart && recreatedSet[parent] {
				out = append(out, ServiceChange{Name: name, Short: name, Change: ChangeRestarted, Because: parent})
				break
			}
		}
	}
	return out
}

// sharedParents are the services whose namespaces or volumes a service shares, by name in order.
func sharedParents(svc types.ServiceConfig) []string {
	var out []string
	for _, mode := range []string{svc.NetworkMode, svc.Ipc, svc.Pid} {
		if name, ok := strings.CutPrefix(mode, types.NetworkModeServicePrefix); ok && name != "" {
			out = append(out, name)
		}
	}
	for _, vol := range svc.VolumesFrom {
		if strings.HasPrefix(vol, types.ContainerPrefix) {
			continue
		}
		name, _, _ := strings.Cut(vol, ":")
		out = append(out, name)
	}
	sort.Strings(out)
	return slices.Compact(out)
}
