// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"strings"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// ErrComposeForbidden is what the guard answers when the Compose SDK asks the engine for something a
// stack deploy must never do. The request is not sent anywhere (RESEARCH Q6, Pitfalls 8 and 9).
var ErrComposeForbidden = errors.New("dockerx: a stack deploy may not make that engine call")

func forbidden(method string) error {
	return fmt.Errorf("%w: %s", ErrComposeForbidden, method)
}

// guardClient is the engine client the Compose SDK is given. It is TierPlan's own client with the calls
// listed below overridden to fail before anything is sent: the agent's allowlist and host policy are the
// second gate on a host that has an agent, and this is the first, and the only one on the server's own
// socket. Everything else passes through to the embedded client, so the SDK sees one engine.
//
// The list is closed against the whole of client.APIClient by TestEveryClientMethodIsDecided: a method a
// later client version adds must be put in the refused list or the forwarded one, so a new capability is
// never granted by omission.
type guardClient struct {
	client.APIClient
}

// Close does nothing: the engine client belongs to the dockerx.Client that made it, which is still using
// it, and the SDK closes the client it was given when it is closed.
func (guardClient) Close() error { return nil }

// Building images: the Compose SDK builds an image from a build context. TierPlan strips every `build` key before
// it hands over a project and never builds an image (A5), and a build context would be read from the server's own
// filesystem.
func (guardClient) ImageBuild(_ context.Context, _ io.Reader, _ client.ImageBuildOptions) (client.ImageBuildResult, error) {
	var zero client.ImageBuildResult
	return zero, forbidden("ImageBuild")
}

func (guardClient) BuildCachePrune(_ context.Context, _ client.BuildCachePruneOptions) (client.BuildCachePruneResult, error) {
	var zero client.BuildCachePruneResult
	return zero, forbidden("BuildCachePrune")
}

func (guardClient) BuildCancel(_ context.Context, _ string, _ client.BuildCancelOptions) (client.BuildCancelResult, error) {
	var zero client.BuildCancelResult
	return zero, forbidden("BuildCancel")
}

// Moving image content in or out of the engine: a stack deploy pulls by reference and never saves, loads, pushes
// or imports an image.
func (guardClient) ImageSave(_ context.Context, _ []string, _ ...client.ImageSaveOption) (client.ImageSaveResult, error) {
	var zero client.ImageSaveResult
	return zero, forbidden("ImageSave")
}

func (guardClient) ImageLoad(_ context.Context, _ io.Reader, _ ...client.ImageLoadOption) (client.ImageLoadResult, error) {
	var zero client.ImageLoadResult
	return zero, forbidden("ImageLoad")
}

func (guardClient) ImagePush(_ context.Context, _ string, _ client.ImagePushOptions) (client.ImagePushResponse, error) {
	var zero client.ImagePushResponse
	return zero, forbidden("ImagePush")
}

func (guardClient) ImageImport(_ context.Context, _ client.ImageImportSource, _ string, _ client.ImageImportOptions) (client.ImageImportResult, error) {
	var zero client.ImageImportResult
	return zero, forbidden("ImageImport")
}

func (guardClient) ImageSearch(_ context.Context, _ string, _ client.ImageSearchOptions) (client.ImageSearchResult, error) {
	var zero client.ImageSearchResult
	return zero, forbidden("ImageSearch")
}

func (guardClient) ImageAttestations(_ context.Context, _ string, _ ...client.ImageAttestationsOption) (client.ImageAttestationsResult, error) {
	var zero client.ImageAttestationsResult
	return zero, forbidden("ImageAttestations")
}

// Reading or changing a container's filesystem or state beyond what a deploy needs: exporting, committing, diffing
// and copying out of a container would read a stack's data into the TierPlan server, and an in-place update would
// change a container the deploy did not recreate.
func (guardClient) ContainerExport(_ context.Context, _ string, _ client.ContainerExportOptions) (client.ContainerExportResult, error) {
	var zero client.ContainerExportResult
	return zero, forbidden("ContainerExport")
}

