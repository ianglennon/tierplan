// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package stackopstest stands a deployed standalone stack on a fake engine, for the tests of the packages
// that review and deploy stacks. It is test support and is imported by tests only.
package stackopstest

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"sort"
	"strconv"
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
	// The SDK stamps the hash of the config it created a network or a volume from, and a later up compares it
	// (plan 08-17): the objects of a deployed stack carry it too.
	owned := func(hash string) map[string]string {
		out := maps.Clone(owner)
		out[stacks.LabelComposeConfigHash] = hash
		return out
	}
	for key, n := range project.Networks {
		nets[key] = n.Name
		if !bool(n.External) {
			hash, err := dockerx.ComposeNetworkHash(&n)
			if err != nil {
				t.Fatal(err)
			}
			h.Networks = append(h.Networks, dockertest.FakeNetwork{ID: "net-" + n.Name, Name: n.Name, Driver: "bridge", Scope: "local", Labels: owned(hash)})
		}
	}
	for key, v := range project.Volumes {
		vols[key] = v.Name
		if !bool(v.External) {
			hash, err := dockerx.ComposeVolumeHash(v)
			if err != nil {
				t.Fatal(err)
			}
			h.Volumes = append(h.Volumes, dockertest.FakeVolume{Name: v.Name, Driver: "local", Labels: owned(hash)})
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
			ID: h.IDs[n], Names: []string{"/" + project.Name + "-" + n + "-1"}, Image: h.Pins[n], ImageID: img.ID, State: "running",
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

// Foreign stands a project that someone ran with the docker compose command on a fake standalone host, the
// way the CLI leaves it (plan 08-12): no label of Bayplan's on a container, a network or a volume, the image
// of each service as the file wrote it, the config hash of each service computed on the file as loaded, and
// on each network and volume the com.docker.compose.config-hash label of its config, computed with the SDK's
// own functions on the config as loaded. A service with a scale has that many containers, a depends_on
// leaves its label, and the engine reports the objects the file declares (the default network too, when a
// service uses it). A test changes the document of a container through Docs, or an image or an object in
// the lists, before it stands the host up with Apply.
func Foreign(t testing.TB, name, compose string) *Hosted {
	t.Helper()
	copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}}
	checked := stacks.Check(context.Background(), stacks.CheckInput{Name: name, Target: stacks.TargetStandalone, Copy: copy})
	if checked.Loaded == nil {
		t.Fatalf("check: %v", checked.Problems)
	}
	project := checked.Loaded.Project
	h := &Hosted{Name: name, Project: project, Hash: map[string]string{}, IDs: map[string]string{}, Pins: map[string]string{}}
	owner := func(extra map[string]string, labels map[string]string) map[string]string {
		out := map[string]string{"com.docker.compose.project": project.Name, "com.docker.compose.version": "5.6.0"}
		maps.Copy(out, labels)
		maps.Copy(out, extra)
		return out
	}

	nets, vols := map[string]string{}, map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(project.Networks)) {
		n := project.Networks[key]
		nets[key] = n.Name
		if bool(n.External) {
			h.Networks = append(h.Networks, dockertest.FakeNetwork{ID: "net-" + n.Name, Name: n.Name, Driver: "bridge", Scope: "local"})
			continue
		}
		hash, err := dockerx.ComposeNetworkHash(&n)
		if err != nil {
			t.Fatal(err)
		}
		fake := dockertest.FakeNetwork{
			ID: "net-" + n.Name, Name: n.Name, Driver: cmp.Or(n.Driver, "bridge"), Scope: "local",
			Labels:   owner(map[string]string{"com.docker.compose.network": key, "com.docker.compose.config-hash": hash}, n.Labels),
			Internal: n.Internal, Attachable: n.Attachable, IPv6: n.EnableIPv6 != nil && *n.EnableIPv6, Options: maps.Clone(n.DriverOpts),
		}
		for _, pool := range n.Ipam.Config {
			if pool.Subnet != "" {
				fake.Subnets = append(fake.Subnets, pool.Subnet)
				fake.Gateways = append(fake.Gateways, pool.Gateway)
			}
		}
		h.Networks = append(h.Networks, fake)
	}
	for _, key := range slices.Sorted(maps.Keys(project.Volumes)) {
		v := project.Volumes[key]
		vols[key] = v.Name
		if bool(v.External) {
			h.Volumes = append(h.Volumes, dockertest.FakeVolume{Name: v.Name, Driver: "local"})
			continue
		}
		hash, err := dockerx.ComposeVolumeHash(v)
		if err != nil {
			t.Fatal(err)
		}
		h.Volumes = append(h.Volumes, dockertest.FakeVolume{
			Name: v.Name, Driver: cmp.Or(v.Driver, "local"), Options: maps.Clone(v.DriverOpts),
			Labels: owner(map[string]string{"com.docker.compose.volume": key, "com.docker.compose.config-hash": hash}, v.Labels),
		})
	}

	names := slices.Sorted(maps.Keys(project.Services))
	images := map[string]dockertest.FakeImage{}
	ids := map[string][]string{}
	for _, n := range names {
		svc := project.Services[n]
		if svc.Image != "" {
			images[svc.Image] = ImageFor(svc.Image)
			h.Pins[n] = svc.Image
		}
		for number := 1; number <= svc.GetScale(); number++ {
			ids[n] = append(ids[n], hexOf("container:"+n+"-"+strconv.Itoa(number)))
		}
		h.IDs[n] = ids[n][0]
	}
	h.Docs = map[string]*container.InspectResponse{}
	for _, n := range names {
		resolved := stacks.ResolveServiceRefs(project.Services[n], ids)
		hash, err := dockerx.ServiceHash(resolved)
		if err != nil {
			t.Fatal(err)
		}
		h.Hash[n] = hash
		var deps []string
		for _, dep := range slices.Sorted(maps.Keys(resolved.DependsOn)) {
			d := resolved.DependsOn[dep]
			deps = append(deps, dep+":"+d.Condition+":"+strconv.FormatBool(d.Restart))
		}
		img := images[resolved.Image]
		for i, id := range ids[n] {
			doc := dockertest.ComposeInspect(resolved, dockertest.ComposeOptions{
				Project: project.Name, Number: i + 1, ID: id, ConfigHash: hash, Networks: nets, Volumes: vols, Image: img,
			})
			doc.Config.Labels["com.docker.compose.depends_on"] = strings.Join(deps, ",")
			h.Containers = append(h.Containers, dockertest.FakeContainer{
				ID: id, Names: []string{doc.Name}, Image: resolved.Image, ImageID: img.ID, State: "running",
				Labels: doc.Config.Labels, Inspect: dockertest.FakeInspect{Document: &doc, ImageID: img.ID},
			})
			if i == 0 {
				h.Docs[n] = &doc
			}
		}
	}
	for _, img := range images {
		h.Images = append(h.Images, img)
	}
	slices.SortFunc(h.Images, func(a, b dockertest.FakeImage) int { return strings.Compare(a.ID, b.ID) })
	return h
}
