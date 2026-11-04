// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ChevronDown, ChevronRight, Ellipsis, TriangleAlert } from "lucide-react";
import { Fragment, useRef, useState } from "react";

import type { HealthState } from "@/api/environments";
import type { ContainerRow } from "@/api/resources";
import type { ServiceItem } from "@/api/services";
import { type StackDetail, type StackService, stackServicesQuery } from "@/api/stacks";
import { StatusDot } from "@/components/status-dot";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ContainerStateCell } from "@/features/containers/container-state-cell";
import { SettingsCard } from "@/features/environments/settings-card";
import { TruncatedText } from "@/features/resources/resource-table";
import { ServiceStateCell } from "@/features/services/service-state-cell";
import { DigestChip } from "@/features/stacks/digest-chip";
import { IgnoreUpdatesDialog } from "@/features/updates/ignore-updates-dialog";
import { UpdateBadge } from "@/features/updates/update-badge";
import { UpdateMenuItems } from "@/features/updates/update-menu-items";
import { useT } from "@/i18n/use-t";

interface StackServicesCardProps {
  envId: string;
  stackId: string;
  name: string;
  target: StackDetail["target"];
  // True while a stack operation runs, which the poll follows more closely.
  busy?: boolean;
}

// Missing values read as a dash, never blank.
const DASH = "—";

// The colour of each roll-up word of a standalone service (UI-SPEC "State cell (service)").
const ROLLUP_TONE: Record<StackService["state"], HealthState> = {
  running: "online",
  degraded: "degraded",
  not_running: "offline",
  stopped: "unknown",
};

// digestOf is the part of a pinned reference after the @, or the image ID of a local image.
function digestOf(row: StackService): string {
  const at = row.pinned.lastIndexOf("@");
  return at >= 0 ? row.pinned.slice(at + 1) : row.pinned;
}

// ImageCell is the Image column: the image as the compose file wrote it, in mono and cut with its
// whole text in a Tooltip, then what the engine pins it to. A pinned image has its digest chip; one
// with no digest yet reads New, because the next deploy looks it up; one from no registry carries
// the warning in words. The reference is Docker's and the file's text, drawn as text.
function ImageCell({ row }: { row: StackService }) {
  const t = useT();
  return (
    <span className="flex min-w-0 items-center gap-2">
      <TruncatedText mono>{row.image || DASH}</TruncatedText>
      {row.pinState === "pinned" ? <DigestChip digest={digestOf(row)} /> : null}
      {row.pinState === "new" ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              type="button"
              className="rounded-full focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            >
              <Badge variant="outline">{t("stackOverview.image.new")}</Badge>
            </button>
          </TooltipTrigger>
          <TooltipContent>{t("stackOverview.image.newTooltip")}</TooltipContent>
        </Tooltip>
      ) : null}
      {row.pinState === "local" ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              type="button"
              className="inline-flex items-center gap-1 rounded-sm text-xs whitespace-nowrap text-status-warning-text focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            >
              <TriangleAlert aria-hidden="true" className="size-3.5" />
              {t("stackOverview.image.local")}
            </button>
          </TooltipTrigger>
          <TooltipContent className="max-w-sm break-all">
            {t("stackOverview.image.localTooltip")} <span className="font-mono">{row.pinned}</span>
          </TooltipContent>
        </Tooltip>
      ) : null}
    </span>
  );
}

// RollupStateCell is the roll-up of a standalone service's containers as a dot and a word.
function RollupStateCell({ state }: { state: StackService["state"] }) {
  const t = useT();
  return <StatusDot state={ROLLUP_TONE[state]} label={t(`stackOverview.rollup.${state}`)} />;
}

function PortsCell({ ports }: { ports: string[] }) {
  if (ports.length === 0) return <span aria-hidden="true">{DASH}</span>;
  return <TruncatedText mono>{ports.join(", ")}</TruncatedText>;
}

