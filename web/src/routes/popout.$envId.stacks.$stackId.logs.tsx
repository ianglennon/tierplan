// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { stackQuery } from "@/api/stacks";
import { NoPermission } from "@/components/full-page-state";
import { PopoutFrame } from "@/components/popout-frame";
import { TooltipProvider } from "@/components/ui/tooltip";
import { type LogViewParams, stackLogSearchSchema } from "@/features/logs/log-prefs";
import { useEnvName } from "@/features/resources/use-env-name";
import { StackLogs } from "@/features/stacks/stack-logs";
import { StackStateCell } from "@/features/stacks/stack-state-cell";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { requireSession, useSessionExpiry } from "@/lib/require-session";

// The logs of a stack in a window of their own (plan 07-29, STK-10, D-22): the same viewer and
// toolbar as the Logs tab, under PopoutFrame instead of the app's sidebar and header, with the
// stack's name, its state and the environment in the bar. The range, the search, the toggles, the
// prefixes and the services, tasks, nodes and containers it is narrowed to come from the address the
// Logs tab opened it with, and a change replaces the address, so a reload keeps the view.
function PopoutStackLogsRoute() {
  const t = useT();
  const { envId, stackId } = Route.useParams();
  const { since, q, ts, utc, wrap, prefix, service, task, node, container, stopped } =
    Route.useSearch();
  const navigate = Route.useNavigate();
  const allowed = usePermission("stacks.read", envId);
  const stack = useQuery({ ...stackQuery(envId, stackId), enabled: allowed });
  const environment = useEnvName(envId);
  const name = stack.data?.name ?? stackId;
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
        service: next.service,
        task: next.task,
        node: next.node,
        container: next.container,
        stopped: next.stopped,
      },
      replace: true,
    });

  return (
    <TooltipProvider>
      <PopoutFrame
        title={name}
        state={
          stack.data ? (
            <StackStateCell
              state={stack.data.state}
              running={stack.data.servicesRunning}
              total={stack.data.servicesTotal}
              failedService={stack.data.lastOperation?.failedService}
            />
          ) : undefined
        }
        environment={environment}
      >
        {allowed ? (
          <StackLogs
            envId={envId}
            stackId={stackId}
            params={{ since, q, ts, utc, wrap, prefix, service, task, node, container, stopped }}
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

export const Route = createFileRoute("/popout/$envId/stacks/$stackId/logs")({
  // Outside the app layout, so the guard it gives the app is asked for here.
  beforeLoad: requireSession,
  validateSearch: stackLogSearchSchema,
  component: PopoutStackLogsRoute,
});
