// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// What an update would do, before it is asked for (plan 12-16, D-05). A preview reads the check that is stored and the live state
// of the service; it asks no registry, so opening the confirm is cheap and shows what the badge showed.

// The refusals of a preview: why the service is not offered an update. They are the codes of a NotUpdatableError, and the words
// of the states that have nothing to update.
const (
	RefusalNothingToUpdate = "nothing_to_update"
	RefusalNotChecked      = "not_checked"
	RefusalCheckFailed     = "check_failed"
)

// previewInspectTimeout bounds the one inspect of the running image a preview makes (T-12-45).
const previewInspectTimeout = 5 * time.Second

// PreviewItem is one workload an update would change.
type PreviewItem struct {
	Kind, Name, Stack string
	// Image is the image the workload follows, without a digest (repository:tag).
	Image string
	// FromDigest is the digest it runs and ToDigest the one the stored check found.
	FromDigest, ToDigest string
	// Platforms are the platforms it runs on and ChangedPlatforms those of them whose image changed.
	Platforms, ChangedPlatforms []string
}

// Preview is what the confirm of an update shows.
type Preview struct {
	Items    []PreviewItem
	Warnings []Warning
	// LeftOut are the services of a stack that an update of the stack leaves out because they are skipped or ignored.
	LeftOut []StackLeftOut
	// Lock is who holds the environment's operation lock, nil when nobody does.
	Lock *stackops.Holder
	// Offered is true when an update would be offered. Refusal says why not, empty when it is.
	Offered bool
	Refusal string
}

// Preview describes the update of the service name of an environment: the digest change and the warnings of running it, or the
// reason it would not run. ErrWorkloadGone is returned when there is no such service and ErrUnsupportedKind for a kind this
// applier does not update. Nothing is written and no registry is asked.
func (a *Applier) Preview(ctx context.Context, envID, kind, name string) (Preview, error) {
	if a.Service == nil || a.Locks == nil {
		return Preview{}, errors.New("updates: the applier is not wired")
	}
	if kind == "stack" {
		sp, err := a.PreviewStack(ctx, envID, name, nil)
		if err != nil {
			return Preview{}, err
		}
		return Preview(sp), nil
	}
	if kind == "container" {
		return a.previewContainer(ctx, envID, name)
	}
	if kind != "service" {
		return Preview{}, ErrUnsupportedKind
	}
	row, found, err := a.serviceRow(ctx, envID, name)
	if err != nil {
		return Preview{}, err
	}
	if !found {
		return Preview{}, ErrWorkloadGone
	}
	var p Preview
	if h, held := a.Locks.Held(envID); held {
		p.Lock = &h
	}
	var nu *NotUpdatableError
	if err := a.updatable(ctx, envID, row); errors.As(err, &nu) {
		if nu.Code == NotUpdatableManaged {
			// A service of a stack TierPlan manages is updated through a revision of that stack (plan 12-18): the preview is
			// that of the stack, limited to this service.
			sp, err := a.PreviewStack(ctx, envID, row.Stack, []string{row.Name})
			if err != nil {
				return Preview{}, err
			}
			return Preview(sp), nil
		}
		p.Refusal = nu.Code
		return p, nil
	} else if err != nil {
		return Preview{}, err
	}
	sv, found, err := a.Service.Find(ctx, envID, "service", row.Name)
	if err != nil {
		return Preview{}, err
	}
	if !found {
		return Preview{}, ErrWorkloadGone
	}
	v, w := sv.Verdict, sv.Workload
	p.Refusal = previewRefusal(v, w)
	if p.Refusal != "" {
		return p, nil
	}
	p.Offered = true
	p.Items = []PreviewItem{{
		Kind: "service", Name: row.Name, Stack: row.Stack, Image: imageOf(w),
		FromDigest: v.RunningDigest, ToDigest: v.RemoteDigest,
		Platforms: append([]string{}, w.Platforms...), ChangedPlatforms: append([]string{}, v.ChangedPlatforms...),
	}}
	p.Warnings = append(failureWarnings(row.Name, v.Failure), a.serviceWarnings(ctx, envID, row)...)
	return p, nil
}

// previewRefusal is why a workload with that verdict is not offered an update, empty when it is.
func previewRefusal(v Verdict, w Workload) string {
	switch {
	case w.PinnedInFile():
		return NotUpdatablePinned
	case !w.Checkable():
		return NotUpdatableNoSource
	case v.State == StateCheckFailed:
		return RefusalCheckFailed
	case v.State == StateNotChecked:
		return RefusalNotChecked
	case v.RemoteDigest == "" || v.RemoteDigest == v.RunningDigest:
		return RefusalNothingToUpdate
	case v.State == StateUpToDate || v.State == StateCheckStale:
		return RefusalNothingToUpdate
	}
	return ""
}

