// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Ellipsis, TriangleAlert } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";

import { type ServiceItem, servicesQuery } from "@/api/services";
import { NoPermission } from "@/components/full-page-state";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { type Column, ResourceTable } from "@/features/resources/resource-table";
import { useEnvName } from "@/features/resources/use-env-name";
import { ScaleDialog, type ScaledService } from "@/features/services/scale-dialog";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// How long a row reads "Scaling…" at most. The row goes back to the list's own word when the
// list shows the count that was asked for, or after this long, whichever comes first.
export const SCALING_MS = 30_000;

// The part of an image the list shows: the reference without its digest. The whole value,
// digest included, is in the tooltip.
function shownImage(image: string): string {
  return image.split("@")[0] ?? image;
}

function ImageCell({ image, missing }: { image: string; missing: string }) {
  if (!image) return <span className="text-muted-foreground">{missing}</span>;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="block max-w-64 truncate font-mono text-xs">{shownImage(image)}</span>
      </TooltipTrigger>
      <TooltipContent className="max-w-96 break-all font-mono">{image}</TooltipContent>
    </Tooltip>
  );
}

interface RowActionsProps {
  envId: string;
  service: ServiceItem;
  onScale: (service: ServiceItem) => void;
}

// RowActions is the row menu of the services list. Open is there but off until the service's
// page exists; Scale… is for replicated services only, and a caller below Operator sees it
// disabled with the reason (D-19). The server decides either way.
function RowActions({ envId, service, onScale }: RowActionsProps) {
  const t = useT();
  const needs = useNeeds("services.control", envId);
  const canScale = service.mode === "replicated";
  // Set when an item was chosen, so the menu does not hand focus back to its button before
  // the dialog has opened.
  const chose = useRef(false);
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="max-md:size-11"
          aria-label={t("actions.menu.label", { name: service.name })}
        >
          <Ellipsis aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent
        align="end"
        onCloseAutoFocus={(event) => {
          if (!chose.current) return;
          chose.current = false;
          event.preventDefault();
        }}
      >
        <DropdownMenuItem disabled className="max-md:min-h-11">
          {t("actions.menu.open")}
        </DropdownMenuItem>
        {canScale ? (
          <>
            <DropdownMenuSeparator />
            <NeedsTooltip reason={needs.reason}>
              <DropdownMenuItem
                className="max-md:min-h-11"
                disabled={!needs.allowed && !needs.reason}
                onSelect={() => {
                  chose.current = true;
                  onScale(service);
                }}
              >
                {t("services.list.menu.scale")}
              </DropdownMenuItem>
            </NeedsTooltip>
          </>
        ) : null}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

interface ServiceListProps {
  envId: string;
}

// ServiceList is the Services page of a swarm environment: each service with its mode,
// replicas, image and last update, and the row menu to scale one. It reads the list the
// server serves from a manager; with no manager connected the last rows stay with a notice
// above them (UI-SPEC "Services list"). A row reads "Scaling…" from the moment a scale is
// accepted until the list shows the count that was asked for, or for 30 seconds.
export function ServiceList({ envId }: ServiceListProps) {
  const t = useT();
  const allowed = usePermission("services.read", envId);
  const query = useQuery({ ...servicesQuery(envId), enabled: allowed });
  const envName = useEnvName(envId);
  const [scaling, setScaling] = useState<ServiceItem | null>(null);
  const [open, setOpen] = useState(false);
  // The count each row was last asked to have, by service ID, while it is awaited.
  const [pending, setPending] = useState<ReadonlyMap<string, number>>(new Map());
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>());
  const missing = t("dashboard.time.missing");

  useEffect(() => {
    const live = timers.current;
    return () => {
      for (const timer of live) clearTimeout(timer);
      live.clear();
    };
  }, []);

  const markScaling = useCallback((done: ScaledService[]) => {
    const sent = done.filter((d) => d.changed);
    if (sent.length === 0) return;
    setPending((prev) => {
      const next = new Map(prev);
      for (const d of sent) next.set(d.id, d.replicas);
      return next;
    });
    const timer = setTimeout(() => {
      timers.current.delete(timer);
      setPending((prev) => {
        const next = new Map(prev);
        for (const d of sent) if (next.get(d.id) === d.replicas) next.delete(d.id);
        return next.size === prev.size ? prev : next;
      });
    }, SCALING_MS);
    timers.current.add(timer);
  }, []);

  // A row stops waiting once the list shows the count it was asked for.
  const rows = query.data?.items;
  useEffect(() => {
    if (!rows) return;
    setPending((prev) => {
      let next: Map<string, number> | undefined;
      for (const [id, replicas] of prev) {
        if (rows.find((r) => r.id === id)?.desired === replicas) {
          next ??= new Map(prev);
          next.delete(id);
        }
      }
      return next ?? prev;
    });
  }, [rows]);

  const columns: Column<ServiceItem>[] = [
    {
      id: "name",
      header: t("services.list.columns.name"),
      cell: (s) => (
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="block max-w-64 truncate">{s.name}</span>
          </TooltipTrigger>
          <TooltipContent>{s.name}</TooltipContent>
        </Tooltip>
      ),
    },
    {
      id: "mode",
      header: t("services.list.columns.mode"),
      cell: (s) => t(`services.list.mode.${s.mode}`),
    },
    {
      id: "replicas",
      header: t("services.list.columns.replicas"),
      className: "text-end tabular-nums",
      cell: (s) => {
        if (pending.has(s.id)) {
          return (
            <span data-pending="scale" className="inline-flex justify-end">
              <StatusDot state="reconnecting" label={t("services.list.scaling")} />
            </span>
          );
        }
        // A job runs to completion and has no count to show.
        if (s.mode === "replicated-job" || s.mode === "global-job") return <span>{missing}</span>;
        return <span>{`${s.running}/${s.desired}`}</span>;
      },
    },
    {
      id: "image",
      header: t("services.list.columns.image"),
      cell: (s) => <ImageCell image={s.image} missing={missing} />,
    },
    {
      id: "updated",
      header: t("services.list.columns.updated"),
      cell: (s) => <RelativeTime value={s.updatedAt} />,
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-px px-1 text-end",
      cell: (s) => (
        <RowActions
          envId={envId}
          service={s}
          onScale={(service) => {
            setScaling(service);
            setOpen(true);
          }}
        />
      ),
    },
  ];

  const banner = query.data?.noManager ? (
    <Alert variant="warning">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("services.list.noManager.title")}</AlertTitle>
      <AlertDescription className={cn("text-foreground")}>
        {t("services.list.noManager.body")}
      </AlertDescription>
    </Alert>
  ) : undefined;

  if (!allowed) return <NoPermission />;
  return (
    <>
      <ResourceTable
        resource="services"
        i18nKey="services.list"
        columns={columns}
        query={query}
        envName={envName}
        banner={banner}
        getRowKey={(s) => s.id}
      />
      <ScaleDialog
        envId={envId}
        targets={scaling ? [scaling] : []}
        open={open}
        onOpenChange={setOpen}
        onScaled={markScaling}
      />
    </>
  );
}