func (guardClient) ContainerCommit(_ context.Context, _ string, _ client.ContainerCommitOptions) (client.ContainerCommitResult, error) {
	var zero client.ContainerCommitResult
	return zero, forbidden("ContainerCommit")
}

func (guardClient) ContainerDiff(_ context.Context, _ string, _ client.ContainerDiffOptions) (client.ContainerDiffResult, error) {
	var zero client.ContainerDiffResult
	return zero, forbidden("ContainerDiff")
}

func (guardClient) CopyFromContainer(_ context.Context, _ string, _ client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	var zero client.CopyFromContainerResult
	return zero, forbidden("CopyFromContainer")
}

func (guardClient) ContainerUpdate(_ context.Context, _ string, _ client.ContainerUpdateOptions) (client.ContainerUpdateResult, error) {
	var zero client.ContainerUpdateResult
	return zero, forbidden("ContainerUpdate")
}

// Exec and attach: a stack deploy never runs a process in a container or attaches to one. The shell and the
// terminal are TierPlan's own routes with their own permission.
func (guardClient) ContainerAttach(_ context.Context, _ string, _ client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	var zero client.ContainerAttachResult
	return zero, forbidden("ContainerAttach")
}

func (guardClient) ExecCreate(_ context.Context, _ string, _ client.ExecCreateOptions) (client.ExecCreateResult, error) {
	var zero client.ExecCreateResult
	return zero, forbidden("ExecCreate")
}

func (guardClient) ExecStart(_ context.Context, _ string, _ client.ExecStartOptions) (client.ExecStartResult, error) {
	var zero client.ExecStartResult
	return zero, forbidden("ExecStart")
}

func (guardClient) ExecAttach(_ context.Context, _ string, _ client.ExecAttachOptions) (client.ExecAttachResult, error) {
	var zero client.ExecAttachResult
	return zero, forbidden("ExecAttach")
}

func (guardClient) ExecInspect(_ context.Context, _ string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	var zero client.ExecInspectResult
	return zero, forbidden("ExecInspect")
}

func (guardClient) ExecResize(_ context.Context, _ string, _ client.ExecResizeOptions) (client.ExecResizeResult, error) {
	var zero client.ExecResizeResult
	return zero, forbidden("ExecResize")
}

// Sweeping prunes: removing what nothing uses is the janitor's job, with its own rules and its own lock, never a
// side effect of a deploy.
func (guardClient) ContainerPrune(_ context.Context, _ client.ContainerPruneOptions) (client.ContainerPruneResult, error) {
	var zero client.ContainerPruneResult
	return zero, forbidden("ContainerPrune")
}

func (guardClient) ImagePrune(_ context.Context, _ client.ImagePruneOptions) (client.ImagePruneResult, error) {
	var zero client.ImagePruneResult
	return zero, forbidden("ImagePrune")
}

func (guardClient) NetworkPrune(_ context.Context, _ client.NetworkPruneOptions) (client.NetworkPruneResult, error) {
	var zero client.NetworkPruneResult
	return zero, forbidden("NetworkPrune")
}

func (guardClient) VolumePrune(_ context.Context, _ client.VolumePruneOptions) (client.VolumePruneResult, error) {
	var zero client.VolumePruneResult
	return zero, forbidden("VolumePrune")
}

// Swarm: a standalone deploy has nothing to do with the swarm, and the swarm membership, nodes, services, tasks,
// secrets and configs are changed only through TierPlan's own typed routes.
func (guardClient) SwarmInit(_ context.Context, _ client.SwarmInitOptions) (client.SwarmInitResult, error) {
	var zero client.SwarmInitResult
	return zero, forbidden("SwarmInit")
}

