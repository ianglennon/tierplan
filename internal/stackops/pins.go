// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/distribution/reference"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// The codes of the image lookup. CodeRegistryUnreachable and CodeImageUnresolved end the lookup
// item of a deploy; CodeLocalImage is the code of the warning for an image no registry has.
const (
	CodeRegistryUnreachable = "registry_unreachable"
	CodeImageUnresolved     = "image_unresolved"
	CodeLocalImage          = "local_image"
)

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
func isImageFailure(code string) bool {
	return code == CodeImageUnresolved || code == CodeRegistryUnreachable
}

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
			switch {
			case res.failure != "":
				out.Problems = append(out.Problems, imageProblem(res, sp))
				if out.Code == "" {
					out.Code, out.FailedService = res.failure, sp.Name
				}
				continue
			case res.local:
				pin.Action, pin.Ref, pin.Note = stacks.PinLocal, res.ref, localImageMessage(text)
				out.Problems = append(out.Problems, stacks.Problem{
					Severity: stacks.SeverityWarning, Code: CodeLocalImage, Message: pin.Note,
					File: stacks.ComposeFile, Key: "services." + sp.Short + ".image",
				})
			default:
				pin.Ref = res.ref
			}
		}
		out.Pins[sp.Name] = pin
	}
	return out
}

// lookupResult is the answer of one lookup: the reference to send and whether it is a local
// image ID, or why there is none. failure is a lookup failure that blocks only the services that
// need it (CodeRegistryUnreachable or CodeImageUnresolved), infra a deploy code for one that
// blocks the deploy.
type lookupResult struct {
	ref            string
	local          bool
	failure, infra string
	host           string
}

// lookupImage resolves one reference to the digest its registry holds for it now, with the
// credential chosen for it, through a manager. A registry that does not give the image, when the
// manager holds an image of that name that no registry ever gave it (it has no repository
// digest), is a local image and is pinned to its ID (D-18). A registry that cannot be reached
// says nothing about the image, so it never falls back to one.
func (d Deps) lookupImage(ctx context.Context, envID, text string) lookupResult {
	res := lookupResult{host: registryHost(text)}
	auth, err := d.authFor(ctx, envID, text)
	if err != nil {
		d.log().Error("could not resolve the registry credential of a stack image", "environment", envID, "image", text, "error", err)
		res.infra = CodeRegistryUnresolved
		return res
	}
	var resolved dockerx.ResolvedImage
	var local *dockerx.ImageDetail
	err = d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		local = nil
		var rerr error
		resolved, rerr = c.ResolveImage(ctx, text, auth)
		if rerr == nil {
			return nil
		}
		switch dockerx.PullFailure(rerr) {
		case dockerx.PullCodeNotFound, dockerx.PullCodeUnauthorized, dockerx.PullCodeDenied:
			// A registry that does not give the image may be asked for one built on the host.
			if det, _, e := c.ImageDetail(ctx, text); e == nil && len(det.RepoDigests) == 0 {
				local = &det
			}
		}
		return rerr
	})
	switch {
	case err == nil:
		res.ref = resolved.Ref
	case local != nil:
		res.ref, res.local = local.ID, true
	default:
		if code := codeOf(ctx, err); code != CodeEngineError {
			// The agent refused, no manager is connected or the deploy was cancelled: not a
			// fact about this image.
			res.infra = code
			return res
		}
		res.failure = CodeImageUnresolved
		if unreachable(err) {
			res.failure = CodeRegistryUnreachable
		}
		d.log().Warn("could not look up a stack image", "environment", envID, "image", text, "failure", res.failure, "reason", dockerx.PullFailure(err))
	}
	return res
}

// unreachable reports whether a lookup failed because the registry could not be reached: a
// connection that was refused or never made, or a timeout.
func unreachable(err error) bool {
	var ne net.Error
	return dockerx.PullFailure(err) == dockerx.PullCodeRegistryUnreachable ||
		errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// imageProblem is the blocking problem for a service whose image could not be looked up.
func imageProblem(res lookupResult, sp convert.ServicePlan) stacks.Problem {
	p := stacks.Problem{
		Severity: stacks.SeverityError, Code: res.failure,
		File: stacks.ComposeFile, Key: "services." + sp.Short + ".image",
	}
	if res.failure == CodeRegistryUnreachable {
		p.Message = fmt.Sprintf("Bayplan can't reach %s, and %s needs a fresh lookup. Services that are already pinned don't need the registry.", res.host, sp.Short)
	} else {
		p.Message = fmt.Sprintf("Couldn't look up %s on %s to pin it to a digest. Check the name, the tag and the credential, then try again.", sp.ImageAsWritten, res.host)
	}
	return p
}

func localImageMessage(image string) string {
	return fmt.Sprintf("“%s” isn't from a registry, so Bayplan pins it to the local image ID.", image)
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

// placeProblems gives each problem about a key of the compose file the line and column of that
// key, so the editor and the review can mark it. A problem whose key the file does not have, or a
// file that cannot be indexed, keeps no position.
func placeProblems(files stacks.FileSet, ps stacks.Problems) stacks.Problems {
	f, ok := files[stacks.ComposeFile]
	if !ok || len(ps) == 0 {
		return ps
	}
	ix, err := stacks.NewPositionIndex(f.Content)
	if err != nil {
		return ps
	}
	out := make(stacks.Problems, len(ps))
	copy(out, ps)
	for i := range out {
		if pos, found := ix.Lookup(out[i].Key); found && out[i].File == stacks.ComposeFile {
			out[i].Line, out[i].Col, out[i].EndLine, out[i].EndCol = pos.Line, pos.Col, pos.EndLine, pos.EndCol
		}
	}
	return out
}
