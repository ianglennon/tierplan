// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { stackQuery, stackServicesQuery } from "@/api/stacks";
import { Skeleton } from "@/components/ui/skeleton";
import type { LogViewParams } from "@/features/logs/log-prefs";
import { LogViewer } from "@/features/logs/log-viewer";

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
  return (
    <LogViewer
      envId={envId}
      source={{ kind: "stack", id: stackId, target }}
      name={name}
      serviceNames={serviceNames}
      params={params}
      onParamsChange={onParamsChange}
      popout={popout}
    />
  );
}
