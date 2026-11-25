// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestLoadedImageNeedsNoRegistry: an image pinned by digest that LoadImage put into a node is there under its NodeImage name, so
// a test that starts it never pulls it again. Engines before 29 keep neither the name nor the digest of such an image, and every
// node of every test pulled it from Docker Hub, up to its pull limit.
func TestLoadedImageNeedsNoRegistry(t *testing.T) {
	node := dockertest.StartHost(t, dockertest.EngineFromEnv())
	dockertest.LoadImage(t, traefiktest.WhoamiImage, node)
	name := dockertest.NodeImage(traefiktest.WhoamiImage)
	if out, err := node.Exec(testsupport.Context(t), "docker", "run", "-d", "--pull", "never", name); err != nil {
		t.Fatalf("start %s without a pull: %v: %s", name, err, out)
	}
	if got := dockertest.NodeImage("busybox:1.36.1"); got != "busybox:1.36.1" {
		t.Errorf("NodeImage of a tag = %q, want it unchanged", got)
	}
}
