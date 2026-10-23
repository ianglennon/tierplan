// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { ScrollText } from "lucide-react";
import { useMemo } from "react";

import {
  isStackGone,
  type StackResources,
  type StackServices,
  stackQuery,
  stackResourcesQuery,
  stackServicesQuery,
} from "@/api/stacks";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
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
import { useT } from "@/i18n/use-t";

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

// NotRunning stands in the place of the log for a stack that has nothing to read: one that was
// stopped, or that was never deployed. It opens no stream (UI-SPEC "Empty states").
function NotRunning({ name, stopped }: { name: string; stopped: boolean }) {
  const t = useT();
  return (
    <Empty className="h-fit border">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <ScrollText aria-hidden="true" />
        </EmptyMedia>
        <EmptyTitle role="heading" aria-level={2} className="break-words">
          {t("stackLogs.notRunning.title", { name })}
        </EmptyTitle>
        <EmptyDescription>
          {t(stopped ? "stackLogs.notRunning.stopped" : "stackLogs.notRunning.notDeployed")}
        </EmptyDescription>
      </EmptyHeader>
    </Empty>
  );
}

// Unreadable stands in the place of the log when the stack itself cannot be read: it is gone, or the
// read failed (the window of a pop-out has no page around it to say so).
function Unreadable({ gone, retry }: { gone: boolean; retry: () => void }) {
  const t = useT();
  return (
    <div
      role="alert"
      className="flex flex-col items-center justify-center gap-2 rounded-lg border bg-card p-6 text-center"
    >
      <p className="font-medium">
        {t(gone ? "stacks.detail.gone.heading" : "logs.loadError.title")}
      </p>
      <p className="text-sm text-muted-foreground">
        {t(gone ? "stacks.detail.gone.body" : "logs.loadError.body")}
      </p>
      {gone ? null : (
        <Button type="button" variant="outline" size="sm" onClick={retry}>
          {t("logs.loadError.retry")}
        </Button>
      )}
    </div>
  );
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

  if (stack.isError && !stack.data) {
    return <Unreadable gone={isStackGone(stack.error)} retry={() => void stack.refetch()} />;
  }
  if (!stack.data || !target) {
    return (
      <div className="flex flex-col gap-1.5 rounded-lg border bg-card p-3" aria-hidden="true">
        <Skeleton className="h-3 w-3/5" />
        <Skeleton className="h-3 w-4/5" />
        <Skeleton className="h-3 w-2/5" />
      </div>
    );
  }
  const { name, state } = stack.data;
  if (state === "stopped" || state === "not_deployed") {
    return <NotRunning name={name} stopped={state === "stopped"} />;
  }
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
