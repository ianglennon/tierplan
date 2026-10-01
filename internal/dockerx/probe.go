// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"time"
)

// Probe reasons.
const (
	ReasonAPITooOld     = "api_too_old"
	ReasonUnknownAPI    = "unknown_api"
	ReasonPodman        = "podman"
	ReasonRootless      = "rootless"
	ReasonDockerDesktop = "docker_desktop"
	ReasonUnreachable   = "unreachable"
	ReasonProbeFailed   = "probe_failed"
)

// Environment types.
const (
	TypeSwarmManager = "swarm-manager"
	TypeSwarmWorker  = "swarm-worker"
	TypeStandalone   = "standalone"
)

// ProbeResult is what Bayplan learned about an engine.
type ProbeResult struct {
	Reachable      bool      `json:"reachable"`
	Supported      bool      `json:"supported"`
	Reason         string    `json:"reason,omitempty"`
	Detail         string    `json:"detail,omitempty"`
	EngineName     string    `json:"engineName,omitempty"`
	EngineVersion  string    `json:"engineVersion,omitempty"`
	APIVersion     string    `json:"apiVersion,omitempty"`
	NegotiatedAPI  string    `json:"negotiatedApi,omitempty"`
	OSType         string    `json:"osType,omitempty"`
	OS             string    `json:"os,omitempty"`
	Arch           string    `json:"arch,omitempty"`
	KernelVersion  string    `json:"kernelVersion,omitempty"`
	StorageDriver  string    `json:"storageDriver,omitempty"`
	NCPU           int       `json:"ncpu,omitempty"`
	MemTotal       int64     `json:"memTotal,omitempty"`
	Hostname       string    `json:"hostname,omitempty"`
	Type           string    `json:"type,omitempty"`
	SwarmNodeState string    `json:"swarmNodeState,omitempty"`
	ProbedAt       time.Time `json:"probedAt"`
}

// ComponentFacts is one component of the engine's /version answer.
type ComponentFacts struct {
	Name    string
	Version string
}

// VersionFacts is the part of /version that classification reads.
type VersionFacts struct {
	Version      string
	PlatformName string
	Components   []ComponentFacts
}

// InfoFacts is the part of /info that classification reads.
type InfoFacts struct {
	OperatingSystem  string
	SecurityOptions  []string
	SwarmNodeState   string
	ControlAvailable bool
}

// Classify decides whether an engine is supported and what it is, from the raw
// ping API version and whatever /version and /info said. It is pure, so the
// table test can cover every engine without a daemon.
func Classify(pingAPI string, ver VersionFacts, info InfoFacts) (supported bool, reason, engineName, typ string) {
	return false, "", "", ""
}

// Probe asks the engine who it is. See the package documentation of the plan:
// a raw ping first, negotiation only for supported engines.
func (c *Client) Probe(ctx context.Context) (ProbeResult, error) {
	return ProbeResult{}, errors.New("not implemented")
}
