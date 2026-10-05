// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/client"
)

// minSupportedAPI is the API version of Docker Engine 24.0, the oldest engine
// Bayplan supports (D-25). Engine 23 tops out at 1.42.
const minSupportedAPI = "1.43"

// Engine names Classify can return.
const (
	engineDocker  = "Docker Engine"
	enginePodman  = "Podman"
	engineDesktop = "Docker Desktop"
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
	Reachable      bool   `json:"reachable"`
	Supported      bool   `json:"supported"`
	Reason         string `json:"reason,omitempty"`
	Detail         string `json:"detail,omitempty"`
	EngineName     string `json:"engineName,omitempty"`
	EngineVersion  string `json:"engineVersion,omitempty"`
	APIVersion     string `json:"apiVersion,omitempty"`
	NegotiatedAPI  string `json:"negotiatedApi,omitempty"`
	OSType         string `json:"osType,omitempty"`
	OS             string `json:"os,omitempty"`
	Arch           string `json:"arch,omitempty"`
	KernelVersion  string `json:"kernelVersion,omitempty"`
	StorageDriver  string `json:"storageDriver,omitempty"`
	NCPU           int    `json:"ncpu,omitempty"`
	MemTotal       int64  `json:"memTotal,omitempty"`
	Hostname       string `json:"hostname,omitempty"`
	Type           string `json:"type,omitempty"`
	SwarmNodeState string `json:"swarmNodeState,omitempty"`
	// EngineID is the engine's own ID (/info ID). SwarmNodeID is the node's ID in a
	// swarm it has joined, and SwarmClusterID the swarm's ID, kept for a manager only:
	// a worker never learns it, and an agent drops it for anything but a manager, so
	// the two sides compare the same facts (D-48).
	EngineID       string    `json:"engineId,omitempty"`
	SwarmNodeID    string    `json:"swarmNodeId,omitempty"`
	SwarmClusterID string    `json:"swarmClusterId,omitempty"`
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
//
// The order matters. What the engine is (Podman, Docker Desktop, rootless) is
// decided first, so an unsupported engine is named even when its API version
// is also unusable. Only then is the API version judged: unparseable, or older
// than Engine 24, is unsupported. Everything else is a supported engine.
func Classify(pingAPI string, ver VersionFacts, info InfoFacts) (supported bool, reason, engineName, typ string) {
	typ = swarmType(info)

	engineName = ""
	if ver.Version != "" || ver.PlatformName != "" || len(ver.Components) > 0 {
		engineName = engineDocker
	}

	switch {
	case isPodman(ver):
		return false, ReasonPodman, enginePodman, typ
	case isDockerDesktop(ver, info):
		return false, ReasonDockerDesktop, engineDesktop, typ
	case isRootless(info):
		return false, ReasonRootless, engineName, typ
	}

	if _, _, ok := parseAPI(pingAPI); !ok {
		return false, ReasonUnknownAPI, engineName, typ
	}
	if !apiAtLeast(pingAPI, minSupportedAPI) {
		return false, ReasonAPITooOld, engineName, typ
	}
	return true, "", engineName, typ
}

// swarmType is the role of the host in a swarm (D-24). A locked node cannot
// act as a manager until it is unlocked, so it reads as a worker.
func swarmType(info InfoFacts) string {
	switch info.SwarmNodeState {
	case "active":
		if info.ControlAvailable {
			return TypeSwarmManager
		}
		return TypeSwarmWorker
	case "locked":
		return TypeSwarmWorker
	default:
		return TypeStandalone
	}
}

// isPodman reads the component and platform names (RESEARCH A1: Podman's
// compatibility API reports a component named "Podman Engine").
func isPodman(v VersionFacts) bool {
	if strings.Contains(strings.ToLower(v.PlatformName), "podman") {
		return true
	}
	for _, c := range v.Components {
		if strings.Contains(strings.ToLower(c.Name), "podman") {
			return true
		}
	}
	return false
}

// isDockerDesktop reads the operating system string, which Desktop sets to
// "Docker Desktop", and the platform name, which carries its version (D-22).
func isDockerDesktop(v VersionFacts, info InfoFacts) bool {
	return strings.Contains(strings.ToLower(info.OperatingSystem), "docker desktop") ||
		strings.Contains(strings.ToLower(v.PlatformName), "docker desktop")
}

// isRootless looks for the name=rootless security option (D-22). An option may
// carry several comma separated parts, so each part is compared on its own.
func isRootless(info InfoFacts) bool {
	for _, opt := range info.SecurityOptions {
		for part := range strings.SplitSeq(opt, ",") {
			if strings.TrimSpace(part) == "name=rootless" {
				return true
			}
		}
	}
	return false
}

// parseAPI parses "major.minor" and nothing else: a daemon that answers
// anything different is not speaking a version Bayplan can reason about.
func parseAPI(v string) (major, minor int, ok bool) {
	maj, min, found := strings.Cut(v, ".")
	if !found || !digits(maj) || !digits(min) {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(maj)
	minor, err2 := strconv.Atoi(min)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// apiAtLeast reports whether version v is at least floor. An unparseable v is
// never at least anything.
func apiAtLeast(v, floor string) bool {
	vMaj, vMin, ok := parseAPI(v)
	if !ok {
		return false
	}
	fMaj, fMin, ok := parseAPI(floor)
	if !ok {
		return false
	}
	return vMaj > fMaj || (vMaj == fMaj && vMin >= fMin)
}

// Probe asks the engine who it is, in the order Pitfalls 2 and 3 require:
//
//  1. A raw ping with no negotiation, which works against any daemon and
//     reports its API version.
//  2. If that API is at least 1.43 (Engine 24), a fresh client negotiates with
//     ForceNegotiate and reads /version and /info. A fresh client is used on
//     every probe because the moby client only ever negotiates down from the
//     version it already has, so a daemon upgraded since the last probe would
//     never be picked up by reusing it (D-26).
//  3. Otherwise /version is read through a client pinned to the ping's own API
//     version, which only serves to name the engine. If that fails the ping
//     data is kept.
//
// An unreachable engine returns a result with Reason "unreachable" and an
// error that names the transport. Concurrent probes queue on probeSem instead
// of racing on the shared client, and a queued probe gives up when its own
// context ends. No client lock is held while the engine is asked anything, so
// a slow Snapshot in progress does not delay a probe (WR-06).
func (c *Client) Probe(ctx context.Context) (ProbeResult, error) {
	res := ProbeResult{ProbedAt: time.Now().UTC()}

	select {
	case c.probeSem <- struct{}{}:
		defer func() { <-c.probeSem }()
	case <-ctx.Done():
		res.Reason = ReasonUnreachable
		res.Detail = ctx.Err().Error()
		return res, fmt.Errorf("the Docker engine at %s was not probed: %w", c.t.Describe(), ctx.Err())
	}

	ping, err := c.cur().Ping(ctx, client.PingOptions{})
	if err != nil {
		res.Reason = ReasonUnreachable
		res.Detail = err.Error()
		return res, fmt.Errorf("the Docker engine at %s is unreachable: %w", c.t.Describe(), err)
	}
	res.Reachable = true
	res.APIVersion = ping.APIVersion
	res.OSType = ping.OSType

	var (
		ver       VersionFacts
		info      InfoFacts
		haveInfo  bool
		probeFail error
	)

	switch {
	case apiAtLeast(ping.APIVersion, minSupportedAPI):
		fresh, err := newMoby(c.t)
		if err != nil {
			return failProbe(res, err)
		}
		if _, err := fresh.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true, ForceNegotiate: true}); err != nil {
			_ = fresh.Close()
			return failProbe(res, fmt.Errorf("negotiate the API version with %s: %w", c.t.Describe(), err))
		}
		c.mu.Lock()
		old := c.c
		c.c = fresh
		c.mu.Unlock()
		// Closing drops only idle connections, so a Snapshot still running on
		// the old client finishes.
		_ = old.Close()
		res.NegotiatedAPI = fresh.ClientVersion()

		sv, err := fresh.ServerVersion(ctx, client.ServerVersionOptions{})
		if err != nil {
			return failProbe(res, fmt.Errorf("read the engine version from %s: %w", c.t.Describe(), err))
		}
		ver = versionFacts(sv)
		res.EngineVersion = sv.Version
		res.OS, res.Arch = sv.Os, sv.Arch

		in, err := fresh.Info(ctx, client.InfoOptions{})
		if err != nil {
			probeFail = fmt.Errorf("read the engine info from %s: %w", c.t.Describe(), err)
			break
		}
		haveInfo = true
		info = InfoFacts{
			OperatingSystem:  in.Info.OperatingSystem,
			SecurityOptions:  in.Info.SecurityOptions,
			SwarmNodeState:   string(in.Info.Swarm.LocalNodeState),
			ControlAvailable: in.Info.Swarm.ControlAvailable,
		}
		res.Hostname = in.Info.Name
		res.KernelVersion = in.Info.KernelVersion
		res.StorageDriver = in.Info.Driver
		res.NCPU = in.Info.NCPU
		res.MemTotal = in.Info.MemTotal
		res.SwarmNodeState = info.SwarmNodeState
		res.EngineID = in.Info.ID
		if info.SwarmNodeState != "" && info.SwarmNodeState != "inactive" {
			res.SwarmNodeID = in.Info.Swarm.NodeID
			if info.ControlAvailable && in.Info.Swarm.Cluster != nil {
				res.SwarmClusterID = in.Info.Swarm.Cluster.ID
			}
		}

	default:
		// Below the floor, or an API version that is not major.minor. A client
		// pinned to the daemon's own version can still ask who it is.
		if _, _, ok := parseAPI(ping.APIVersion); ok {
			if sv, err := c.legacyVersion(ctx, ping.APIVersion); err == nil {
				ver = versionFacts(sv)
				res.EngineVersion = sv.Version
				res.OS, res.Arch = sv.Os, sv.Arch
			}
		}
	}

	supported, reason, name, typ := Classify(ping.APIVersion, ver, info)
	res.Supported, res.Reason, res.EngineName = supported, reason, name
	if haveInfo {
		res.Type = typ
	}
	if probeFail != nil {
		res.Supported = false
		res.Reason = ReasonProbeFailed
		res.Detail = probeFail.Error()
		return res, probeFail
	}
	return res, nil
}

// failProbe marks a reachable engine whose details could not be read.
func failProbe(res ProbeResult, err error) (ProbeResult, error) {
	res.Supported = false
	res.Reason = ReasonProbeFailed
	res.Detail = err.Error()
	return res, err
}

// legacyVersion reads /version through a client fixed to the daemon's own API
// version, which an old daemon accepts and a negotiating client would refuse.
func (c *Client) legacyVersion(ctx context.Context, api string) (client.ServerVersionResult, error) {
	old, err := newMoby(c.t, client.WithAPIVersion(api))
	if err != nil {
		return client.ServerVersionResult{}, err
	}
	defer func() { _ = old.Close() }()
	return old.ServerVersion(ctx, client.ServerVersionOptions{})
}

func versionFacts(sv client.ServerVersionResult) VersionFacts {
	v := VersionFacts{Version: sv.Version, PlatformName: sv.Platform.Name}
	for _, comp := range sv.Components {
		v.Components = append(v.Components, ComponentFacts{Name: comp.Name, Version: comp.Version})
	}
	return v
}
