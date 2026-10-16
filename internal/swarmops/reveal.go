// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops

import (
	"errors"
	"fmt"

	"github.com/moby/moby/api/types/swarm"
)

// ErrRevealRequired means the spec adds a secret reference the service does not have, and the caller
// does not hold secrets.reveal. The error is a *RevealRequiredError.
var ErrRevealRequired = errors.New("swarmops: adding a secret to a service needs secrets.reveal")

// AddedSecret is a secret reference a spec adds: its path in the spec and the name the spec used, or
// the ID when it used no name. Never a value.
type AddedSecret struct{ Path, Name string }

// RevealRequiredError lists the secret references a caller without secrets.reveal may not add.
type RevealRequiredError struct{ Secrets []AddedSecret }

func (e *RevealRequiredError) Error() string {
	return "swarmops: adding a secret to a service needs secrets.reveal"
}
func (e *RevealRequiredError) Is(target error) bool { return target == ErrRevealRequired }

// AddedSecrets lists, in reference order, the secret references of next that current does not have.
// A caller without secrets.reveal who could attach any secret to a service could then change what the
// service runs to print it (D-13 as amended after review finding S1-WR-02), so such a caller may only
// send a spec whose secrets the service already uses. Keeping a reference, removing one, or changing
// the file name, UID, GID or mode of one the service uses adds nothing.
//
// A reference with a SecretID is the same secret as a current reference with that SecretID. One
// without matches a current reference with the same SecretName: a name is unique in a swarm while the
// secret exists, and Docker will not delete a secret a service uses. A reference with neither is
// always added. A nil container spec or reference holds no secret.
func AddedSecrets(current, next swarm.ServiceSpec) []AddedSecret {
	var have []*swarm.SecretReference
	if cs := current.TaskTemplate.ContainerSpec; cs != nil {
		have = cs.Secrets
	}
	cs := next.TaskTemplate.ContainerSpec
	if cs == nil {
		return nil
	}
	var added []AddedSecret
	for i, ref := range cs.Secrets {
		if ref == nil || usesSecret(have, ref) {
			continue
		}
		name := ref.SecretName
		if name == "" {
			name = ref.SecretID
		}
		added = append(added, AddedSecret{Path: fmt.Sprintf("TaskTemplate.ContainerSpec.Secrets[%d]", i), Name: name})
	}
	return added
}

// usesSecret says whether one of have is the secret ref names.
func usesSecret(have []*swarm.SecretReference, ref *swarm.SecretReference) bool {
	for _, h := range have {
		switch {
		case h == nil:
		case ref.SecretID != "":
			if h.SecretID == ref.SecretID {
				return true
			}
		case ref.SecretName != "" && h.SecretName == ref.SecretName:
			return true
		}
	}
	return false
}
