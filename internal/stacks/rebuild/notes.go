// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rebuild

import "fmt"

// The codes of the notes a rebuild can make. A code is a stable name for the kind of note; the sentence is the
// page's words for it.
const (
	// NoteServiceNamePrefix: a service of the namespace is not named <namespace>_<name>, so the file names it as
	// it is and a deploy would give it the prefix.
	NoteServiceNamePrefix = "service_name_prefix"
	// NoteNetworkNotOverlay: a network that carries the stack's label is not an overlay network, which a stack
	// cannot create, so it is written external.
	NoteNetworkNotOverlay = "network_not_overlay"
	// NoteImageUnpinned: a service runs an image with no digest, so its pin is the reference as it is.
	NoteImageUnpinned = "image_unpinned"
)

func notePrefix(namespace, service, name string) Note {
	return Note{
		Service: service, Code: NoteServiceNamePrefix,
		Message: fmt.Sprintf("The service %s isn't named %s_…, so the file keeps its name as it is. A deploy would name it %s_%s.", name, namespace, namespace, name),
	}
}

func noteNotOverlay(name, driver string) Note {
	return Note{
		Code:    NoteNetworkNotOverlay,
		Message: fmt.Sprintf("The network %s uses the %s driver, not overlay, so the file uses it as an external network. Bayplan doesn't create it.", name, driver),
	}
}

func noteUnpinned(service, image string) Note {
	return Note{
		Service: service, Code: NoteImageUnpinned,
		Message: fmt.Sprintf("%s runs %s, which has no digest, so it is pinned to the reference as it is.", service, image),
	}
}
