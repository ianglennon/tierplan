// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/distribution/reference"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/jobs"
	"github.com/ianglennon/coaming/internal/registry"
)

// The pre-pull of a standalone deploy (plan 07-21, D-27, IMG-02, STK-09). The Compose SDK pulls an image it
// does not find with the credentials of its own configuration directory, which is empty on purpose (nothing of
// the server's disk reaches a host), so a private image would fail at its pull. Coaming pulls the images
// itself, before the SDK runs, with the credential it stores for the image's registry; the SDK then finds
// every image on the host and never needs a credential.

const (
	// ItemPull is where a pull failure sits in the order of the deploy's steps: after the lookup, before the
	// first object is created. It is no item of the job: each pull is a step of its own under the lookup,
	// worded "Pull {image}".
	ItemPull = "pull"

	idPull = "pull:"

	// pullTimeout is how long one image may take to pull. A large image over a slow link takes many minutes
	// (the same bound as a pull of Phase 5).
	pullTimeout = 30 * time.Minute

	// recordTimeout bounds a write of a credential's bookkeeping after a pull.
	recordTimeout = 10 * time.Second
)

// prePullError is a pre-pull that failed: the image and a service that uses it, the code of the failure and
// the error (whose text stays in the server log).
type prePullError struct {
	Service, Image, Code string
	Err                  error
}

func (e *prePullError) Error() string {
	return fmt.Sprintf("stackops: pull of %s for %s failed (%s): %v", e.Image, e.Service, e.Code, e.Err)
}

func (e *prePullError) Unwrap() error { return e.Err }

// shownImage is an image reference as a person reads it: the repository and the tag it was written with, the
// digest of a pin left off.
func shownImage(ref string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ref
	}
	if t, ok := named.(reference.Tagged); ok {
		if tagged, err := reference.WithTag(reference.TrimNamed(named), t.Tag()); err == nil {
			named = tagged
		}
	} else {
		named = reference.TrimNamed(named)
	}
	return reference.FamiliarString(named)
}

