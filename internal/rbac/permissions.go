// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac

// Permission is one registered resource.action permission.
type Permission struct {
	Name        Perm
	Scope       Scope
	Dangerous   bool
	Description string
}

// All returns every registered permission.
func All() []Permission { return nil }

// Lookup finds a registered permission by name.
func Lookup(p Perm) (Permission, bool) { return Permission{}, false }

// Registered permission names.
const (
	PermEnvironmentsRead  Perm = "environments.read"
	PermEnvironmentsWrite Perm = "environments.write"
	PermContainersRead    Perm = "containers.read"
	PermImagesRead        Perm = "images.read"
	PermVolumesRead       Perm = "volumes.read"
	PermNetworksRead      Perm = "networks.read"
	PermAuditRead         Perm = "audit.read"
	PermSettingsRead      Perm = "settings.read"
	PermSettingsWrite     Perm = "settings.write"
	PermContainersExec    Perm = "containers.exec"
	PermSecretsReveal     Perm = "secrets.reveal"
	PermVolumesDelete     Perm = "volumes.delete"
	PermJanitorPolicy     Perm = "janitor.policy"
)

// Require returns the Requirement for a registered permission.
func Require(p Perm) Requirement { return Requirement{} }
