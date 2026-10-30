// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { containerDetailQuery } from "@/api/container-detail";
import { NoPermission } from "@/components/full-page-state";
import { PopoutFrame } from "@/components/popout-frame";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ContainerStateCell } from "@/features/containers/container-state-cell";
import { type LogViewParams, logSearchSchema } from "@/features/logs/log-prefs";
import { LogViewer } from "@/features/logs/log-viewer";
import { nodeSearchSchema } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { requireSession, useSessionExpiry } from "@/lib/require-session";

// The logs of a container in a window of their own (D-22): the same viewer and toolbar
// as the Logs tab, under PopoutFrame instead of the app's sidebar and header. The range,
// the search and the toggles come from the address the Logs tab opened it with, and a
// change replaces the address, so a reload keeps the view.
function PopoutLogsRoute() {
  const t = useT();
  const { envId, containerId } = Route.useParams();
  const { node, since, q, ts, utc, wrap } = Route.useSearch();
  const navigate = Route.useNavigate();
  const allowed = usePermission("containers.read", envId);
  const detail = useQuery({ ...containerDetailQuery(envId, containerId, node), enabled: allowed });
  const environment = useEnvName(envId);
  const name = detail.data?.name ?? containerId;
  useDocumentTitle(t("logs.popout.title", { name }));
  // The window outlives the page it was opened from: a session that ends takes it to
  // sign-in and back.
  useSessionExpiry();

  const onParamsChange = (next: LogViewParams) =>
    navigate({
      search: (prev) => ({
        node: prev.node,
        since: next.since,
        q: next.q,
        ts: next.ts,
        utc: next.utc,
        wrap: next.wrap,
      }),
      replace: true,
    });

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
          <LogViewer
            envId={envId}
            containerId={containerId}
            nodeId={node}
            name={name}
            params={{ since, q, ts, utc, wrap }}
            onParamsChange={onParamsChange}
            popout
          />
        ) : (
          <NoPermission />
        )}
      </PopoutFrame>
    </TooltipProvider>
  );
}

export const Route = createFileRoute("/popout/$envId/containers/$containerId/logs")({
  // Outside the app layout, so the guard it gives the app is asked for here.
  beforeLoad: requireSession,
  validateSearch: nodeSearchSchema.merge(logSearchSchema),
  component: PopoutLogsRoute,
});
