// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// ItemProxy is the item of the standalone disable job that takes the proxy's compose project down.
const ItemProxy = "proxy"

// CodeDisableProxyFailed ends the proxy item of a standalone switch-off that could not take the project down.
const CodeDisableProxyFailed = "disable_proxy_failed"

// CodeForeignContainer is the refusal of a switch-off of a standalone host whose compose project of the proxy holds a
// container that is not the proxy: the project's down would remove it too, and it is not TierPlan's to remove (WR-A3).
const CodeForeignContainer = "ingress_foreign_container"

// standaloneDisableItems are the items of the standalone disable job in the order they run. TierPlan's own route is released
// first (D-11). The proxy goes before the network, which an attached container would keep, and the network before the volume,
// which holds every private key and so is the last thing of the host's to go (PROH-25-06).
var standaloneDisableItems = []string{ItemSelfRoute, ItemSwitchOff, ItemProxy, ItemNetwork, ItemVolumes, ItemToken, ItemNodes}

// standaloneDisableLabels are the lines of the standalone job's steps.
var standaloneDisableLabels = map[string]string{
	ItemSelfRoute: "Release TierPlan's own route",
	ItemSwitchOff: "Stop serving routes",
	ItemProxy:     "Remove the proxy container",
	ItemNetwork:   "Remove the ingress network",
	ItemVolumes:   "Remove the certificate cache volume",
	ItemToken:     "Disable the join token",
	ItemNodes:     "Remove the proxy node",
}

// checkDisableStandalone is the refusals of a switch-off of a standalone host: a paused environment alone (D-42), then a host
// that is not connected (D-21) and live routes (D-25), each as its own finding when both stand. Nothing is written for any.
func (s *Service) checkDisableStandalone(ctx context.Context, info EnvInfo, canView func(envID string) bool) ([]Finding, error) {
	if f, err := s.disablePaused(ctx, info); err != nil || f != nil {
		return f, err
	}
	var findings []Finding
	if !hostConnected(info) {
		findings = append(findings, Finding{
			Code: CodeHostUnreachable, Severity: SeverityError,
			Message: "The host is not connected to TierPlan, so the ingress proxy cannot be removed from it. " +
				"Reconnect it and switch ingress off again. Nothing was changed.",
		})
	}
	if hostConnected(info) {
		// A read that fails here is left to the step, which reads again and refuses on any failure.
		if foreign, err := s.foreignProjectContainers(ctx, info); err != nil {
			s.log().Warn("could not read the host for a switch-off", "environment", info.ID, "error", err)
		} else if len(foreign) > 0 {
			findings = append(findings, Finding{
				Code: CodeForeignContainer, Severity: SeverityError, Items: foreign,
				Message: "The host holds containers in the " + ingress.ProjectName + " compose project that are not TierPlan's, and switching " +
					"ingress off would remove them with the proxy. Remove them or move them to another project, then switch ingress off " +
					"again. Nothing was changed.",
			})
		}
	}
	routes, err := s.disableRoutesLive(ctx, info, canView)
	if err != nil {
		return nil, err
	}
	return append(findings, routes...), nil
}

// foreignProjectContainers are the names of the containers of the proxy's compose project on the host that are not the
// proxy: the project's down removes every container of the project, so only a project that holds the proxy alone is
// taken down (the swarm's switch-off leaves a service that is not the proxy alone, by the same ownership rule).
func (s *Service) foreignProjectContainers(ctx context.Context, info EnvInfo) ([]string, error) {
	var foreign []string
	err := s.d.Engine.OnNode(ctx, info.ID, info.Nodes[0].AgentID, func(c *dockerx.Client) error {
		rows, err := c.ComposeContainers(ctx, ingress.ProjectName)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if !ingress.OwnsProxyContainer(row.Labels) {
				foreign = append(foreign, row.Name)
			}
		}
		return nil
	})
	return foreign, err
}

