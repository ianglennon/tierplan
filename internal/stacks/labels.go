// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

// Ownership labels. Everything a stack creates carries them, and later phases read them without
// asking the stack store: Phase 8 adopts a foreign stack by its swarm namespace or compose
// project, Phase 10 judges a network or volume an orphan by whether a stack still owns it, and
// the Phase 11 janitor never touches an object a stack owns. They are on-host contracts: the
// networks and volumes a stack made are immutable, so a rename of any of these needs a dual read
// of the old name for as long as such an object lives.
const (
	// LabelStackID is the ID of the Bayplan stack record that owns the object.
	LabelStackID = "bayplan.stack.id"
	// LabelStackImage is the image reference as the user wrote it, before it was pinned.
	LabelStackImage = "bayplan.stack.image"
	// LabelObject marks a secret or config a stack made from one of its files; its value is the
	// name the compose file uses.
	LabelObject = "bayplan.stack.object"
	// LabelObjectHash is the short content hash a stack secret or config carries in its name.
	LabelObjectHash = "bayplan.stack.object.hash"
	// LabelSecretPrefix and LabelConfigPrefix start the key of a label a standalone service carries for
	// each secret or config it uses, whose value is the short hash of the object's content
	// (ObjectHash). The Compose SDK recreates a container only when its service changes, and a secret or
	// config is a file written into the container, so this is what makes new content reach it. The hash
	// depends on the content alone, so a redeploy nobody edited changes nothing.
	LabelSecretPrefix = "bayplan.stack.secret." //nolint:gosec // a label key, not a credential
	LabelConfigPrefix = "bayplan.stack.config."
	// LabelStoppedReplicas remembers a replicated service's count while its stack is stopped.
	LabelStoppedReplicas = "bayplan.stack.stopped-replicas"

	// LabelSwarmNamespace is the label docker stack deploy puts on every object of a stack.
	LabelSwarmNamespace = "com.docker.stack.namespace"
	// LabelSwarmImage is the label docker stack deploy puts on a service with its image as written.
	LabelSwarmImage = "com.docker.stack.image"
	// LabelComposeProject is the label Docker Compose puts on every container, network and volume
	// of a project.
	LabelComposeProject = "com.docker.compose.project"
	// LabelComposeService names the service of the compose file a container was created for.
	LabelComposeService = "com.docker.compose.service"
	// LabelComposeOneoff is "False" on a container of a service and "True" on a one-off one
	// (docker compose run); the SDK lists a project's containers by it.
	LabelComposeOneoff = "com.docker.compose.oneoff"
	// LabelComposeWorkingDir and LabelComposeConfigFiles record where a project was loaded from. A
	// stack is loaded from rows, so they carry the constants below and never a path of the server.
	LabelComposeWorkingDir  = "com.docker.compose.project.working_dir"
	LabelComposeConfigFiles = "com.docker.compose.project.config_files"
	// LabelComposeConfigHash is the hash of the service a container was created from
	// (dockerx.ServiceHash); a container whose hash differs from its service's is recreated.
	LabelComposeConfigHash = "com.docker.compose.config-hash"
)

// The neutral values of the two labels that record where a project was loaded from (RESEARCH Q2,
// Pitfall 5): the Bayplan server's own paths never reach a host, and the value never changes, so it
// never moves a config hash.
const (
	NeutralWorkingDir  = "/bayplan-stack"
	NeutralConfigFiles = ComposeFile
)
