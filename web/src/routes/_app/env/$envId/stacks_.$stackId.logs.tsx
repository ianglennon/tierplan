// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { type LogViewParams, stackLogSearchSchema } from "@/features/logs/log-prefs";
import { StackLogs } from "@/features/stacks/stack-logs";

// The Logs tab of a stack: every service or container of it in one merged, prefixed log, live (plan
// 07-29, STK-10, D-22). The page frame owns the gone state; this draws the viewer for the stack the
// URL names. The range, the search, the toggles, the task prefixes and the services, tasks, nodes and
// containers the log is narrowed to live in the address, so a link and a reload open the same view;
// changing one replaces the address rather than adding a history entry per keystroke.
function StackLogsRoute() {
  const { envId, stackId } = Route.useParams();
  const { since, q, ts, utc, wrap, prefix, service, task, node, container, stopped } =
    Route.useSearch();
  const navigate = Route.useNavigate();
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
    <StackLogs
      envId={envId}
      stackId={stackId}
      params={{ since, q, ts, utc, wrap, prefix, service, task, node, container, stopped }}
      onParamsChange={onParamsChange}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId/logs")({
  validateSearch: stackLogSearchSchema,
  component: StackLogsRoute,
});
