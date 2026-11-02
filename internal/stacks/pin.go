// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/swarm"
)

// PinAction is what a deploy does about the image of one service (D-15).
type PinAction string

// The actions of a pin decision.
const (
	// PinKeep sends the image the service already runs (or the one its last good revision ran),
	// so a redeploy never moves a digest the person did not ask to move.
	PinKeep PinAction = "keep"
	// PinResolve looks the tag up in its registry and deploys tag@sha256.
	PinResolve PinAction = "resolve"
	// PinAsWritten sends a reference that already carries a digest exactly as written.
	PinAsWritten PinAction = "as_written"
	// PinLocal pins an image no registry has to the image ID on the manager (D-18). DecidePin
	// never answers it: it is what a resolve becomes when the registry does not know the image
	// and the manager holds it.
	PinLocal PinAction = "local"
)

// LivePin is an image a service ran or runs: AsWritten is the reference text that was deployed
// (the com.docker.stack.image label of a swarm service) and Image what was actually sent, the
// pinned reference. Platforms are the swarm platforms the stack's revision recorded for that image, empty when it
// recorded none (a service that runs, or an older revision, has none here).
type LivePin struct {
	AsWritten, Image string
	Platforms        []swarm.Platform
}

// PinInput is one service's image for DecidePin.
type PinInput struct {
	Service string
	// AsWritten is the image reference as the compose file says it, after variables.
	AsWritten string
	// Live is the image the service runs now, nil when the engine has no such service or it
	// carries no record of what was written.
	Live *LivePin
	// Stored is the image the last good revision ran, used only when Live is nil.
	Stored *LivePin
	// Update is true when the person asked for this service to move to the newest image.
	Update bool
}

// PinDecision is the action and the reference to send. Ref is empty for PinResolve: the lookup
// decides it.
type PinDecision struct {
	Action PinAction
	Ref    string
}

// HasDigest reports whether an image reference names a digest.
func HasDigest(ref string) bool {
	i := strings.LastIndex(ref, "@")
	return i >= 0 && strings.Contains(ref[i:], ":")
}

// DecidePin decides how one service's image is pinned. It is the whole of the no-silent-upgrade
// rule (STK-09), a table with no I/O:
//
//   - a reference that already carries a digest is used as written;
//   - a service the person asked to update resolves, whatever it runs;
//   - a service that runs the same text keeps the image it runs;
//   - a service the engine does not have, whose last good revision ran the same text, keeps
//     what that revision ran (A7);
//   - everything else, a new service or a changed text, resolves.
func DecidePin(in PinInput) PinDecision {
	switch {
	case HasDigest(in.AsWritten):
		return PinDecision{Action: PinAsWritten, Ref: in.AsWritten}
	case in.Update:
		return PinDecision{Action: PinResolve}
	case in.Live != nil && in.Live.Image != "" && in.Live.AsWritten == in.AsWritten:
		return PinDecision{Action: PinKeep, Ref: in.Live.Image}
	case in.Live == nil && in.Stored != nil && in.Stored.Image != "" && in.Stored.AsWritten == in.AsWritten:
		return PinDecision{Action: PinKeep, Ref: in.Stored.Image}
	}
	return PinDecision{Action: PinResolve}
}

// SameImageText reports whether two image references name the same thing as text: they are equal,
// or they are the same normalised reference once an untagged name gets its implicit :latest, so
// docker.io/library/nginx:1.27 and nginx:1.27 are one text. A digest is part of the text, and a
// reference that does not parse is only ever equal to itself.
func SameImageText(a, b string) bool {
	if a == b {
		return true
	}
	na, err := reference.ParseNormalizedNamed(a)
	if err != nil {
		return false
	}
	nb, err := reference.ParseNormalizedNamed(b)
	if err != nil {
		return false
	}
	return reference.TagNameOnly(na).String() == reference.TagNameOnly(nb).String()
}
