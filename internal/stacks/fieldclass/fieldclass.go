// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package fieldclass is the table of what a swarm does with each compose service key. It is a
// package of its own because two packages that cannot import one another read it: convert (which
// imports package stacks) applies it, and lint (which package stacks imports) words it for the
// editor. Both read this one table, so lint and the converter cannot disagree about which keys a
// swarm drops.
package fieldclass

// Class says what the converter does with one compose service field on a swarm.
type Class int

const (
	// Converted fields are carried into the swarm service spec.
	Converted Class = iota + 1
	// IgnoredOnSwarm fields have no swarm equivalent. docker stack deploy ignores them with a
	// warning, so the converter does too and lists them in ServicePlan.Ignored for the lint.
	IgnoredOnSwarm
	// Blocking fields cannot be deployed to a swarm at all: dropping one would change what the
	// stack does, so Convert refuses the service.
	Blocking
)

// Table classifies every field of compose-go's types.ServiceConfig, keyed by the compose
// YAML key. TestEveryServiceFieldIsClassified fails when compose-go adds a field that is not
// here (RESEARCH Pitfall 3), so a new host-access key cannot be dropped without a decision.
//
// The classes follow docker stack deploy (docker/cli cli/compose/types, Apache-2.0): its
// UnsupportedProperties and DeprecatedProperties are ignored with a warning, its
// ForbiddenProperties are refused. Keys compose-go has that the v3.13 schema never had are
// classed by what dropping them would do: a setting that only tunes the container (a CPU
// period, a shm size) is ignored, one that adds work to the service (a lifecycle hook, a
// provider, a model, a file the load never reads) is refused.
//
// A comment on each key that is not converted gives the action the user can take.
var Table = map[string]Class{
	// --- Converted ---------------------------------------------------------------------
	"name":              Converted, // the service name: <stack>_<name>
	"image":             Converted,
	"command":           Converted, // ContainerSpec.Args
	"entrypoint":        Converted, // ContainerSpec.Command
	"environment":       Converted,
	"labels":            Converted, // container labels; deploy.labels become the service's
	"healthcheck":       Converted,
	"networks":          Converted,
	"ports":             Converted,
	"volumes":           Converted,
	"secrets":           Converted,
	"configs":           Converted,
	"credential_spec":   Converted,
	"deploy":            Converted,
	"scale":             Converted, // compose-go copies it to deploy.replicas
	"restart":           Converted, // the fallback when deploy.restart_policy is not set
	"logging":           Converted,
	"stop_grace_period": Converted,
	"stop_signal":       Converted,
	"ulimits":           Converted,
	"sysctls":           Converted,
	"cap_add":           Converted,
	"cap_drop":          Converted,
	"user":              Converted,
	"working_dir":       Converted,
	"hostname":          Converted,
	"extra_hosts":       Converted,
	"dns":               Converted,
	"dns_search":        Converted,
	"dns_opt":           Converted,
	"init":              Converted,
	"read_only":         Converted,
	"tty":               Converted,
	"stdin_open":        Converted,
	"isolation":         Converted,
	"group_add":         Converted,
	"oom_score_adj":     Converted,

	// --- Ignored on a swarm (a warning) --------------------------------------------------
	"build":               IgnoredOnSwarm, // Build and push the image, then set image.
	"dockerfile":          IgnoredOnSwarm, // Build and push the image, then set image.
	"container_name":      IgnoredOnSwarm, // A swarm names its containers; remove the key.
	"expose":              IgnoredOnSwarm, // Services on one network reach every port; remove the key.
	"depends_on":          IgnoredOnSwarm, // A swarm starts services in no order; make the service retry its connection.
	"profiles":            IgnoredOnSwarm, // A service with a profile is not deployed; remove the profile to deploy it.
	"links":               IgnoredOnSwarm, // Use a shared network and the service name.
	"external_links":      IgnoredOnSwarm, // Use a shared network and the service name.
	"network_mode":        IgnoredOnSwarm, // Use networks:, or a host-mode published port.
	"net":                 IgnoredOnSwarm, // The old name of network_mode.
	"mac_address":         IgnoredOnSwarm, // A swarm assigns the address; remove the key.
	"domainname":          IgnoredOnSwarm, // Use hostname and dns_search.
	"privileged":          IgnoredOnSwarm, // A swarm service cannot run privileged; grant cap_add for what it needs.
	"security_opt":        IgnoredOnSwarm, // Not carried by a swarm service.
	"cgroup":              IgnoredOnSwarm, // Not carried by a swarm service.
	"cgroup_parent":       IgnoredOnSwarm, // Not carried by a swarm service.
	"devices":             IgnoredOnSwarm, // A swarm service cannot map a device; use a generic resource.
	"device_cgroup_rules": IgnoredOnSwarm, // Not carried by a swarm service.
	"gpus":                IgnoredOnSwarm, // Use deploy.resources.reservations.generic_resources.
	"ipc":                 IgnoredOnSwarm, // Not carried by a swarm service.
	"pid":                 IgnoredOnSwarm, // Not carried by a swarm service.
	"uts":                 IgnoredOnSwarm, // Not carried by a swarm service.
	"userns_mode":         IgnoredOnSwarm, // Not carried by a swarm service.
	"runtime":             IgnoredOnSwarm, // Not carried by a swarm service.
	"use_api_socket":      IgnoredOnSwarm, // Mount the socket as a bind volume if the service needs it.
	"shm_size":            IgnoredOnSwarm, // Mount a tmpfs volume at /dev/shm with a size.
	"tmpfs":               IgnoredOnSwarm, // Use a volumes entry with type: tmpfs.
	"storage_opt":         IgnoredOnSwarm, // Not carried by a swarm service.
	"platform":            IgnoredOnSwarm, // Use deploy.placement constraints on node.platform.arch.
	"pull_policy":         IgnoredOnSwarm, // A swarm always pulls what the digest names.
	"pull_refresh_after":  IgnoredOnSwarm, // A swarm always pulls what the digest names.
	"annotations":         IgnoredOnSwarm, // Use labels.
	"attach":              IgnoredOnSwarm, // Only meaningful to docker compose up.
	"develop":             IgnoredOnSwarm, // Only meaningful to docker compose watch.
	"log_driver":          IgnoredOnSwarm, // Use logging: driver:.
	"log_opt":             IgnoredOnSwarm, // Use logging: options:.
	"blkio_config":        IgnoredOnSwarm, // Not carried by a swarm service.
	"cpus":                IgnoredOnSwarm, // Use deploy.resources.limits.cpus.
	"cpu_count":           IgnoredOnSwarm, // Use deploy.resources.limits.cpus.
	"cpu_percent":         IgnoredOnSwarm, // Use deploy.resources.limits.cpus.
	"cpu_period":          IgnoredOnSwarm, // Use deploy.resources.limits.cpus.
	"cpu_rt_period":       IgnoredOnSwarm, // Not carried by a swarm service.
	"cpu_rt_runtime":      IgnoredOnSwarm, // Not carried by a swarm service.
	"mem_reservation":     IgnoredOnSwarm, // Use deploy.resources.reservations.memory.
	"mem_swappiness":      IgnoredOnSwarm, // Not carried by a swarm service.
	"oom_kill_disable":    IgnoredOnSwarm, // Not carried by a swarm service.
	"pids_limit":          IgnoredOnSwarm, // Use deploy.resources.limits.pids.

	// --- Blocking (the service is refused) ---------------------------------------------
	"extends":       Blocking, // Bayplan does not follow extends; write the service out in full.
	"volumes_from":  Blocking, // Define the volume under volumes: and mount it in each service.
	"volume_driver": Blocking, // Define the volume under volumes: with its driver.
	"cpu_quota":     Blocking, // Use deploy.resources.limits.cpus.
	"cpu_shares":    Blocking, // Use deploy.resources.limits.cpus.
	"cpuset":        Blocking, // Use deploy.placement constraints.
	"mem_limit":     Blocking, // Use deploy.resources.limits.memory.
	"memswap_limit": Blocking, // Use deploy.resources.limits.memory.
	"env_file":      Blocking, // Bayplan reads no file from disk; put the variables under environment: or in the stack's variables.
	"label_file":    Blocking, // Bayplan reads no file from disk; put the labels under labels:.
	"pre_start":     Blocking, // A swarm has no lifecycle hooks; run the step in the entrypoint.
	"post_start":    Blocking, // A swarm has no lifecycle hooks; run the step in the entrypoint.
	"pre_stop":      Blocking, // A swarm has no lifecycle hooks; handle the stop signal in the service.
	"provider":      Blocking, // A provider service is not a container; deploy it with docker compose.
	"models":        Blocking, // Compose models need docker compose; a swarm has none.
}
