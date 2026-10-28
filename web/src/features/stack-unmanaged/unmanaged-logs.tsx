// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { unmanagedStackQuery, unmanagedStackResourcesQuery } from "@/api/unmanaged-stacks";
import { Skeleton } from "@/components/ui/skeleton";
import type { LogViewParams } from "@/features/logs/log-prefs";
import { unmanagedStackSource } from "@/features/logs/log-source";
import {
  containerOptions,
  StackLogView,
  swarmOptions,
  Unreadable,
} from "@/features/stacks/stack-logs";

interface UnmanagedLogsProps {
  envId: string;
  name: string;
  // The range, search, toggles and filters from the address, owned by the route as a managed stack's are.
  params: LogViewParams;
  onParamsChange: (params: LogViewParams) => void;
}

// UnmanagedLogs is the Logs tab of a stack Bayplan did not make (UI-SPEC "Unmanaged stack page"): the stack log
// viewer over the services found by the label, merged by time, under the same permission as a managed stack's log.
// The stream is opened at the stack's name; the viewer owns the stream, the toolbar, the search and the download.
export function UnmanagedLogs({ envId, name, params, onParamsChange }: UnmanagedLogsProps) {
  const stack = useQuery(unmanagedStackQuery(envId, name));
  const target = stack.data?.target;
  const swarm = target === "swarm";
  const resources = useQuery({
    ...unmanagedStackResourcesQuery(envId, name, { target: target ?? "swarm" }),
    enabled: swarm,
  });
  const services = stack.data?.services;
  const options = useMemo(
    () => (swarm ? swarmOptions(resources.data) : containerOptions({ items: services ?? [] })),
    [swarm, resources.data, services],
  );
  const serviceNames = useMemo(() => (services ?? []).map((s) => s.short).sort(), [services]);

  // A stack that is gone replaces the page, so this only ever meets a read that failed.
  if (stack.isError && !stack.data) {
    return <Unreadable gone={false} retry={() => void stack.refetch()} />;
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
  return (
    <StackLogView
      envId={envId}
      source={unmanagedStackSource(name, target)}
      name={name}
      services={services ?? []}
      options={options}
      serviceNames={serviceNames}
      params={params}
      onParamsChange={onParamsChange}
    />
  );
}
