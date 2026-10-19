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
	// LabelStoppedReplicas remembers a replicated service's count while its stack is stopped.
	LabelStoppedReplicas = "bayplan.stack.stopped-replicas"

	// LabelSwarmNamespace is the label docker stack deploy puts on every object of a stack.
	LabelSwarmNamespace = "com.docker.stack.namespace"
	// LabelSwarmImage is the label docker stack deploy puts on a service with its image as written.
	LabelSwarmImage = "com.docker.stack.image"
	// LabelComposeProject is the label Docker Compose puts on every container, network and volume
	// of a project.
	LabelComposeProject = "com.docker.compose.project"
)
