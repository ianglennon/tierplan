// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockertest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// SwarmBaseline is what a swarm holds straight after StartSwarm and the image loads
// a caller does before its first use: each node's role and the images on each node.
// Reset brings a swarm back to it, so tests can take turns on one swarm instead of
// each starting its own (about half of a swarm test's time is starting the swarm).
type SwarmBaseline struct {
	managers map[string]bool     // hostname -> is a manager
	images   map[string][]string // hostname -> sorted image IDs
}

// defaultNetworks are the networks every swarm node has before any test adds one.
var defaultNetworks = []string{"bridge", "docker_gwbridge", "host", "ingress", "none"}

// Baseline records the swarm's starting state for Reset.
func (s *Swarm) Baseline(ctx context.Context) (SwarmBaseline, error) {
	b := SwarmBaseline{managers: map[string]bool{}, images: map[string][]string{}}
	for _, n := range s.Nodes() {
		b.managers[n.Hostname] = n.Manager
		ids, err := lines(n.Exec(ctx, "docker", "image", "ls", "-q", "--no-trunc"))
		if err != nil {
			return SwarmBaseline{}, fmt.Errorf("list the images on %s: %w", n.Hostname, err)
		}
		b.images[n.Hostname] = ids
	}
	return b, nil
}

// Reset removes everything a test can leave on the swarm and then checks that the
// swarm matches b: the agent and every other service (and their containers), secrets,
// configs, networks a test made, volumes (the agent keeps its identity in one), other
// containers, images a test pulled or loaded, the test registry's files and any
// registry sign-in, and node labels and availability. It does not mend a swarm whose
// nodes changed role, left or stopped: that is reported, and the caller starts a new
// swarm. Any error means the swarm is not known to be clean and must not be reused.
func (s *Swarm) Reset(ctx context.Context, b SwarmBaseline) error {
	mgr := s.Managers[0]
	for _, line := range []string{
		`ids=$(docker service ls -q); [ -z "$ids" ] || docker service rm $ids >/dev/null`,
	} {
		if out, err := mgr.Sh(ctx, line); err != nil {
			return fmt.Errorf("on %s: %s: %w\n%s", mgr.Hostname, line, err, out)
		}
	}
	// Service tasks stop on their own once the services are gone; wait for that before
	// removing what they used.
	if err := poll(ctx, 2*time.Minute, "the service containers to be gone from every node", func() (bool, string) {
		for _, n := range s.Nodes() {
			ids, err := lines(n.Exec(ctx, "docker", "ps", "-aq", "--filter", "label=com.docker.swarm.service.id"))
			if err != nil {
				return false, err.Error()
			}
			if len(ids) > 0 {
				return false, fmt.Sprintf("%s still has %d service containers", n.Hostname, len(ids))
			}
		}
		return true, ""
	}); err != nil {
		return err
	}
	for _, line := range []string{
		`ids=$(docker secret ls -q); [ -z "$ids" ] || docker secret rm $ids >/dev/null`,
		`ids=$(docker config ls -q); [ -z "$ids" ] || docker config rm $ids >/dev/null`,
		`for id in $(docker node ls -q); do args=""; for k in $(docker node inspect -f '{{range $k, $v := .Spec.Labels}}{{$k}} {{end}}' "$id"); do args="$args --label-rm $k"; done; docker node update --availability active $args "$id" >/dev/null || exit 1; done`,
	} {
		if out, err := mgr.Sh(ctx, line); err != nil {
			return fmt.Errorf("on %s: %s: %w\n%s", mgr.Hostname, line, err, out)
		}
	}
	for _, n := range s.Nodes() {
		keep := strings.Join(b.images[n.Hostname], " ")
		for _, line := range []string{
			`ids=$(docker ps -aq); [ -z "$ids" ] || docker rm -f $ids >/dev/null`,
			`ids=$(docker volume ls -q); [ -z "$ids" ] || docker volume rm -f $ids >/dev/null`,
			`docker network prune -f >/dev/null`,
			`rm -rf ` + registryDir + ` /root/.docker/config.json`,
			`keep="` + keep + `"; for id in $(docker image ls -q --no-trunc | sort -u); do case " $keep " in *" $id "*) ;; *) docker rmi -f "$id" >/dev/null || exit 1;; esac; done`,
		} {
			if out, err := n.Sh(ctx, line); err != nil {
				return fmt.Errorf("on %s: %s: %w\n%s", n.Hostname, line, err, out)
			}
		}
	}
	return s.verify(ctx, b)
}

