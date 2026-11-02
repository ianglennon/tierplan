// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// driftStandalone is Drift for a stack on a standalone host (D-21, RESEARCH Pattern 6). The deployed revision is
// prepared as a roll back to it is (PrepareRollbackStandalone, with the pins it stored) and as a deploy prepares it
// (stacks.PrepareStandalone), and compared with the containers of the stack's Compose project by the review's own
// comparison (compareStandalone: config hash, then the field-level difference of the service against the one rebuilt
// from the container's inspect document). Current is what runs now and Next what the revision deployed, as in the
// swarm check.
//
// Three things are added to what the review compares. A container's image is checked against the revision's pin,
// because the field comparison leaves the image out. A service whose containers differ from the prepared service in
// nothing the person wrote (an adopted stack's containers carry no label of TierPlan's, so their config hash never
// matches) is not drift. And a container of the project that the revision does not declare is Extra when it is the
// stack's, as a service with no container is Missing.
//
// A container is the stack's when it carries the stack's ID, or when it carries none and the stack owns another container
// of the same service: a container the host CLI added to a service (docker compose up --scale) is drift by its scale
// (G-08-6). A container with another stack's ID is not the stack's, and neither is any container of a project the stack
// holds none of, nor a container with no ID of a service the stack runs nothing of (SplitProject, WR-03).
//
// It only reads the host: the Compose SDK is not asked for anything. The caller has already answered a stopped stack
// and a stack with no good revision (Drift).
func (d Deps) driftStandalone(ctx context.Context, in DriftInput) (DriftResult, error) {
	res := DriftResult{CheckedAt: d.clock().Now().UTC(), Revision: in.Deployed.Seq}
	deployed := *in.Deployed

	prep, err := d.PrepareRollbackStandalone(ctx, in.Stack, deployed, in.Pins)
	if err != nil {
		var pe *PrepareError
		if errors.As(err, &pe) {
			res.State = DriftUnknown
			return res, nil
		}
		return DriftResult{}, err
	}
	project := prep.Loaded.Project
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, project, prep.Files, prep.Vars)
	if err != nil {
		res.State = DriftUnknown
		return res, nil
	}
	restored, err := RestoredStandalonePins(project, in.Pins)
	if err != nil {
		res.State = DriftUnknown
		return res, nil
	}
	pins := make(map[string]string, len(restored))
	for name, p := range restored {
		pins[name] = p.Ref
	}
	if err := stacks.PrepareStandalone(project, in.Stack.ID, pins, objs); err != nil {
		res.State = DriftUnknown
		return res, nil
	}

	node, err := d.StandaloneNode(ctx, in.EnvID)
	if err != nil {
		return DriftResult{}, err
	}
	own := OwnershipOf(in.Stack)
	var owned []dockerx.ComposeContainer
	var cmp standaloneComparison
	masker := driftMasker(in, deployed)
	err = d.Aggregator.OnNode(ctx, in.EnvID, node, func(c *dockerx.Client) error {
		all, e := c.ComposeContainers(ctx, in.Stack.Name)
		if e != nil {
			return e
		}
		// What the stack owns, and what the host CLI added to its project without TierPlan's label, is the stack's to
		// compare; a container with another stack's ID, or in a project the stack owns nothing of, is not (SplitProject).
		mine, unlabelled, _ := SplitProject(own, all, func(c dockerx.ComposeContainer) map[string]string { return c.Labels })
		owned = append(mine, unlabelled...)
		cmp, e = compareStandalone(ctx, c, project, owned, nil, masker)
		return e
	})
	if err != nil {
		return DriftResult{}, err
	}

	rows := map[string]DriftService{}
	for _, c := range cmp.Changes {
		if c.Added {
			rows[c.Name] = DriftService{Name: c.Name, Short: c.Name, Change: DriftMissing, Diff: c.Row}
			continue
		}
		// A service that differs only in its config hash, with no setting and nothing hidden that differs, is a
		// container that was made without TierPlan's labels: not drift (D-21).
		if c.Forced || c.Row.Settings > 0 || len(c.Row.HiddenChanges) > 0 {
			rows[c.Name] = DriftService{Name: c.Name, Short: c.Name, Change: DriftChanged, Diff: c.Row}
		}
	}
	for name, run := range imageDrift(cmp.Images, pins) {
		row, ok := rows[name]
		if !ok {
			row = DriftService{Name: name, Short: name, Change: DriftChanged, Diff: stacks.ServiceChange{Name: name, Short: name, Change: stacks.ChangeChanged}}
		}
		row.Diff.Current = withImage(row.Diff.Current, run.have)
		row.Diff.Next = withImage(row.Diff.Next, run.want)
		row.Diff.Settings++
		rows[name] = row
	}
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		row := rows[name]
		if row.Change == DriftMissing {
			res.Count++
		} else {
			res.Count += row.Diff.Settings
		}
		res.Services = append(res.Services, row)
	}
	for _, name := range extraServices(project.Services, owned) {
		extra := stacks.ServiceChange{Name: name, Short: name, Change: stacks.ChangeRemoved}
		// An extra service has no spec to show, but how many containers run it is what the page counts: it is
		// said the way a compose service says it, as its scale, when there is more than one.
		if n := containersOf(owned, name); n > 1 {
			extra.Current = json.RawMessage(`{"scale":` + strconv.Itoa(n) + `}`)
		}
		res.Services = append(res.Services, DriftService{Name: name, Short: name, Change: DriftExtra, Diff: extra})
		res.Count++
	}

	res.Fingerprint = driftContainersFingerprint(owned, deployed.Seq)
	res.State = DriftNone
	if len(res.Services) > 0 {
		res.State = DriftDrifted
	}
	return res, nil
}

