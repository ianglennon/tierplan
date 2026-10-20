// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import {
  type StackResources,
  type StackServices,
  stackQuery,
  stackResourcesQuery,
  stackServicesQuery,
} from "@/api/stacks";
import { Skeleton } from "@/components/ui/skeleton";
import type { LogViewParams } from "@/features/logs/log-prefs";
import { isEndedTaskState } from "@/features/logs/log-source";
import { LogViewer } from "@/features/logs/log-viewer";
import {
  type FilterOptions,
  NO_FILTER_OPTIONS,
  TaskFilterBar,
} from "@/features/logs/task-filter-bar";
import { ServiceChips, ServiceFilter } from "@/features/stacks/service-filter";

// A list from the address: comma separated, empty parts dropped.
const listOf = (v: string | undefined): string[] =>
  v
    ? v
        .split(",")
        .map((x) => x.trim())
        .filter(Boolean)
    : [];

// swarmOptions are the tasks and nodes a swarm stack's log can be narrowed to: the stack's tasks as
// the Resources tab reads them, of every service, and the nodes they run on with how many have not
// ended.
function swarmOptions(resources: StackResources | undefined): FilterOptions {
  const items = resources?.tasks?.items;
  if (!items) return NO_FILTER_OPTIONS;
  const tasks = items.map(({ task, service }) => ({
    id: task.id,
    service,
    slot: task.slot,
    node: task.node,
    state: task.state,
    ended: isEndedTaskState(task.dockerState),
  }));
  const running = new Map<string, number>();
  for (const task of tasks) {
    if (task.node) running.set(task.node, (running.get(task.node) ?? 0) + (task.ended ? 0 : 1));
  }
  const nodes = [...running]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, count]) => ({ name, tasks: count }));
  return { loaded: true, tasks, nodes };
}

// containerOptions are the containers a standalone stack's log can be narrowed to, by name, as the
// stack's services list them.
function containerOptions(services: StackServices | undefined): FilterOptions {
  if (!services) return NO_FILTER_OPTIONS;
  const tasks = (services.items ?? []).flatMap((service) =>
    (service.containers ?? []).map((c, i) => ({
      id: c.id,
      service: service.short,
      name: c.name,
      slot: i + 1,
      node: "",
      state: c.state,
      stateText: c.state,
      ended: isEndedTaskState(c.state),
    })),
  );
  return { loaded: true, tasks, nodes: [] };
}

export interface StackLogsProps {
  envId: string;
  stackId: string;
  // The range, search, toggles and filters from the address. The route owns them, so a link and a
  // reload open the same view (UI-SPEC "Routes").
  params: LogViewParams;
  onParamsChange: (params: LogViewParams) => void;
  // This is the pop-out window (it offers no pop-out of its own).
  popout?: boolean;
}

// StackLogs is the Logs tab of a stack (plan 07-29, STK-10, D-22): the log viewer of Phases 4 and 6
// reading every service of the stack, or every container, merged by time, each line starting with
// its prefix coloured by service. The viewer owns the stream, the toolbar, the search and the
// download; this decides what it reads.
export function StackLogs({ envId, stackId, params, onParamsChange, popout }: StackLogsProps) {
  const stack = useQuery(stackQuery(envId, stackId));
  const target = stack.data?.target;
  const services = useQuery({
    ...stackServicesQuery(envId, stackId, { target: target ?? "swarm" }),
    enabled: target !== undefined,
  });
  const swarm = target === "swarm";
  const resources = useQuery({
    ...stackResourcesQuery(envId, stackId, { target: target ?? "swarm" }),
    enabled: swarm,
  });
  const options = useMemo(
    () => (swarm ? swarmOptions(resources.data) : containerOptions(services.data)),
    [swarm, resources.data, services.data],
  );
  // The stack's services by name colour the prefixes the same way on every visit.
  const serviceNames = useMemo(
    () => (services.data?.items ?? []).map((s) => s.short).sort(),
    [services.data],
  );

  if (!stack.data || !target) {
    return (
      <div className="flex flex-col gap-1.5 rounded-lg border bg-card p-3" aria-hidden="true">
        <Skeleton className="h-3 w-3/5" />
        <Skeleton className="h-3 w-4/5" />
        <Skeleton className="h-3 w-2/5" />
      </div>
    );
  }
  const { name } = stack.data;
  const change = (patch: Partial<LogViewParams>) => {
    const next: LogViewParams = { ...params, ...patch };
    for (const k of Object.keys(next) as Array<keyof LogViewParams>) {
      if (next[k] === undefined) delete next[k];
    }
    onParamsChange(next);
  };
  const join = (names: string[]): string | undefined => names.join(",") || undefined;
  const chosenServices = listOf(params.service);
  // The third row of the toolbar: Services, then Tasks and Nodes on a swarm or Containers on a
  // host, the Include stopped switch, and a chip for each thing chosen.
  const filterBar = (
    <TaskFilterBar
      stack
      unit={swarm ? "task" : "container"}
      service={name}
      options={options}
      tasks={listOf(swarm ? params.task : params.container)}
      nodes={listOf(params.node)}
      stopped={params.stopped === true}
      onTasksChange={(ids) => change(swarm ? { task: join(ids) } : { container: join(ids) })}
      onNodesChange={(names) => change({ node: join(names) })}
      onStoppedChange={(on) => change({ stopped: on || undefined })}
      leading={
        <ServiceFilter
          services={services.data?.items ?? []}
          chosen={chosenServices}
          onChange={(names) => change({ service: join(names) })}
        />
      }
      trailing={
        <ServiceChips
          chosen={chosenServices}
          onChange={(names) => change({ service: join(names) })}
        />
      }
    />
  );
  return (
    <LogViewer
      envId={envId}
      source={{ kind: "stack", id: stackId, target }}
      name={name}
      serviceNames={serviceNames}
      filterBar={filterBar}
      params={params}
      onParamsChange={onParamsChange}
      popout={popout}
    />
  );
}