// ServiceUpdateMenu is the row menu of a swarm service of the stack: the update items of the lists
// (skip, ignore, check), for the service the row is. The ignore dialog is the menu's sibling, since
// the menu's content is gone once an item is chosen.
function ServiceUpdateMenu({ envId, row }: { envId: string; row: StackService }) {
  const t = useT();
  const [ignoring, setIgnoring] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  const chose = useRef(false);
  const update = row.update;
  if (!update) return null;
  const workload = { kind: "service", name: row.name } as const;
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            ref={button}
            type="button"
            variant="ghost"
            size="icon"
            className="max-md:size-11"
            aria-label={t("actions.menu.label", { name: row.short || row.name })}
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
          <UpdateMenuItems
            envId={envId}
            workload={workload}
            update={update}
            onChosen={() => {
              chose.current = true;
            }}
            onIgnore={() => setIgnoring(true)}
          />
        </DropdownMenuContent>
      </DropdownMenu>
      {ignoring ? (
        <IgnoreUpdatesDialog
          envId={envId}
          workload={workload}
          update={update}
          open
          onOpenChange={(isOpen) => {
            if (!isOpen) setIgnoring(false);
          }}
          onClosed={() => button.current?.focus()}
        />
      ) : null}
    </>
  );
}

interface RowProps {
  envId: string;
  row: StackService;
  swarm: boolean;
  open: boolean;
  onToggle: () => void;
}

// ServiceRows is a service and, once opened on a standalone host, its containers beneath it.
function ServiceRows({ envId, row, swarm, open, onToggle }: RowProps) {
  const t = useT();
  const panel = `containers-${row.name}`;
  return (
    <Fragment>
      <TableRow>
        <TableCell className="max-w-64 min-w-48 align-middle">
          {swarm ? (
            <Link
              to="/env/$envId/services/$serviceId"
              params={{ envId, serviceId: row.id }}
              className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            >
              <TruncatedText>{row.short || row.name}</TruncatedText>
            </Link>
          ) : (
            <span className="flex min-w-0 items-center gap-1">
              <Button
                type="button"
                variant="ghost"
                size="icon-sm"
                className="max-md:size-11"
                aria-expanded={open}
                aria-controls={open ? panel : undefined}
                aria-label={t(
                  open
                    ? "stackOverview.services.hideContainers"
                    : "stackOverview.services.showContainers",
                  {
                    service: row.name,
                  },
                )}
                onClick={onToggle}
              >
                {open ? <ChevronDown aria-hidden="true" /> : <ChevronRight aria-hidden="true" />}
              </Button>
              <TruncatedText>{row.name}</TruncatedText>
            </span>
          )}
        </TableCell>
        <TableCell className="whitespace-nowrap">
          {swarm ? (
            <ServiceStateCell
              service={{
                mode: row.mode as ServiceItem["mode"],
                running: row.running,
                desired: row.desired,
                updateState: row.updateState,
              }}
            />
          ) : (
            <RollupStateCell state={row.state} />
          )}
        </TableCell>
        <TableCell className="whitespace-nowrap">
          {row.update ? (
            <UpdateBadge info={row.update} />
          ) : (
            <span className="text-muted-foreground">{DASH}</span>
          )}
        </TableCell>
        <TableCell className="whitespace-nowrap tabular-nums">
          {row.running}/{row.desired}
        </TableCell>
        <TableCell className="max-w-96 min-w-48">
          <ImageCell row={row} />
        </TableCell>
        <TableCell className="max-w-48">
          <PortsCell ports={row.ports ?? []} />
        </TableCell>
        {swarm ? (
          <TableCell className="w-11 px-1 text-end">
            <ServiceUpdateMenu envId={envId} row={row} />
          </TableCell>
        ) : null}
      </TableRow>
      {!swarm && open
        ? (row.containers ?? []).map((c, i) => (
            <TableRow key={c.id} id={i === 0 ? panel : undefined} className="bg-muted/30">
              <TableCell className="max-w-64 ps-10">
                <Link
                  to="/env/$envId/containers/$containerId"
                  params={{ envId, containerId: c.id }}
                  className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                >
                  <TruncatedText mono>{c.name}</TruncatedText>
                </Link>
              </TableCell>
              <TableCell className="whitespace-nowrap">
                <ContainerStateCell
                  state={c.state as ContainerRow["state"]}
                  health={c.health as ContainerRow["health"]}
                />
              </TableCell>
              <TableCell aria-hidden="true" />
              <TableCell aria-hidden="true">{DASH}</TableCell>
              <TableCell aria-hidden="true" />
              <TableCell aria-hidden="true" />
            </TableRow>
          ))
        : null}
    </Fragment>
  );
}

