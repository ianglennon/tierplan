// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The consumer table of D-28 and DEP-08. TierPlan's own ingress objects (the proxy service and its task
// containers, the overlay network, the join token secret and the certificate cache volume) must never be read
// as stale or as the user's by anything that acts on what the engines list: the janitor and the orphan check
// would remove them, the unmanaged-stack listing would offer them for adoption, the update detector would offer
// to update the proxy. Each consumer is one named subtest here that is fed the objects the deploy code builds
// (ProxyServiceSpec, NetworkSpec, SecretSpec) and has to leave them alone; each subtest first shows that a
// control object of the same shape without the ingress labels is acted on, so a row cannot pass because the
// fixture was never in scope of the consumer. The guard test below fails when an exported function that sees
// engine objects or their labels is neither a row nor allowed with a reason, so a new consumer cannot be added
// without a row.

var consumersNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

const (
	proxyNode    = "node-1"
	proxyImageID = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	otherImageID = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	// controlRanImageID is the image the control container ran, which is not the plain dangling image, so that
	// image is unused on its node and the planner judges it.
	controlRanImageID = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

// consumerWorld is the objects of one proxy install, built from the deploy code, and the plain objects of the
// same shape without the labels that act as the controls.
type consumerWorld struct {
	spec    swarm.ServiceSpec
	netSpec dockerx.NetworkCreateSpec
	policy  janitor.Policy

	// proxyLabels are the labels of the exited task container of the proxy: the container spec's own plus the
	// swarm's task labels.
	proxyLabels map[string]string
	// proxyContainer is the proxy's exited task container, controlContainer the same shape with no ingress
	// labels. Their group, rank and inspect facts are the ones that make a plain container removable.
	proxyContainer, controlContainer janitor.Container
	proxyInput, controlInput         janitor.ContainerInput
	// image is the image the proxy runs, tagged; danglingImage is a plain old nameless one.
	image, digestImage, danglingImage janitor.Image
	// netRow and controlNet are the networks as the orphan check judges them.
	proxyOrphan, controlOrphan netspace.OrphanInput
	snapshot                   janitor.Snapshot
	ends                       map[string]janitor.ContainerEnd
}

func swarmTaskLabels(service, nodeID, taskID string) map[string]string {
	return map[string]string{
		"com.docker.swarm.service.id":   "svc-" + service,
		"com.docker.swarm.service.name": service,
		"com.docker.swarm.task.id":      taskID,
		"com.docker.swarm.task.name":    service + "." + nodeID + "." + taskID,
	}
}

func newConsumerWorld(t *testing.T) consumerWorld {
	t.Helper()
	w := consumerWorld{policy: janitor.DefaultPolicy()}
	w.policy.KeepN = 0
	w.policy.RemoveUnusedNetworks = true
	w.policy.RemoveStoppedContainers = true

	w.spec = ingressdeploy.ProxyServiceSpec(ingressdeploy.SpecInput{
		Image: ingressdeploy.IngressImage("0.1.0"), URL: "wss://tierplan.example/ingress/connect", SecretName: "tierplan-ingress-token-g1-env-abcd1234",
	})
	w.netSpec = ingressdeploy.NetworkSpec("10.77.0.0/22")
	secret := ingressdeploy.SecretSpec("tierplan-ingress-token-g1-env-abcd1234", []byte("token"))

	// Every object carries the labels each consumer relies on (Pitfall 5): a row that passes because the label
	// is missing from the fixture proves nothing.
	cs := w.spec.TaskTemplate.ContainerSpec
	volLabels := cs.Mounts[0].VolumeOptions.Labels
	for what, labels := range map[string]map[string]string{
		"service": w.spec.Labels, "task container": cs.Labels, "network": w.netSpec.Labels, "secret": secret.Labels, "cache volume": volLabels,
	} {
		if !ingress.HasComponentLabel(labels) || labels[stacks.LabelKeep] != "true" {
			t.Fatalf("the %s carries labels %v: want the component label and %s=true", what, labels, stacks.LabelKeep)
		}
	}
	if !ingress.OwnsService(w.spec.Name, w.spec.Labels) || !ingress.OwnsNetwork(w.netSpec.Name, w.netSpec.Labels) ||
		!ingress.OwnsVolume(cs.Mounts[0].Source, volLabels) || !ingress.OwnsSecret(secret.Name, secret.Labels) {
		t.Fatalf("the deploy code builds objects the Owns functions do not recognise")
	}

	old := consumersNow.Add(-90 * 24 * time.Hour)
	finished := consumersNow.Add(-60 * 24 * time.Hour)
	w.proxyLabels = swarmTaskLabels(ingress.ServiceName, proxyNode, "task1")
	for k, v := range cs.Labels {
		w.proxyLabels[k] = v
	}
	controlLabels := swarmTaskLabels("web", proxyNode, "task2")

	w.proxyContainer = janitor.Container{
		ID: "ctr-proxy", Name: "/" + ingress.ServiceName + "." + proxyNode + ".task1", NodeID: proxyNode, Node: "node-one",
		State: "exited", ImageID: proxyImageID, Created: old, Labels: w.proxyLabels,
	}
	w.controlContainer = janitor.Container{
		ID: "ctr-control", Name: "/web." + proxyNode + ".task2", NodeID: proxyNode, Node: "node-one",
		State: "exited", ImageID: controlRanImageID, Created: old, Labels: controlLabels,
	}
	end := janitor.ContainerEnd{StartedAt: old, FinishedAt: finished, SizeRw: 1000}
	w.ends = map[string]janitor.ContainerEnd{"ctr-proxy": end, "ctr-control": end}
	// Both are the last exited task of their own service, which is gone: the hardest case for the label, since
	// nothing but the label keeps the proxy's container.
	mk := func(c janitor.Container) janitor.ContainerInput {
		return janitor.ContainerInput{
			Container: c, Group: c.Labels["com.docker.swarm.service.id"] + "/node/" + c.NodeID, Rank: 1, Total: 1, KeepN: 0,
			ServiceGone: true, End: end, Inspected: true, Grace: w.policy.Grace,
		}
	}
	w.proxyInput, w.controlInput = mk(w.proxyContainer), mk(w.controlContainer)

	// The image the service names. The swarm resolves it to a digest, and a node that pulled it holds it
	// tagged or by digest only.
	imageName := cs.Image
	w.image = janitor.Image{ID: proxyImageID, NodeID: proxyNode, Node: "node-one", RepoTags: []string{imageName}, Size: 50 << 20, Created: old}
	w.digestImage = janitor.Image{ID: proxyImageID, NodeID: proxyNode, Node: "node-one", RepoDigests: []string{imageName[:strings.LastIndex(imageName, ":")] + "@" + proxyImageID}, Size: 50 << 20, Created: old}
	w.danglingImage = janitor.Image{ID: otherImageID, NodeID: proxyNode, Node: "node-one", Size: 10 << 20, Created: old, DanglingSince: old}

	// The network as envs/orphans.go builds the orphan input: not in use (the hardest case), nothing owns it by a
	// stack label, and its only protection is the keep label.
	w.proxyOrphan = netspace.OrphanInput{
		Known: true, Created: old, KeepLabel: w.netSpec.Labels[envs.LabelKeep] == "true", Grace: w.policy.Grace,
	}
	w.controlOrphan = w.proxyOrphan
	w.controlOrphan.KeepLabel = false

	w.snapshot = janitor.Snapshot{
		Swarm: true, ManagerRead: true, ServicesRead: true, StacksRead: true, ReadAt: consumersNow,
		Nodes:         []janitor.Node{{ID: proxyNode, Name: "node-one"}},
		Containers:    []janitor.Container{w.proxyContainer, w.controlContainer},
		Images:        []janitor.Image{w.image, w.danglingImage},
		ServiceImages: []string{imageName + "@" + proxyImageID},
		Services:      map[string]janitor.Service{},
	}
	return w
}

// networkRow is the janitor's row of a network after the orphan check judged it, built as the runner builds it.
func networkRow(id, name string, in netspace.OrphanInput) janitor.NetworkRow {
	return janitor.NetworkRow{
		ID: id, Name: name, Scope: "swarm", Node: "Cluster", Class: netspace.Classify(in, consumersNow),
		KeptByLabel: in.KeepLabel, IdleSince: netspace.IdleSince(in.Created, in.LastInUse),
	}
}

// leftAlone asserts a classifier did not act on an object: it is not listed, or it is listed as protected by the
// keep label (the preview shows what the label protects). It is never removable, in grace, reported or unclear.
func leftAlone(t *testing.T, what string, item janitor.Item, listed bool) {
	t.Helper()
	if !listed {
		return
	}
	if item.Class != janitor.ClassProtected || item.Reason.Code != janitor.ReasonKeptLabel {
		t.Errorf("%s: class %s, reason %s: want it left alone (protected by the keep label, or not listed)", what, item.Class, item.Reason.Code)
	}
}

// named reports whether any item of the plan names one of the refs or names.
func named(items []janitor.Item, ids ...string) []janitor.Item {
	var out []janitor.Item
	for _, it := range items {
		if slices.Contains(ids, it.Ref) || slices.Contains(ids, it.Name) {
			out = append(out, it)
		}
	}
	return out
}

// recordingEngine is a janitor.Engine that answers Inspect and remembers what it was asked.
type recordingEngine struct {
	janitor.Engine
	mu        sync.Mutex
	inspected []string
}

func (e *recordingEngine) Inspect(_ context.Context, _, _ string, ids []string) (map[string]janitor.ContainerEnd, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inspected = append(e.inspected, ids...)
	out := map[string]janitor.ContainerEnd{}
	for _, id := range ids {
		out[id] = janitor.ContainerEnd{FinishedAt: consumersNow.Add(-60 * 24 * time.Hour), SizeRw: 1}
	}
	return out, nil
}

// consumerRows is the table: one named subtest per consumer. covers names the exported functions of the
// scanned files the row stands for, which the guard reads.
type consumerRow struct {
	name   string
	covers []string
	run    func(t *testing.T, w consumerWorld)
}

func consumerRows() []consumerRow {
	return []consumerRow{
		{name: "ClassifyContainer", covers: []string{"ClassifyContainer"}, run: func(t *testing.T, w consumerWorld) {
			ctl, listed := janitor.ClassifyContainer(w.controlInput, consumersNow)
			if !listed || ctl.Class != janitor.ClassRemovable {
				t.Fatalf("control container: %+v listed %v, want removable: the fixture is not in scope of the classifier", ctl, listed)
			}
			item, listed := janitor.ClassifyContainer(w.proxyInput, consumersNow)
			leftAlone(t, "the proxy's task container", item, listed)
			if !listed {
				t.Errorf("the proxy's exited container is not listed, want protected (kept_label) so the preview says why")
			}
			// Beside a swarm task: the same container read as a plain one.
			plain := w.proxyInput
			plain.Standalone, plain.Group = true, ""
			plain.OptIn = true
			plainCtl := w.controlInput
			plainCtl.Standalone, plainCtl.Group = true, ""
			plainCtl.OptIn = true
			if it, ok := janitor.ClassifyContainer(plainCtl, consumersNow); !ok || it.Class != janitor.ClassRemovable {
				t.Fatalf("control standalone container: %+v listed %v, want removable", it, ok)
			}
			item, listed = janitor.ClassifyContainer(plain, consumersNow)
			leftAlone(t, "the proxy's container as a standalone one", item, listed)
		}},
		{name: "ClassifyNetwork", covers: []string{"ClassifyNetwork"}, run: func(t *testing.T, w consumerWorld) {
			ctl, listed := janitor.ClassifyNetwork(networkRow("net-control", "plain-overlay", w.controlOrphan), true, w.policy, consumersNow)
			if !listed || ctl.Class != janitor.ClassRemovable {
				t.Fatalf("control network: %+v listed %v, want removable", ctl, listed)
			}
			item, listed := janitor.ClassifyNetwork(networkRow("net-ingress", ingress.NetworkName, w.proxyOrphan), true, w.policy, consumersNow)
			leftAlone(t, "the ingress network", item, listed)
		}},
		{name: "ClassifyImage", covers: []string{"ClassifyImage"}, run: func(t *testing.T, w consumerWorld) {
			ctl, listed := janitor.ClassifyImage(w.danglingImage, false, false, true, w.policy, consumersNow)
			if !listed || ctl.Class != janitor.ClassRemovable {
				t.Fatalf("control image: %+v listed %v, want removable", ctl, listed)
			}
			// Tagged, and in use by the proxy's container.
			item, listed := janitor.ClassifyImage(w.image, true, true, true, w.policy, consumersNow)
			leftAlone(t, "the proxy image, tagged", item, listed)
			// By digest only, after every task container is gone: the service spec names it.
			item, listed = janitor.ClassifyImage(w.digestImage, false, true, true, w.policy, consumersNow)
			leftAlone(t, "the proxy image, by digest", item, listed)
			// Nameless and named only by the service spec (the swarm pinned the digest).
			bare := w.digestImage
			bare.RepoDigests = nil
			spec := []string{w.spec.TaskTemplate.ContainerSpec.Image + "@" + bare.ID}
			if !janitor.ImageNamedBySpec(spec, bare.ID) {
				t.Fatalf("the service spec %v does not name the image %s", spec, bare.ID)
			}
			item, listed = janitor.ClassifyImage(bare, false, janitor.ImageNamedBySpec(spec, bare.ID), true, w.policy, consumersNow)
			leftAlone(t, "the proxy image, named by the service spec", item, listed)
		}},
		{name: "Build", covers: []string{"Build"}, run: func(t *testing.T, w consumerWorld) {
			plan := janitor.Build(janitor.Input{
				Snapshot: w.snapshot, Ends: w.ends,
				Networks: &janitor.NetworkReport{Complete: true, Rows: []janitor.NetworkRow{
					networkRow("net-control", "plain-overlay", w.controlOrphan),
					networkRow("net-ingress", ingress.NetworkName, w.proxyOrphan),
				}},
			}, w.policy, consumersNow)
			ids := []string{"ctr-proxy", ingress.NetworkName, "net-ingress", proxyImageID}
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
				t.Errorf("the plan would remove the ingress objects: %+v", got)
			}
			for _, it := range named(plan.NotRemoved, ids...) {
				if it.Class != janitor.ClassProtected || it.Reason.Code != janitor.ReasonKeptLabel {
					t.Errorf("not-removed item %+v: want protected by the keep label", it)
				}
			}
			if len(plan.NotRemoved) == 0 {
				t.Errorf("the plan lists nothing as protected: the proxy's container should be shown as kept")
			}
		}},
		{name: "CandidatesFor", covers: []string{"Candidates", "CandidatesFor", "InspectCandidates"}, run: func(t *testing.T, w consumerWorld) {
			refs := janitor.CandidatesFor(w.snapshot, w.policy, nil)
			if len(refs) != 1 || refs[0].ID != "ctr-control" {
				t.Fatalf("candidates = %+v, want only the control container: the proxy's is kept by its label", refs)
			}
			eng := &recordingEngine{}
			ends := janitor.InspectCandidates(context.Background(), eng, "env-1", refs)
			if len(ends) != 1 || !slices.Equal(eng.inspected, []string{"ctr-control"}) {
				t.Fatalf("inspected %v, ends %v: want the control only", eng.inspected, ends)
			}
			if slices.Contains(eng.inspected, "ctr-proxy") {
				t.Errorf("the proxy's container was inspected")
			}
		}},
		{name: "HeldByStack", covers: []string{"HeldByStack"}, run: func(t *testing.T, w consumerWorld) {
			facts := []janitor.StackFact{
				{ID: "stack-a", Name: "shop", Excluded: true},
				{ID: "stack-b", Name: "blog", Declared: true},
			}
			// A control: an object of a stack the person excluded is held.
			if code, held := janitor.HeldByStack(facts, map[string]string{stacks.LabelStackID: "stack-a"}); !held || code != janitor.ReasonStackExcluded {
				t.Fatalf("control: held %v code %q, want held by stack_excluded", held, code)
			}
			for what, labels := range map[string]map[string]string{"task container": w.proxyLabels, "service": w.spec.Labels, "network": w.netSpec.Labels} {
				if code, held := janitor.HeldByStack(facts, labels); held || code != "" {
					t.Errorf("the ingress %s is held by a stack (%q): it belongs to no stack, so no stack exclusion can hold or release it", what, code)
				}
			}
		}},
		{name: "IsKept", covers: []string{"IsKept"}, run: func(t *testing.T, w consumerWorld) {
			keeps := map[janitor.KeepKey]bool{{Kind: janitor.KindContainer, NodeID: proxyNode, Ref: "ctr-control"}: true}
			if !janitor.IsKept(keeps, janitor.KindContainer, proxyNode, "ctr-control") {
				t.Fatalf("control: a keep row does not hold its container")
			}
			// No keep row names the proxy: the label alone decides.
			if janitor.IsKept(map[janitor.KeepKey]bool{}, janitor.KindContainer, proxyNode, "ctr-proxy") ||
				janitor.IsKept(map[janitor.KeepKey]bool{}, janitor.KindNetwork, "", ingress.NetworkName) {
				t.Errorf("an empty keep map holds an ingress object")
			}
			if len(w.snapshot.Keeps) != 0 {
				t.Fatalf("the snapshot already carries keep rows: %v", w.snapshot.Keeps)
			}
			item, listed := janitor.ClassifyContainer(w.proxyInput, consumersNow)
			leftAlone(t, "the proxy's container with no keep row", item, listed)
			if listed && item.Reason.Code == janitor.ReasonKeptRow {
				t.Errorf("the proxy's container is protected by a keep row, want the label")
			}
		}},
		{name: "DanglingStore", covers: []string{"Stamp"}, run: func(t *testing.T, w consumerWorld) {
			ctx := testsupport.Context(t)
			st := storetest.New(t)
			if _, err := st.Write.ExecContext(ctx,
				`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'agent', 'e', '', 1, 1, 1)`); err != nil {
				t.Fatal(err)
			}
			snap := w.snapshot
			snap.Images = []janitor.Image{w.image, w.digestImage, w.danglingImage}
			snap.Images[1].ID = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
			snap.Images[2].DanglingSince = time.Time{}
			if err := (janitor.DanglingStore{St: st}).Stamp(ctx, "env-1", &snap, consumersNow); err != nil {
				t.Fatalf("Stamp: %v", err)
			}
			if !snap.Images[2].DanglingSince.Equal(consumersNow) {
				t.Fatalf("control image: dangling since %v, want a clock started now", snap.Images[2].DanglingSince)
			}
			for _, img := range snap.Images[:2] {
				if !img.DanglingSince.IsZero() {
					t.Errorf("the proxy image %v got a dangling clock %v", img.RepoTags, img.DanglingSince)
				}
			}
		}},
		{name: "OrphanClassification", run: func(t *testing.T, w consumerWorld) {
			ctl := netspace.Classify(w.controlOrphan, consumersNow)
			if ctl != netspace.ClassUnused {
				t.Fatalf("control network: class %s, want unused: the fixture is not in scope of the check", ctl)
			}
			if got := netspace.Classify(w.proxyOrphan, consumersNow); got != netspace.ClassKept {
				t.Errorf("the ingress network: class %s, want kept (by its label)", got)
			}
			// Even idle with no owner and with every node answering, only the label holds it.
			if !w.proxyOrphan.KeepLabel || w.proxyOrphan.KeepRow || w.proxyOrphan.InUse {
				t.Errorf("the orphan input %+v does not isolate the label as the protection", w.proxyOrphan)
			}
		}},
		{name: "Discover", covers: []string{"Discover"}, run: func(t *testing.T, w consumerWorld) {
			proxyRow := dockerx.ServiceRow{
				ID: "svc-proxy", Name: w.spec.Name, Mode: "global", Running: 3, Desired: 3, Image: w.spec.TaskTemplate.ContainerSpec.Image,
				Stack: w.spec.Labels[stacks.LabelSwarmNamespace], Labels: w.spec.Labels, Ingress: true,
			}
			controlRow := dockerx.ServiceRow{ID: "svc-web", Name: "shop_web", Mode: "replicated", Running: 1, Desired: 1, Stack: "shop", Labels: map[string]string{stacks.LabelSwarmNamespace: "shop"}}
			got := stackops.Discover(stackops.Held{Services: []dockerx.ServiceRow{proxyRow, controlRow}}, nil, stacks.TargetSwarm, stackops.SelfMarks{})
			if len(got) != 1 || got[0].Name != "shop" {
				t.Fatalf("control: discovered %+v, want only the shop stack: the fixture is not in scope of the listing", got)
			}
			for _, d := range got {
				for _, r := range d.Rows {
					if r.Name == ingress.ServiceName {
						t.Errorf("the ingress service is listed in %s", d.Name)
					}
				}
			}
			// A standalone host holds no proxy, but its containers carry the labels all the same.
			ctr := dockerx.ContainerRow{ID: "ctr-proxy", Name: "/proxy", State: "exited", Labels: w.proxyLabels}
			ctl := dockerx.ContainerRow{ID: "ctr-shop", Name: "/shop-web-1", State: "running", Labels: map[string]string{stacks.LabelComposeProject: "shop", stacks.LabelComposeService: "web"}}
			got = stackops.Discover(stackops.Held{Containers: []dockerx.ContainerRow{ctr, ctl}}, nil, stacks.TargetStandalone, stackops.SelfMarks{})
			if len(got) != 1 || got[0].Name != "shop" {
				t.Errorf("standalone: discovered %+v, want only the shop project", got)
			}
		}},
	}
}