// previewContainer describes the update of the plain container name: the digest change and the warnings of recreating it, or the
// reason it would not be offered. A container of a stack TierPlan manages is described through that stack, limited to its service.
func (a *Applier) previewContainer(ctx context.Context, envID, name string) (Preview, error) {
	var p Preview
	if h, held := a.Locks.Held(envID); held {
		p.Lock = &h
	}
	var nu *NotUpdatableError
	w, found, err := a.containerWorkload(ctx, envID, name)
	if errors.As(err, &nu) {
		// The name is on more than one node: it is said, and no update is offered.
		p.Refusal = nu.Code
		return p, nil
	} else if err != nil {
		return Preview{}, err
	}
	if !found {
		return Preview{}, ErrWorkloadGone
	}
	if err := a.containerUpdatable(ctx, envID, w); errors.As(err, &nu) {
		if nu.Code == NotUpdatableManaged {
			sp, err := a.PreviewStack(ctx, envID, w.Stack, []string{w.Labels[stacks.LabelComposeService]})
			if err != nil {
				return Preview{}, err
			}
			return Preview(sp), nil
		}
		p.Refusal = nu.Code
		return p, nil
	} else if err != nil {
		return Preview{}, err
	}
	sv, found, err := a.Service.findForUpdate(ctx, envID, "container", name)
	if errors.As(err, &nu) {
		p.Refusal = nu.Code
		return p, nil
	} else if err != nil {
		return Preview{}, err
	}
	if !found {
		return Preview{}, ErrWorkloadGone
	}
	v, w := sv.Verdict, sv.Workload
	if p.Refusal = previewRefusal(v, w); p.Refusal != "" {
		return p, nil
	}
	p.Offered = true
	p.Items = []PreviewItem{{
		Kind: "container", Name: strings.TrimPrefix(w.Name, "/"), Image: imageOf(w),
		FromDigest: v.RunningDigest, ToDigest: v.RemoteDigest,
		Platforms: append([]string{}, w.Platforms...), ChangedPlatforms: append([]string{}, v.ChangedPlatforms...),
	}}
	p.Warnings = append(failureWarnings(strings.TrimPrefix(w.Name, "/"), v.Failure), a.containerWarnings(ctx, envID, w)...)
	return p, nil
}

// containerWarnings are the pre-flight warnings of the container, from one inspect of it and, when it defines no health check
// of its own, one inspect of its image on its node (T-12-45). A container that cannot be read has only the downtime warning: the
// update itself reads it again and refuses what it cannot update.
func (a *Applier) containerWarnings(ctx context.Context, envID string, w Workload) []Warning {
	name := strings.TrimPrefix(w.Name, "/")
	if a.Aggregator == nil || len(w.NodeIDs) == 0 {
		return []Warning{{Workload: name, Code: WarnContainerDowntime}}
	}
	ictx, cancel := context.WithTimeout(ctx, previewInspectTimeout)
	defer cancel()
	var insp container.InspectResponse
	if err := a.Aggregator.OnNode(ictx, envID, w.NodeIDs[0], func(c *dockerx.Client) error {
		var e error
		insp, e = c.ContainerInspectFull(ictx, w.ID)
		return e
	}); err != nil {
		a.log().Warn("could not read a container to warn about its update", "environment", envID, "container", name, "error", err)
		return []Warning{{Workload: name, Code: WarnContainerDowntime}}
	}
	var image *bool
	if insp.Config == nil || insp.Config.Healthcheck == nil || len(insp.Config.Healthcheck.Test) == 0 {
		if cfg := a.imageConfig(ictx, target{envID: envID, nodeID: w.NodeIDs[0], name: name}, insp.Image); cfg != nil {
			has := cfg.Healthcheck != nil && len(cfg.Healthcheck.Test) > 0 && cfg.Healthcheck.Test[0] != "NONE"
			image = &has
		}
	}
	return ContainerWarnings(name, insp, image)
}

// serviceWarnings are the pre-flight warnings of the service. A service that cannot be read has none to give: the update itself
// reads it again and refuses what it cannot update.
func (a *Applier) serviceWarnings(ctx context.Context, envID string, row dockerx.ServiceRow) []Warning {
	if a.Service.Clusters == nil {
		return nil
	}
	var detail dockerx.ServiceDetail
	if err := a.Service.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		detail, _, e = c.ServiceInspect(ctx, row.ID)
		return e
	}); err != nil {
		a.log().Warn("could not read a service to warn about its update", "environment", envID, "service", row.Name, "error", err)
		return nil
	}
	var spec swarm.ServiceSpec
	if err := json.Unmarshal(detail.Spec, &spec); err != nil {
		return nil
	}
	var image *bool
	if spec.TaskTemplate.ContainerSpec != nil && spec.TaskTemplate.ContainerSpec.Image != "" && !specDefinesHealthcheck(spec) {
		image = a.imageHasHealthcheck(ctx, envID, row, spec.TaskTemplate.ContainerSpec.Image)
	}
	return ServiceWarnings(row.Name, spec, image)
}

// specDefinesHealthcheck reports whether the spec itself answers whether a check runs, so the image need not be asked.
func specDefinesHealthcheck(spec swarm.ServiceSpec) bool {
	cs := spec.TaskTemplate.ContainerSpec
	return cs != nil && cs.Healthcheck != nil && len(cs.Healthcheck.Test) > 0
}

// imageHasHealthcheck inspects the image the service runs on the first node that runs one of its tasks, and reports whether it
// defines a health check. It makes one inspect on one node (T-12-45); nil when there is no such node or the image cannot be read.
func (a *Applier) imageHasHealthcheck(ctx context.Context, envID string, row dockerx.ServiceRow, image string) *bool {
	if a.Aggregator == nil {
		return nil
	}
	ictx, cancel := context.WithTimeout(ctx, previewInspectTimeout)
	defer cancel()
	nodes, err := a.taskNodes(ictx, target{envID: envID, id: row.ID, name: row.Name})
	if err != nil || len(nodes) == 0 {
		return nil
	}
	var has *bool
	_ = a.Aggregator.OnNode(ictx, envID, nodes[0].id, func(c *dockerx.Client) error {
		_, raw, err := c.ImageDetail(ictx, image)
		if err != nil {
			return err
		}
		var doc struct {
			Config *struct {
				Healthcheck *struct{ Test []string } `json:"Healthcheck"`
			} `json:"Config"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return err
		}
		v := doc.Config != nil && doc.Config.Healthcheck != nil && len(doc.Config.Healthcheck.Test) > 0 && doc.Config.Healthcheck.Test[0] != "NONE"
		has = &v
		return nil
	})
	return has
}
