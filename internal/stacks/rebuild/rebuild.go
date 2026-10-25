// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package rebuild is the reverse of internal/stacks/convert: it turns what a stack is running into the compose
// file that would run the same thing. The Generated compose tab shows it as a preview and adoption starts from it
// (D-09, D-12), so what a person sees there is what Bayplan would store.
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
	"github.com/compose-spec/compose-go/v2/types"
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
// name the engine has for it.
type Object struct {
	Kind, Name, Treatment string
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
	// Notes are what the rebuild could not say plainly, in service then code order.
	Notes []Note
	// Objects are the networks, volumes, secrets and configs the stack uses, in kind then name order.
	Objects []Object
}
