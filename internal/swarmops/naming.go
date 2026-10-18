// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops

import (
	"regexp"
	"strconv"

	"github.com/moby/moby/api/types/swarm"
)

// maxObjectName is the longest name a swarm secret or config may have.
const maxObjectName = 64

// versionSuffix is the trailing _v<digits> a rotated object's name carries.
var versionSuffix = regexp.MustCompile(`^(.*)_v([0-9]{1,9})$`)

// nextVersionName is the name of the next version of an object called old: {base}_v{n}, where {base} is
// old without a trailing _v<digits> and {n} is the lowest integer above old's own version (1 when it has
// none) that taken says is free. An object that is not versioned yet therefore goes to _v2, and
// db_password_v2 with db_password_v3 taken goes to _v4. A number too large to be a version is part of the
// base. The base is trimmed so that the result fits the 64 characters a name may have.
func nextVersionName(old string, taken func(string) bool) string {
	base, version := old, 1
	if m := versionSuffix.FindStringSubmatch(old); m != nil {
		if v, err := strconv.Atoi(m[2]); err == nil {
			base, version = m[1], v
		}
	}
	for n := version + 1; ; n++ {
		suffix := "_v" + strconv.Itoa(n)
		name := base
		if room := maxObjectName - len(suffix); len(name) > room {
			name = name[:room]
		}
		name += suffix
		if !taken(name) {
			return name
		}
	}
}

// replaceReference points every reference of the spec's container spec to the secret or config oldID at
// the object newID called newName, keeping the file name or path, the owner, the group and the mode (or a
// config's runtime target), and returns how many references it changed. kind is KindSecret or KindConfig;
// another kind changes nothing. The spec's references are changed in place, so the caller passes a copy it
// owns.
func replaceReference(spec *swarm.ServiceSpec, kind, oldID, newID, newName string) int {
	cs := spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return 0
	}
	n := 0
	switch kind {
	case KindSecret:
		for _, s := range cs.Secrets {
			if s != nil && s.SecretID == oldID {
				s.SecretID, s.SecretName = newID, newName
				n++
			}
		}
	case KindConfig:
		for _, c := range cs.Configs {
			if c != nil && c.ConfigID == oldID {
				c.ConfigID, c.ConfigName = newID, newName
				n++
			}
		}
	}
	return n
}
