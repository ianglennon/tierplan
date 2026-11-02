// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package rbac

// Permission is one registered resource.action permission (D-46). The Go
// registry below is the only place permissions and role grants are defined: a
// role holds a permission when it is at least the permission's MinRole, so the
// database never says what a role may do (D-15b).
type Permission struct {
	Name  Perm
	Scope Scope
	// MinRole is the lowest environment role that holds the permission. It is
	// RoleNone for a global permission, which no environment role holds: only an
	// instance admin does (D-01).
	MinRole Role
	// Extra marks a permission outside the role matrix that an Operator holds
	// only when the assignment carries it explicitly (D-15). An Admin holds it
	// always and a Viewer never.
	Extra bool
	// Dangerous marks permissions AUTH-07 treats specially: exec, secret
	// reveal, volume delete, janitor policy, and the instance-admin-only
	// tokens, deletes and backups.
	Dangerous   bool
	Description string
}

// Registered permission names.
const (
	PermEnvironmentsRead      Perm = "environments.read"
	PermEnvironmentsCreate    Perm = "environments.create"
	PermEnvironmentsManage    Perm = "environments.manage"
	PermEnvironmentsToken     Perm = "environments.token"
	PermEnvironmentsRevoke    Perm = "environments.revoke"
	PermEnvironmentsDelete    Perm = "environments.delete"
	PermEnvironmentsConfigure Perm = "environments.configure"
	PermEnvironmentsAccess    Perm = "environments.access"
	PermEnvironmentsAudit     Perm = "environments.audit"
	PermContainersRead        Perm = "containers.read"
	PermImagesRead            Perm = "images.read"
	PermImagesPull            Perm = "images.pull"
	PermImagesDelete          Perm = "images.delete"
	PermVolumesRead           Perm = "volumes.read"
	PermNetworksRead          Perm = "networks.read"
	PermContainersControl     Perm = "containers.control"
	PermContainersRemove      Perm = "containers.remove"
	PermNetworksManage        Perm = "networks.manage"
	PermNetworksKeep          Perm = "networks.keep"
	PermNetworksAddressing    Perm = "networks.addressing"
	PermNetworksConnect       Perm = "networks.connect"
	PermVolumesCreate         Perm = "volumes.create"
	PermAuditRead             Perm = "audit.read"
	PermSettingsRead          Perm = "settings.read"
	PermSettingsWrite         Perm = "settings.write"
	PermUsersManage           Perm = "users.manage"
	PermTeamsManage           Perm = "teams.manage"
	PermBackupManage          Perm = "backup.manage"
	PermContainersExec        Perm = "containers.exec"
	PermSecretsReveal         Perm = "secrets.reveal"
	PermVolumesDelete         Perm = "volumes.delete"
	PermJanitorPolicy         Perm = "janitor.policy"
	PermShellsManage          Perm = "shells.manage"
	PermRegistriesInstance    Perm = "registries.instance"
	PermRegistriesManage      Perm = "registries.manage"
	PermServicesRead          Perm = "services.read"
	PermServicesControl       Perm = "services.control"
	PermServicesManage        Perm = "services.manage"
	PermNodesRead             Perm = "nodes.read"
	PermNodesManage           Perm = "nodes.manage"
	PermSecretsRead           Perm = "secrets.read"
	PermSecretsManage         Perm = "secrets.manage"
	PermConfigsRead           Perm = "configs.read"
	PermConfigsManage         Perm = "configs.manage"
	PermStacksRead            Perm = "stacks.read"
	PermStacksUpdate          Perm = "stacks.update"
	PermStacksDeploy          Perm = "stacks.deploy"
	PermStacksCreate          Perm = "stacks.create"
	PermStacksDelete          Perm = "stacks.delete"
	PermGitRead               Perm = "git.read"
	PermGitManage             Perm = "git.manage"
	PermGitInstance           Perm = "git.instance"
)

// Registry is a list of permissions. The Checker consults one, so a test can
// give it a registry of its own; production uses DefaultRegistry.
type Registry []Permission

