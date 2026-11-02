// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package lint

import (
	"archive/tar"
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// StandaloneUnsupported is the compose attributes a standalone host ignores. It is TierPlan's copy
// of the list in docker/compose's pkg/compose/unsupported_attributes.go (Apache-2.0), version
// v5.6.0, where it is unexported: presenceUnsupportedAttributes (an attribute that is ignored
// whatever its value) and valueConditionalAttributes (only some values are ignored), written here
// as patterns over the compose file. A segment is a key, "*" for any key at that level, or "[]"
// for any item of a list; a last segment of the form key=value matches only that value.
//
// The SDK also reports, from compose-go's schema inventory, any attribute a newer compose-spec adds
// that nobody has yet decided on. That fail-closed catch-all is not copied; when plan 07-20 bumps
// the pinned SDK it re-reads unsupported_attributes.go and records the comparison in its summary.
//
// TestEveryStandaloneUnsupportedPathIsReported has a fixture for each path and fails for a path
// without one.
var StandaloneUnsupported = []string{
	"services.*.deploy.mode",
	"services.*.deploy.labels",
	"services.*.deploy.update_config",
	"services.*.deploy.rollback_config",
	"services.*.deploy.endpoint_mode",
	"services.*.deploy.placement",
	"services.*.credential_spec",
	"configs.*.labels",
	"secrets.*.driver_opts",
	"secrets.*.labels",
	"services.*.ports.[].mode=host",
	"services.*.volumes.[].type=cluster",
	"services.*.configs.[].uid",
	"services.*.configs.[].gid",
	"services.*.configs.[].mode",
	"services.*.secrets.[].uid",
	"services.*.secrets.[].gid",
	"services.*.secrets.[].mode",
}

// standaloneAdvice words each pattern: the key as a person writes it, and what to do about it.
// The sentence is "`{key}` is ignored on a standalone host. {action}".
var standaloneAdvice = map[string]struct{ key, action string }{
	"services.*.deploy.mode":             {"deploy.mode", "A standalone host runs the service as replicas. Remove the key, or deploy this stack to a Swarm."},
	"services.*.deploy.labels":           {"deploy.labels", "Put the labels under `labels` to set them on the container."},
	"services.*.deploy.update_config":    {"deploy.update_config", "A standalone host has no rolling updates. Remove the key."},
	"services.*.deploy.rollback_config":  {"deploy.rollback_config", "A standalone host has no rolling updates. Remove the key."},
	"services.*.deploy.endpoint_mode":    {"deploy.endpoint_mode", "Only Swarm's routing mesh has an endpoint mode. Remove the key."},
	"services.*.deploy.placement":        {"deploy.placement", "A standalone host runs the container on itself. Remove the key."},
	"services.*.credential_spec":         {"credential_spec", "It only applies to Windows containers on a Swarm. Remove the key."},
	"configs.*.labels":                   {"configs[].labels", "Only Swarm config objects carry labels. Remove the key."},
	"secrets.*.driver_opts":              {"secrets[].driver_opts", "Only a Swarm secret driver reads it. Remove the key."},
	"secrets.*.labels":                   {"secrets[].labels", "Only Swarm secret objects carry labels. Remove the key."},
	"services.*.ports.[].mode=host":      {"ports[].mode: host", "A standalone host publishes the port on the host either way. Remove the mode."},
	"services.*.volumes.[].type=cluster": {"volumes[].type: cluster", "Cluster volumes need a Swarm. Use a named volume."},
	"services.*.configs.[].uid":          {"configs[].uid", "A standalone host copies the file in as it is. Remove the key."},
	"services.*.configs.[].gid":          {"configs[].gid", "A standalone host copies the file in as it is. Remove the key."},
	"services.*.configs.[].mode":         {"configs[].mode", "A standalone host copies the file in as it is. Remove the key."},
	"services.*.secrets.[].uid":          {"secrets[].uid", "A standalone host copies the file in as it is. Remove the key."},
	"services.*.secrets.[].gid":          {"secrets[].gid", "A standalone host copies the file in as it is. Remove the key."},
	"services.*.secrets.[].mode":         {"secrets[].mode", "A standalone host copies the file in as it is. Remove the key."},
}

// standaloneFindings reports what a standalone host ignores or refuses: every attribute of
// StandaloneUnsupported the file sets is a warning at its line, and a read-only service that
// needs a file written into it is an error.
func standaloneFindings(in Input) []Finding {
	var out []Finding
	for _, pattern := range StandaloneUnsupported {
		advice, ok := standaloneAdvice[pattern]
		if !ok || ownerRefused[pattern] {
			continue // the owner of a secret or config is an error of its own, below
		}
		for _, path := range expand(in.Index, strings.Split(pattern, "."), "") {
			pos, key := in.at(path)
			out = append(out, Finding{
				Severity: SeverityWarning, Code: CodeIgnoredOnTarget, Key: key, Pos: pos,
				Message: ignoredMessage(advice.key, TargetStandalone, advice.action),
			})
		}
	}
	out = append(out, readOnlyFindings(in)...)
	out = append(out, apiSocketFindings(in)...)
	out = append(out, unsupportedKeyFindings(in)...)
	return append(out, fileObjectFindings(in)...)
}

// unsupportedKeys are the keys of a service that the Compose SDK carries out by running something the deploy
// must not: a program of the TierPlan server (a provider service runs the executable its type names, a model
// runs the docker model plugin), or a command in the container through exec, which the guard refuses (a
// lifecycle hook). Each is an error, whatever the person's role.
var unsupportedKeys = []struct{ key, why string }{
	{"provider", "A provider service runs a program on the TierPlan server."},
	{"models", "A model is pulled and served by a program on the TierPlan server."},
	{"pre_start", "A lifecycle hook runs a command in a container, which TierPlan doesn't allow. Run the step in the entrypoint."},
	{"post_start", "A lifecycle hook runs a command in a container, which TierPlan doesn't allow. Run the step in the entrypoint."},
	{"pre_stop", "A lifecycle hook runs a command in a container, which TierPlan doesn't allow. Run the step in the entrypoint."},
}

// unsupportedKeyFindings reports the unsupportedKeys a service sets, and the top-level models.
func unsupportedKeyFindings(in Input) []Finding {
	var out []Finding
	flag := func(path, key, why string) {
		pos, at := in.at(path)
		out = append(out, Finding{
			Severity: SeverityError, Code: CodeStandaloneUnsupported, Key: at, Pos: pos,
			Message: fmt.Sprintf(messages[CodeStandaloneUnsupported], key, why),
		})
	}
	for _, name := range serviceNames(in.Project) {
		svc := in.Project.Services[name]
		set := map[string]bool{
			"provider": svc.Provider != nil, "models": len(svc.Models) > 0,
			"pre_start": len(svc.PreStart) > 0, "post_start": len(svc.PostStart) > 0, "pre_stop": len(svc.PreStop) > 0,
		}
		for _, k := range unsupportedKeys {
			if set[k.key] {
				flag("services."+name+"."+k.key, k.key, k.why)
			}
		}
	}
	if len(in.Project.Models) > 0 {
		flag("models", "models", unsupportedKeys[1].why)
	}
	return out
}

// apiSocketFindings reports use_api_socket: the Compose SDK turns it into a bind mount of the host's Docker
// socket with the registry logins of its configuration beside it, which no host-access rule sees, because
// the rules judge a socket written as a bind volume. It is an error whatever the person's role is.
func apiSocketFindings(in Input) []Finding {
	var out []Finding
	for _, name := range serviceNames(in.Project) {
		if !in.Project.Services[name].UseAPISocket {
			continue
		}
		pos, key := in.at("services."+name+".use_api_socket", "services."+name)
		out = append(out, Finding{Severity: SeverityError, Code: CodeUseAPISocket, Key: key, Pos: pos, Message: messages[CodeUseAPISocket]})
	}
	return out
}

// ownerRefused are the patterns of StandaloneUnsupported that are an error and not a warning here. The SDK
// ignores a uid or gid outside a swarm (its unsupported_attributes.go), but it also sends them to the engine
// with CopyUIDGID when they are set, and the agent's archive route refuses that request (plan 07-04, T-07-16),
// so a deploy through an agent would fail where the warning said it only had no effect.
var ownerRefused = map[string]bool{
	"services.*.configs.[].uid": true, "services.*.configs.[].gid": true,
	"services.*.secrets.[].uid": true, "services.*.secrets.[].gid": true,
}

// fileObjectFindings reports what the agent's archive route would refuse to write into a container for a
// secret or config a service uses (plan 07-04): an owner (uid or gid), a target the tar header cannot hold
// (an extended record is refused), and an external object, which has no content to write (the SDK itself
// refuses it). They are errors: a deploy would stop at the services step.
func fileObjectFindings(in Input) []Finding {
	var out []Finding
	for _, name := range serviceNames(in.Project) {
		svc := in.Project.Services[name]
		for i, ref := range svc.Secrets {
			out = append(out, fileRefFindings(in, name, "secrets", "secret", i, types.FileReferenceConfig(ref), in.Project.Secrets[ref.Source].External)...)
		}
		for i, ref := range svc.Configs {
			out = append(out, fileRefFindings(in, name, "configs", "config", i, types.FileReferenceConfig(ref), in.Project.Configs[ref.Source].External)...)
		}
	}
	return out
}

func fileRefFindings(in Input, service, section, kind string, i int, ref types.FileReferenceConfig, external types.External) []Finding {
	var out []Finding
	base := "services." + service + "." + section
	pos, path := in.at(base+"."+strconv.Itoa(i), base, "services."+service)
	for _, field := range []struct{ name, value string }{{"uid", ref.UID}, {"gid", ref.GID}} {
		if field.value == "" {
			continue
		}
		key := section + "[]." + field.name
		fpos, fpath := in.at(base+"."+strconv.Itoa(i)+"."+field.name, base+"."+strconv.Itoa(i), base)
		out = append(out, Finding{
			Severity: SeverityError, Code: CodeFileObjectOwner, Key: fpath, Pos: fpos,
			Message: fmt.Sprintf(messages[CodeFileObjectOwner], key, kind),
		})
	}
	if bool(external) {
		out = append(out, Finding{
			Severity: SeverityError, Code: CodeExternalFileObject, Key: path, Pos: pos,
			Message: fmt.Sprintf(messages[CodeExternalFileObject], kind, ref.Source),
		})
		return out
	}
	if target := fileObjectTarget(kind, ref); !tarNameFits(target) {
		out = append(out, Finding{
			Severity: SeverityError, Code: CodeFileObjectName, Key: path, Pos: pos,
			Message: fmt.Sprintf(messages[CodeFileObjectName], service, kind, ref.Source, target),
		})
	}
	return out
}

// fileObjectTarget is the path the Compose SDK writes a secret or config to: the name of the entry in the
// tar it sends (secrets.go in docker/compose v5.6.0, Apache-2.0). A secret defaults to /run/secrets/<source>
// and a relative target is put under /run/secrets/; a config defaults to /<source>, and a relative target is
// put under /.
func fileObjectTarget(kind string, ref types.FileReferenceConfig) string {
	base := "/"
	if kind == "secret" {
		base = "/run/secrets/"
	}
	switch {
	case ref.Target == "":
		return base + ref.Source
	case !strings.HasPrefix(ref.Target, "/"):
		return base + ref.Target
	}
	return ref.Target
}

// tarNameFits reports whether a tar header can hold name without an extended record. The SDK writes the
// header with Go's archive/tar, which uses the plain USTAR form while the name fits it and an extended (PAX)
// record when it does not, and the agent refuses any extended record; the same writer and reader decide
// here, so the answer is the agent's.
func tarNameFits(name string) bool {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Size: 1, Mode: 0o444}); err != nil {
		return false
	}
	if _, err := tw.Write([]byte{0}); err != nil || tw.Close() != nil {
		return false
	}
	h, err := tar.NewReader(&buf).Next()
	return err == nil && len(h.PAXRecords) == 0
}