// notConsumers are the exported functions of the scanned files that see engine objects or their labels and are
// not consumers that could remove, claim or update them, each with the reason. A function that is neither a row
// of the table nor here fails TestIngressConsumersAreAllClassified.
var notConsumers = map[string]string{
	"KeepNodeKey":     "formats the node part of a keep row's key from a network row the caller already judged; it reads nothing and acts on nothing",
	"PortainerMarked": "reads Portainer's marker labels to flag a group Discover has already built; it is a flag on a listed stack and the proxy carries none of those labels",
}

// scanFiles are the sources the guard reads, relative to this package: the whole of internal/janitor (a
// directory, non-test files) and the single files of the other consumers. Plan 24-09 task 2 adds the update
// detector's two files with their rows.
var scanFiles = []string{
	"../janitor",
	"../envs/orphans.go",
	"../stackops/discover.go",
}

// sensitiveTypes are the types that carry engine objects or their labels. A parameter of one of them, or a
// map[string]string, makes an exported function a candidate consumer.
var sensitiveTypes = map[string]bool{
	"Snapshot": true, "Input": true, "Container": true, "ContainerInput": true, "ContainerRef": true, "NetworkRow": true,
	"Image": true, "StackFact": true, "ServiceRow": true, "ContainerRow": true, "Held": true,
}