func (guardClient) SwarmJoin(_ context.Context, _ client.SwarmJoinOptions) (client.SwarmJoinResult, error) {
	var zero client.SwarmJoinResult
	return zero, forbidden("SwarmJoin")
}

func (guardClient) SwarmInspect(_ context.Context, _ client.SwarmInspectOptions) (client.SwarmInspectResult, error) {
	var zero client.SwarmInspectResult
	return zero, forbidden("SwarmInspect")
}

func (guardClient) SwarmUpdate(_ context.Context, _ client.SwarmUpdateOptions) (client.SwarmUpdateResult, error) {
	var zero client.SwarmUpdateResult
	return zero, forbidden("SwarmUpdate")
}

func (guardClient) SwarmLeave(_ context.Context, _ client.SwarmLeaveOptions) (client.SwarmLeaveResult, error) {
	var zero client.SwarmLeaveResult
	return zero, forbidden("SwarmLeave")
}

func (guardClient) SwarmGetUnlockKey(_ context.Context) (client.SwarmGetUnlockKeyResult, error) {
	var zero client.SwarmGetUnlockKeyResult
	return zero, forbidden("SwarmGetUnlockKey")
}

func (guardClient) SwarmUnlock(_ context.Context, _ client.SwarmUnlockOptions) (client.SwarmUnlockResult, error) {
	var zero client.SwarmUnlockResult
	return zero, forbidden("SwarmUnlock")
}

func (guardClient) NodeInspect(_ context.Context, _ string, _ client.NodeInspectOptions) (client.NodeInspectResult, error) {
	var zero client.NodeInspectResult
	return zero, forbidden("NodeInspect")
}

func (guardClient) NodeList(_ context.Context, _ client.NodeListOptions) (client.NodeListResult, error) {
	var zero client.NodeListResult
	return zero, forbidden("NodeList")
}

func (guardClient) NodeUpdate(_ context.Context, _ string, _ client.NodeUpdateOptions) (client.NodeUpdateResult, error) {
	var zero client.NodeUpdateResult
	return zero, forbidden("NodeUpdate")
}

func (guardClient) NodeRemove(_ context.Context, _ string, _ client.NodeRemoveOptions) (client.NodeRemoveResult, error) {
	var zero client.NodeRemoveResult
	return zero, forbidden("NodeRemove")
}

func (guardClient) ServiceCreate(_ context.Context, _ client.ServiceCreateOptions) (client.ServiceCreateResult, error) {
	var zero client.ServiceCreateResult
	return zero, forbidden("ServiceCreate")
}

func (guardClient) ServiceInspect(_ context.Context, _ string, _ client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	var zero client.ServiceInspectResult
	return zero, forbidden("ServiceInspect")
}

func (guardClient) ServiceList(_ context.Context, _ client.ServiceListOptions) (client.ServiceListResult, error) {
	var zero client.ServiceListResult
	return zero, forbidden("ServiceList")
}

func (guardClient) ServiceUpdate(_ context.Context, _ string, _ client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
	var zero client.ServiceUpdateResult
	return zero, forbidden("ServiceUpdate")
}

func (guardClient) ServiceRemove(_ context.Context, _ string, _ client.ServiceRemoveOptions) (client.ServiceRemoveResult, error) {
	var zero client.ServiceRemoveResult
	return zero, forbidden("ServiceRemove")
}

func (guardClient) ServiceLogs(_ context.Context, _ string, _ client.ServiceLogsOptions) (client.ServiceLogsResult, error) {
	var zero client.ServiceLogsResult
	return zero, forbidden("ServiceLogs")
}

func (guardClient) TaskInspect(_ context.Context, _ string, _ client.TaskInspectOptions) (client.TaskInspectResult, error) {
	var zero client.TaskInspectResult
	return zero, forbidden("TaskInspect")
}

func (guardClient) TaskList(_ context.Context, _ client.TaskListOptions) (client.TaskListResult, error) {
	var zero client.TaskListResult
	return zero, forbidden("TaskList")
}

