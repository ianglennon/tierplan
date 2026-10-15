// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { serviceQuery } from "@/api/services";
import { type LogViewParams, serviceLogSearchSchema } from "@/features/logs/log-prefs";
import { LogViewer } from "@/features/logs/log-viewer";
import { useServiceFilterOptions } from "@/features/logs/use-filter-options";

// The Logs tab of a service: the log of all its tasks in one, live (plan 06-29, OBS-02,
// D-35). The page frame owns the gone state and the permission check; this draws the viewer
// for the service the URL names. The range, the search, the toggles, the task prefixes, the
// tasks and nodes the log is narrowed to and the stopped switch live in the address, so a link
// and a reload open the same view; changing one replaces the address rather than adding a
// history entry per keystroke. The Tasks and Nodes lists of the filters come from the service's
// tasks (the Tasks tab's own query, so one read serves both) and, for a caller who may read
// nodes, the swarm's nodes, which add the role badge (plan 06-30, D-33).
function ServiceLogsRoute() {
  const { envId, serviceId } = Route.useParams();
  const { since, q, ts, utc, wrap, prefix, task, node, stopped } = Route.useSearch();
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
        task: next.task,
        node: next.node,
        stopped: next.stopped,
      },
      replace: true,
    });
  // The layout reads the detail too, so this is the cached answer; it names the surface.
  const detail = useQuery(serviceQuery(envId, serviceId));
  const filterOptions = useServiceFilterOptions(envId, serviceId);
  return (
    <LogViewer
      envId={envId}
      source={{ kind: "service", id: serviceId }}
      name={detail.data?.name ?? serviceId}
      params={{ since, q, ts, utc, wrap, prefix, task, node, stopped }}
      onParamsChange={onParamsChange}
      filterOptions={filterOptions}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/services_/$serviceId/logs")({
  validateSearch: serviceLogSearchSchema,
  component: ServiceLogsRoute,
});
