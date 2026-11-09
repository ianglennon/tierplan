// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package wire

import "strings"

// This file holds the control-stream messages that keep an installed agent
// maintainable: the server telling it to update or uninstall itself, and its
// answer.
//
// Frozen for all of 0.x (D-25): do not change field names, types or meanings; add
// only optional fields. An agent in the field can only be moved off an old version
// by a message it already understands, so a server of any 0.x release must be able
// to speak exactly this to an agent of any earlier 0.x release. The golden files in
// testdata pin the bytes (frozen_test.go); a test that needs a golden file to change
// is a test that found a break.
//
// Frozen with them, from earlier plans, and kept where they were declared in wire.go:
//
//	agent to server  {"t":"heartbeat",...}  {"t":"denied",...}  {"t":"renew"}
//	server to agent  {"t":"renewed",...}  {"t":"renew_refused",...}  {"t":"urls",...}
//
// and the two rules that let any of this grow without a protocol change: a line whose
// "t" the receiver does not know is ignored, and a field the receiver does not know
// is ignored. A later release may therefore add a message type or an optional field;
// it may never rename, retype or reuse one.
const (
	// TypeUpdate is sent by the server: replace the agent with the image named.
	TypeUpdate = "update"
	// TypeUninstall is sent by the server: remove the agent from its host.
	TypeUninstall = "uninstall"
	// TypeUpdateResult is the agent's answer to either of them.
	TypeUpdateResult = "update_result"
)

const (
	// MaxMessageIDLen is the longest id the agent echoes. An id is opaque to the
	// agent and optional on the request; the agent cuts a longer one at this many
	// bytes (on a character boundary) before echoing it, so a server should send
	// no more.
	MaxMessageIDLen = 64
	// MaxImageLen is the longest image reference the agent accepts.
	MaxImageLen = 512
)

// Update asks the agent to run another image of its own repository. Image is a
// full reference, a tag or a digest ("ghcr.io/ianglennon/tierplan-agent:0.2.1" or
// "...@sha256:..."), so a digest needs no format change. The agent parses it with
// the standard reference grammar and refuses any repository but its own. ID is
// optional and opaque; the answer echoes it.
type Update struct {
	T     string `json:"t"`
	ID    string `json:"id,omitempty"`
	Image string `json:"image"`
}

// Uninstall asks the agent to remove itself. RemoveData also removes its state
// volume. ID is optional and opaque; the answer echoes it.
//
// Service is an optional field added after the first release of the message (D-30,
// plan 02-29): on a swarm it asks a manager agent to remove the swarm service it
// belongs to and the join-token secret that service names, which removes the agent
// of every node. Only one agent of a swarm is asked that; every other agent is sent
// the message without it, and starts removing its own state volume. It is absent from
// the bytes when false, so a message without it is exactly what it always was, and an
// agent that does not know it ignores it. It means nothing to a standalone agent.
type Uninstall struct {
	T          string `json:"t"`
	ID         string `json:"id,omitempty"`
	RemoveData bool   `json:"removeData"`
	Service    bool   `json:"service,omitempty"`
}

// UpdateResult answers an Update or an Uninstall. ID echoes the request's id,
// capped at MaxMessageIDLen, and is empty when the request had none. OK true means
// the agent accepted the request and started it, not that it finished: for an update
// that is the swarm service update call returning, or the standalone helper starting.
// The server learns that it worked from the agent reconnecting with the new
// agentVersion. Error is a machine code and is empty on success; the codes below
// are the ones in use, and a server must treat one it does not know as a failure.
type UpdateResult struct {
	T     string `json:"t"`
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Error codes of an UpdateResult. New codes may be added in any release.
const (
	// UpdateErrImageNotOwnRepository means the image is in a repository other than the agent's own.
	UpdateErrImageNotOwnRepository = "image_not_own_repository"
	// UpdateErrImageInvalid means the image is not a full reference (a tag or a digest is required), or is too long.
	UpdateErrImageInvalid = "image_invalid"
	// UpdateErrNotAManager means a swarm agent without control of the swarm was asked to update the service.
	UpdateErrNotAManager = "not_a_manager"
	// UpdateErrSelfUpdateDisabled means the host owner set TIERPLAN_AGENT_SELF_UPDATE=false.
	UpdateErrSelfUpdateDisabled = "self_update_disabled"
	// UpdateErrBusy means an update is already running on this agent.
	UpdateErrBusy = "busy"
	// UpdateErrOwnContainerUnknown means the agent could not identify its own container, or it lacks the tierplan.agent=true label.
	UpdateErrOwnContainerUnknown = "own_container_unknown"
	// UpdateErrFailed means a Docker call failed; the detail is in the agent's log.
	UpdateErrFailed = "update_failed"
	// UpdateErrNotImplemented means this agent does not implement the request.
	UpdateErrNotImplemented = "not_implemented"
	// UpdateErrInsecureSession means the agent's session is not TLS-verified, so it
	// refused to update or uninstall itself on the word of a server it could not
	// verify. It made no Docker call. Older agents could reach such a session over
	// plain ws://; the constant stays because they still send it.
	UpdateErrInsecureSession = "insecure_session"
)

// CapID cuts an id to MaxMessageIDLen bytes without splitting a character.
func CapID(id string) string {
	if len(id) <= MaxMessageIDLen {
		return id
	}
	return strings.ToValidUTF8(id[:MaxMessageIDLen], "")
}