// paramTypeName is the bare type name of a parameter's type, "map[string]string" for that map.
func paramTypeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return paramTypeName(x.X)
	case *ast.ArrayType:
		return paramTypeName(x.Elt)
	case *ast.Ellipsis:
		return paramTypeName(x.Elt)
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.MapType:
		k, kok := x.Key.(*ast.Ident)
		v, vok := x.Value.(*ast.Ident)
		if kok && vok && k.Name == "string" && v.Name == "string" {
			return "map[string]string"
		}
	}
	return ""
}

// sourceFiles expands scanFiles to the non-test Go files.
func sourceFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, p := range scanFiles {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("scan target %s: %v", p, err)
		}
		if !fi.IsDir() {
			out = append(out, p)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
				out = append(out, filepath.Join(p, n))
			}
		}
	}
	return out
}

// scannedConsumers returns the exported functions and methods of the scanned files that have a sensitive
// parameter, by name with the file they are in.
func scannedConsumers(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	fset := token.NewFileSet()
	for _, p := range sourceFiles(t) {
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || !fd.Name.IsExported() || fd.Type.Params == nil {
				continue
			}
			for _, fl := range fd.Type.Params.List {
				if n := paramTypeName(fl.Type); sensitiveTypes[n] || n == "map[string]string" {
					found[fd.Name.Name] = p
					break
				}
			}
		}
	}
	return found
}