// defaultRegistry lists every permission. environments.read stays scoped to one
// environment, so the overview and the global health stream show only what the
// caller may read. The enrolment permissions that Phase 2 made global
// (create, manage, token, revoke, delete) stay global until the plan that
// rescopes their routes; environments.configure, .access and .audit are the
// environment-scoped ones an environment Admin holds (D-07, D-12).
var defaultRegistry = Registry{
	{Name: PermEnvironmentsRead, Scope: ScopeEnvironment, MinRole: RoleViewer, Description: "View an environment"},
	{Name: PermEnvironmentsCreate, Scope: ScopeGlobal, Description: "Add environments"},
	{Name: PermEnvironmentsManage, Scope: ScopeGlobal, Description: "Fold the local host into an environment"},
	{Name: PermEnvironmentsToken, Scope: ScopeGlobal, Dangerous: true, Description: "Show or rotate an environment's join token"},
	{Name: PermEnvironmentsRevoke, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Revoke, block and unblock this environment's agents"},
	{Name: PermEnvironmentsDelete, Scope: ScopeGlobal, Dangerous: true, Description: "Remove environments"},
	{Name: PermEnvironmentsConfigure, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Rename an environment, manage its agents and auto-update, adopt a changed cluster"},
	{Name: PermEnvironmentsAccess, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "See and change who has access to an environment"},
	{Name: PermEnvironmentsAudit, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Read the audit log for an environment"},
	{Name: PermContainersRead, Scope: ScopeEnvironment, MinRole: RoleViewer, Description: "View containers"},
	{Name: PermImagesRead, Scope: ScopeEnvironment, MinRole: RoleViewer, Description: "View images"},
	{Name: PermVolumesRead, Scope: ScopeEnvironment, MinRole: RoleViewer, Description: "View volumes"},
	{Name: PermNetworksRead, Scope: ScopeEnvironment, MinRole: RoleViewer, Description: "View networks"},
	{Name: PermContainersControl, Scope: ScopeEnvironment, MinRole: RoleOperator, Description: "Start, stop, restart, kill, pause and unpause containers"},
	{Name: PermContainersRemove, Scope: ScopeEnvironment, MinRole: RoleOperator, Description: "Remove containers"},
	{Name: PermImagesPull, Scope: ScopeEnvironment, MinRole: RoleOperator, Description: "Pull images"},
	{Name: PermImagesDelete, Scope: ScopeEnvironment, MinRole: RoleOperator, Description: "Remove and untag images"},
	{Name: PermNetworksManage, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Create and delete networks"},
	{Name: PermNetworksKeep, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Keep or stop keeping networks out of the orphan list"},
	{Name: PermNetworksAddressing, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Change the address space settings: small stack subnets and entered address pools"},
	{Name: PermNetworksConnect, Scope: ScopeEnvironment, MinRole: RoleOperator, Description: "Connect and disconnect containers and networks"},
	{Name: PermVolumesCreate, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Create volumes"},
	{Name: PermAuditRead, Scope: ScopeGlobal, Description: "Read the audit log"},
	{Name: PermSettingsRead, Scope: ScopeGlobal, Description: "View settings"},
	{Name: PermSettingsWrite, Scope: ScopeGlobal, Description: "Change settings"},
	{Name: PermUsersManage, Scope: ScopeGlobal, Description: "Create, change, disable and delete users"},
	{Name: PermTeamsManage, Scope: ScopeGlobal, Description: "Create teams and change their members"},
	{Name: PermBackupManage, Scope: ScopeGlobal, Dangerous: true, Description: "Back up and restore TierPlan"},
	{Name: PermContainersExec, Scope: ScopeEnvironment, MinRole: RoleOperator, Extra: true, Dangerous: true, Description: "Open a shell in a container"},
	{Name: PermSecretsReveal, Scope: ScopeEnvironment, MinRole: RoleOperator, Extra: true, Dangerous: true, Description: "Reveal secret values"},
	{Name: PermVolumesDelete, Scope: ScopeEnvironment, MinRole: RoleOperator, Extra: true, Dangerous: true, Description: "Delete volumes"},
	{Name: PermJanitorPolicy, Scope: ScopeEnvironment, MinRole: RoleOperator, Extra: true, Dangerous: true, Description: "Change cleanup policy"},
	{Name: PermShellsManage, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "See and end other people's shells"},
	{Name: PermRegistriesInstance, Scope: ScopeGlobal, Dangerous: true, Description: "Manage registry credentials that apply to every environment"},
	{Name: PermRegistriesManage, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Manage this environment's registry credentials"},
	{Name: PermServicesRead, Scope: ScopeEnvironment, MinRole: RoleViewer, Description: "View services, their tasks, logs, stats and placement"},
	{Name: PermServicesControl, Scope: ScopeEnvironment, MinRole: RoleOperator, Description: "Scale, force update, roll back and change the settings of services, and mute their restart-loop alerts"},
	{Name: PermServicesManage, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Create and remove services"},
	{Name: PermNodesRead, Scope: ScopeEnvironment, MinRole: RoleViewer, Description: "View swarm nodes"},
	{Name: PermNodesManage, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Drain, pause, activate, promote, demote, label and remove nodes"},
	{Name: PermSecretsRead, Scope: ScopeEnvironment, MinRole: RoleViewer, Description: "View swarm secrets' names, labels and users, never their values"},
	{Name: PermSecretsManage, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Create, rotate and delete swarm secrets"},
	{Name: PermConfigsRead, Scope: ScopeEnvironment, MinRole: RoleViewer, Description: "View swarm configs and their content"},
	{Name: PermConfigsManage, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Create, rotate and delete swarm configs"},
	{Name: PermStacksRead, Scope: ScopeEnvironment, MinRole: RoleViewer, Description: "View stacks, their files, reviews, deploys and logs"},
	{Name: PermStacksUpdate, Scope: ScopeEnvironment, MinRole: RoleOperator, Description: "Save and discard a stack's draft"},
	{Name: PermStacksDeploy, Scope: ScopeEnvironment, MinRole: RoleOperator, Description: "Deploy, update, roll back, stop, start and cancel stack operations"},
	{Name: PermStacksCreate, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Create stacks"},
	{Name: PermStacksDelete, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Remove stacks"},
	{Name: PermGitRead, Scope: ScopeEnvironment, MinRole: RoleViewer, Description: "View the git repositories and credentials that apply to this environment"},
	{Name: PermGitManage, Scope: ScopeEnvironment, MinRole: RoleAdmin, Description: "Manage this environment's git repositories and credentials, the webhooks of its git stacks, and convert or detach git stacks"},
	{Name: PermGitInstance, Scope: ScopeGlobal, Dangerous: true, Description: "Manage git repositories and credentials that apply to every environment"},
}

// DefaultRegistry returns a copy of the registry production runs with.
func DefaultRegistry() Registry {
	return append(Registry(nil), defaultRegistry...)
}

// All returns a copy of every permission, in registry order.
func (r Registry) All() []Permission {
	return append([]Permission(nil), r...)
}

// Lookup finds a permission by name.
func (r Registry) Lookup(p Perm) (Permission, bool) {
	for _, e := range r {
		if e.Name == p {
			return e, true
		}
	}
	return Permission{}, false
}

// All returns a copy of every registered permission, in registry order.
func All() []Permission { return defaultRegistry.All() }

// Lookup finds a registered permission by name.
func Lookup(p Perm) (Permission, bool) { return defaultRegistry.Lookup(p) }

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
