// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { z } from "zod";

import { environmentsQuery, summaryQuery } from "@/api/environments";
import { type Column, TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";

// The list routes' search: the node the list is narrowed to, so a filtered list
// is a link. The server matches it exactly against the names it reports and
// refuses nothing else about it, and 128 is its own limit; a value that does not
// fit is dropped here rather than sent (T-02-75).
export const nodeSearchSchema = z.object({
  node: z.string().min(1).max(128).optional().catch(undefined),
});

// useEnvIsSwarm says whether the environment's lists are aggregated over nodes
// (D-46). The environment list knows the type once an agent has enrolled; the
// summary, which the environment layout has already read, tells for the local
// socket. Anything else, standalone hosts included, has one node and no Node
// column.
export function useEnvIsSwarm(envId: string): boolean {
  const { data: environments } = useQuery(environmentsQuery);
  const { data: summary } = useQuery(summaryQuery(envId));
  const listed = environments?.items?.find((e) => e.id === envId);
  const type = summary?.environment.type;
  const kind =
    listed?.envType ?? (type ? (type === "standalone" ? "standalone" : "swarm") : undefined);
  return kind === "swarm";
}

// nodeColumn is the Node column of a swarm list: the host name, cut with an
// ellipsis when long and whole in a tooltip. A swarm-wide network says Cluster.
export function useNodeColumn<T extends { node: string }>(): Column<T> {
  const t = useT();
  return {
    id: "node",
    header: t("aggregate.filter.label"),
    cell: (row) => <TruncatedText className="max-w-48">{row.node}</TruncatedText>,
  };
}
