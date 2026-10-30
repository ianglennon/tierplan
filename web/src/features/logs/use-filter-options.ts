// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { nodesQuery } from "@/api/nodes";
import { serviceTasksQuery } from "@/api/services";
import { type FilterOptions, filterOptionsFrom } from "@/features/logs/task-filter-bar";
import { usePermission } from "@/hooks/use-me";

// useServiceFilterOptions are the Tasks and Nodes lists a service's log can be narrowed by
// (plan 06-30, D-33), for the Logs tab and for its pop-out. They come from the service's tasks
// (the Tasks tab's own query, so one read serves both) and, for a caller who may read nodes, the
// swarm's nodes, which add the role badge. A caller without nodes.read is not asked: the read
// would be refused, and a refused read under an environment reads as lost access.
export function useServiceFilterOptions(
  envId: string,
  serviceId: string,
  enabled = true,
): FilterOptions {
  const tasks = useQuery({ ...serviceTasksQuery(envId, serviceId), enabled });
  const mayReadNodes = usePermission("nodes.read", envId);
  const nodes = useQuery({
    ...nodesQuery(envId),
    enabled: enabled && mayReadNodes,
    refetchInterval: false,
  });
  return useMemo(() => filterOptionsFrom(tasks.data, nodes.data), [tasks.data, nodes.data]);
}
