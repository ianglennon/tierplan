// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package swarmops

import (
	"errors"

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

// AddedSecrets is not written yet.
func AddedSecrets(current, next swarm.ServiceSpec) []AddedSecret { return nil }