func (guardClient) TaskLogs(_ context.Context, _ string, _ client.TaskLogsOptions) (client.TaskLogsResult, error) {
	var zero client.TaskLogsResult
	return zero, forbidden("TaskLogs")
}

func (guardClient) SecretCreate(_ context.Context, _ client.SecretCreateOptions) (client.SecretCreateResult, error) {
	var zero client.SecretCreateResult
	return zero, forbidden("SecretCreate")
}

func (guardClient) SecretInspect(_ context.Context, _ string, _ client.SecretInspectOptions) (client.SecretInspectResult, error) {
	var zero client.SecretInspectResult
	return zero, forbidden("SecretInspect")
}

func (guardClient) SecretList(_ context.Context, _ client.SecretListOptions) (client.SecretListResult, error) {
	var zero client.SecretListResult
	return zero, forbidden("SecretList")
}

func (guardClient) SecretUpdate(_ context.Context, _ string, _ client.SecretUpdateOptions) (client.SecretUpdateResult, error) {
	var zero client.SecretUpdateResult
	return zero, forbidden("SecretUpdate")
}

func (guardClient) SecretRemove(_ context.Context, _ string, _ client.SecretRemoveOptions) (client.SecretRemoveResult, error) {
	var zero client.SecretRemoveResult
	return zero, forbidden("SecretRemove")
}

func (guardClient) ConfigCreate(_ context.Context, _ client.ConfigCreateOptions) (client.ConfigCreateResult, error) {
	var zero client.ConfigCreateResult
	return zero, forbidden("ConfigCreate")
}

func (guardClient) ConfigInspect(_ context.Context, _ string, _ client.ConfigInspectOptions) (client.ConfigInspectResult, error) {
	var zero client.ConfigInspectResult
	return zero, forbidden("ConfigInspect")
}

func (guardClient) ConfigList(_ context.Context, _ client.ConfigListOptions) (client.ConfigListResult, error) {
	var zero client.ConfigListResult
	return zero, forbidden("ConfigList")
}

func (guardClient) ConfigUpdate(_ context.Context, _ string, _ client.ConfigUpdateOptions) (client.ConfigUpdateResult, error) {
	var zero client.ConfigUpdateResult
	return zero, forbidden("ConfigUpdate")
}

func (guardClient) ConfigRemove(_ context.Context, _ string, _ client.ConfigRemoveOptions) (client.ConfigRemoveResult, error) {
	var zero client.ConfigRemoveResult
	return zero, forbidden("ConfigRemove")
}

// Plugins: installing or changing an engine plugin runs third-party code with host privileges, and no stack deploy
// needs one.
func (guardClient) PluginCreate(_ context.Context, _ io.Reader, _ client.PluginCreateOptions) (client.PluginCreateResult, error) {
	var zero client.PluginCreateResult
	return zero, forbidden("PluginCreate")
}

func (guardClient) PluginInstall(_ context.Context, _ string, _ client.PluginInstallOptions) (client.PluginInstallResult, error) {
	var zero client.PluginInstallResult
	return zero, forbidden("PluginInstall")
}

func (guardClient) PluginInspect(_ context.Context, _ string, _ client.PluginInspectOptions) (client.PluginInspectResult, error) {
	var zero client.PluginInspectResult
	return zero, forbidden("PluginInspect")
}

func (guardClient) PluginList(_ context.Context, _ client.PluginListOptions) (client.PluginListResult, error) {
	var zero client.PluginListResult
	return zero, forbidden("PluginList")
}

func (guardClient) PluginRemove(_ context.Context, _ string, _ client.PluginRemoveOptions) (client.PluginRemoveResult, error) {
	var zero client.PluginRemoveResult
	return zero, forbidden("PluginRemove")
}

func (guardClient) PluginEnable(_ context.Context, _ string, _ client.PluginEnableOptions) (client.PluginEnableResult, error) {
	var zero client.PluginEnableResult
	return zero, forbidden("PluginEnable")
}

