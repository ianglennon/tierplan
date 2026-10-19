// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ChevronRight } from "lucide-react";
import { type ReactNode, useState } from "react";

import type { HealthState } from "@/api/environments";
import type { StackJobItem } from "@/api/stacks";
import { StatusDot } from "@/components/status-dot";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { useT } from "@/i18n/use-t";

// The dot of a service row (UI-SPEC Color): neutral while it waits, the pulsing warning while it
// works, online when it runs, offline when it failed, neutral again for skipped and cancelled. The
// word beside the dot says the same, so colour is never alone.
const TONE: Record<StackJobItem["state"], HealthState> = {
  waiting: "pending",
  running: "reconnecting",
  done: "online",
  failed: "offline",
  skipped: "unknown",
  cancelled: "unknown",
};

interface DeployServiceRowProps {
  item: StackJobItem;
  // What the row can open: why it failed, and later its tasks. A row with none has no chevron.
  children?: ReactNode;
  // Whether the row starts open. The row of a failed service does.
  defaultOpen?: boolean;
}

// DeployServiceRow is one service of the deploy: the dot and the state in words, the service's name
// in mono (long names wrap, never overflow), and a chevron when there is something under it. The
// name and everything under the row are text.
export function DeployServiceRow({ item, children, defaultOpen = false }: DeployServiceRowProps) {
  const t = useT();
  const [open, setOpen] = useState(defaultOpen);
  const head = (
    <span className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
      <StatusDot state={TONE[item.state]} label={t(`stackDeploy.service.state.${item.state}`)} />
      <span className="min-w-0 font-mono text-sm break-all">{item.label}</span>
    </span>
  );
  if (!children) {
    return (
      <li data-slot="deploy-service-row" className="flex flex-col gap-1 py-1.5">
        {head}
      </li>
    );
  }
  return (
    <li data-slot="deploy-service-row" className="py-1.5">
      <Collapsible open={open} onOpenChange={setOpen} className="flex flex-col gap-1">
        <CollapsibleTrigger className="flex min-h-6 w-full items-center gap-2 rounded-sm text-left focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11">
          <ChevronRight
            aria-hidden="true"
            className="size-4 shrink-0 transition-transform data-[open=true]:rotate-90 motion-reduce:transition-none"
            data-open={open}
          />
          {head}
          <span className="sr-only">{t("stackDeploy.service.details", { name: item.label })}</span>
        </CollapsibleTrigger>
        <CollapsibleContent className="ps-6">{children}</CollapsibleContent>
      </Collapsible>
    </li>
  );
}
