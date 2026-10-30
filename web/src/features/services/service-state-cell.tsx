// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { ServiceItem } from "@/api/services";
import { StatusDot } from "@/components/status-dot";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { serviceState } from "@/features/services/service-state";
import { useT } from "@/i18n/use-t";

interface ServiceStateCellProps {
  service: Pick<ServiceItem, "mode" | "running" | "desired" | "updateState">;
  // The action the row is waiting on, when it is: a scale. Its pulsing word stands in for
  // the state until the list shows the change.
  pending?: "scale";
}

// ServiceStateCell is the State column: the state's dot and word, with the running and
// desired counts in a tooltip. A row waiting on a scale shows a pulsing warning dot and
// "Scaling…" instead. The words are text nodes, so nothing a service is named can add markup.
export function ServiceStateCell({ service, pending }: ServiceStateCellProps) {
  const t = useT();
  if (pending) {
    return (
      <span className="inline-flex items-center whitespace-nowrap" data-pending={pending}>
        <StatusDot state="reconnecting" label={t("services.list.scaling")} />
      </span>
    );
  }
  const { tone, labelKey } = serviceState(service);
  const dot = <StatusDot state={tone} label={t(labelKey)} />;
  // A job has no replica count to explain.
  if (service.mode === "replicated-job" || service.mode === "global-job") {
    return <span className="inline-flex items-center whitespace-nowrap">{dot}</span>;
  }
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="inline-flex items-center whitespace-nowrap">{dot}</span>
      </TooltipTrigger>
      <TooltipContent>
        {t("services.list.stateTooltip", { running: service.running, desired: service.desired })}
      </TooltipContent>
    </Tooltip>
  );
}
