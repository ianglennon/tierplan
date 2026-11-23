// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// standaloneLive is what a standalone host holds of the ingress proxy's objects.
type standaloneLive struct {
	// proxy is the proxy container of the compose project, nil when there is none.
	proxy *dockerx.ComposeContainer
	// StartedAt is when the proxy container last started, the zero time when the engine says none.
	StartedAt time.Time
	// accessLog is true when the proxy's environment switches the access log on.
	accessLog bool
	// network is the proxy's bridge network when the host holds one that is TierPlan's, nil otherwise.
	network *dockerx.NetworkRow
	// volume is true when the host holds the cache volume and it is TierPlan's.
	volume bool
}

// running is true when the proxy container is up or restarting: neither needs an up.
func (l standaloneLive) running() bool {
	return l.proxy != nil && (l.proxy.State == "running" || l.proxy.State == "restarting")
}

// readStandaloneLive reads the proxy container, its network and its volume from the host's own engine, through the
// node the environment's one engine answers on. It is a read.
func (s *Service) readStandaloneLive(ctx context.Context, info EnvInfo) (standaloneLive, error) {
	var l standaloneLive
	if len(info.Nodes) == 0 {
		return l, errors.New("the standalone environment has no node")
	}
	err := s.d.Engine.OnNode(ctx, info.ID, info.Nodes[0].AgentID, func(c *dockerx.Client) error {
		rows, err := c.ComposeContainers(ctx, ingress.ProjectName)
		if err != nil {
			return err
		}
		for i := range rows {
			if ingress.OwnsProxyContainer(rows[i].Labels) {
				l.proxy = &rows[i]
				break
			}
		}
		if l.proxy != nil {
			det, _, err := c.ContainerDetail(ctx, l.proxy.ID)
			if err != nil {
				return err
			}
			if t, err := time.Parse(time.RFC3339Nano, det.StartedAt); err == nil && t.Year() > 1 {
				l.StartedAt = t.UTC()
			}
			for _, kv := range det.Env {
				if kv == ingressbundle.EnvAccessLog+"="+accessLogOn {
					l.accessLog = true
				}
			}
		}
		nets, err := c.Networks(ctx)
		if err != nil {
			return err
		}
		for i := range nets {
			if ingress.OwnsNetwork(nets[i].Name, nets[i].Labels) {
				l.network = &nets[i]
				break
			}
		}
		vol, _, err := c.VolumeDetail(ctx, ingress.VolumeName)
		switch {
		case err == nil:
			l.volume = ingress.OwnsVolume(vol.Name, vol.Labels)
		case dockerx.IsNotFound(err):
		default:
			return err
		}
		return nil
	})
	return l, err
}

// enrolled says the proxy on the host has enrolled: an active ingress node row of the environment and the cache
// volume, which holds the credential the proxy got by enrolling. A deployer with no Nodes has seen none enrol.
func (s *Service) enrolled(ctx context.Context, envID string, l standaloneLive) (bool, error) {
	if s.d.Nodes == nil || !l.volume {
		return false, nil
	}
	rows, err := s.d.Nodes.ListNodes(ctx, envID)
	if err != nil {
		return false, err
	}
	for _, n := range rows {
		if n.Status == "active" {
			return true, nil
		}
	}
	return false, nil
}

// hasActiveNode says an ingress proxy node of the environment is enrolled and active, whatever the host holds. A deployer
// with no Nodes has seen none enrol.
func (s *Service) hasActiveNode(ctx context.Context, envID string) (bool, error) {
	if s.d.Nodes == nil {
		return false, nil
	}
	rows, err := s.d.Nodes.ListNodes(ctx, envID)
	if err != nil {
		return false, err
	}
	for _, n := range rows {
		if n.Status == "active" {
			return true, nil
		}
	}
	return false, nil
}

// standalonePlan is what the host needs: an up of the project, and in it a fresh join token.
type standalonePlan struct {
	// Up is true when the project has to be brought up.
	Up bool
	// Token is true when the up needs a fresh join token: the proxy has to enrol.
	Token bool
	// Reason is a short word for the log: missing_container, missing_network, not_running, image, access_log or token.
	Reason string
}

// planStandalone decides what a standalone host needs (D-26). The project is brought up when the proxy container or
// its network is missing, the container is neither running nor restarting, its image label or its access-log variable
// is not what is wanted, or the host's proxy has not enrolled and the container's token label is not the current,
// enabled token. A token is made only when the host has not enrolled and an up will create or start the container:
// the token is read when the proxy has to enrol, and never again once it has, so a token that rolls over is no reason
// to touch a running proxy.
func planStandalone(l standaloneLive, wantImage string, accessLog, enrolled bool, tok ingresshub.TokenStatus) standalonePlan {
	reason := ""
	switch {
	case l.proxy == nil:
		reason = "missing_container"
	case l.network == nil:
		reason = "missing_network"
	case !l.running():
		reason = "not_running"
	case l.proxy.Labels[stacks.LabelStackImage] != wantImage:
		reason = "image"
	case l.accessLog != accessLog:
		reason = "access_log"
	case !enrolled && (!tok.Exists || tok.Disabled || strings.TrimSpace(l.proxy.Labels[TokenLabel]) != tok.ID):
		reason = "token"
	}
	if reason == "" {
		return standalonePlan{}
	}
	return standalonePlan{Up: true, Token: !enrolled, Reason: reason}
}
