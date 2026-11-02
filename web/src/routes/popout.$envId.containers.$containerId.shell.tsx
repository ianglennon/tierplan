// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { containerDetailQuery } from "@/api/container-detail";
import { NoPermission } from "@/components/full-page-state";
import { PopoutFrame } from "@/components/popout-frame";
import { TooltipProvider } from "@/components/ui/tooltip";
import { rowOf } from "@/features/container-detail/header-actions";
import { ContainerStateCell } from "@/features/containers/container-state-cell";
import { nodeSearchSchema, useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { ShellPanel } from "@/features/terminal/shell-panel";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { requireSession, useSessionExpiry } from "@/lib/require-session";

// A shell in a window of its own (D-32): the same launcher, terminal and toolbar as the
// Shell tab, under PopoutFrame instead of the app's sidebar and header. A window that has
// not opened a shell shows the launcher first; opening one in the tab does not move it
// here, and each window holds its own session. The permission to open a shell is checked
// by the panel and by the server, as in the tab.
function PopoutShellRoute() {
  const t = useT();
  const { envId, containerId } = Route.useParams();
  const { node } = Route.useSearch();
  const allowed = usePermission("containers.read", envId);
  const detail = useQuery({ ...containerDetailQuery(envId, containerId, node), enabled: allowed });
  const isSwarm = useEnvIsSwarm(envId);
  const environment = useEnvName(envId);
  const name = detail.data?.name ?? containerId;
  useDocumentTitle(t("terminal.popout.title", { name }));
  // The window outlives the page it was opened from: a session that ends takes it to
  // sign-in and back.
  useSessionExpiry();

  return (
    <TooltipProvider>
      <PopoutFrame
        title={name}
        state={
          detail.data ? (
            <ContainerStateCell state={detail.data.state} health={detail.data.health} />
          ) : undefined
        }
        environment={environment}
      >
        {allowed ? (
          <ShellPanel
            envId={envId}
            containerId={containerId}
            nodeId={node ?? detail.data?.nodeId}
            name={name}
            node={isSwarm ? (detail.data?.node ?? undefined) : undefined}
            image={detail.data?.image}
            running={detail.data ? detail.data.state === "running" : undefined}
            row={detail.data ? rowOf(detail.data) : undefined}
            popout
          />
        ) : (
          <NoPermission />
        )}
      </PopoutFrame>
    </TooltipProvider>
  );
}

export const Route = createFileRoute("/popout/$envId/containers/$containerId/shell")({
  // Outside the app layout, so the guard it gives the app is asked for here.
  beforeLoad: requireSession,
  validateSearch: nodeSearchSchema,
  component: PopoutShellRoute,
});
