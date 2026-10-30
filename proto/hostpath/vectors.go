// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package hostpath

import (
	_ "embed"
	"encoding/json"
)

//go:embed testdata/service-specs.json
var specVectors []byte

//go:embed testdata/container-specs.json
var containerVectors []byte

// SpecVector is one service spec, in Docker's field names, with the verdict
// every policy that reads a service spec must agree on. HostNetworkIDs lists
// the network IDs a policy must treat as the host network (it looks them up on
// the engine; a test answers for them).
type SpecVector struct {
	Name           string          `json:"name"`
	Spec           json.RawMessage `json:"spec"`
	Escalates      bool            `json:"escalates"`
	HostNetworkIDs []string        `json:"hostNetworkIDs,omitempty"`
	// AgentRefuses is true when the agent refuses the spec on a host without the
	// opt-in although no shared policy finds host access in it: a volume driver other
	// than local, a cluster volume, or a named volume whose backing on the engine gives
	// host access. Escalates stays false for these, so the server and the compose
	// policy, which pass them (D-38), still agree with the vector.
	AgentRefuses bool `json:"agentRefuses,omitempty"`
	// Volumes are the named volumes the engine holds for this vector. A policy that
	// inspects named volumes answers from it and treats every other name as absent.
	Volumes map[string]VectorVolume `json:"volumes,omitempty"`
}

// VectorVolume is what an engine answers for a named volume that exists.
type VectorVolume struct {
	Driver  string            `json:"driver"`
	Options map[string]string `json:"options,omitempty"`
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

// ContainerVector is one container create body, in Docker's field names, with the
// verdict every policy that reads a container create must agree on (STK-02). Refused
// lists, in order, the field paths a policy without the host owner's opt-in names as
// giving the container host access, for example "HostConfig.Privileged" or
// "HostConfig.Binds[0]"; it is empty when the body is forwarded. A policy that holds the
// opt-in forwards every vector.
type ContainerVector struct {
	Name string          `json:"name"`
	Body json.RawMessage `json:"body"`
	// Refused is the finding paths, in the order of the body; empty means forwarded.
	Refused []string `json:"refused"`
	// Volumes are the named volumes the engine holds for this vector. A policy that
	// inspects named volumes answers from it and treats every other name as absent.
	Volumes map[string]VectorVolume `json:"volumes,omitempty"`
	// HostNetworkIDs lists the network names or IDs a policy must treat as the host
	// network when it looks them up on the engine.
	HostNetworkIDs []string `json:"hostNetworkIDs,omitempty"`
}

// ContainerVectors returns the embedded container-create vectors. It panics if the
// embedded file is not valid JSON, which a test catches.
func ContainerVectors() []ContainerVector {
	var out []ContainerVector
	if err := json.Unmarshal(containerVectors, &out); err != nil {
		panic("hostpath: bad embedded container-create vectors: " + err.Error())
	}
	return out
}
