// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/moby/moby/client"
)

// ErrComposeForbidden is what the guard answers when the Compose SDK asks the engine for something a
// stack deploy must never do. The request is not sent anywhere (RESEARCH Q6, Pitfalls 8 and 9).
var ErrComposeForbidden = errors.New("dockerx: a stack deploy may not make that engine call")

func forbidden(method string) error {
	return fmt.Errorf("%w: %s", ErrComposeForbidden, method)
}

// guardClient is the engine client the Compose SDK is given. It is Bayplan's own client with the calls
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

// Building images: the Compose SDK builds an image from a build context. Bayplan strips every `build` key before
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
// and copying out of a container would read a stack's data into the Bayplan server, and an in-place update would
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
// terminal are Bayplan's own routes with their own permission.
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
// secrets and configs are changed only through Bayplan's own typed routes.
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

// Registry sign-in and checkpoints: the SDK's registry credentials come from Bayplan (Resolver.AuthFor), never
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