// previewDisableStandalone says what a switch-off of a standalone host would remove and what would stop it, and writes
// nothing. The host is read only when it is connected; a host that is not has nothing said of its objects, since the finding
// says why.
func (s *Service) previewDisableStandalone(ctx context.Context, info EnvInfo, canView func(envID string) bool) (DisablePreview, error) {
	findings, err := s.checkDisableStandalone(ctx, info, canView)
	if err != nil {
		return DisablePreview{}, err
	}
	res := DisablePreview{Findings: findings, Removes: DisableRemoves{Secrets: []string{}, Volumes: []NodeName{}}, Unreachable: []string{}}
	for _, n := range info.Nodes {
		res.Removes.Volumes = append(res.Removes.Volumes, NodeName{ID: n.AgentID, Name: n.Name})
	}
	if !hostConnected(info) {
		return res, nil
	}
	l, err := s.readStandaloneLive(ctx, info)
	if err != nil {
		// Read failed: the preview lists the names it would try, as the swarm's does.
		s.log().Warn("could not read the host for a switch-off preview", "environment", info.ID, "error", err)
		// StandaloneProject sets no container name, so Compose names the one container <project>-<service>-1.
		res.Removes.Container, res.Removes.Network = ingress.ProjectName+"-"+ingress.ProxyComposeService+"-1", ingress.NetworkName
		return res, nil
	}
	if l.proxy != nil {
		res.Removes.Container = l.proxy.Name
	}
	if l.network != nil {
		res.Removes.Network = ingress.NetworkName
	}
	return res, nil
}

// disableStandalone switches ingress off for a standalone environment: the swarm's lock, refusals and job pattern over
// the items of standaloneDisableItems. Its refusals are those of checkDisableStandalone, so a host that cannot be reached
// changes nothing (D-21).
func (s *Service) disableStandalone(ctx context.Context, actor audit.Actor, owner string, info EnvInfo, canView func(envID string) bool) (string, error) {
	return s.startDisable(ctx, actor, owner, info, canView, standaloneDisableItems, standaloneDisableLabels, s.checkDisableStandalone)
}

// hostNode reads the standalone environment as the steps of a job see it now: a host that has gone away since the job
// started is a step failure, never a write to somewhere else.
func (e *disableRun) hostNode(ctx context.Context) (EnvInfo, error) {
	info, err := e.s.d.Envs.Info(ctx, e.envID)
	if err != nil {
		return EnvInfo{}, err
	}
	if !hostConnected(info) {
		return EnvInfo{}, errHostGone
	}
	return info, nil
}

// errHostGone is a host that is not connected any more by the time a step needs it.
var errHostGone = errors.New("the standalone host is not connected")

// hostGoneDetail is what a step says of a host that went away: nothing more was removed and a rerun finishes the rest.
const hostGoneDetail = "The host is not connected to TierPlan any more. Reconnect it and switch ingress off again to remove what is left."

// proxy takes the proxy's compose project down on the host: the container and the network are removed, the volume never is.
// A project that is not there is done.
func (e *disableRun) proxy(ctx context.Context) error {
	info, err := e.hostNode(ctx)
	switch {
	case errors.Is(err, errHostGone):
		return fail(CodeDisableProxyFailed, err, hostGoneDetail)
	case err != nil:
		return fail(CodeDisableProxyFailed, err)
	}
	if e.s.d.System == nil {
		return fail(CodeDisableProxyFailed, errors.New("ingress: no deployer of the standalone proxy's project"))
	}
	// The down removes every container of the project, so it is not run while one is not the proxy (WR-A3): the check at
	// the start of the job refused that, and a container that came since is refused here.
	foreign, err := e.s.foreignProjectContainers(ctx, info)
	if err != nil {
		return fail(CodeDisableProxyFailed, err, dockerx.DaemonText(err))
	}
	if len(foreign) > 0 {
		return fail(CodeDisableProxyFailed, errors.New("the ingress compose project holds containers that are not TierPlan's"),
			"These containers are in the "+ingress.ProjectName+" compose project and are not TierPlan's, so the project was not taken down: "+
				strings.Join(foreign, ", ")+". Remove them or move them to another project, then switch ingress off again.")
	}
	if err := e.s.d.System.DownSystem(ctx, e.envID, info.Nodes[0].AgentID); err != nil {
		return fail(CodeDisableProxyFailed, err, dockerx.DaemonText(err))
	}
	e.done(ItemProxy, nil)
	return nil
}

