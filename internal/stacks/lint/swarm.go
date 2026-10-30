// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package lint

import (
	"sort"

	"github.com/ianglennon/coaming/internal/stacks/fieldclass"
)

// swarmActions is what to do about each compose service key that a swarm ignores or refuses, the
// second half of the sentence "`{key}` is ignored on Swarm. {what to do}". The keys are those of
// fieldclass.Table (the table convert.FieldClasses is; it lives in package fieldclass because
// convert imports package stacks, which imports lint), which decides whether a key is a warning
// or an error, so lint and the converter cannot disagree about which keys a swarm drops. TestEverySwarmKeyHasAnAction fails
// for a key the converter classifies and this table does not word.
//
// restart, scale, deploy and the other Converted keys are not here: a swarm applies them.
var swarmActions = map[string]string{
	// Ignored on a swarm (a warning).
	"build":               "Build and push the image, then set `image`.",
	"dockerfile":          "Build and push the image, then set `image`.",
	"container_name":      "A swarm names its containers. Remove the key.",
	"expose":              "Services on one network reach every port. Remove the key.",
	"depends_on":          "A swarm starts services in no order. Make the service retry its connection.",
	"profiles":            "A service with a profile is not deployed. Remove the profile to deploy it.",
	"links":               "Use a shared network and the service name.",
	"external_links":      "Use a shared network and the service name.",
	"network_mode":        "Use `networks`, or a host-mode published port.",
	"net":                 "Use `networks`, or a host-mode published port.",
	"mac_address":         "A swarm assigns the address. Remove the key.",
	"domainname":          "Use `hostname` and `dns_search`.",
	"privileged":          "A swarm service can't run privileged. Grant `cap_add` for what it needs.",
	"security_opt":        "A swarm service can't set it. Remove the key.",
	"cgroup":              "A swarm service can't set it. Remove the key.",
	"cgroup_parent":       "A swarm service can't set it. Remove the key.",
	"devices":             "A swarm service can't map a device. Use a generic resource.",
	"device_cgroup_rules": "A swarm service can't set it. Remove the key.",
	"gpus":                "Use `deploy.resources.reservations.generic_resources`.",
	"ipc":                 "A swarm service can't set it. Remove the key.",
	"pid":                 "A swarm service can't set it. Remove the key.",
	"uts":                 "A swarm service can't set it. Remove the key.",
	"userns_mode":         "A swarm service can't set it. Remove the key.",
	"runtime":             "A swarm service can't set it. Remove the key.",
	"use_api_socket":      "Mount the socket as a bind volume if the service needs it.",
	"shm_size":            "Mount a tmpfs volume at /dev/shm with a size.",
	"tmpfs":               "Use a `volumes` entry with `type: tmpfs`.",
	"storage_opt":         "A swarm service can't set it. Remove the key.",
	"platform":            "Use a `deploy.placement` constraint on node.platform.arch.",
	"pull_policy":         "A swarm always pulls what the image names. Remove the key.",
	"pull_refresh_after":  "A swarm always pulls what the image names. Remove the key.",
	"annotations":         "Use `labels`.",
	"attach":              "It only applies to docker compose up. Remove the key.",
	"develop":             "It only applies to docker compose watch. Remove the key.",
	"log_driver":          "Use `logging` with `driver`.",
	"log_opt":             "Use `logging` with `options`.",
	"blkio_config":        "A swarm service can't set it. Remove the key.",
	"cpus":                "Use `deploy.resources.limits.cpus`.",
	"cpu_count":           "Use `deploy.resources.limits.cpus`.",
	"cpu_percent":         "Use `deploy.resources.limits.cpus`.",
	"cpu_period":          "Use `deploy.resources.limits.cpus`.",
	"cpu_rt_period":       "A swarm service can't set it. Remove the key.",
	"cpu_rt_runtime":      "A swarm service can't set it. Remove the key.",
	"mem_reservation":     "Use `deploy.resources.reservations.memory`.",
	"mem_swappiness":      "A swarm service can't set it. Remove the key.",
	"oom_kill_disable":    "A swarm service can't set it. Remove the key.",
	"pids_limit":          "Use `deploy.resources.limits.pids`.",

	// Blocking (an error).
	"extends":       "Write the service out in full.",
	"volumes_from":  "Define the volume under `volumes` and mount it in each service.",
	"volume_driver": "Define the volume under `volumes` with its driver.",
	"cpu_quota":     "Use `deploy.resources.limits.cpus`.",
	"cpu_shares":    "Use `deploy.resources.limits.cpus`.",
	"cpuset":        "Use `deploy.placement` constraints.",
	"mem_limit":     "Use `deploy.resources.limits.memory`.",
	"memswap_limit": "Use `deploy.resources.limits.memory`.",
	"env_file":      "Put the variables under `environment`, or in the stack's variables.",
	"label_file":    "Put the labels under `labels`.",
	"pre_start":     "A swarm has no lifecycle hooks. Run the step in the entrypoint.",
	"post_start":    "A swarm has no lifecycle hooks. Run the step in the entrypoint.",
	"pre_stop":      "A swarm has no lifecycle hooks. Handle the stop signal in the service.",
	"provider":      "A provider service is not a container. Deploy it with docker compose.",
	"models":        "Compose models need docker compose. A swarm has none.",
}

// handledElsewhere are keys the table classes Blocking that lint leaves to another step. env_file
// is checked by the prescan (it must name a file of the stack, unless it is optional) and is read
// by Coaming from the stack's own files in stacks.ResolveEnvFiles, which empties EnvFiles before
// any deploy sees the project. It stays Blocking in the converter's table on purpose: a project
// that reaches the converter with an env_file still on it was not resolved, and is refused rather
// than deployed with the file ignored. TestEnvFileIsLeftToThePrescanUntilItIsClassedConverted pins
// the exemption so that, if the key is ever reclassified, it cannot outlive that change unnoticed.
var handledElsewhere = map[string]bool{"env_file": true}

// swarmFindings reports, for each service, every key the file sets that fieldclass.Table says
// a swarm ignores (a warning at the key's line) or cannot deploy at all (an error). The keys come
// from the stored YAML, so the line is the key's own, and a key written through a merge key or an
// anchor is found where it is written.
//
// build is left to buildRules: whether it is a warning or an error depends on the image.
func swarmFindings(in Input) []Finding {
	var out []Finding
	for _, name := range serviceNames(in.Project) {
		base := "services." + name
		keys := in.Index.Keys(base)
		sort.Strings(keys)
		for _, key := range keys {
			class, ok := fieldclass.Table[key]
			if !ok || class == fieldclass.Converted || key == "build" || handledElsewhere[key] {
				continue
			}
			pos, path := in.at(base+"."+key, base)
			f := Finding{Key: path, Pos: pos}
			if class == fieldclass.Blocking {
				f.Severity, f.Code = SeverityError, CodeBlockingKey
				f.Message = blockingMessage(key)
			} else {
				f.Severity, f.Code = SeverityWarning, CodeIgnoredOnTarget
				f.Message = ignoredMessage(key, TargetSwarm, swarmActions[key])
			}
			out = append(out, f)
		}
	}
	return out
}
