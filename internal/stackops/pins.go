// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/swarm"

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
// what decided it. Note is the sentence for a warning (a local image), empty otherwise. Platforms are the image's
// platforms, the way a swarm schedules by them (TaskTemplate.Placement.Platforms), empty when they are not known.
type Pin struct {
	Service, AsWritten, Ref string
	Action                  stacks.PinAction
	Note                    string
	Platforms               []swarm.Platform
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

// pinItem is the part of a service the pin decision reads: the name it is keyed by (the swarm service
// name on a swarm, the name in the compose file on a standalone host), its name in the compose file and
// the image as written.
type pinItem struct{ Name, Short, AsWritten string }

// reader runs fn against the engine that answers for a stack's environment: a manager on a swarm, the
// host's own engine on a standalone environment.
type reader func(ctx context.Context, fn func(*dockerx.Client) error) error

// clusterReader reads through a manager of the environment, as the swarm lookups always did.
func (d Deps) clusterReader(envID string) reader {
	return func(ctx context.Context, fn func(*dockerx.Client) error) error {
		return d.Clusters.ClusterRead(ctx, envID, fn)
	}
}

// swarmItems are the pin items of the converted services of a swarm stack.
func swarmItems(services []convert.ServicePlan) []pinItem {
	items := make([]pinItem, 0, len(services))
	for _, sp := range services {
		items = append(items, pinItem{Name: sp.Name, Short: sp.Short, AsWritten: sp.ImageAsWritten})
	}
	return items
}

// swarmLivePins are the images the swarm's services run now and the text they were deployed from: the
// com.docker.stack.image label and the image of the spec. A service that records no text is left out.
func swarmLivePins(live map[string]dockerx.ServiceDetail) map[string]stacks.LivePin {
	out := make(map[string]stacks.LivePin, len(live))
	for name, det := range live {
		if written := det.Labels[stacks.LabelSwarmImage]; written != "" && det.Image != "" {
			out[name] = stacks.LivePin{AsWritten: written, Image: det.Image}
		}
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
	return d.resolvePins(ctx, envID, swarmItems(services), swarmLivePins(live), stored, update, d.clusterReader(envID))
}

// resolvePins is ResolvePins over the items, the live pins and the engine that answers for the stack.
func (d Deps) resolvePins(ctx context.Context, envID string, items []pinItem, live, stored map[string]stacks.LivePin, update map[string]bool, read reader) PinOutcome {
	out := PinOutcome{Pins: make(map[string]Pin, len(items))}
	looked := map[string]lookupResult{}
	for _, it := range items {
		text := it.AsWritten
		if text == "" {
			continue // a service with no image has nothing to pin
		}
		dec := stacks.DecidePin(pinInputFor(it, live, stored, update))
		pin := Pin{Service: it.Name, AsWritten: text, Ref: dec.Ref, Action: dec.Action}
		if dec.Action == stacks.PinResolve {
			res, done := looked[text]
			if !done {
				res = d.lookupImage(ctx, envID, text, read)
				looked[text] = res
			}
			if res.infra != "" {
				out.Code, out.FailedService = res.infra, it.Name
				return out
			}
			switch {
			case res.failure != "":
				out.Problems = append(out.Problems, imageProblem(res, it))
				if out.Code == "" {
					out.Code, out.FailedService = res.failure, it.Name
				}
				continue
			case res.local:
				pin.Action, pin.Ref, pin.Note = stacks.PinLocal, res.ref, localImageMessage(text)
				out.Problems = append(out.Problems, stacks.Problem{
					Severity: stacks.SeverityWarning, Code: CodeLocalImage, Message: pin.Note,
					File: stacks.ComposeFile, Key: "services." + it.Short + ".image",
				})
			default:
				pin.Ref = res.ref
			}
		}
		out.Pins[it.Name] = pin
	}
	return out
}

// pinInputFor is what DecidePin is asked about one service: the text of its image, the image it
// runs now (when the engine has the service and it records what was written), the image its last
// good revision ran for the same text, and whether the person asked for it to move.
func pinInputFor(it pinItem, live, stored map[string]stacks.LivePin, update map[string]bool) stacks.PinInput {
	text := it.AsWritten
	in := stacks.PinInput{Service: it.Name, AsWritten: text, Update: update[it.Short] || update[it.Name]}
	if lp, ok := live[it.Name]; ok {
		in.Live = &lp
	}
	if st, ok := stored[it.Name]; ok && sameImageText(st.AsWritten, text) {
		in.Stored = &stacks.LivePin{AsWritten: text, Image: st.Image}
	}
	return in
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
// credential chosen for it, through the engine read answers for (a manager on a swarm). A registry that does not give the image, when the
// manager holds an image of that name that no registry ever gave it (it has no repository
// digest), is a local image and is pinned to its ID (D-18). A registry that cannot be reached
// says nothing about the image, so it never falls back to one.
func (d Deps) lookupImage(ctx context.Context, envID, text string, read reader) lookupResult {
	res := lookupResult{host: registryHost(text)}
	auth, err := d.authFor(ctx, envID, text)
	if err != nil {
		d.log().Error("could not resolve the registry credential of a stack image", "environment", envID, "image", text, "error", err)
		res.infra = CodeRegistryUnresolved
		return res
	}
	var resolved dockerx.ResolvedImage
	var local *dockerx.ImageDetail
	err = read(ctx, func(c *dockerx.Client) error {
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
func imageProblem(res lookupResult, it pinItem) stacks.Problem {
	p := stacks.Problem{
		Severity: stacks.SeverityError, Code: res.failure,
		File: stacks.ComposeFile, Key: "services." + it.Short + ".image",
	}
	if res.failure == CodeRegistryUnreachable {
		p.Message = fmt.Sprintf("Bayplan can't reach %s, and %s needs a fresh lookup. Services that are already pinned don't need the registry.", res.host, it.Short)
	} else {
		p.Message = fmt.Sprintf("Couldn't look up %s on %s to pin it to a digest. Check the name, the tag and the credential, then try again.", it.AsWritten, res.host)
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
func sameImageText(a, b string) bool { return stacks.SameImageText(a, b) }

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

// CodePinsInvalid is the code of a deploy whose reviewed images do not fit what the server
// decides for the stack: a pin the review could not have produced.
const CodePinsInvalid = "pins_invalid"

// PinsInvalidError is a reviewed pin that does not fit the decision: Service is the swarm service
// it is about, Reason a short word for why (missing, unknown, differs, outside_repository,
// not_local).
type PinsInvalidError struct{ Service, Reason string }

func (e *PinsInvalidError) Error() string {
	return "stackops: the reviewed image of " + e.Service + " is not acceptable: " + e.Reason
}

var (
	digestRe  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	pinRefTag = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
)

// ReviewedPins checks the images a deploy was asked to use, which a review chose, against what the
// server decides now (stacks.DecidePin), and returns them as the pins to deploy with. A service
// that keeps the digest it runs must be sent that digest; a reference that carries its own is sent
// as written; a service that is looked up must be sent a sha256 digest of the repository the
// reference names, or the image ID of an image that is on the manager and was never given by a
// registry. A pin for a service the stack does not have, or none for one that has an image, is
// refused. Nothing here asks a registry: the deploy that follows a review makes no lookup.
func (d Deps) ReviewedPins(ctx context.Context, envID string, services []convert.ServicePlan, live map[string]dockerx.ServiceDetail, stored map[string]stacks.LivePin, update map[string]bool, reviewed map[string]string) (map[string]Pin, error) {
	return d.reviewedPins(ctx, envID, swarmItems(services), swarmLivePins(live), stored, update, reviewed, d.clusterReader(envID))
}

// reviewedPins is ReviewedPins over the items, the live pins and the engine that answers for the stack.
func (d Deps) reviewedPins(ctx context.Context, envID string, items []pinItem, live, stored map[string]stacks.LivePin, update map[string]bool, reviewed map[string]string, read reader) (map[string]Pin, error) {
	out := make(map[string]Pin, len(items))
	named := make(map[string]bool, len(items))
	for _, it := range items {
		text := it.AsWritten
		if text == "" {
			continue
		}
		named[it.Name] = true
		ref, ok := reviewed[it.Name]
		if !ok {
			return nil, &PinsInvalidError{Service: it.Name, Reason: "missing"}
		}
		dec := stacks.DecidePin(pinInputFor(it, live, stored, update))
		pin := Pin{Service: it.Name, AsWritten: text, Ref: ref, Action: dec.Action}
		switch dec.Action {
		case stacks.PinKeep, stacks.PinAsWritten:
			if ref != dec.Ref {
				return nil, &PinsInvalidError{Service: it.Name, Reason: "differs"}
			}
		default:
			switch {
			case digestRe.MatchString(ref):
				local, err := d.isLocalImage(ctx, text, ref, read)
				if err != nil {
					return nil, err
				}
				if !local {
					return nil, &PinsInvalidError{Service: it.Name, Reason: "not_local"}
				}
				pin.Action, pin.Note = stacks.PinLocal, localImageMessage(text)
			case sameRepository(text, ref):
			default:
				return nil, &PinsInvalidError{Service: it.Name, Reason: "outside_repository"}
			}
		}
		out[it.Name] = pin
	}
	for name := range reviewed {
		if !named[name] {
			return nil, &PinsInvalidError{Service: name, Reason: "unknown"}
		}
	}
	return out, nil
}

// sameRepository reports whether ref is a reference to the repository text names, pinned to a
// sha256 digest of 64 hexadecimal characters.
func sameRepository(text, ref string) bool {
	if !pinRefTag.MatchString(ref) {
		return false
	}
	want, err := reference.ParseNormalizedNamed(text)
	if err != nil {
		return false
	}
	got, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return false
	}
	return reference.TrimNamed(want).Name() == reference.TrimNamed(got).Name()
}

// isLocalImage reports whether the manager holds an image of that name whose ID is id and that no
// registry ever gave it: the image a deploy pins by its ID.
func (d Deps) isLocalImage(ctx context.Context, text, id string, read reader) (bool, error) {
	local := false
	err := read(ctx, func(c *dockerx.Client) error {
		det, _, e := c.ImageDetail(ctx, text)
		if e != nil {
			return nil //nolint:nilerr // an image the manager does not hold is not a local image
		}
		local = det.ID == id && len(det.RepoDigests) == 0
		return nil
	})
	return local, err
}

// ValidatePins checks the reviewed pins of a deploy before the deploy starts, so a request whose
// pins do not fit is refused with nothing stored: it reads the services the stack has and the
// pins of its last good revision, and applies ReviewedPins.
func (d Deps) ValidatePins(ctx context.Context, envID string, stack Stack, services []convert.ServicePlan, store PinStore, update map[string]bool, reviewed map[string]string) error {
	var svcs []dockerx.ServiceDetail
	if err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		svcs, e = c.ServicesWithLabel(ctx, stacks.LabelSwarmNamespace, stack.Name)
		return e
	}); err != nil {
		return err
	}
	live := make(map[string]dockerx.ServiceDetail, len(svcs))
	for _, s := range svcs {
		live[s.Name] = s
	}
	var stored map[string]stacks.LivePin
	if store != nil {
		var err error
		if stored, err = store.GoodPins(ctx, stack.ID); err != nil {
			d.log().Warn("could not read the pins of a stack's last good revision", "environment", envID, "stack", stack.Name, "error", err)
		}
	}
	_, err := d.ReviewedPins(ctx, envID, services, live, stored, update, reviewed)
	return err
}

// ValidateStandalonePins is ValidatePins for a stack of a standalone host: it checks the reviewed pins of a
// deploy before the deploy starts, so a request whose pins do not fit is refused with nothing stored. It
// reads the containers the host holds under the stack's Compose project and the pins of its last good
// revision, and applies the same table. project is the stack as checked, not yet prepared.
func (d Deps) ValidateStandalonePins(ctx context.Context, envID, nodeID string, stack Stack, project *types.Project, store PinStore, update map[string]bool, reviewed map[string]string) error {
	var containers []dockerx.ComposeContainer
	read := d.nodeReader(envID, nodeID)
	if err := read(ctx, func(c *dockerx.Client) error {
		var e error
		containers, e = c.ComposeContainers(ctx, stack.Name)
		return e
	}); err != nil {
		return err
	}
	var stored map[string]stacks.LivePin
	if store != nil {
		var err error
		if stored, err = store.GoodPins(ctx, stack.ID); err != nil {
			d.log().Warn("could not read the pins of a stack's last good revision", "environment", envID, "stack", stack.Name, "error", err)
		}
	}
	_, err := d.reviewedPins(ctx, envID, standaloneItems(project), standaloneLivePins(containers), stored, update, reviewed, read)
	return err
}
