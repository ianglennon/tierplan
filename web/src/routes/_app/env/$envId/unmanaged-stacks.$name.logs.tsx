// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { type LogViewParams, stackLogSearchSchema } from "@/features/logs/log-prefs";
import { UnmanagedLogs } from "@/features/stack-unmanaged/unmanaged-logs";

// The Logs tab of an unmanaged stack: every service or container found by its label in one merged, prefixed log,
// live, under the permission of a managed stack's log. The page frame owns the gone state. The range, the search,
// the toggles, the task prefixes and the services, tasks, nodes and containers the log is narrowed to live in the
// address, so a link and a reload open the same view; changing one replaces the address.
function UnmanagedLogsRoute() {
  const { envId, name } = Route.useParams();
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
    <UnmanagedLogs
      envId={envId}
      name={name}
      params={{ since, q, ts, utc, wrap, prefix, service, task, node, container, stopped }}
      onParamsChange={onParamsChange}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/unmanaged-stacks/$name/logs")({
  validateSearch: stackLogSearchSchema,
  component: UnmanagedLogsRoute,
});