// extraServices are the services the stack's containers belong to that the project does not declare, in name order.
func extraServices[V any](declared map[string]V, owned []dockerx.ComposeContainer) []string {
	var out []string
	for _, c := range owned {
		if _, ok := declared[c.Service]; !ok && c.Service != "" && !slices.Contains(out, c.Service) {
			out = append(out, c.Service)
		}
	}
	sort.Strings(out)
	return out
}

// containersOf is how many of the containers run a service.
func containersOf(containers []dockerx.ComposeContainer, service string) int {
	n := 0
	for _, c := range containers {
		if c.Service == service {
			n++
		}
	}
	return n
}

// imageChange is the image a service's container runs and the one the revision pinned for it.
type imageChange struct{ have, want string }

// imageDrift is, for each service with a container made from another image than the revision pinned, the first such
// container's reference and the pin (D-21). A service the revision pinned nothing for is left out.
func imageDrift(images []runningImage, pins map[string]string) map[string]imageChange {
	out := map[string]imageChange{}
	for _, run := range images {
		pin := pins[run.Service]
		if _, seen := out[run.Service]; seen || pin == "" || sameImage(pin, run) {
			continue
		}
		out[run.Service] = imageChange{have: run.Ref, want: pin}
	}
	return out
}

// sameImage says whether a container runs the image a pin names. A pin that holds a digest is met by a reference with
// that digest, or by a reference without one whose image the host knows by it (a container made from the tag);
// an image ID is met by the ID of the image the container runs; a pin with no digest is met by the same name and tag.
func sameImage(pin string, run runningImage) bool {
	if run.Ref == "" || pin == run.Ref {
		return true
	}
	if want := digestOf(pin); want != "" {
		if have := digestOf(run.Ref); have != "" {
			return have == want
		}
		return slices.Contains(run.Digests, want) || run.ImageID == want
	}
	name, _, _ := strings.Cut(run.Ref, "@")
	return name == pin
}

// withImage sets the image of a service's settings, which the comparison leaves out, in the JSON of one side of a row.
// A side that is empty or not an object becomes an object with the image alone.
func withImage(side json.RawMessage, image string) json.RawMessage {
	tree := map[string]json.RawMessage{}
	if len(side) > 0 {
		if err := json.Unmarshal(side, &tree); err != nil || tree == nil {
			tree = map[string]json.RawMessage{}
		}
	}
	quoted, _ := json.Marshal(image)
	tree["image"] = quoted
	out, err := json.Marshal(tree)
	if err != nil {
		return side
	}
	return out
}

// driftContainersFingerprint names what the host held for the stack at a check: the revision and, for each container that is
// the stack's (owned, or added from the host CLI), its ID and config hash, so a container that is recreated or added or removed moves it.
func driftContainersFingerprint(owned []dockerx.ComposeContainer, seq int64) string {
	parts := make([]string, 0, len(owned))
	for _, c := range owned {
		parts = append(parts, c.ID+":"+c.ConfigHash)
	}
	sort.Strings(parts)
	out := "rev" + strconv.FormatInt(seq, 10)
	for _, p := range parts {
		out += "," + p
	}
	return out
}
