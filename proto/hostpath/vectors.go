// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package hostpath

import (
	_ "embed"
	"encoding/json"
)

//go:embed testdata/service-specs.json
var specVectors []byte

// SpecVector is one service spec, in Docker's field names, with the verdict
// every policy that reads a service spec must agree on. HostNetworkIDs lists
// the network IDs a policy must treat as the host network (it looks them up on
// the engine; a test answers for them).
type SpecVector struct {
	Name           string          `json:"name"`
	Spec           json.RawMessage `json:"spec"`
	Escalates      bool            `json:"escalates"`
	HostNetworkIDs []string        `json:"hostNetworkIDs,omitempty"`
}

// Vectors returns the embedded service-spec vectors. It panics if the embedded
// file is not valid JSON, which a test catches.
func Vectors() []SpecVector {
	var out []SpecVector
	if err := json.Unmarshal(specVectors, &out); err != nil {
		panic("hostpath: bad embedded service-spec vectors: " + err.Error())
	}
	return out
}
