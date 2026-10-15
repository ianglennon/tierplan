// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useIsMutating } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { ChevronDown } from "lucide-react";
import { useRef, useState } from "react";

import { type ServiceDetail, type ServiceItem, serviceActionKey } from "@/api/services";
import { copyText } from "@/components/copyable-code";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { ForceUpdateDialog } from "@/features/services/force-update-dialog";
import {
  RemoveServiceDialog,
  RemoveServiceMenuItem,
} from "@/features/services/remove-service-dialog";
import { RollbackDialog } from "@/features/services/rollback-dialog";
import { RollbackMenuItem } from "@/features/services/rollback-menu-item";
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

// ServiceHeaderActions are the buttons in a service page's header: Edit…, Scale… (not for a
// global service or a job), Force update… and More, which holds Roll back…, Copy service ID and,
// last, Remove…. A caller below Operator sees the first four unavailable with the reason, and
// Remove… needs Admin; the server decides either way. Scale, force update and roll back stay
// available on a service that belongs to a stack, with the stack's warning in their dialogs, but
// Edit… and Remove… do not: a stack's service is changed through the stack (D-05). Edit… is a
// link to the editor when it is available, and a button that only says why when it is not.
export function ServiceHeaderActions({ envId, detail }: ServiceHeaderActionsProps) {
  const t = useT();
  const needs = useNeeds("services.control", envId);
  const manage = useNeeds("services.manage", envId);
  const navigate = useNavigate();
  // A scale, force update or rollback waiting for the manager's answer: the actions wait too, so
  // a second one cannot start on a service that is changing.
  const pending = useIsMutating({ mutationKey: serviceActionKey(envId) }) > 0;
  const [scaling, setScaling] = useState(false);
  const [forcing, setForcing] = useState(false);
  const [rollingBack, setRollingBack] = useState(false);
  const [removing, setRemoving] = useState(false);
  const host = useRef<HTMLDivElement>(null);
  const editBlocked = !needs.allowed || detail.stack !== "";
  const editReason =
    needs.reason ??
    (detail.stack ? t("services.remove.stackOwned", { stack: detail.stack }) : undefined);

  async function copyId() {
    if (await copyText(detail.id, host.current)) {
      toastSuccess(t("servicedetail.actions.copied"));
    } else {
      toastError(t("servicedetail.actions.copyFailed"));
    }
  }

  return (
    <div ref={host} className="flex flex-wrap items-center gap-2">
      {editBlocked ? (
        <NeedsTooltip reason={editReason}>
          <Button type="button" variant="outline" className="max-md:h-11">
            {t("servicedetail.actions.edit")}
          </Button>
        </NeedsTooltip>
      ) : (
        <Button asChild variant="outline" className="max-md:h-11">
          <Link to="/env/$envId/services/$serviceId/edit" params={{ envId, serviceId: detail.id }}>
            {t("servicedetail.actions.edit")}
          </Link>
        </Button>
      )}
      {canScale(detail) ? (
        <NeedsTooltip reason={needs.reason}>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={pending}
            onClick={needs.allowed ? () => setScaling(true) : undefined}
          >
            {t("servicedetail.actions.scale")}
          </Button>
        </NeedsTooltip>
      ) : null}
      <NeedsTooltip reason={needs.reason}>
        <Button
          type="button"
          variant="outline"
          className="max-md:h-11"
          disabled={pending}
          onClick={needs.allowed ? () => setForcing(true) : undefined}
        >
          {t("servicedetail.actions.forceUpdate")}
        </Button>
      </NeedsTooltip>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={pending}
            aria-label={t("servicedetail.actions.more")}
          >
            {t("servicedetail.actions.moreText")}
            <ChevronDown aria-hidden="true" data-icon="inline-end" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <RollbackMenuItem
            hasPreviousSpec={detail.hasPreviousSpec}
            needs={needs}
            onSelect={() => setRollingBack(true)}
          />
          <DropdownMenuItem className="max-md:min-h-11" onSelect={() => void copyId()}>
            {t("servicedetail.actions.copyId")}
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <RemoveServiceMenuItem
            stack={detail.stack}
            needs={manage}
            onSelect={() => setRemoving(true)}
          />
        </DropdownMenuContent>
      </DropdownMenu>
      {needs.allowed ? (
        <>
          <ForceUpdateDialog
            envId={envId}
            target={itemOf(detail)}
            open={forcing}
            onOpenChange={setForcing}
          />
          <RollbackDialog
            envId={envId}
            target={itemOf(detail)}
            open={rollingBack}
            onOpenChange={setRollingBack}
          />
        </>
      ) : null}
      {manage.allowed && !detail.stack ? (
        <RemoveServiceDialog
          envId={envId}
          target={removing ? itemOf(detail) : null}
          onClose={() => setRemoving(false)}
          onRemoved={() => void navigate({ to: "/env/$envId/services", params: { envId } })}
        />
      ) : null}
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