// expand lists the concrete dotted paths a pattern names in the file. "*" and "[]" take each key or
// item at that level; a last segment key=value names the path only when its scalar is value.
func expand(ix Index, segs []string, prefix string) []string {
	if len(segs) == 0 {
		return nil
	}
	seg, rest := segs[0], segs[1:]
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	switch {
	case seg == "*" || seg == "[]":
		var out []string
		for _, k := range ix.Keys(prefix) {
			out = append(out, expand(ix, rest, join(k))...)
		}
		return out
	case len(rest) == 0:
		key, want, valued := strings.Cut(seg, "=")
		path := join(key)
		if valued {
			if v, ok := ix.Scalar(path); ok && v == want {
				return []string{path}
			}
			return nil
		}
		if _, ok := ix.Lookup(path); ok {
			return []string{path}
		}
		return nil
	default:
		return expand(ix, rest, join(seg))
	}
}

// readOnlyFindings reports a read_only service that uses a secret or config of the stack. A
// standalone host cannot mount those the way a swarm does: Compose copies the content into the
// container, which a read-only container refuses (the SDK's injectFileReferences, secrets.go in
// docker/compose v5.6.0, Apache-2.0). An external object has no content to copy, so it is fine.
func readOnlyFindings(in Input) []Finding {
	var out []Finding
	for _, name := range serviceNames(in.Project) {
		svc := in.Project.Services[name]
		if !svc.ReadOnly {
			continue
		}
		pos, path := in.at("services."+name+".read_only", "services."+name)
		flag := func(kind, object string) {
			out = append(out, Finding{
				Severity: SeverityError, Code: CodeReadOnlyFileObject, Key: path, Pos: pos,
				Message: fmt.Sprintf(messages[CodeReadOnlyFileObject], name, kind, object),
			})
		}
		for _, ref := range svc.Secrets {
			if o, ok := in.Project.Secrets[ref.Source]; ok && writtenIn(types.FileObjectConfig(o)) {
				flag("secret", ref.Source)
			}
		}
		for _, ref := range svc.Configs {
			if o, ok := in.Project.Configs[ref.Source]; ok && writtenIn(types.FileObjectConfig(o)) {
				flag("config", ref.Source)
			}
		}
	}
	return out
}

// writtenIn reports whether an object's content is something TierPlan has to put into the container.
func writtenIn(o types.FileObjectConfig) bool {
	return !bool(o.External) && (o.File != "" || o.Content != "" || o.Environment != "")
}