func (guardClient) PluginDisable(_ context.Context, _ string, _ client.PluginDisableOptions) (client.PluginDisableResult, error) {
	var zero client.PluginDisableResult
	return zero, forbidden("PluginDisable")
}

func (guardClient) PluginUpgrade(_ context.Context, _ string, _ client.PluginUpgradeOptions) (client.PluginUpgradeResult, error) {
	var zero client.PluginUpgradeResult
	return zero, forbidden("PluginUpgrade")
}

func (guardClient) PluginPush(_ context.Context, _ string, _ client.PluginPushOptions) (client.PluginPushResult, error) {
	var zero client.PluginPushResult
	return zero, forbidden("PluginPush")
}

func (guardClient) PluginSet(_ context.Context, _ string, _ client.PluginSetOptions) (client.PluginSetResult, error) {
	var zero client.PluginSetResult
	return zero, forbidden("PluginSet")
}

// Registry sign-in and checkpoints: the SDK's registry credentials come from TierPlan (Resolver.AuthFor), never
// from a login it makes, and checkpoint and restore is experimental.
func (guardClient) RegistryLogin(_ context.Context, _ client.RegistryLoginOptions) (client.RegistryLoginResult, error) {
	var zero client.RegistryLoginResult
	return zero, forbidden("RegistryLogin")
}

func (guardClient) CheckpointCreate(_ context.Context, _ string, _ client.CheckpointCreateOptions) (client.CheckpointCreateResult, error) {
	var zero client.CheckpointCreateResult
	return zero, forbidden("CheckpointCreate")
}

func (guardClient) CheckpointRemove(_ context.Context, _ string, _ client.CheckpointRemoveOptions) (client.CheckpointRemoveResult, error) {
	var zero client.CheckpointRemoveResult
	return zero, forbidden("CheckpointRemove")
}

func (guardClient) CheckpointList(_ context.Context, _ string, _ client.CheckpointListOptions) (client.CheckpointListResult, error) {
	var zero client.CheckpointListResult
	return zero, forbidden("CheckpointList")
}

// composeServiceLabel is the label the Compose SDK puts on every container it creates, naming the service.
const composeServiceLabel = "com.docker.compose.service"

// The labels the Compose SDK puts on every container it creates naming its project and its replica number.
const (
	composeProjectLabel = "com.docker.compose.project"
	composeNumberLabel  = "com.docker.compose.container-number"
)

// ErrIngressContainerName is a create or connect on the shared ingress network for a container whose name is not the one
// the SDK gives a replica, <project>-<service>-<number>. The engine resolves a container's own name on that bridge, so a
// chosen name could equal another stack's alias and take its route (T-25-47). Check refuses container_name there
// (ingress_container_name); this is the backstop for any path that reaches the engine without that check.
var ErrIngressContainerName = errors.New("dockerx: on the ingress network a container keeps the name the stack deploy gives it")

// ingressNameAllowed reports whether name is the SDK's own name for the replica the labels describe, allowing the
// temporary <id12>_ prefix of a recreate.
func ingressNameAllowed(name string, labels map[string]string) bool {
	project, service, number := labels[composeProjectLabel], labels[composeServiceLabel], labels[composeNumberLabel]
	if project == "" || service == "" || number == "" {
		return false
	}
	final := recreateNamePrefix.ReplaceAllString(strings.TrimPrefix(name, "/"), "")
	return final == project+"-"+service+"-"+number
}

