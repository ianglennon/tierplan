// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ChevronDown } from "lucide-react";
import { useRef, useState } from "react";

import type { ServiceDetail, ServiceItem } from "@/api/services";
import { copyText } from "@/components/copyable-code";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { ScaleDialog } from "@/features/services/scale-dialog";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

// itemOf is the service as the list's scale dialog knows it. The fields the dialog does not
// read are left empty.
export function itemOf(d: ServiceDetail): ServiceItem {
  return {
    id: d.id,
    name: d.name,
    mode: d.mode,
    running: d.running,
    desired: d.desired,
    updateState: d.updateStatus,
    image: d.image.ref,
    stack: d.stack,
    agent: d.agent,
    updatedAt: d.updatedAt,
    ports: [],
    updateMessage: d.updateMessage,
    hasPreviousSpec: d.hasPreviousSpec,
    createdAt: d.createdAt,
  };
}

// canScale says whether a service has a replica count to set: a global service runs one task
// per node and a job runs to completion.
export const canScale = (d: Pick<ServiceDetail, "mode">) => d.mode === "replicated";

interface ServiceHeaderActionsProps {
  envId: string;
  detail: ServiceDetail;
}

// ServiceHeaderActions are the buttons in a service page's header: Scale… (not for a global
// service or a job) and More. A caller below Operator sees Scale… unavailable with the reason;
// the server decides either way. Later plans add Edit…, Force update… and Roll back… here.
export function ServiceHeaderActions({ envId, detail }: ServiceHeaderActionsProps) {
  const t = useT();
  const needs = useNeeds("services.control", envId);
  const [scaling, setScaling] = useState(false);
  const host = useRef<HTMLDivElement>(null);

  async function copyId() {
    if (await copyText(detail.id, host.current)) {
      toastSuccess(t("servicedetail.actions.copied"));
    } else {
      toastError(t("servicedetail.actions.copyFailed"));
    }
  }

  return (
    <div ref={host} className="flex flex-wrap items-center gap-2">
      {canScale(detail) ? (
        <NeedsTooltip reason={needs.reason}>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            onClick={needs.allowed ? () => setScaling(true) : undefined}
          >
            {t("servicedetail.actions.scale")}
          </Button>
        </NeedsTooltip>
      ) : null}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            aria-label={t("servicedetail.actions.more")}
          >
            {t("servicedetail.actions.moreText")}
            <ChevronDown aria-hidden="true" data-icon="inline-end" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem className="max-md:min-h-11" onSelect={() => void copyId()}>
            {t("servicedetail.actions.copyId")}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      {needs.allowed && canScale(detail) ? (
        <ScaleDialog
          envId={envId}
          targets={[itemOf(detail)]}
          open={scaling}
          onOpenChange={setScaling}
        />
      ) : null}
    </div>
  );
}