// networkStandalone confirms the ingress network is gone after the project came down, and removes it when it is not and
// nothing holds it. A network that a container which is not the proxy is attached to is never forced: the item fails naming
// the containers (Pitfall 7), the steps after it are not run, and switching off again once they are gone finishes what is left.
func (e *disableRun) networkStandalone(ctx context.Context) error {
	s := e.s
	info, err := e.hostNode(ctx)
	switch {
	case errors.Is(err, errHostGone):
		return fail(CodeDisableNetworkFailed, err, hostGoneDetail)
	case err != nil:
		return fail(CodeDisableNetworkFailed, err)
	}
	node := info.Nodes[0].AgentID
	l, err := s.readStandaloneLive(ctx, info)
	if err != nil {
		return fail(CodeDisableNetworkFailed, err)
	}
	if l.network == nil {
		e.done(ItemNetwork, map[string]string{"removed": "false"})
		return nil
	}
	var attached []string
	if err := s.d.Engine.OnNode(ctx, e.envID, node, func(c *dockerx.Client) (err error) {
		attached, err = c.ContainersOnNetwork(ctx, ingress.NetworkName)
		return err
	}); err != nil {
		return fail(CodeDisableNetworkFailed, err)
	}
	if len(attached) > 0 {
		return fail(CodeDisableNetworkFailed, errors.New("containers are attached to the ingress network"),
			"These containers are still attached to the ingress network: "+strings.Join(attached, ", ")+
				". Disconnect or remove them, then switch ingress off again.")
	}
	if err := s.d.Engine.WriteOnNode(ctx, e.envID, node, func(c *dockerx.Client) error {
		if err := c.NetworkRemove(ctx, l.network.ID); err != nil && !dockerx.IsNotFound(err) {
			return err
		}
		return nil
	}); err != nil {
		if dockerx.IsNetworkInUse(err) {
			return fail(CodeDisableNetworkFailed, err, "A container is attached to the ingress network. Disconnect or remove it, then switch ingress off again.")
		}
		return fail(CodeDisableNetworkFailed, err, dockerx.DaemonText(err))
	}
	e.done(ItemNetwork, map[string]string{"removed": "true"})
	return nil
}

// errVolumeInUse is a cache volume a container still mounts, which no removal is forced past.
type errVolumeInUse struct{ users []string }

func (e errVolumeInUse) Error() string {
	return "containers still mount the certificate cache volume: " + strings.Join(e.users, ", ")
}

// volumeStandalone removes the certificate cache volume of the host, which holds every private key of the environment's
// routes. A volume that is not there is done and one that is not TierPlan's is left alone. One a container still mounts is
// never removed: the item fails naming the container, so the volume is never left behind while the job says it is done
// (PROH-25-06). Nothing is queued: the host was connected when the job started, and a rerun finishes what is left.
func (e *disableRun) volumeStandalone(ctx context.Context) error {
	s := e.s
	info, err := e.hostNode(ctx)
	switch {
	case errors.Is(err, errHostGone):
		return fail(CodeDisableVolumesFailed, err, hostGoneDetail)
	case err != nil:
		return fail(CodeDisableVolumesFailed, err)
	}
	removed := false
	err = s.d.Engine.WriteOnNode(ctx, e.envID, info.Nodes[0].AgentID, func(c *dockerx.Client) error {
		vol, _, err := c.VolumeDetail(ctx, ingress.VolumeName)
		switch {
		case dockerx.IsNotFound(err):
			return nil
		case err != nil:
			return err
		case !ingress.OwnsVolume(vol.Name, vol.Labels):
			e.r.Output("Left the volume " + vol.Name + " in place: it is not TierPlan's.")
			return nil
		}
		users, err := c.VolumeUsers(ctx, ingress.VolumeName)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return errVolumeInUse{users: users}
		}
		if err := c.VolumeRemove(ctx, ingress.VolumeName); err != nil && !dockerx.IsNotFound(err) {
			return err
		}
		removed = true
		return nil
	})
	if err != nil {
		var inUse errVolumeInUse
		if errors.As(err, &inUse) {
			return fail(CodeDisableVolumesFailed, err,
				"These containers still mount the certificate cache volume: "+strings.Join(inUse.users, ", ")+
					". Remove them, then switch ingress off again.")
		}
		return fail(CodeDisableVolumesFailed, err, dockerx.DaemonText(err))
	}
	e.done(ItemVolumes, map[string]string{"removed": strconv.FormatBool(removed)})
	return nil
}
