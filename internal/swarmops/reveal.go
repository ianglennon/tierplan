// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package swarmops

import (
	"errors"
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/dockerx"
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
// The engine's secret ID is the only identity (review finding CR-01). A reference with a SecretID is
// that ID, and its SecretName is never read: the name in a spec is caller-writable, and trusting the
// name of a current reference let a caller keep a used ID under another secret's name and then name
// that secret alone. A reference with only a SecretName is the ID that name has in have, the live
// secret list. A next reference is added unless its ID is the ID of a current one; a name the swarm
// does not have (a secret staged in the editor) and a reference with neither are added. A nil
// container spec or reference holds no secret.
func AddedSecrets(current, next swarm.ServiceSpec, have []dockerx.SecretRow) []AddedSecret {
	cs := next.TaskTemplate.ContainerSpec
	if cs == nil {
		return nil
	}
	used := map[string]bool{}
	if cur := current.TaskTemplate.ContainerSpec; cur != nil {
		for _, ref := range cur.Secrets {
			if id := secretIDOf(ref, have); id != "" {
				used[id] = true
			}
		}
	}
	var added []AddedSecret
	for i, ref := range cs.Secrets {
		if ref == nil {
			continue
		}
		if id := secretIDOf(ref, have); id != "" && used[id] {
			continue
		}
		name := ref.SecretName
		if name == "" {
			name = ref.SecretID
		}
		added = append(added, AddedSecret{Path: secretPath(i), Name: name})
	}
	return added
}

// MismatchedSecrets lists, in reference order, the secret references of spec that carry both a
// SecretID and a SecretName where have holds that ID under another name. A name that is not its
// secret's would mislead everyone who reads the spec, so it is never stored. An ID that have does not
// hold is not a mismatch: the spec names an object the swarm lacks, which objects_missing reports.
func MismatchedSecrets(spec swarm.ServiceSpec, have []dockerx.SecretRow) []AddedSecret {
	cs := spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return nil
	}
	var out []AddedSecret
	for i, ref := range cs.Secrets {
		if ref == nil || ref.SecretID == "" || ref.SecretName == "" {
			continue
		}
		for _, row := range have {
			if row.ID == ref.SecretID && row.Name != ref.SecretName {
				out = append(out, AddedSecret{Path: secretPath(i), Name: ref.SecretName})
				break
			}
		}
	}
	return out
}

// JudgeSecrets is the one secret rule of every server path that writes a spec. For every caller, a
// reference whose SecretName is not the name the engine holds for its SecretID is refused as a
// *SpecInvalidError. Then, for a caller without secrets.reveal, a secret reference the current spec
// does not use is refused as a *RevealRequiredError. have is the live secret list. Keeping, removing
// or re-targeting a secret the service uses stays allowed (D-13 as amended).
func JudgeSecrets(current, next swarm.ServiceSpec, have []dockerx.SecretRow, reveal bool) error {
	if bad := MismatchedSecrets(next, have); len(bad) > 0 {
		parts := make([]string, len(bad))
		for i, b := range bad {
			parts[i] = fmt.Sprintf("%s: the SecretName %s is not the name of the secret its SecretID points at", b.Path, b.Name)
		}
		return &SpecInvalidError{Message: strings.Join(parts, "; ")}
	}
	if !reveal {
		if added := AddedSecrets(current, next, have); len(added) > 0 {
			return &RevealRequiredError{Secrets: added}
		}
	}
	return nil
}

// secretIDOf is the engine ID a reference stands for: its SecretID, else the ID its SecretName has in
// have, else empty.
func secretIDOf(ref *swarm.SecretReference, have []dockerx.SecretRow) string {
	if ref == nil {
		return ""
	}
	if ref.SecretID != "" {
		return ref.SecretID
	}
	if ref.SecretName == "" {
		return ""
	}
	for _, row := range have {
		if row.Name == ref.SecretName {
			return row.ID
		}
	}
	return ""
}

func secretPath(i int) string { return fmt.Sprintf("TaskTemplate.ContainerSpec.Secrets[%d]", i) }