// prePull pulls, on the node, each image of pins that the host does not have, one at a time, with the
// credential Resolver.AuthFor chooses for it. pins are the references to pull by service name; a reference
// that is a local image's ID is not a registry's and is left alone, and an image shared by services is pulled
// once. Each pull is a step of the job under the lookup (id "pull:<reference>", labelled "Pull {image}"), with
// the engine's progress counted in bytes; a pull that fails ends its step with the code (pull_denied,
// image_not_found, rate_limited, registry_tls ...) and its sentence, never the registry's own words, the steps
// after it end skipped, and the error returned is a *prePullError. Nothing is retried. The credential is a
// header: it goes to the pull and nowhere else.
func (d Deps) prePull(ctx context.Context, envID, nodeID string, pins map[string]string, r *jobs.Reporter) error {
	type image struct{ ref, service string }
	services := make([]string, 0, len(pins))
	for svc := range pins {
		services = append(services, svc)
	}
	sort.Strings(services)
	var wanted []image
	seen := map[string]bool{}
	for _, svc := range services {
		ref := pins[svc]
		if ref == "" || imageIDRe.MatchString(ref) || seen[ref] {
			continue
		}
		seen[ref] = true
		wanted = append(wanted, image{ref: ref, service: svc})
	}
	if len(wanted) == 0 {
		return nil
	}

	// Which of them the host lacks. The engine finds an image by the digest of the reference, whatever tag
	// the reference was written with.
	var missing []image
	err := d.nodeReader(envID, nodeID)(ctx, func(c *dockerx.Client) error {
		missing = missing[:0]
		for _, w := range wanted {
			if _, _, err := c.ImageDetail(ctx, w.ref); err != nil {
				if !dockerx.IsNotFound(err) {
					return err
				}
				missing = append(missing, w)
			}
		}
		return nil
	})
	if err != nil {
		d.log().Warn("could not look for the images of a standalone deploy on the host", "environment", envID, "error", err)
		return &prePullError{Service: wanted[0].service, Image: wanted[0].ref, Code: PullErrorCode(err), Err: err}
	}
	if len(missing) == 0 {
		return nil
	}

	for _, m := range missing {
		r.Add(jobs.ItemSpec{ID: idPull + m.ref, Label: "Pull " + shownImage(m.ref), Parent: ItemLookup, Kind: KindStep})
	}
	// finishRest ends the steps after the one that failed: they were not run.
	finishRest := func(from int, state, code string) {
		for _, m := range missing[from:] {
			r.Finish(idPull+m.ref, state, code, CodeMessage(code), nil)
		}
	}
	cleared := map[string]bool{}
	for i, m := range missing {
		id, shown := idPull+m.ref, shownImage(m.ref)
		result := map[string]string{"image": shown}
		r.Update(id, func(it *jobs.Item) { it.State, it.Result = jobs.ItemRunning, map[string]string{"image": shown} })
		r.Output("Pull " + shown)

		var header string
		var used *registry.Candidate
		parsed, perr := registry.ParseRef(m.ref)
		if d.Resolver != nil && perr == nil {
			var aerr error
			if header, used, aerr = d.Resolver.AuthFor(ctx, envID, parsed); aerr != nil {
				// The error names the credential by ID and holds no value.
				d.log().Error("could not resolve the registry credential of a stack image", "environment", envID, "image", m.ref, "error", aerr)
				r.Finish(id, jobs.ItemFailed, CodeRegistryUnresolved, CodeMessage(CodeRegistryUnresolved), result)
				finishRest(i+1, jobs.ItemSkipped, CodeNotRun)
				return &prePullError{Service: m.service, Image: m.ref, Code: CodeRegistryUnresolved, Err: aerr}
			}
		}
		pullRef := m.ref
		if perr == nil {
			pullRef = parsed.String()
			// The reference as the pull dialog words it: the registry written out, the tag, no digest.
			if tagged, err := reference.WithTag(reference.TrimNamed(parsed.Named), parsed.Tag); err == nil && parsed.Tag != "" {
				result["ref"] = tagged.String()
			} else {
				result["ref"] = reference.TrimNamed(parsed.Named).String()
			}
		}
		if used != nil {
			result["credential"], result["credentialId"], result["credentialLayer"] = used.Name, used.ID, used.Layer
		}

		pctx, cancel := context.WithTimeout(ctx, pullTimeout)
		index := map[string]int{}
		var res dockerx.PullResult
		err := d.Aggregator.WriteOnNode(pctx, envID, nodeID, func(c *dockerx.Client) error {
			var e error
			res, e = c.ImagePull(pctx, pullRef, dockerx.PullOptions{EncodedAuth: header}, func(ev dockerx.PullEvent) {
				r.Update(id, func(it *jobs.Item) { ApplyPullEvent(it, index, ev) })
			})
			return e
		})
		cancel()
		if err != nil {
			code := PullErrorCode(err)
			if code == dockerx.PullCodeFailed || code == CodeNodeUnreachable {
				// The text stays in the server log; the job carries the code only.
				d.log().Warn("the pull of a stack image failed", "environment", envID, "image", m.ref, "code", code, "error", err)
			}
			d.noteCredential(ctx, used, code, cleared)
			state, later := jobs.ItemFailed, jobs.ItemSkipped
			if code == dockerx.PullCodeCancelled || ctx.Err() != nil {
				code, state, later = CodeCancelled, jobs.ItemCancelled, jobs.ItemCancelled
			}
			r.Finish(id, state, code, CodeMessage(code), result)
			finishRest(i+1, later, CodeNotRun)
			return &prePullError{Service: m.service, Image: m.ref, Code: code, Err: err}
		}
		d.noteCredential(ctx, used, "", cleared)
		result["digest"], result["upToDate"] = res.Digest, strconv.FormatBool(res.UpToDate)
		r.Update(id, func(it *jobs.Item) {
			it.Done = it.Count
			if it.Total > 0 {
				it.Current = it.Total
			}
		})
		r.Finish(id, jobs.ItemDone, "", "", result)
	}
	return nil
}

// noteCredential keeps the credential's bookkeeping after a pull (D-07): a registry that refused the sign-in
// records the failure with its code, and a pull that worked clears it, once per credential and deploy. A
// pull the registry answered "denied" does not count: that is what it says of a repository that does not
// exist or that the account may not read, so anyone who can deploy could otherwise flag a working credential
// (WR-B03). A failed write is logged and changes nothing else.
func (d Deps) noteCredential(ctx context.Context, used *registry.Candidate, code string, cleared map[string]bool) {
	if used == nil || d.Resolver == nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	var err error
	switch code {
	case dockerx.PullCodeUnauthorized:
		err = d.Resolver.RecordFailure(wctx, used.ID, code)
	case "":
		if cleared[used.ID] {
			return
		}
		cleared[used.ID] = true
		err = d.Resolver.ClearFailure(wctx, used.ID)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		d.log().Error("could not update a credential's last failure", "credential", used.ID, "error", err)
	}
}
