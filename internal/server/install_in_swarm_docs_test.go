// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// These tests tie docs/install-in-swarm.md to what it promises: the stack file parses and holds the
// lines that keep a swarm install safe, and the sections other pages link to exist. guideSection is
// defined in docs_https_test.go (same package).

// swarmGuideText reads docs/install-in-swarm.md. The tests of this package run in internal/server.
func swarmGuideText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/install-in-swarm.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// firstYAMLBlock is the body of the first fenced yaml block of a section.
func firstYAMLBlock(t *testing.T, section string) string {
	t.Helper()
	var out []string
	in := false
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case !in && trimmed == "```yaml":
			in = true
		case in && strings.HasPrefix(trimmed, "```"):
			return strings.Join(out, "\n")
		case in:
			out = append(out, line)
		}
	}
	t.Fatal("the section has no fenced yaml block")
	return ""
}

type swarmStackFile struct {
	Services map[string]struct {
		Image string `yaml:"image"`
		Ports []struct {
			Target    int    `yaml:"target"`
			Published int    `yaml:"published"`
			Mode      string `yaml:"mode"`
		} `yaml:"ports"`
		Volumes []string `yaml:"volumes"`
		Deploy  struct {
			Replicas  int `yaml:"replicas"`
			Placement struct {
				Constraints []string `yaml:"constraints"`
			} `yaml:"placement"`
			UpdateConfig struct {
				Order string `yaml:"order"`
			} `yaml:"update_config"`
		} `yaml:"deploy"`
	} `yaml:"services"`
	Volumes map[string]any `yaml:"volumes"`
}

func TestInstallInSwarmDocsStackFile(t *testing.T) {
	doc := swarmGuideText(t)
	section := guideSection(doc, "The stack file")
	if section == "" {
		t.Fatal(`docs/install-in-swarm.md has no "## The stack file" section`)
	}
	var stack swarmStackFile
	if err := yaml.Unmarshal([]byte(firstYAMLBlock(t, section)), &stack); err != nil {
		t.Fatalf("the stack file does not parse: %v", err)
	}
	if len(stack.Services) != 1 {
		t.Fatalf("the stack file has %d services, want exactly 1", len(stack.Services))
	}
	for name, svc := range stack.Services {
		if !strings.HasPrefix(svc.Image, "ghcr.io/ianglennon/tierplan:") || strings.HasSuffix(svc.Image, ":latest") {
			t.Errorf("%s: image %q must be ghcr.io/ianglennon/tierplan:<X.Y> and never :latest", name, svc.Image)
		}
		if svc.Deploy.Replicas != 1 {
			t.Errorf("%s: deploy.replicas = %d, want 1 (SQLite has one writer)", name, svc.Deploy.Replicas)
		}
		if svc.Deploy.UpdateConfig.Order != "stop-first" {
			t.Errorf("%s: deploy.update_config.order = %q, want stop-first", name, svc.Deploy.UpdateConfig.Order)
		}
		pinned := false
		for _, c := range svc.Deploy.Placement.Constraints {
			if strings.HasPrefix(c, "node.hostname") || strings.HasPrefix(c, "node.labels.") {
				pinned = true
			}
		}
		if !pinned {
			t.Errorf("%s: no placement constraint on node.hostname or node.labels.*: %v", name, svc.Deploy.Placement.Constraints)
		}
		dataVolume, socket := false, false
		for _, v := range svc.Volumes {
			parts := strings.Split(v, ":")
			if len(parts) < 2 {
				continue
			}
			if _, declared := stack.Volumes[parts[0]]; declared && parts[1] == "/data" {
				dataVolume = true
			}
			if parts[0] == "/var/run/docker.sock" {
				socket = true
			}
		}
		if !dataVolume {
			t.Errorf("%s: no named volume declared under volumes: is mounted at /data: %v", name, svc.Volumes)
		}
		if !socket {
			t.Errorf("%s: the Docker socket is not bind-mounted: %v", name, svc.Volumes)
		}
		hostMode := false
		for _, p := range svc.Ports {
			if p.Mode == "host" {
				hostMode = true
			}
		}
		if !hostMode {
			t.Errorf("%s: no published port with mode: host (D-20): %v", name, svc.Ports)
		}
	}
}

func TestInstallInSwarmDocsSectionsAndLinks(t *testing.T) {
	doc := swarmGuideText(t)
	for _, h := range []string{
		"Before you start",
		"Deploy",
		"Add the swarm",
		"Fold this host into the swarm",
		"Fold a host that has ingress set up", // plan 19-10 links this anchor
		"What each node role gives",
		"What not to do",
	} {
		if guideSection(doc, h) == "" {
			t.Errorf("docs/install-in-swarm.md has no %q section", h)
		}
	}
	if !strings.Contains(doc, "(#fold-a-host-that-has-ingress-set-up)") {
		t.Error("the contents list does not link #fold-a-host-that-has-ingress-set-up")
	}
	for _, c := range []struct{ section, phrase string }{
		{"What not to do", "never drain"},
		{"What not to do", "ingress hostname"},
		{"Before you start", "docker stack deploy"},
		{"Fold this host into the swarm", "standby"},
		{"Fold this host into the swarm", "Git repositories"},
		{"Fold this host into the swarm", "counts"},
	} {
		if !strings.Contains(strings.ToLower(guideSection(doc, c.section)), strings.ToLower(c.phrase)) {
			t.Errorf("section %q does not mention %q", c.section, c.phrase)
		}
	}

	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(readme), "docs/install-in-swarm.md"); n < 2 {
		t.Errorf("README.md links docs/install-in-swarm.md %d times, want at least 2", n)
	}
	// The old advice pinned TierPlan to a manager. A worker is supported now.
	if strings.Contains(string(readme), "node.role==manager") {
		t.Error("README.md still tells operators to pin TierPlan to a manager node")
	}
	agent, err := os.ReadFile("../../docs/agent.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agent), "install-in-swarm.md") {
		t.Error("docs/agent.md does not link install-in-swarm.md")
	}
}
