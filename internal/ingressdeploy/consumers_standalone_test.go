// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The standalone half of the consumer table (plan 25-08, D-09, D-41 of Phase 24). On a standalone host the
// system project's container (compose labels, the component, keep and updates labels), its bridge network and
// its cache volume must be left alone by everything that acts on what an engine lists: the janitor's classifiers
// and planner, the dangling store, the orphan check and the update detector. Each row first shows that a control
// of the same shape without TierPlan's labels is acted on, so a row cannot pass because its fixture was never in
// scope of the consumer. The swarm table and its scan guard (consumers_test.go) are untouched; the standalone
// consumers read the same functions, so the guard already names them. Discovery's skip is 25-06's
// TestDiscoverHidesTheIngressSystemProject.

// standaloneWorld is the objects of one standalone proxy install, built from the ingress package's names and
// labels (the labels every object of the system project carries), and the controls without them.
type standaloneWorld struct {
	policy janitor.Policy
	// proxyLabels are the labels of the exited proxy container: TierPlan's three object labels and compose's own
	// project and service. controlLabels are a lone exited container of another compose project.
	proxyLabels, controlLabels map[string]string

	proxyContainer, controlContainer janitor.Container
	// proxyInput and controlInput are what the planner builds for each: a standalone container, judged by its
	// compose project, with the opt-in to removing stopped containers on.
	proxyInput, controlInput janitor.ContainerInput
	image, danglingImage     janitor.Image
	// proxyOrphan is the bridge network as the orphan check judges it: idle, with its compose project as its
	// owner label (which no stack record names), so its only protection is the keep label. controlOrphan is the
	// same with no keep label.
	proxyOrphan, controlOrphan netspace.OrphanInput
	snapshot                   janitor.Snapshot
	ends                       map[string]janitor.ContainerEnd
}

const (
	standaloneControlProject = "shop"
	standaloneNetworkID      = "net-ingress"
)

func newStandaloneWorld(t *testing.T) standaloneWorld {
	t.Helper()
	w := standaloneWorld{policy: janitor.DefaultPolicy()}
	w.policy.KeepN = 0
	w.policy.RemoveUnusedNetworks = true
	w.policy.RemoveStoppedContainers = true

	// Every object of the system project carries the labels each consumer relies on, so a row that passes
	// because the label is missing from the fixture proves nothing.
	for what, labels := range map[string]map[string]string{"container": ingress.ObjectLabels(), "network": ingress.ObjectLabels(), "volume": ingress.ObjectLabels()} {
		if !ingress.HasComponentLabel(labels) || labels[stacks.LabelKeep] != "true" || labels[stacks.LabelUpdates] != "ignore" {
			t.Fatalf("the ingress %s carries labels %v: want the component, keep=true and updates=ignore", what, labels)
		}
	}
	w.proxyLabels = ingress.ObjectLabels()
	w.proxyLabels[stacks.LabelComposeProject] = ingress.ProjectName
	w.proxyLabels[stacks.LabelComposeService] = ingress.ProxyComposeService
	if !ingress.OwnsProxyContainer(w.proxyLabels) || !ingress.OwnsNetwork(ingress.NetworkName, ingress.ObjectLabels()) ||
		!ingress.OwnsVolume(ingress.VolumeName, ingress.ObjectLabels()) {
		t.Fatalf("the labels built here are not ones the Owns functions recognise")
	}
	w.controlLabels = map[string]string{stacks.LabelComposeProject: standaloneControlProject, stacks.LabelComposeService: "web"}

	old := consumersNow.Add(-90 * 24 * time.Hour)
	finished := consumersNow.Add(-60 * 24 * time.Hour)
	w.proxyContainer = janitor.Container{
		ID: "ctr-proxy", Name: "/" + ingress.ProjectName + "-" + ingress.ProxyComposeService + "-1", NodeID: proxyNode, Node: "node-one",
		State: "exited", ImageID: proxyImageID, Created: old, Labels: w.proxyLabels,
	}
	w.controlContainer = janitor.Container{
		ID: "ctr-control", Name: "/" + standaloneControlProject + "-web-1", NodeID: proxyNode, Node: "node-one",
		State: "exited", ImageID: controlRanImageID, Created: old, Labels: w.controlLabels,
	}
	end := janitor.ContainerEnd{StartedAt: old, FinishedAt: finished, SizeRw: 1000}
	w.ends = map[string]janitor.ContainerEnd{"ctr-proxy": end, "ctr-control": end}
	mk := func(c janitor.Container) janitor.ContainerInput {
		return janitor.ContainerInput{
			Container: c, Standalone: true, KeepN: 0, Grace: w.policy.Grace, OptIn: true, StacksRead: true,
			Project: c.Labels[stacks.LabelComposeProject], Inspected: true, End: end,
		}
	}
	w.proxyInput, w.controlInput = mk(w.proxyContainer), mk(w.controlContainer)

	w.image = janitor.Image{ID: proxyImageID, NodeID: proxyNode, Node: "node-one", RepoTags: []string{ingressdeploy.IngressImage("0.1.0")}, Size: 50 << 20, Created: old}
	w.danglingImage = janitor.Image{ID: otherImageID, NodeID: proxyNode, Node: "node-one", Size: 10 << 20, Created: old, DanglingSince: old}

	// The network as envs/orphans.go builds the orphan input: not in use (the hardest case), its compose project
	// as the owner label with no stack of that name, and its only protection the keep label.
	w.proxyOrphan = netspace.OrphanInput{
		Known: true, Created: old, KeepLabel: ingress.ObjectLabels()[envs.LabelKeep] == "true",
		OwnerLabel: ingress.ProjectName, Grace: w.policy.Grace,
	}
	w.controlOrphan = w.proxyOrphan
	w.controlOrphan.KeepLabel, w.controlOrphan.OwnerLabel = false, standaloneControlProject

	w.snapshot = janitor.Snapshot{
		ManagerRead: false, ServicesRead: true, StacksRead: true, ReadAt: consumersNow,
		StackNames: map[string]bool{}, StackIDs: map[string]bool{},
		Nodes:      []janitor.Node{{ID: proxyNode, Name: "node-one"}},
		Containers: []janitor.Container{w.proxyContainer, w.controlContainer},
		Images:     []janitor.Image{w.image, w.danglingImage},
	}
	return w
}

