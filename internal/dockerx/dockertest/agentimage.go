// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockertest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// LocalAgentImage is what `make agent-image` builds on this machine's engine.
	LocalAgentImage = "tierplan-agent:dev"
	// ReleaseAgentImage is the name a development server's install commands use
	// (agents.AgentImage for a non-release version). The harness tags the local
	// image with it on every node, so no registry is contacted.
	ReleaseAgentImage = "ghcr.io/ianglennon/tierplan-agent:edge"
)

// hostClient is a moby client to the machine's own engine, where the images
// the tests load were built or pulled.
func hostClient(t testing.TB) *client.Client {
	t.Helper()
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		t.Fatalf("connect to this machine's Docker engine: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// LoadAgentImage streams `docker save tierplan-agent:dev` from this machine's
// engine into `docker load` on every node and tags it
// ghcr.io/ianglennon/tierplan-agent:edge, the image a development server's
// install commands name. When the image does not exist the test is skipped with
// the command that builds it.
func LoadAgentImage(t testing.TB, nodes ...*Node) {
	t.Helper()
	if _, err := hostClient(t).ImageInspect(context.Background(), LocalAgentImage); err != nil {
		t.Skipf("the agent image %s is not on this machine's engine (%v): run `make agent-image` first", LocalAgentImage, err)
	}
	loadImage(t, LocalAgentImage, ReleaseAgentImage, nodes)
}

// LoadAgentImageAs is LoadAgentImage for an image built under another tag (make
// agent-image AGENT_VERSION=v0.2.1 AGENT_TAG=tierplan-agent:v0.2.1): it copies src
// into every node and tags it as target, the name an install command or an update
// of that version uses. The test is skipped, with the command that builds src, when
// this machine's engine does not have it.
func LoadAgentImageAs(t testing.TB, src, target string, nodes ...*Node) {
	t.Helper()
	if _, err := hostClient(t).ImageInspect(context.Background(), src); err != nil {
		t.Skipf("the agent image %s is not on this machine's engine (%v): run `make agent-image AGENT_TAG=%s AGENT_VERSION=<version>` first", src, err, src)
	}
	loadImage(t, src, target, nodes)
}

// LoadImage copies an image the machine's engine has, pulling it first when it
// does not, into every node under the same name. It is for the small images a
// test starts inside a node, such as busybox.
func LoadImage(t testing.TB, ref string, nodes ...*Node) {
	t.Helper()
	cli := hostClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := cli.ImageInspect(ctx, ref); err != nil {
		pull, err := cli.ImagePull(ctx, ref, client.ImagePullOptions{})
		if err != nil {
			t.Fatalf("pull %s on this machine's engine: %v", ref, err)
		}
		if err := pull.Wait(ctx); err != nil {
			t.Fatalf("pull %s on this machine's engine: %v", ref, err)
		}
	}
	loadImage(t, ref, "", nodes)
}

// loadImage saves src once and loads it into each node, tagging it as also when
// that is not empty. Only this machine's platform is saved: on a containerd image store a plain save is an index that names
// the platforms never pulled here too, and Engine 24's containerd store refuses such a tar for the blobs it lacks.
func loadImage(t testing.TB, src, also string, nodes []*Node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	saved, err := hostClient(t).ImageSave(ctx, []string{src}, client.ImageSaveWithPlatforms(ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		t.Fatalf("docker save %s: %v", src, err)
	}
	tarball, err := io.ReadAll(saved)
	_ = saved.Close()
	if err != nil {
		t.Fatalf("read docker save %s: %v", src, err)
	}
	for _, n := range nodes {
		if err := loadInto(ctx, n.Client(t), tarball, src, also); err != nil {
			t.Fatalf("load %s into %s: %v", src, n.Hostname, err)
		}
	}
}

func loadInto(ctx context.Context, cli *client.Client, tarball []byte, src, also string) error {
	res, err := cli.ImageLoad(ctx, bytes.NewReader(tarball), client.ImageLoadWithQuiet(true))
	if err != nil {
		return err
	}
	_, drain := io.Copy(io.Discard, res)
	if err := errors.Join(drain, res.Close()); err != nil {
		return err
	}
	if also != "" {
		if _, err := cli.ImageTag(ctx, client.ImageTagOptions{Source: src, Target: also}); err != nil {
			return fmt.Errorf("tag %s as %s: %w", src, also, err)
		}
	}
	return nil
}
