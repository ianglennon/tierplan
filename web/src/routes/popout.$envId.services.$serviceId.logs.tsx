// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { serviceQuery } from "@/api/services";
import { NoPermission } from "@/components/full-page-state";
import { PopoutFrame } from "@/components/popout-frame";
import { TooltipProvider } from "@/components/ui/tooltip";
import { type LogViewParams, serviceLogSearchSchema } from "@/features/logs/log-prefs";
import { LogViewer } from "@/features/logs/log-viewer";
import { useServiceFilterOptions } from "@/features/logs/use-filter-options";
import { useEnvName } from "@/features/resources/use-env-name";
import { ServiceStateCell } from "@/features/services/service-state-cell";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { requireSession, useSessionExpiry } from "@/lib/require-session";

// The logs of a service in a window of their own (plan 06-31, OBS-02, D-35): the same viewer
// and toolbar as the Logs tab, under PopoutFrame instead of the app's sidebar and header. The
// range, the search, the toggles, the task prefixes and the tasks, nodes and stopped switch
// come from the address the Logs tab opened it with, and a change replaces the address, so a
// reload keeps the view.
function PopoutServiceLogsRoute() {
  const t = useT();
  const { envId, serviceId } = Route.useParams();
  const { since, q, ts, utc, wrap, prefix, task, node, stopped } = Route.useSearch();
  const navigate = Route.useNavigate();
  const allowed = usePermission("services.read", envId);
  const detail = useQuery({ ...serviceQuery(envId, serviceId), enabled: allowed });
  const filterOptions = useServiceFilterOptions(envId, serviceId, allowed);
  const environment = useEnvName(envId);
  const name = detail.data?.name ?? serviceId;
  useDocumentTitle(t("logs.popout.title", { name }));
  // The window outlives the page it was opened from: a session that ends takes it to
  // sign-in and back.
  useSessionExpiry();

  const onParamsChange = (next: LogViewParams) =>
    navigate({
      search: {
        since: next.since,
        q: next.q,
        ts: next.ts,
        utc: next.utc,
        wrap: next.wrap,
        prefix: next.prefix,
        task: next.task,
        node: next.node,
        stopped: next.stopped,
      },
      replace: true,
    });

  return (
    <TooltipProvider>
      <PopoutFrame
        title={name}
        state={
          detail.data ? (
            <ServiceStateCell
              service={{
                mode: detail.data.mode,
                running: detail.data.running,
                desired: detail.data.desired,
                updateState: detail.data.updateStatus,
              }}
            />
          ) : undefined
        }
        environment={environment}
      >
        {allowed ? (
          <LogViewer
            envId={envId}
            source={{ kind: "service", id: serviceId }}
            name={name}
            showServiceName
            params={{ since, q, ts, utc, wrap, prefix, task, node, stopped }}
            onParamsChange={onParamsChange}
            filterOptions={filterOptions}
            popout
          />
        ) : (
          <NoPermission />
        )}
      </PopoutFrame>
    </TooltipProvider>
  );
}

export const Route = createFileRoute("/popout/$envId/services/$serviceId/logs")({
  // Outside the app layout, so the guard it gives the app is asked for here.
  beforeLoad: requireSession,
  validateSearch: serviceLogSearchSchema,
  component: PopoutServiceLogsRoute,
});
