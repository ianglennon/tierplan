// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { HealthState } from "@/api/environments";
import type { StackService } from "@/api/stacks";
import { StatusDot } from "@/components/status-dot";
import { Chip, FilterItem, FilterPopover } from "@/features/logs/task-filter-bar";
import { useT } from "@/i18n/use-t";

// The Services filter of a stack's log (plan 07-29, STK-10, D-22; UI-SPEC "Logs tab"): which of the
// stack's services the log is narrowed to. It sits first in the third row of the toolbar, a
// popover with a search, the services with their replicas and state, and a footer button that
// clears the choice; what is chosen is also a chip that removes it. The choice is the address's:
// this keeps nothing.

// The colour of each roll-up word of a service, as the Overview draws it.
const ROLLUP_TONE: Record<StackService["state"], HealthState> = {
  running: "online",
  degraded: "degraded",
  not_running: "offline",
  stopped: "unknown",
};

export interface ServiceFilterProps {
  // The stack's services as the engine holds them now.
  services: readonly StackService[];
  // The services chosen, by their name in the compose file.
  chosen: readonly string[];
  onChange: (names: string[]) => void;
}

// ServiceFilter is the Services button and the list it opens.
export function ServiceFilter({ services, chosen, onChange }: ServiceFilterProps) {
  const t = useT();
  const sorted = [...services].sort((a, b) => a.short.localeCompare(b.short));
  const toggle = (short: string) =>
    onChange(chosen.includes(short) ? chosen.filter((c) => c !== short) : [...chosen, short]);
  return (
    <FilterPopover
      label={
        chosen.length > 0
          ? t("stackLogs.filter.servicesCount", { count: chosen.length })
          : t("stackLogs.filter.services")
      }
      listLabel={t("stackLogs.filter.serviceList")}
      search={t("stackLogs.filter.findService")}
      none={t("stackLogs.filter.noServices")}
      empty={sorted.length === 0}
      canClear={chosen.length > 0}
      onClear={() => onChange([])}
      clearLabel={t("stackLogs.filter.clearService")}
    >
      {sorted.map((service) => (
        <FilterItem
          key={service.short}
          value={`${service.short} ${t(`stackOverview.rollup.${service.state}`)}`}
          checked={chosen.includes(service.short)}
          onSelect={() => toggle(service.short)}
        >
          <span className="min-w-0 truncate font-medium">{service.short}</span>
          <span className="ms-auto flex shrink-0 items-center gap-2">
            <span className="text-xs text-muted-foreground">
              {t("stackLogs.filter.replicas", {
                running: service.running,
                desired: service.desired,
              })}
            </span>
            <StatusDot
              state={ROLLUP_TONE[service.state]}
              label={t(`stackOverview.rollup.${service.state}`)}
            />
          </span>
        </FilterItem>
      ))}
    </FilterPopover>
  );
}

// ServiceChips is a removable chip for each service the log is narrowed to.
export function ServiceChips({
  chosen,
  onChange,
}: Pick<ServiceFilterProps, "chosen" | "onChange">) {
  const t = useT();
  return (
    <>
      {chosen.map((service) => (
        <Chip
          key={`service-${service}`}
          text={t("stackLogs.filter.serviceChip", { service })}
          removeLabel={t("stackLogs.filter.removeService", { service })}
          onRemove={() => onChange(chosen.filter((c) => c !== service))}
        />
      ))}
    </>
  );
}
