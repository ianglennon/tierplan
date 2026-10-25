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
	// NoteCPUsInexact: a processor limit or reservation is exact to nine decimals in the swarm spec but compose
	// holds it as a 32-bit float, so the file carries the nearest value it can.
	NoteCPUsInexact = "cpus_inexact"
	// NoteImageLocal: a service of a standalone project runs an image no registry has (it was built on the host),
	// so it is pinned to its image ID. For this note and NoteObjectExternalHash, Service names the service or
	// the object the note is about, which the page puts into its sentence.
	NoteImageLocal = "image_local"
	// NoteObjectExternalHash: a network or volume of a standalone project has settings Bayplan could not write
	// so that the Compose SDK holds the object to be unchanged, so the file uses it as an external object.
	NoteObjectExternalHash = "object_external_hash"
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

func noteCPUs(where string, nano int64) Note {
	return Note{
		Code:    NoteCPUsInexact,
		Message: fmt.Sprintf("The processor %s is %d billionths of a CPU, which a compose file can't hold exactly. The file has the nearest value.", where, nano),
	}
}

func noteImageLocal(service string) Note {
	return Note{
		Service: service, Code: NoteImageLocal,
		Message: fmt.Sprintf("%s runs an image built on this host. It is pinned to its image ID; Bayplan can't look it up in a registry.", service),
	}
}

func noteObjectExternal(name string) Note {
	return Note{
		Service: name, Code: NoteObjectExternalHash,
		Message: fmt.Sprintf("%s is kept external: Bayplan couldn't match its settings, so a deploy leaves it as it is.", name),
	}
}