// verify checks the swarm against b and returns every difference it finds.
func (s *Swarm) verify(ctx context.Context, b SwarmBaseline) error {
	var problems []string
	mgr := s.Managers[0]
	for _, what := range [][]string{
		{"service", "ls", "-q"}, {"secret", "ls", "-q"}, {"config", "ls", "-q"},
	} {
		ids, err := lines(mgr.Exec(ctx, append([]string{"docker"}, what...)...))
		if err != nil {
			return fmt.Errorf("docker %s on %s: %w", strings.Join(what, " "), mgr.Hostname, err)
		}
		if len(ids) > 0 {
			problems = append(problems, fmt.Sprintf("%d left by docker %s", len(ids), strings.Join(what, " ")))
		}
	}
	nodes, err := lines(mgr.Exec(ctx, "docker", "node", "ls", "--format", "{{.Hostname}}|{{.Status}}|{{.Availability}}|{{.ManagerStatus}}"))
	if err != nil {
		return fmt.Errorf("docker node ls on %s: %w", mgr.Hostname, err)
	}
	if len(nodes) != len(b.managers) {
		problems = append(problems, fmt.Sprintf("%d nodes, want %d: %v", len(nodes), len(b.managers), nodes))
	}
	leader := false
	for _, line := range nodes {
		f := strings.Split(line, "|")
		if len(f) != 4 {
			problems = append(problems, "unreadable node line "+line)
			continue
		}
		wantManager, known := b.managers[f[0]]
		switch {
		case !known:
			problems = append(problems, "unknown node "+f[0])
		case f[1] != "Ready" || f[2] != "Active":
			problems = append(problems, fmt.Sprintf("%s is %s/%s", f[0], f[1], f[2]))
		case wantManager != (f[3] != ""):
			problems = append(problems, fmt.Sprintf("%s changed role (manager status %q)", f[0], f[3]))
		}
		leader = leader || f[3] == "Leader"
	}
	if !leader {
		problems = append(problems, "no leader")
	}
	labels, err := mgr.Sh(ctx, `for id in $(docker node ls -q); do docker node inspect -f '{{.Description.Hostname}} {{len .Spec.Labels}}' "$id"; done`)
	if err != nil {
		return fmt.Errorf("read the node labels on %s: %w", mgr.Hostname, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(labels), "\n") {
		if host, n, _ := strings.Cut(line, " "); n != "0" {
			problems = append(problems, fmt.Sprintf("%s keeps %s labels", host, n))
		}
	}
	for _, n := range s.Nodes() {
		for _, what := range [][]string{{"ps", "-aq"}, {"volume", "ls", "-q"}} {
			ids, err := lines(n.Exec(ctx, append([]string{"docker"}, what...)...))
			if err != nil {
				return fmt.Errorf("docker %s on %s: %w", strings.Join(what, " "), n.Hostname, err)
			}
			if len(ids) > 0 {
				problems = append(problems, fmt.Sprintf("%s: %d left by docker %s", n.Hostname, len(ids), strings.Join(what, " ")))
			}
		}
		nets, err := lines(n.Exec(ctx, "docker", "network", "ls", "--format", "{{.Name}}"))
		if err != nil {
			return fmt.Errorf("docker network ls on %s: %w", n.Hostname, err)
		}
		if !slices.Equal(nets, defaultNetworks) {
			problems = append(problems, fmt.Sprintf("%s: networks %v, want %v", n.Hostname, nets, defaultNetworks))
		}
		ids, err := lines(n.Exec(ctx, "docker", "image", "ls", "-q", "--no-trunc"))
		if err != nil {
			return fmt.Errorf("docker image ls on %s: %w", n.Hostname, err)
		}
		if !slices.Equal(ids, b.images[n.Hostname]) {
			problems = append(problems, fmt.Sprintf("%s: images %v, want %v", n.Hostname, ids, b.images[n.Hostname]))
		}
	}
	if len(problems) > 0 {
		return errors.New("the swarm is not back to its baseline: " + strings.Join(problems, "; "))
	}
	return nil
}

// Terminate removes the swarm's nodes now rather than when the test ends, so a
// caller that replaces a swarm does not hold two at once (each node is a dind
// engine, and four of them need 6-8 GB). The network goes with the test.
func (s *Swarm) Terminate(ctx context.Context) error {
	var errs []error
	for _, n := range s.Nodes() {
		if err := n.ctr.Terminate(ctx); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", n.Hostname, err))
		}
	}
	return errors.Join(errs...)
}

// lines splits a command's output into its non-empty lines, sorted and without
// duplicates, and passes an error through.
func lines(out string, err error) ([]string, error) {
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	var l []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			l = append(l, line)
		}
	}
	slices.Sort(l)
	return slices.Compact(l), nil
}
