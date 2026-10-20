// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

// SecretUse knows which values of a compose file come from a secret variable (stub, replaced by the
// implementation in the next commit).
type SecretUse struct{}

// NewSecretUse reads the working copy's compose file.
func NewSecretUse(WorkingCopy) SecretUse { return SecretUse{} }

// Any reports whether the file uses a secret variable.
func (SecretUse) Any() bool { return false }

// Uses reports whether the value at path comes from a secret variable.
func (SecretUse) Uses(string) bool { return false }

// Quotes reports whether text holds a run of a secret.
func (SecretUse) Quotes(string) bool { return false }

// Withholds is the one decision callers use.
func (SecretUse) Withholds(string, string) bool { return false }

// SecretValueMessage is the fixed sentence for a value that comes from a secret variable.
func SecretValueMessage(path string) string {
	if path == "" {
		return "A value in compose.yaml isn't valid. The file uses a secret variable, so the details aren't shown here, because they could quote it."
	}
	return path + ": this value isn't valid. It comes from a secret variable, so the details aren't shown here, because they could quote it."
}
