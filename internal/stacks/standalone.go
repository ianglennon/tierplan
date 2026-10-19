// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// SecretEnvPrefix starts the name of the project variable that carries a stack secret's value to the
// Compose SDK. The SDK delivers a secret from an environment variable by writing it into the container
// (CopyToContainer) and by that alone; a secret that names a file is bind-mounted from the server's disk
// (RESEARCH Pitfall 4).
const SecretEnvPrefix = "BAYPLAN_SECRET_"

var (
	// ErrPinMissing means a service that has an image was given no pin: a standalone deploy never sends
	// an image tag that nobody decided on.
	ErrPinMissing = errors.New("stacks: a service has no pinned image")
	// ErrUseAPISocket means a service asks for use_api_socket. The SDK would bind-mount the Docker socket
	// of the host into the container and put the registry logins of its configuration beside it, past the
	// host-access rules, which judge a socket that is written as a bind.
	ErrUseAPISocket = errors.New("stacks: use_api_socket is not supported on a standalone host")
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
//     the SDK's own loader would have set, and bayplan.stack.id and bayplan.stack.image beside them);
//   - every secret and config a service uses puts the short hash of its content on the service as a label
//     (LabelSecretPrefix, LabelConfigPrefix), which is in the SDK's config hash: new content recreates the
//     container, unchanged content does not;
//   - every secret the services use gets no file: it names the variable BAYPLAN_SECRET_<name> and the
//     value goes in the project's environment, from where the SDK writes it into the container; every
//     config the services use carries its content. An object no service uses is dropped;
//   - the networks and volumes the project creates carry the stack's ID, so a later look for a project
//     that Bayplan does not manage finds them to be its own.
//
// Nothing here is per deploy: no time, no job and no ID of one reaches a service, so a redeploy of a stack
// nobody edited builds the same services and the SDK recreates nothing (Pitfall 5).
//
// It refuses what the SDK cannot deliver or what would read the server: a service with an image and no
// pin (ErrPinMissing), a service with use_api_socket (ErrUseAPISocket), an external secret or config
// (ErrExternalFileObject), and, after the work, a
// build, a file-backed secret or config, an env_file or a relative bind mount that is still there
// (ErrNotSandboxed). The first error is returned.
func PrepareStandalone(p *types.Project, stackID string, pins map[string]string, objects []ObjectContent) error {
	if p.Environment == nil {
		p.Environment = types.Mapping{}
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
