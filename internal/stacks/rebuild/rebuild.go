// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Package rebuild is the reverse of internal/stacks/convert: it turns what a stack is running into the compose
// file that would run the same thing. The Generated compose tab shows it as a preview and adoption starts from it
// (D-09, D-12), so what a person sees there is what Coaming would store.
//
// It is pure: no HTTP, no store and no Docker client, only the moby API types and the compose-go types. The
// caller reads the engine and hands in typed data, and a secret or a config is only ever a name here, so a rebuild
// cannot read a value it was not given (D-13).
//
// The proof of the swarm direction is offline: for every compose file of the converter's own corpus, converting,
// rebuilding and converting again gives the same specs (roundtrip_test.go). What the rebuild cannot say in
// compose is not an error here; the adoption check shows it as a gap that will change on the first deploy (D-20).
package rebuild

import (
	"cmp"
	"slices"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/swarm"
)

// What a Note is about and how an object of the stack is treated. Kind is one of the Kind constants and Treatment
// one of the Treatment ones.
const (
	// TreatmentOwned is an object the stack owns: it is created and removed with the stack, and is written as a
	// top-level entry of the compose file under the name it has now (D-18).
	TreatmentOwned = "owned"
	// TreatmentExternal is an object the stack only uses: it is written external, with its name, and is never
	// created or removed (D-13, D-18).
	TreatmentExternal = "external"
)

// The kinds of object a Result lists.
const (
	KindNetwork = "network"
	KindVolume  = "volume"
	KindSecret  = "secret"
	KindConfig  = "config"
)

// Note is something a person should know about the rebuilt file: Service is the compose service it is about, or
// empty when it concerns the whole file, Code a stable name for the kind of note and Message a sentence for the
// page.
type Note struct {
	Service, Code, Message string
}

// Object is one network, volume, secret or config of the stack and how the rebuilt file treats it. Name is the
// name the engine has for it. Key is the key the file declares it under (a standalone rebuild sets it for a
// network and a volume; it is also the key of Result.Variants and StandaloneInput.Choice).
type Object struct {
	Kind, Name, Treatment string
	Key                   string
}

// Result is a rebuilt stack.
type Result struct {
	// Project is the compose project the YAML was written from.
	Project *types.Project
	// YAML is the compose file, ready to be stored and deployed: a dollar sign in a value is already doubled.
	YAML []byte
	// Pins are the images the services run, as the engine holds them (tag@sha256:...), by the name the engine
	// gives the service. The file names the image as it was written; the pin is what a deploy keeps (D-14).
	Pins map[string]string
	// Platforms are the platforms the services run with (their Placement.Platforms), by the name the engine gives
	// the service, for each service that has any. They travel with the pins and never appear in the file: an
	// adoption stores them on its first revision so a roll back to it puts them back (D-14).
	Platforms map[string][]swarm.Platform
	// Notes are what the rebuild could not say plainly, in service then code order.
	Notes []Note
	// Objects are the networks, volumes, secrets and configs the stack uses, in kind then name order.
	Objects []Object
	// Variants is how many renderings an owned network or volume of a standalone rebuild has (see
	// NetworkCandidates), by the key StandaloneInput.Choice uses ("network:back"). The caller tries them against
	// the hash the host holds for the object (D-46).
	Variants map[string]int
	// Fingerprint says what the engine held when the stack was read: the digest of each service's ID and version
	// on a swarm, of each container's ID, config hash and image ID on a host. stackops.RebuildLive sets it from the
	// very reads the file was made from; the adoption refuses a commit whose fingerprint is not the page's (T-08-46).
	Fingerprint string
}

func sortNotes(in []Note) []Note {
	out := slices.Clone(in)
	slices.SortStableFunc(out, func(x, y Note) int {
		return cmp.Or(cmp.Compare(x.Service, y.Service), cmp.Compare(x.Code, y.Code), cmp.Compare(x.Message, y.Message))
	})
	return out
}

func sortObjects(in map[string]Object) []Object {
	out := make([]Object, 0, len(in))
	for _, o := range in {
		out = append(out, o)
	}
	slices.SortFunc(out, func(x, y Object) int {
		return cmp.Or(cmp.Compare(x.Kind, y.Kind), cmp.Compare(x.Name, y.Name))
	})
	return out
}