// localNetworkRow is the janitor's row of a local-scope bridge after the orphan check judged it.
func localNetworkRow(id, name string, in netspace.OrphanInput) janitor.NetworkRow {
	return janitor.NetworkRow{
		ID: id, NodeID: proxyNode, Name: name, Scope: "local", Node: "node-one", Class: netspace.Classify(in, consumersNow),
		KeptByLabel: in.KeepLabel, IdleSince: netspace.IdleSince(in.Created, in.LastInUse),
	}
}

// standaloneRow is one consumer of the standalone table.
type standaloneRow struct {
	name string
	run  func(t *testing.T)
}

func standaloneRows() []standaloneRow {
	return []standaloneRow{
		{name: "ClassifyContainer", run: func(t *testing.T) {
			w := newStandaloneWorld(t)
			ctl, listed := janitor.ClassifyContainer(w.controlInput, consumersNow)
			if !listed || ctl.Class != janitor.ClassRemovable {
				t.Fatalf("control container: %+v listed %v, want removable: the fixture is not in scope of the classifier", ctl, listed)
			}
			item, listed := janitor.ClassifyContainer(w.proxyInput, consumersNow)
			if !listed || item.Class != janitor.ClassProtected || item.Reason.Code != janitor.ReasonKeptLabel {
				t.Errorf("the proxy's container: %+v listed %v, want protected by the keep label", item, listed)
			}
			// Without the opt-in the control is only reported; the proxy is still protected, not reported.
			w.proxyInput.OptIn, w.controlInput.OptIn = false, false
			if it, ok := janitor.ClassifyContainer(w.controlInput, consumersNow); !ok || it.Class != janitor.ClassReported {
				t.Fatalf("control without the opt-in: %+v listed %v, want reported", it, ok)
			}
			if it, ok := janitor.ClassifyContainer(w.proxyInput, consumersNow); !ok || it.Class != janitor.ClassProtected {
				t.Errorf("the proxy's container without the opt-in: %+v listed %v, want protected", it, ok)
			}
			// Not knowing the stacks makes both cant_tell (never removable), and the proxy is still not removable.
			w.proxyInput.StacksRead = false
			if it, _ := janitor.ClassifyContainer(w.proxyInput, consumersNow); it.Class == janitor.ClassRemovable {
				t.Errorf("the proxy's container is removable with the stacks unread: %+v", it)
			}
		}},
		{name: "ClassifyNetwork", run: func(t *testing.T) {
			w := newStandaloneWorld(t)
			ctl, listed := janitor.ClassifyNetwork(localNetworkRow("net-control", "plain-bridge", w.controlOrphan), true, w.policy, consumersNow)
			if !listed || ctl.Class != janitor.ClassRemovable {
				t.Fatalf("control network: %+v listed %v, want removable", ctl, listed)
			}
			item, listed := janitor.ClassifyNetwork(localNetworkRow(standaloneNetworkID, ingress.NetworkName, w.proxyOrphan), true, w.policy, consumersNow)
			leftAlone(t, "the ingress bridge", item, listed)
		}},
		{name: "ClassifyImage", run: func(t *testing.T) {
			w := newStandaloneWorld(t)
			ctl, listed := janitor.ClassifyImage(w.danglingImage, false, false, true, w.policy, consumersNow)
			if !listed || ctl.Class != janitor.ClassRemovable {
				t.Fatalf("control image: %+v listed %v, want removable", ctl, listed)
			}
			// Tagged and in use by the exited proxy container: an image of an exited container is in use.
			item, listed := janitor.ClassifyImage(w.image, true, false, true, w.policy, consumersNow)
			leftAlone(t, "the proxy image, used by its exited container", item, listed)
		}},
		{name: "Build", run: func(t *testing.T) {
			w := newStandaloneWorld(t)
			plan := janitor.Build(janitor.Input{
				Snapshot: w.snapshot, Ends: w.ends,
				Networks: &janitor.NetworkReport{Complete: true, Rows: []janitor.NetworkRow{
					localNetworkRow("net-control", "plain-bridge", w.controlOrphan),
					localNetworkRow(standaloneNetworkID, ingress.NetworkName, w.proxyOrphan),
				}},
			}, w.policy, consumersNow)
			ids := []string{"ctr-proxy", ingress.NetworkName, standaloneNetworkID, proxyImageID, ingress.VolumeName}
			wantControls := map[string]bool{"ctr-control": false, "net-control": false, otherImageID: false}
			for _, it := range plan.Items {
				if _, ok := wantControls[it.Ref]; ok {
					wantControls[it.Ref] = true
				}
			}
			for ref, seen := range wantControls {
				if !seen {
					t.Errorf("control %s is not in the plan's removable items %+v: the fixture is not in scope of the planner", ref, plan.Items)
				}
			}
			if got := named(plan.Items, ids...); len(got) != 0 {
				t.Errorf("the plan would remove the standalone ingress objects: %+v", got)
			}
			for _, it := range named(plan.NotRemoved, ids...) {
				if it.Class != janitor.ClassProtected || it.Reason.Code != janitor.ReasonKeptLabel {
					t.Errorf("not-removed item %+v: want protected by the keep label", it)
				}
			}
			if len(named(plan.NotRemoved, "ctr-proxy")) == 0 {
				t.Errorf("the plan does not show the proxy's container as kept: %+v", plan.NotRemoved)
			}
		}},
		{name: "CandidatesFor", run: func(t *testing.T) {
			w := newStandaloneWorld(t)
			refs := janitor.CandidatesFor(w.snapshot, w.policy, nil)
			if len(refs) != 1 || refs[0].ID != "ctr-control" {
				t.Fatalf("candidates = %+v, want only the control container: the proxy's is kept by its label", refs)
			}
			eng := &recordingEngine{}
			ends := janitor.InspectCandidates(context.Background(), eng, "env-1", refs)
			if len(ends) != 1 || !slices.Equal(eng.inspected, []string{"ctr-control"}) {
				t.Fatalf("inspected %v, ends %v: want the control only", eng.inspected, ends)
			}
		}},
		{name: "DanglingStore", run: func(t *testing.T) {
			w := newStandaloneWorld(t)
			ctx := testsupport.Context(t)
			st := storetest.New(t)
			if _, err := st.Write.ExecContext(ctx,
				`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'agent', 'e', '', 1, 1, 1)`); err != nil {
				t.Fatal(err)
			}
			snap := w.snapshot
			snap.Images = []janitor.Image{w.image, w.danglingImage}
			snap.Images[1].DanglingSince = time.Time{}
			if err := (janitor.DanglingStore{St: st}).Stamp(ctx, "env-1", &snap, consumersNow); err != nil {
				t.Fatalf("Stamp: %v", err)
			}
			if !snap.Images[1].DanglingSince.Equal(consumersNow) {
				t.Fatalf("control image: dangling since %v, want a clock started now", snap.Images[1].DanglingSince)
			}
			if !snap.Images[0].DanglingSince.IsZero() {
				t.Errorf("the proxy image got a dangling clock %v", snap.Images[0].DanglingSince)
			}
		}},
		{name: "OrphanClassification", run: func(t *testing.T) {
			w := newStandaloneWorld(t)
			if got := netspace.Classify(w.controlOrphan, consumersNow); got != netspace.ClassOrphaned {
				t.Fatalf("control network: class %s, want orphaned (its project %q has no stack): the fixture is not in scope of the check", got, standaloneControlProject)
			}
			if got := netspace.Classify(w.proxyOrphan, consumersNow); got != netspace.ClassKept {
				t.Errorf("the ingress bridge: class %s, want kept (by its label)", got)
			}
			if !w.proxyOrphan.KeepLabel || w.proxyOrphan.KeepRow || w.proxyOrphan.InUse || w.proxyOrphan.OwnerLabel == "" {
				t.Errorf("the orphan input %+v does not isolate the label as the protection of an idle, project-labelled network", w.proxyOrphan)
			}
		}},
		{name: "ContainerWorkloads", run: func(t *testing.T) {
			w := newStandaloneWorld(t)
			row := func(id string, labels map[string]string) envs.ContainerRow {
				return envs.ContainerRow{ContainerRow: dockerx.ContainerRow{ID: id, Name: "/" + id, State: "running", Image: "registry.example.com/team/" + id + ":1", Labels: labels}, NodeID: proxyNode, Node: "node-one"}
			}
			agg := envs.Aggregate{Containers: []envs.ContainerRow{row("proxy", w.proxyLabels), row("web", w.controlLabels)}}
			ws := updates.ContainerWorkloads(agg, func(string) (string, bool) { return "", false })
			if len(ws) != 1 || ws[0].ID != "web" {
				t.Errorf("container workloads = %+v, want only the control: the detector offers no update of the standalone proxy", ws)
			}
			// The apply path (apply_container.go) finds a container among these workloads, so the proxy is
			// never found there either, and its containerUpdatable reads the same component label.
		}},
		{name: "NoVolumeConsumer", run: func(t *testing.T) {
			// The cache volume is unused while the container is stopped. Nothing that cleans up or updates a
			// host removes a volume (janitor/doc.go), so the volume needs no classifier row; this guard fails the
			// day a volume removal appears in a consumer's source, so that change has to add the row.
			for _, rel := range []string{"internal/janitor", "internal/updates", "internal/envs/orphans.go"} {
				p := filepath.Join(repoRoot, rel)
				var files []string
				if fi, err := os.Stat(p); err != nil {
					t.Fatal(err)
				} else if fi.IsDir() {
					entries, _ := os.ReadDir(p)
					for _, e := range entries {
						if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
							files = append(files, filepath.Join(p, n))
						}
					}
				} else {
					files = []string{p}
				}
				if len(files) == 0 {
					t.Fatalf("%s holds no source to scan", rel)
				}
				for _, f := range files {
					src, err := os.ReadFile(f)
					if err != nil {
						t.Fatal(err)
					}
					for _, call := range []string{".VolumeRemove(", ".VolumesPrune(", ".VolumePrune("} {
						if strings.Contains(string(src), call) {
							t.Errorf("%s calls %s: a consumer now removes volumes, so it needs a row for the standalone cache volume", f, call)
						}
					}
				}
			}
		}},
	}
}

// TestStandaloneIngressObjectsClassifiedByEveryConsumer is the standalone half of the D-28 table: every consumer
// that reads standalone objects leaves the proxy's container, bridge network and cache volume alone and treats
// the controls as it treats any object.
func TestStandaloneIngressObjectsClassifiedByEveryConsumer(t *testing.T) {
	for _, row := range standaloneRows() {
		t.Run(row.name, func(t *testing.T) {
			row.run(t)
		})
	}
}
