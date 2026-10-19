// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"fmt"

	"github.com/distribution/reference"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// CodeImageUnresolved ends the lookup item of a deploy when an image could not be looked up.
const CodeImageUnresolved = "image_unresolved"

// Pin is how one service's image is pinned: the reference as written, the reference to send and
// what decided it. Note is the sentence for a warning (a local image), empty otherwise.
type Pin struct {
	Service, AsWritten, Ref string
	Action                  stacks.PinAction
	Note                    string
}

// PinOutcome is the answer of ResolvePins. Pins is by swarm service name. Problems holds a
// blocking problem for each service whose image could not be pinned. Code and FailedService are
// empty when every service has a pin; otherwise they are the code of the deploy that stops and
// the first service it stops at: an image that could not be looked up, or a failure that is not
// about an image at all (no manager is connected, the agent refused, the credential could not be
// read, the deploy was cancelled), which ends the lookup at once.
type PinOutcome struct {
	Pins                map[string]Pin
	Problems            stacks.Problems
	Code, FailedService string
}

// isImageFailure reports whether a deploy code says an image could not be looked up.
func isImageFailure(code string) bool { return code == CodeImageUnresolved }

// Blocked reports whether the deploy cannot go on.
func (o PinOutcome) Blocked() bool { return o.Code != "" }

// Refs are the references to send, by swarm service name, the form a revision stores.
func (o PinOutcome) Refs() map[string]string {
	out := make(map[string]string, len(o.Pins))
	for name, p := range o.Pins {
		out[name] = p.Ref
	}
	return out
}

// ResolvePins decides, for each service, which image to send (stacks.DecidePin) and looks up the
// ones that need a fresh digest. live holds the services the swarm has by name, stored the pins
// of the stack's last good revision by name, and update the services the person asked to move
// to the newest image, by compose name or by swarm name (nil for an ordinary deploy). A
// reference is looked up once however many services use it, through a manager's engine with the
// credential Resolver.AuthFor chooses for it, and never falls back to an unpinned tag: a lookup
// that fails blocks the services that need it and no others, since a service that keeps its
// running digest never needs the registry (D-18).
func (d Deps) ResolvePins(ctx context.Context, envID string, services []convert.ServicePlan, live map[string]dockerx.ServiceDetail, stored map[string]stacks.LivePin, update map[string]bool) PinOutcome {
	out := PinOutcome{Pins: make(map[string]Pin, len(services))}
	looked := map[string]lookupResult{}
	for _, sp := range services {
		text := sp.ImageAsWritten
		if text == "" {
			continue // a service with no image has nothing to pin
		}
		in := stacks.PinInput{Service: sp.Name, AsWritten: text, Update: update[sp.Short] || update[sp.Name]}
		if det, ok := live[sp.Name]; ok {
			if written := det.Labels[stacks.LabelSwarmImage]; written != "" && det.Image != "" {
				in.Live = &stacks.LivePin{AsWritten: written, Image: det.Image}
			}
		}
		if st, ok := stored[sp.Name]; ok && sameImageText(st.AsWritten, text) {
			in.Stored = &stacks.LivePin{AsWritten: text, Image: st.Image}
		}
		dec := stacks.DecidePin(in)
		pin := Pin{Service: sp.Name, AsWritten: text, Ref: dec.Ref, Action: dec.Action}
		if dec.Action == stacks.PinResolve {
			res, done := looked[text]
			if !done {
				res = d.lookupImage(ctx, envID, text)
				looked[text] = res
			}
			if res.infra != "" {
				out.Code, out.FailedService = res.infra, sp.Name
				return out
			}
			if res.failure != "" {
				out.Problems = append(out.Problems, imageProblem(res, sp))
				if out.Code == "" {
					out.Code, out.FailedService = res.failure, sp.Name
				}
				continue
			}
			pin.Ref = res.ref
		}
		out.Pins[sp.Name] = pin
	}
	return out
}

// lookupResult is the answer of one lookup: the reference to send, or why there is none. failure
// is a lookup failure that blocks only the services that need it, infra a deploy code for one
// that blocks the deploy.
type lookupResult struct {
	ref            string
	failure, infra string
	host           string
}

// lookupImage resolves one reference to the digest its registry holds for it now, with the
// credential chosen for it, through a manager.
func (d Deps) lookupImage(ctx context.Context, envID, text string) lookupResult {
	res := lookupResult{host: registryHost(text)}
	auth, err := d.authFor(ctx, envID, text)
	if err != nil {
		d.log().Error("could not resolve the registry credential of a stack image", "environment", envID, "image", text, "error", err)
		res.infra = CodeRegistryUnresolved
		return res
	}
	var resolved dockerx.ResolvedImage
	err = d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var rerr error
		resolved, rerr = c.ResolveImage(ctx, text, auth)
		return rerr
	})
	if err != nil {
		if code := codeOf(ctx, err); code != CodeEngineError {
			// The agent refused, no manager is connected or the deploy was cancelled: not a
			// fact about this image.
			res.infra = code
			return res
		}
		res.failure = CodeImageUnresolved
		d.log().Warn("could not look up a stack image", "environment", envID, "image", text, "reason", dockerx.PullFailure(err))
		return res
	}
	res.ref = resolved.Ref
	return res
}

// imageProblem is the blocking problem for a service whose image could not be looked up.
func imageProblem(res lookupResult, sp convert.ServicePlan) stacks.Problem {
	p := stacks.Problem{
		Severity: stacks.SeverityError, Code: res.failure,
		File: stacks.ComposeFile, Key: "services." + sp.Short + ".image",
	}
	p.Message = fmt.Sprintf("Couldn't look up %s on %s to pin it to a digest. Check the name, the tag and the credential, then try again.", sp.ImageAsWritten, res.host)
	return p
}

// registryHost is the registry an image reference names, docker.io for a Docker Hub name; the
// word "the registry" when the reference does not parse.
func registryHost(text string) string {
	ref, err := registry.ParseRef(text)
	if err != nil {
		return "the registry"
	}
	return ref.Domain
}

// sameImageText reports whether two image references name the same thing as text: they are
// equal, or they are the same after docker's normalisation (docker.io/library/ and an omitted
// :latest). A stored pin is spelled the way docker spells a reference, a compose file as its
// author wrote it.
func sameImageText(a, b string) bool {
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
