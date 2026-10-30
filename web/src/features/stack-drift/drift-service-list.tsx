// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { ChevronRight } from "lucide-react";
import { type ReactNode, useMemo, useState } from "react";

import type { StackDriftService } from "@/api/stack-drift";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { diffSpecs } from "@/features/spec-editor/diff";
import { SpecDiff } from "@/features/spec-editor/spec-diff";
import { DriftCause } from "@/features/stack-drift/drift-cause";
import { HiddenChanges } from "@/features/stack-review/service-diff-list";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// A changed service is open by default when this many services or fewer changed (UI-SPEC "Drift").
const OPEN_BY_DEFAULT = 3;

interface DriftServiceListProps {
  services: StackDriftService[];
  // A standalone host's services are run by containers, which the rows count.
  target?: "swarm" | "standalone";
  // The stack's name, for the sentence about a difference found when the stack was adopted.
  stackName?: string;
}

// The glyph beside a change's word. The word carries the meaning; the glyph is for scanning.
const GLYPH = { changed: "~", missing: "−", extra: "+" } as const;

// A missing service is tinted as gone and an extra one as new, at 8% over the card; a changed one is plain.
const TINT = { changed: "", missing: "bg-status-offline/8", extra: "bg-status-online/8" } as const;

interface RowProps {
  service: StackDriftService;
  defaultOpen: boolean;
  target: "swarm" | "standalone";
  stackName?: string;
}

// scaleOf is how many containers a compose service's spec says run it: its scale, and one when it says none.
function scaleOf(spec: StackDriftService["current"]): number {
  const scale = spec?.scale;
  return typeof scale === "number" && scale > 0 ? scale : 1;
}

// containersOf is how many containers a service of a standalone host drifted by. A drift body reads from what the
// revision deployed (current) to what runs now (next): a missing service is counted by the containers it was deployed
// with, and a changed or an extra one by the containers that run now.
export function containersOf(service: StackDriftService): number {
  return scaleOf(service.change === "missing" ? service.current : service.next);
}

// ServiceRow is one service that drifted: the change in words with its glyph, the name in mono and how many settings
// differ. A changed service expands to the field-level difference between what was deployed and what runs now; a
// missing or an extra one has no spec to compare and is its summary row.
function ServiceRow({ service, defaultOpen, target, stackName }: RowProps) {
  const t = useT();
  const [open, setOpen] = useState(defaultOpen);
  const hidden = service.hiddenChanges ?? [];
  const rows = useMemo(
    () => diffSpecs(service.current, service.next),
    [service.current, service.next],
  );

  const summary: ReactNode = (
    <>
      <span className="flex shrink-0 items-center gap-1 font-medium">
        <span aria-hidden="true" className="font-mono">
          {GLYPH[service.change]}
        </span>
        {t(`stackDrift.list.${service.change}`)}
      </span>
      <span data-slot="service-name" className="font-mono text-sm font-medium break-all">
        {service.short}
      </span>
      {service.change === "changed" ? (
        <span className="text-xs text-muted-foreground">
          {t("stackDrift.list.settings", { count: service.settings })}
        </span>
      ) : null}
      {target === "standalone" ? (
        <span className="text-xs text-muted-foreground">
          {t("stackDrift.list.containers", { count: containersOf(service) })}
        </span>
      ) : null}
    </>
  );
  // Why the service differs, in muted text below its name: what the audit log knows, or that it changed outside Coaming.
  // It is never inside the row's button, because its time is a control of its own.
  const cause = (
    <p data-slot="drift-cause" className="basis-full text-xs text-muted-foreground">
      <DriftCause cause={service.cause} stackName={stackName} />
    </p>
  );

  if (service.change !== "changed") {
    return (
      <li className={cn("rounded-lg ring-1 ring-foreground/10", TINT[service.change])}>
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-sm">
          {summary}
          {cause}
        </div>
      </li>
    );
  }

  return (
    <li className="rounded-lg ring-1 ring-foreground/10">
      <Collapsible open={open} onOpenChange={setOpen}>
        <CollapsibleTrigger asChild>
          <button
            type="button"
            className="flex w-full flex-wrap items-center gap-x-3 gap-y-1 rounded-lg px-3 py-2 text-start text-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11"
          >
            <ChevronRight
              aria-hidden="true"
              className={cn("size-4 shrink-0 transition-transform", open && "rotate-90")}
            />
            {summary}
          </button>
        </CollapsibleTrigger>
        <div className="px-3 pb-2 ps-10 max-md:ps-3">{cause}</div>
        <CollapsibleContent className="flex flex-col gap-3 border-t p-3">
          {rows.length > 0 || hidden.length === 0 ? (
            <SpecDiff
              name={service.short}
              current={service.current}
              next={service.next}
              headings={{
                current: t("stackDrift.list.deployed"),
                next: t("stackDrift.list.running"),
              }}
              caption={t("stackDrift.list.caption", { service: service.short })}
            />
          ) : null}
          {hidden.length > 0 ? <HiddenChanges name={service.short} paths={hidden} /> : null}
        </CollapsibleContent>
      </Collapsible>
    </li>
  );
}

// DriftServiceList is the page's list of the services that drifted (UI-SPEC "Drift"), in the order the server gave:
// changed and missing ones by name, then the extra ones. Each changed service opens to the settings that differ, all
// of them open when three or fewer services changed. On a standalone host each row also counts the containers. Masked values never appear: the server sends them hidden, and a
// change to a hidden one is only a path.
export function DriftServiceList({ services, target = "swarm", stackName }: DriftServiceListProps) {
  const t = useT();
  if (services.length === 0) return null;
  const changed = services.filter((s) => s.change === "changed").length;
  return (
    <ul className="flex flex-col gap-2" aria-label={t("stackDrift.list.label")}>
      {services.map((s) => (
        <ServiceRow
          key={s.name}
          service={s}
          defaultOpen={changed <= OPEN_BY_DEFAULT}
          target={target}
          stackName={stackName}
        />
      ))}
    </ul>
  );
}