// ingressAliases is the alias list the guard lets reach the engine on the shared ingress network: the aliases
// the SDK built, less the bare compose service name and the container's own name, which the SDK adds to every
// endpoint (create.go getAliases). Every other alias keeps its place; nil when none is left.
//
// The SDK recreates a container under a temporary name, <first 12 characters of the old ID>_<name>, and renames it when the
// old one is gone, so a create that replaces a container is made under that name while the alias it adds is the final
// one. The final name is dropped as well (WR-C2).
func ingressAliases(aliases []string, service, container string) []string {
	container = strings.TrimPrefix(container, "/")
	final := recreateNamePrefix.ReplaceAllString(container, "")
	var kept []string
	for _, a := range aliases {
		bare := strings.TrimPrefix(a, "/")
		if (service != "" && a == service) || (container != "" && (bare == container || bare == final)) {
			continue
		}
		kept = append(kept, a)
	}
	return kept
}

// recreateNamePrefix is what the SDK puts before a container's name while it is replaced: the first 12 characters of the
// old container's ID, which is lowercase hex, and an underscore.
var recreateNamePrefix = regexp.MustCompile(`^[0-9a-f]{12}_`)

// ingressEndpoint returns a copy of ep with the bare names removed from its aliases. The caller's endpoint is
// never edited: the SDK keeps using it after the call.
func ingressEndpoint(ep *network.EndpointSettings, service, container string) *network.EndpointSettings {
	cp := *ep
	cp.Aliases = ingressAliases(ep.Aliases, service, container)
	return &cp
}

// ContainerCreate forwards a create to the engine with one change, on the shared ingress network only: the
// SDK adds the bare compose service name (and the container name) as an alias on every network a container
// joins, and on the one bridge every stack of a host shares, another stack's container could answer a
// sibling's name by embedded DNS (D-27, PROH-25-05). A routed service is reached there only by its unique
// alias, which the deployer puts in the endpoint, so the bare names are dropped. Only the endpoint keyed by
// ingress.NetworkName is touched; the options and the map the SDK built are copied, not edited, so the agent
// allowlist sees the rewritten body and the SDK's own view is unchanged.
func (g guardClient) ContainerCreate(ctx context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if o.NetworkingConfig != nil {
		if ep := o.NetworkingConfig.EndpointsConfig[ingress.NetworkName]; ep != nil {
			var labels map[string]string
			if o.Config != nil {
				labels = o.Config.Labels
			}
			if !ingressNameAllowed(o.Name, labels) {
				return client.ContainerCreateResult{}, fmt.Errorf("%w: %s", ErrIngressContainerName, o.Name)
			}
			service := labels[composeServiceLabel]
			nc := *o.NetworkingConfig
			nc.EndpointsConfig = maps.Clone(o.NetworkingConfig.EndpointsConfig)
			nc.EndpointsConfig[ingress.NetworkName] = ingressEndpoint(ep, service, o.Name)
			o.NetworkingConfig = &nc
		}
	}
	return g.APIClient.ContainerCreate(ctx, o)
}

// NetworkConnect is the same rewrite for an engine older than API 1.44, where the SDK joins the extra networks
// of a container one by one after the create with the same endpoint settings. The network is named by name,
// as the SDK passes it. The container is read from the engine for its name and labels: a container not named as
// the SDK names a replica is refused (T-25-47), and the aliases are rewritten when there are any.
func (g guardClient) NetworkConnect(ctx context.Context, networkID string, o client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	if networkID == ingress.NetworkName {
		res, err := g.ContainerInspect(ctx, o.Container, client.ContainerInspectOptions{})
		if err != nil {
			return client.NetworkConnectResult{}, err
		}
		var labels map[string]string
		if res.Container.Config != nil {
			labels = res.Container.Config.Labels
		}
		if !ingressNameAllowed(res.Container.Name, labels) {
			return client.NetworkConnectResult{}, fmt.Errorf("%w: %s", ErrIngressContainerName, strings.TrimPrefix(res.Container.Name, "/"))
		}
		if o.EndpointConfig != nil && len(o.EndpointConfig.Aliases) > 0 {
			o.EndpointConfig = ingressEndpoint(o.EndpointConfig, labels[composeServiceLabel], res.Container.Name)
		}
	}
	return g.APIClient.NetworkConnect(ctx, networkID, o)
}