// TestIngressObjectsClassifiedByEveryConsumer is the D-28 table: every consumer leaves the ingress objects
// alone. After the rows it runs the scan guard (checkEveryConsumerHasARow), so the table and its guard are one
// test and a consumer without a row fails it.
func TestIngressObjectsClassifiedByEveryConsumer(t *testing.T) {
	for _, row := range consumerRows() {
		t.Run(row.name, func(t *testing.T) {
			row.run(t, newConsumerWorld(t))
		})
	}
	checkEveryConsumerHasARow(t)
}

// checkEveryConsumerHasARow is the scan guard: it lists every exported function of the consumers' sources that
// takes engine objects or labels and fails for each that is neither a row of the table nor allowed with a
// reason, and when it found too few to have looked at anything.
func checkEveryConsumerHasARow(t *testing.T) {
	t.Helper()
	found := scannedConsumers(t)
	if len(found) < 8 {
		t.Fatalf("the scan found %d functions that take engine objects or labels, want at least 8: it is not looking at the consumers (%v)", len(found), found)
	}
	covered := map[string]bool{}
	for _, row := range consumerRows() {
		for _, fn := range row.covers {
			covered[fn] = true
		}
	}
	for fn, why := range notConsumers {
		if strings.TrimSpace(why) == "" {
			t.Errorf("notConsumers[%q] has no reason", fn)
		}
		if covered[fn] {
			t.Errorf("%s is both a row of the table and in notConsumers", fn)
		}
	}
	var missing []string
	for fn, file := range found {
		if !covered[fn] && notConsumers[fn] == "" {
			missing = append(missing, fmt.Sprintf("%s (%s)", fn, file))
		}
	}
	slices.Sort(missing)
	for _, m := range missing {
		t.Errorf("%s takes engine objects or labels and is neither a row of TestIngressObjectsClassifiedByEveryConsumer nor in notConsumers: add a row that feeds it the ingress objects, or allow it with a reason", m)
	}
	// A row that covers a function the scan never finds is stale, or names a function that was renamed away.
	for fn := range covered {
		if _, ok := found[fn]; !ok && !coveredByHand[fn] {
			t.Errorf("a row covers %s but the scan does not find it: the row is stale", fn)
		}
	}
}

// coveredByHand are the functions a row covers that the scan cannot select by their parameters (they take a
// keep map or a plain value), kept so a stale row is still reported.
var coveredByHand = map[string]bool{"IsKept": true}
