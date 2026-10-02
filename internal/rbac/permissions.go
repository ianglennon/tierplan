// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac

// Permission is one registered resource.action permission (D-46). The Go
// registry below is the only place permissions are defined; the database holds
// which roles are granted which names, never what a name means.
type Permission struct {
	Name  Perm
	Scope Scope
	// Dangerous marks permissions Phase 3's AUTH-07 treats specially: exec,
	// secret reveal, volume delete and janitor policy.
	Dangerous   bool
	Description string
}

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

// registry lists every permission. The four Dangerous entries are registered
// now although no Phase 1 route uses them, so Phase 3 can rely on the flag.
var registry = []Permission{
	{Name: PermEnvironmentsRead, Scope: ScopeEnvironment, Description: "View an environment"},
	{Name: PermEnvironmentsWrite, Scope: ScopeGlobal, Description: "Add, rename and remove environments"},
	{Name: PermContainersRead, Scope: ScopeEnvironment, Description: "View containers"},
	{Name: PermImagesRead, Scope: ScopeEnvironment, Description: "View images"},
	{Name: PermVolumesRead, Scope: ScopeEnvironment, Description: "View volumes"},
	{Name: PermNetworksRead, Scope: ScopeEnvironment, Description: "View networks"},
	{Name: PermAuditRead, Scope: ScopeGlobal, Description: "Read the audit log"},
	{Name: PermSettingsRead, Scope: ScopeGlobal, Description: "View settings"},
	{Name: PermSettingsWrite, Scope: ScopeGlobal, Description: "Change settings"},
	{Name: PermContainersExec, Scope: ScopeEnvironment, Dangerous: true, Description: "Open a shell in a container"},
	{Name: PermSecretsReveal, Scope: ScopeEnvironment, Dangerous: true, Description: "Reveal secret values"},
	{Name: PermVolumesDelete, Scope: ScopeEnvironment, Dangerous: true, Description: "Delete volumes"},
	{Name: PermJanitorPolicy, Scope: ScopeEnvironment, Dangerous: true, Description: "Change cleanup policy"},
}

// All returns a copy of every registered permission, in registry order.
func All() []Permission {
	return append([]Permission(nil), registry...)
}

// Lookup finds a registered permission by name.
func Lookup(p Perm) (Permission, bool) {
	for _, r := range registry {
		if r.Name == p {
			return r, true
		}
	}
	return Permission{}, false
}

// Require returns the Requirement for a registered permission, with the scope
// the registry declares. It panics for an unknown name, so a typo fails at
// startup.
func Require(p Perm) Requirement {
	reg, ok := Lookup(p)
	if !ok {
		panic("rbac: permission " + string(p) + " is not registered")
	}
	return Requirement{Perm: reg.Name, Scope: reg.Scope}
}
