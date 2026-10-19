// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package stackopstest stands a deployed standalone stack on a fake engine, for the tests of the packages
// that review and deploy stacks. It is test support and is imported by tests only.
package stackopstest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Digest is a made-up image digest: sha256: and 64 copies of c.
func Digest(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// Hosted is a stack as a deploy leaves it on a standalone host: a container for each service, made from the
// service as the SDK would send it (references to other services resolved to their containers, the image's
// own configuration merged in), with the config hash the SDK stamps, the images, and the project's networks
// and volumes. A test changes a container through Docs before it stands the host up with Apply.
type Hosted struct {
	Name, StackID string
	Project       *types.Project
	// Hash is the config hash of each service, IDs the ID of its container.
	Hash, IDs map[string]string
	// Docs are the inspect documents by service, shared with the fake containers: a test changes one to
	// change what the host reports of the container.
	Docs       map[string]*container.InspectResponse
	Containers []dockertest.FakeContainer
	Images     []dockertest.FakeImage
	Networks   []dockertest.FakeNetwork
	Volumes    []dockertest.FakeVolume
	// Pins are the references the services were deployed with, by service.
	Pins map[string]string
}

func hexOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ImageFor is the image a reference stands for on the host: its own environment, command, labels and
// exposed port, which the containers made from it carry.
func ImageFor(ref string) dockertest.FakeImage {
	return dockertest.FakeImage{
		ID: "sha256:" + hexOf("image:"+ref), RepoTags: []string{ref},
		RepoDigests: []string{strings.SplitN(ref, ":", 2)[0] + "@" + Digest("a")},
		Env:         []string{"PATH=/usr/local/bin:/usr/bin", "IMAGE_REF=" + ref},
		Cmd:         []string{"/bin/run", "--default"}, Labels: map[string]string{"org.example.maintainer": "image"},
		ExposedPorts: []string{"80/tcp"},
	}
}

// Host prepares a compose file as a deploy does and returns the stack as the host holds it afterwards. The
// stack's ID is what the containers, networks and volumes carry as their owner.
func Host(t testing.TB, name, stackID, compose string, vars []stacks.Var, files ...stacks.File) *Hosted {
	t.Helper()
	return HostPinned(t, name, stackID, compose, vars, nil, files...)
}

// HostPinned is Host with the references the services were deployed with, by service name, for a test that
// already holds them from a review. A service without one runs the image as written with a made-up digest.
func HostPinned(t testing.TB, name, stackID, compose string, vars []stacks.Var, pins map[string]string, files ...stacks.File) *Hosted {
	t.Helper()
	copy := stacks.WorkingCopy{Files: append([]stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, files...), Vars: vars}
	checked := stacks.Check(context.Background(), stacks.CheckInput{Name: name, Target: stacks.TargetStandalone, Copy: copy})
	if checked.Loaded == nil {
		t.Fatalf("check: %v", checked.Problems)
	}
	project := checked.Loaded.Project
	table := map[string]string{}
	for _, v := range vars {
		table[v.Name] = v.Value
	}
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, project, stacks.NewFileSet(copy.Files), table)
	if err != nil {
		t.Fatal(err)
	}
	h := &Hosted{Name: name, StackID: stackID, Project: project, Hash: map[string]string{}, IDs: map[string]string{}, Pins: map[string]string{}}
	images := map[string]dockertest.FakeImage{}
	for n, svc := range project.Services {
		if svc.Image != "" {
			h.Pins[n] = svc.Image + "@" + Digest("a")
			if ref := pins[n]; ref != "" {
				h.Pins[n] = ref
			}
			images[svc.Image] = ImageFor(svc.Image)
		}
	}
	if err := stacks.PrepareStandalone(project, stackID, h.Pins, objs); err != nil {
		t.Fatal(err)
	}
	nets, vols := map[string]string{}, map[string]string{}
	owner := map[string]string{stacks.LabelComposeProject: project.Name, stacks.LabelStackID: stackID}
	for key, n := range project.Networks {
		nets[key] = n.Name
		if !bool(n.External) {
			h.Networks = append(h.Networks, dockertest.FakeNetwork{ID: "net-" + n.Name, Name: n.Name, Driver: "bridge", Scope: "local", Labels: owner})
		}
	}
	for key, v := range project.Volumes {
		vols[key] = v.Name
		if !bool(v.External) {
			h.Volumes = append(h.Volumes, dockertest.FakeVolume{Name: v.Name, Driver: "local", Labels: owner})
		}
	}
	ids := map[string][]string{}
	names := make([]string, 0, len(project.Services))
	for n := range project.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h.IDs[n] = hexOf("container:" + n)
		ids[n] = []string{h.IDs[n]}
	}
	h.Docs = map[string]*container.InspectResponse{}
	for _, n := range names {
		resolved := stacks.ResolveServiceRefs(project.Services[n], ids)
		hash, err := dockerx.ServiceHash(resolved)
		if err != nil {
			t.Fatal(err)
		}
		h.Hash[n] = hash
		img := images[project.Services[n].CustomLabels[stacks.LabelStackImage]]
		doc := dockertest.ComposeInspect(resolved, dockertest.ComposeOptions{
			Project: project.Name, ID: h.IDs[n], ConfigHash: hash, Networks: nets, Volumes: vols, Image: img,
		})
		h.Containers = append(h.Containers, dockertest.FakeContainer{
			ID: h.IDs[n], Names: []string{"/" + project.Name + "-" + n + "-1"}, Image: h.Pins[n], State: "running",
			Labels: doc.Config.Labels, Inspect: dockertest.FakeInspect{Document: &doc, ImageID: img.ID},
		})
		h.Docs[n] = &doc
	}
	for _, img := range images {
		h.Images = append(h.Images, img)
	}
	slices.SortFunc(h.Images, func(a, b dockertest.FakeImage) int { return strings.Compare(a.ID, b.ID) })
	return h
}

// Apply puts the stack's containers, images, networks and volumes into an engine's configuration.
func (h *Hosted) Apply(c *dockertest.FakeConfig) {
	c.Containers, c.Images, c.Networks, c.Volumes = h.Containers, h.Images, h.Networks, h.Volumes
}
