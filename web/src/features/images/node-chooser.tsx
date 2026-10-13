// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useId } from "react";

import { Badge } from "@/components/ui/badge";
import { Checkbox } from "@/components/ui/checkbox";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// A node a person can choose: a row of the pull targets answer, as the chooser needs it.
export interface ChooserNode {
  nodeId: string;
  name: string;
  // manager, worker or standalone.
  role: string;
  // The architecture by the registry's name: amd64, arm64, arm/v7.
  arch: string;
  reachable: boolean;
  readOnly?: boolean;
}

interface NodeChooserProps {
  nodes: ReadonlyArray<ChooserNode>;
  // The IDs of the chosen nodes.
  value: ReadonlyArray<string>;
  onChange: (ids: string[]) => void;
  // A reason a row cannot be chosen, by node ID, from the caller (remove and untag have
  // their own). It wins over the reasons the chooser knows: not reachable and read-only.
  disabledReasons?: Readonly<Record<string, string>>;
  // The accessible name of the list; the pull dialog's own wording by default.
  label?: string;
  // Whether the list is flagged as in error, for "Choose at least one node."
  invalid?: boolean;
  describedBy?: string;
}

// NodeChooser is the list of nodes a pull, a remove or an untag can run on (UI-SPEC "Node
// chooser rows"): a checkbox, the name, outline badges for the role and the architecture,
// and for a node that cannot be chosen "Not reachable" or the reason it cannot, with the
// full sentence in a tooltip and for assistive technology. It scrolls inside 240px and its
// rows are 44px under 768px. The count under it is "{n} of {m} selected".
export function NodeChooser({
  nodes,
  value,
  onChange,
  disabledReasons,
  label,
  invalid,
  describedBy,
}: NodeChooserProps) {
  const t = useT();
  const ids = useId();
  const chosen = new Set(value);

  function toggle(id: string, on: boolean) {
    const next = new Set(chosen);
    if (on) next.add(id);
    else next.delete(id);
    // In the order of the nodes, whatever order they were chosen in.
    onChange(nodes.filter((n) => next.has(n.nodeId)).map((n) => n.nodeId));
  }

  return (
    <div className="flex flex-col gap-2">
      <fieldset
        aria-label={label ?? t("pulls.targets.chooserLabel")}
        aria-invalid={invalid ? true : undefined}
        aria-describedby={describedBy}
        className={cn("m-0 min-w-0 rounded-lg border p-0", invalid && "border-destructive")}
      >
        <ScrollArea className="max-h-60 [&_[data-slot=scroll-area-viewport]]:max-h-60 [&_[data-slot=scroll-area-viewport]>div]:block!">
          <ul className="flex flex-col divide-y">
            {nodes.map((n, i) => {
              const reason =
                disabledReasons?.[n.nodeId] ??
                (!n.reachable
                  ? t("pulls.targets.unreachableReason", { node: n.name })
                  : n.readOnly
                    ? t("pulls.targets.readOnlyReason")
                    : undefined);
              const id = `${ids}-${i}`;
              const row = (
                <label
                  htmlFor={id}
                  className={cn(
                    "flex min-h-11 items-center gap-3 px-3 py-1.5 md:min-h-8",
                    reason ? "cursor-not-allowed text-muted-foreground" : "cursor-pointer",
                  )}
                >
                  <Checkbox
                    id={id}
                    checked={chosen.has(n.nodeId)}
                    disabled={Boolean(reason)}
                    aria-describedby={reason ? `${id}-why` : undefined}
                    onCheckedChange={(v) => toggle(n.nodeId, v === true)}
                  />
                  <span className="min-w-0 flex-1 truncate font-medium text-foreground">
                    {n.name}
                  </span>
                  {n.role === "manager" || n.role === "worker" ? (
                    <Badge variant="outline">
                      {t(n.role === "manager" ? "pulls.targets.manager" : "pulls.targets.worker")}
                    </Badge>
                  ) : null}
                  {n.arch ? <Badge variant="outline">{n.arch}</Badge> : null}
                  {!n.reachable ? (
                    <span className="text-sm text-muted-foreground">
                      {t("pulls.targets.notReachable")}
                    </span>
                  ) : null}
                </label>
              );
              return (
                <li key={n.nodeId}>
                  {reason ? (
                    <Tooltip>
                      <TooltipTrigger asChild>{row}</TooltipTrigger>
                      <TooltipContent>{reason}</TooltipContent>
                    </Tooltip>
                  ) : (
                    row
                  )}
                  {reason ? (
                    <span id={`${id}-why`} className="sr-only">
                      {reason}
                    </span>
                  ) : null}
                </li>
              );
            })}
          </ul>
        </ScrollArea>
      </fieldset>
      <p className="text-sm text-muted-foreground" aria-live="polite">
        {t("pulls.targets.count", { selected: value.length, total: nodes.length })}
      </p>
    </div>
  );
}
