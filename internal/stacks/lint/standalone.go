// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package lint

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

// standaloneFindings reports what a standalone host ignores or refuses.
func standaloneFindings(in Input) []Finding {
	return nil
}