interface ServicesTableProps {
  envId: string;
  rows: StackService[];
  // True for a swarm, whose services link to their page; a standalone host opens each to its containers.
  swarm: boolean;
}

// StackServicesTable is the table of services of a stack: each with its state, its replicas or containers, its
// image with the pin and its ports. A swarm service links to its page; a standalone service opens to its
// containers, which link to theirs. It is the table of a managed stack's Overview and of an unmanaged stack's.
export function StackServicesTable({ envId, rows, swarm }: ServicesTableProps) {
  const t = useT();
  const [opened, setOpened] = useState<ReadonlySet<string>>(new Set());

  function toggle(service: string) {
    setOpened((prev) => {
      const next = new Set(prev);
      if (!next.delete(service)) next.add(service);
      return next;
    });
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead scope="col">{t("stackOverview.services.columns.service")}</TableHead>
          <TableHead scope="col">{t("stackOverview.services.columns.state")}</TableHead>
          <TableHead scope="col">{t("updates.list.column")}</TableHead>
          <TableHead scope="col">
            {t(
              swarm
                ? "stackOverview.services.columns.replicas"
                : "stackOverview.services.columns.containers",
            )}
          </TableHead>
          <TableHead scope="col">{t("stackOverview.services.columns.image")}</TableHead>
          <TableHead scope="col">{t("stackOverview.services.columns.ports")}</TableHead>
          {swarm ? (
            <TableHead scope="col" className="w-11">
              <span className="sr-only">{t("actions.column")}</span>
            </TableHead>
          ) : null}
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => (
          <ServiceRows
            key={row.name}
            envId={envId}
            row={row}
            swarm={swarm}
            open={opened.has(row.name)}
            onToggle={() => toggle(row.name)}
          />
        ))}
      </TableBody>
    </Table>
  );
}

// StackServicesCard is the Services card of the Overview (UI-SPEC "Stack page"): the services table, or a line
// saying nothing runs. A read that fails says so in the card with its own retry and keeps the rows it had, so the
// Details card beside it is never blocked by Docker.
export function StackServicesCard({ envId, stackId, name, target, busy }: StackServicesCardProps) {
  const t = useT();
  const query = useQuery(stackServicesQuery(envId, stackId, { target, busy }));
  const swarm = target === "swarm";
  const rows = query.data?.items ?? [];

  return (
    <SettingsCard title={t("stackOverview.services.title")}>
      {query.isError ? (
        <div
          role="alert"
          className="flex flex-wrap items-center gap-3 text-sm text-status-offline-text"
        >
          <span>{t("stackOverview.services.loadFailed", { name })}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("stackOverview.services.retry")}
          </Button>
        </div>
      ) : null}
      {query.isPending ? (
        <div className="flex flex-col gap-2" aria-hidden="true">
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-full" />
        </div>
      ) : query.data ? (
        rows.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("stackOverview.services.empty")}</p>
        ) : (
          <StackServicesTable envId={envId} rows={rows} swarm={swarm} />
        )
      ) : null}
    </SettingsCard>
  );
}
