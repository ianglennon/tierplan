// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { containerDetailQuery } from "@/api/container-detail";
import { type LogViewParams, logSearchSchema } from "@/features/logs/log-prefs";
import { LogViewer } from "@/features/logs/log-viewer";

// The Logs tab of a container: its log, live. The page frame owns the gone state and
// the permission check; this draws the viewer for the container the URL names. The
// range, the search and the toggles live in the address (since, q, ts, utc, wrap), so
// a link, a reload and the pop-out open the same view; changing one replaces the
// address rather than adding a history entry per keystroke (D-22).
function ContainerLogsRoute() {
  const { envId, containerId } = Route.useParams();
  const { node, since, q, ts, utc, wrap } = Route.useSearch();
  const navigate = Route.useNavigate();
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
  // The layout reads the detail too, so this is the cached answer; it names the surface.
  const detail = useQuery(containerDetailQuery(envId, containerId, node));
  return (
    <LogViewer
      envId={envId}
      containerId={containerId}
      nodeId={node}
      name={detail.data?.name ?? containerId}
      params={{ since, q, ts, utc, wrap }}
      onParamsChange={onParamsChange}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/containers_/$containerId/logs")({
  validateSearch: logSearchSchema,
  component: ContainerLogsRoute,
});
