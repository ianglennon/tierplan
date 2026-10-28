// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ChevronRight, Info } from "lucide-react";
import { useId, useState } from "react";

import type { StackReviewService } from "@/api/stacks";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { SpecDiff } from "@/features/spec-editor/spec-diff";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface OwnershipOnlyGroupProps {
  // The services of the review that differ from what runs in nothing but Bayplan's ownership labels.
  services: StackReviewService[];
  // "standalone" words the group for the containers of a host and not the services of a swarm.
  context?: "standalone";
}

// tasksOf is the task count of a replicated service as the review's masked spec states it, or undefined for a
// service the count does not follow from (a global one).
function tasksOf(spec: unknown): number | undefined {
  const replicas = (spec as { Mode?: { Replicated?: { Replicas?: unknown } } } | null | undefined)
    ?.Mode?.Replicated?.Replicas;
  return typeof replicas === "number" ? replicas : undefined;
}

// A service's name is drawn in full when it fits and cut with a tooltip when it does not (UI-SPEC "long text").
function ServiceName({ name }: { name: string }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span data-slot="service-name" className="min-w-0 truncate font-mono text-sm font-medium">
          {name}
        </span>
      </TooltipTrigger>
      <TooltipContent className="font-mono break-all">{name}</TooltipContent>
    </Tooltip>
  );
}

// OwnershipRow is one service of the group: its name, the muted word "labels" and its task count; it expands to the
// difference, which is the label rows and nothing else, captioned "Labels on {service}". A container of a standalone
// host has no label rows to show (Bayplan's labels change its configuration hash, not a setting) and no task count, so
// its row says what the recreate is for instead of a diff with nothing in it.
function OwnershipRow({
  service,
  context,
}: {
  service: StackReviewService;
  context?: "standalone";
}) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const tasks = tasksOf(service.current);
  return (
    <li className="rounded-lg ring-1 ring-foreground/10">
      <Collapsible open={open} onOpenChange={setOpen}>
        <CollapsibleTrigger asChild>
          <button
            type="button"
            className="flex w-full min-w-0 items-center gap-x-3 rounded-lg px-3 py-2 text-start text-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11"
          >
            <ChevronRight
              aria-hidden="true"
              className={cn("size-4 shrink-0 transition-transform", open && "rotate-90")}
            />
            <ServiceName name={service.short} />
            <span className="ms-auto flex shrink-0 items-center gap-x-3 text-xs text-muted-foreground">
              <span>{t("stackOwnership.group.labels")}</span>
              {tasks === undefined ? null : (
                <span>{t("stackOwnership.group.tasks", { count: tasks })}</span>
              )}
            </span>
          </button>
        </CollapsibleTrigger>
        <CollapsibleContent className="border-t p-3">
          {context === "standalone" && service.settings === 0 ? (
            <p className="text-sm text-muted-foreground">
              {t("stackOwnership.group.standaloneNote")}
            </p>
          ) : (
            <SpecDiff
              name={service.short}
              current={service.current}
              next={service.next}
              caption={t("stackOwnership.group.caption", { service: service.short })}
            />
          )}
        </CollapsibleContent>
      </Collapsible>
    </li>
  );
}

// OwnershipOnlyGroup is the collapsible card "Ownership labels only" above the changed services in the Changes tab
// (UI-SPEC "Review additions", D-16): the services the first deploy of an adopted stack recreates to put Bayplan's
// labels on, and changes in nothing else. It is closed by default, a headed region, and draws nothing when no service
// is ownership-only. A service with a real change as well is not in it: it is in the normal list.
export function OwnershipOnlyGroup({ services, context }: OwnershipOnlyGroupProps) {
  const t = useT();
  const titleId = useId();
  const [open, setOpen] = useState(false);
  if (services.length === 0) return null;
  const options = { count: services.length, ...(context ? { context } : {}) };
  return (
    <section aria-labelledby={titleId}>
      <Card className="gap-3 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]">
        <Collapsible open={open} onOpenChange={setOpen}>
          <CardHeader>
            <h4 id={titleId} className="text-base font-semibold">
              <CollapsibleTrigger className="flex items-center gap-2 rounded-sm text-start focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11">
                <ChevronRight
                  aria-hidden="true"
                  className={cn("size-4 shrink-0 transition-transform", open && "rotate-90")}
                />
                <Info aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />
                {t("stackOwnership.group.title")}
              </CollapsibleTrigger>
            </h4>
            <p className="text-sm text-muted-foreground">
              {t("stackOwnership.group.sub", options)}
            </p>
          </CardHeader>
          <CollapsibleContent>
            <CardContent>
              <ul className="flex flex-col gap-2 pt-3">
                {services.map((s) => (
                  <OwnershipRow key={s.name} service={s} context={context} />
                ))}
              </ul>
            </CardContent>
          </CollapsibleContent>
        </Collapsible>
      </Card>
    </section>
  );
}
