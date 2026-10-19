// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package lint

import (
	"fmt"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// StandaloneUnsupported is the compose attributes a standalone host ignores. It is Bayplan's copy
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
		if !ok {
			continue
		}
		for _, path := range expand(in.Index, strings.Split(pattern, "."), "") {
			pos, key := in.at(path)
			out = append(out, Finding{
				Severity: SeverityWarning, Code: CodeIgnoredOnTarget, Key: key, Pos: pos,
				Message: ignoredMessage(advice.key, TargetStandalone, advice.action),
			})
		}
	}
	return append(out, readOnlyFindings(in)...)
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

// writtenIn reports whether an object's content is something Bayplan has to put into the container.
func writtenIn(o types.FileObjectConfig) bool {
	return !bool(o.External) && (o.File != "" || o.Content != "" || o.Environment != "")
}
