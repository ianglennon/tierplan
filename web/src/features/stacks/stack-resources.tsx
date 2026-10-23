// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import type { ContainerRow } from "@/api/resources";
import {
  type StackResourceContainer,
  type StackResourceNetwork,
  type StackResourceObject,
  type StackResourceTask,
  type StackResourceVolume,
  stackQuery,
  stackResourcesQuery,
} from "@/api/stacks";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
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
import { TaskStateCell } from "@/features/tasks/task-state-cell";
import { useT } from "@/i18n/use-t";

interface StackResourcesProps {
  envId: string;
  stackId: string;
}

// Rows are 36px (UI-SPEC "Shared state and overflow rules").
const ROW = "h-9";
// How many characters of a task's ID the Task column shows.
const SHORT_ID = 12;

interface CardShellProps {
  // The card's kind, the last part of its i18n keys: tasks, containers, networks ...
  kind: string;
  // True when the card's rows are known (they may be none), false while they are on their way or when the
  // card's read failed. A read that failed after a good one leaves the rows it had.
  loaded: boolean;
  // True when the last read of the card failed.
  failed: boolean;
  count: number;
  // A line under the count, as the Tasks card's note on what it shows.
  note?: string;
  // The muted line of a card with no rows.
  empty: string;
  onRetry: () => void;
  retrying: boolean;
  // A line under the table of a card that has been read, as the volumes' nodes that were not reporting.
  after?: ReactNode;
  children: ReactNode;
}

// CardShell is one card of the tab (UI-SPEC "Resources tab"): its title, its count in words, then its table,
// its muted empty line, three skeleton rows while it loads, or the failure with its own Retry. A card never
// hides: a kind with nothing in it says so.
function CardShell({
  kind,
  loaded,
  failed,
  count,
  note,
  empty,
  onRetry,
  retrying,
  after,
  children,
}: CardShellProps) {
  const t = useT();
  const noun = t(`stackResources.kind.${kind}`);
  return (
    <SettingsCard title={t(`stackResources.titles.${kind}`)}>
      {!loaded && !failed ? (
        <div className="flex flex-col gap-2" aria-hidden="true" data-testid={`skeleton-${kind}`}>
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-full" />
          <Skeleton className="h-9 w-full" />
        </div>
      ) : null}
      {failed ? (
        <div
          role="alert"
          className="flex flex-wrap items-center gap-3 text-sm text-status-offline-text"
        >
          <span>{t("stackResources.loadFailed", { kind: noun })}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={retrying}
            aria-label={t("stackResources.retryLabel", { kind: noun })}
            onClick={onRetry}
          >
            {t("stackResources.retry")}
          </Button>
        </div>
      ) : null}
      {loaded ? (
        <>
          <p className="text-sm text-muted-foreground">
            {t(`stackResources.count.${kind}`, { count })}
            {note ? ` · ${note}` : ""}
          </p>
          {count === 0 ? <p className="text-sm text-muted-foreground">{empty}</p> : children}
          {after}
        </>
      ) : null}
    </SettingsCard>
  );
}

const linkClass =
  "block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none";

// OwnershipBadge says whether an object is the stack's own or one the compose file only refers to. An
// external object carries the Tooltip that says Bayplan never changes it. Never a badge for what the page
// does not know: only the labels of an object make it the stack's (STK-10).
function OwnershipBadge({ ownership }: { ownership: "stack" | "external" }) {
  const t = useT();
  if (ownership === "stack") {
    return <Badge variant="secondary">{t("stackResources.ownership.stack")}</Badge>;
  }
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          type="button"
          className="rounded-full focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          <Badge variant="outline">{t("stackResources.ownership.external")}</Badge>
        </button>
      </TooltipTrigger>
      <TooltipContent>{t("stackResources.ownership.externalTooltip")}</TooltipContent>
    </Tooltip>
  );
}

// The name a task is called: its service and slot, web.2, or for a global service's task the service alone,
// since the node is in its own column.
function taskName(row: StackResourceTask): string {
  return row.global ? row.service : `${row.service}.${row.task.slot}`;
}

function NodeCell({ envId, task }: { envId: string; task: StackResourceTask["task"] }) {
  const t = useT();
  if (!task.nodeId) return <span aria-hidden="true">{t("stackResources.missing")}</span>;
  if (!task.nodeKnown) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="text-muted-foreground">{t("stackResources.node.removed")}</span>
        </TooltipTrigger>
        <TooltipContent className="break-all">
          {t("stackResources.node.removedTooltip", { id: task.nodeId })}
        </TooltipContent>
      </Tooltip>
    );
  }
  return (
    <Link
      to="/env/$envId/nodes/$nodeId"
      params={{ envId, nodeId: task.nodeId }}
      className={linkClass}
    >
      <TruncatedText>{task.node || task.nodeId}</TruncatedText>
    </Link>
  );
}

function TasksTable({ envId, rows }: { envId: string; rows: readonly StackResourceTask[] }) {
  const t = useT();
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead scope="col">{t("stackResources.columns.task")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.service")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.state")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.node")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.updated")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.error")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => (
          <TableRow key={row.task.id} className={ROW}>
            <TableCell className="max-w-56">
              <span className="flex min-w-0 flex-col">
                <TruncatedText mono>{taskName(row)}</TruncatedText>
                <span className="font-mono text-xs text-muted-foreground">
                  {row.task.id.slice(0, SHORT_ID)}
                </span>
              </span>
            </TableCell>
            <TableCell className="max-w-48">
              <Link
                to="/env/$envId/services/$serviceId"
                params={{ envId, serviceId: row.serviceId }}
                className={linkClass}
              >
                <TruncatedText>{row.service}</TruncatedText>
              </Link>
            </TableCell>
            <TableCell>
              <TaskStateCell task={row.task} />
            </TableCell>
            <TableCell className="max-w-48">
              <NodeCell envId={envId} task={row.task} />
            </TableCell>
            <TableCell className="whitespace-nowrap">
              <RelativeTime value={row.task.updatedAt} />
            </TableCell>
            <TableCell className="max-w-64">
              {row.task.error ? (
                <TruncatedText className="text-status-offline-text">{row.task.error}</TruncatedText>
              ) : (
                <span aria-hidden="true">{t("stackResources.missing")}</span>
              )}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function ContainersTable({
  envId,
  rows,
}: {
  envId: string;
  rows: readonly StackResourceContainer[];
}) {
  const t = useT();
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead scope="col">{t("stackResources.columns.name")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.service")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.state")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.image")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.ports")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.updated")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => (
          <TableRow key={row.id} className={ROW}>
            <TableCell className="max-w-64">
              <Link
                to="/env/$envId/containers/$containerId"
                params={{ envId, containerId: row.id }}
                className={linkClass}
              >
                <TruncatedText mono>{row.name}</TruncatedText>
              </Link>
            </TableCell>
            <TableCell className="max-w-48">
              <TruncatedText>{row.service}</TruncatedText>
            </TableCell>
            <TableCell className="whitespace-nowrap">
              <ContainerStateCell
                state={row.state as ContainerRow["state"]}
                health={row.health as ContainerRow["health"]}
              />
            </TableCell>
            <TableCell className="max-w-72">
              <TruncatedText mono>{row.image}</TruncatedText>
            </TableCell>
            <TableCell className="max-w-48">
              {(row.ports ?? []).length > 0 ? (
                <TruncatedText mono>{(row.ports ?? []).join(", ")}</TruncatedText>
              ) : (
                <span aria-hidden="true">{t("stackResources.missing")}</span>
              )}
            </TableCell>
            <TableCell className="whitespace-nowrap">
              <RelativeTime value={row.created} />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function NetworksTable({ envId, rows }: { envId: string; rows: readonly StackResourceNetwork[] }) {
  const t = useT();
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead scope="col">{t("stackResources.columns.name")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.driver")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.scope")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.ownership")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => (
          <TableRow key={row.id} className={ROW}>
            <TableCell className="max-w-72">
              <Link
                to="/env/$envId/networks/$networkId"
                params={{ envId, networkId: row.id }}
                className={linkClass}
              >
                <TruncatedText mono>{row.name}</TruncatedText>
              </Link>
            </TableCell>
            <TableCell>{row.driver}</TableCell>
            <TableCell>{row.scope}</TableCell>
            <TableCell>
              <OwnershipBadge ownership={row.ownership} />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function VolumesTable({
  envId,
  rows,
  swarm,
}: {
  envId: string;
  rows: readonly StackResourceVolume[];
  swarm: boolean;
}) {
  const t = useT();
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead scope="col">{t("stackResources.columns.name")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.driver")}</TableHead>
          {swarm ? <TableHead scope="col">{t("stackResources.columns.node")}</TableHead> : null}
          <TableHead scope="col">{t("stackResources.columns.ownership")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => (
          <TableRow key={`${row.nodeId}|${row.name}`} className={ROW}>
            <TableCell className="max-w-72">
              <Link
                to="/env/$envId/volumes/$volumeName"
                params={{ envId, volumeName: row.name }}
                search={{ node: row.nodeId }}
                className={linkClass}
              >
                <TruncatedText mono>{row.name}</TruncatedText>
              </Link>
            </TableCell>
            <TableCell>{row.driver}</TableCell>
            {swarm ? (
              <TableCell className="max-w-48">
                <TruncatedText>{row.node}</TruncatedText>
              </TableCell>
            ) : null}
            <TableCell>
              <OwnershipBadge ownership={row.ownership} />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

// UsedBy is the "{n} services" cell of a secret or a config: a button that opens the services of this stack
// that use it, each linking to its page on a swarm. An object nothing uses says so in plain words.
function UsedBy({ envId, row }: { envId: string; row: StackResourceObject }) {
  const t = useT();
  const users = row.usedBy ?? [];
  const label = t("stackResources.objects.usedBy", { count: users.length });
  if (users.length === 0) return <span className="text-muted-foreground">{label}</span>;
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button type="button" variant="link" size="sm" className="h-auto p-0 max-md:h-11">
          {label}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-64">
        <ul
          aria-label={t("stackResources.objects.usersLabel", { name: row.name })}
          className="flex flex-col gap-1"
        >
          {users.map((u) => (
            <li key={`${u.id}|${u.name}`} className="min-w-0">
              {u.id ? (
                <Link
                  to="/env/$envId/services/$serviceId"
                  params={{ envId, serviceId: u.id }}
                  className={linkClass}
                >
                  <TruncatedText>{u.name}</TruncatedText>
                </Link>
              ) : (
                <TruncatedText>{u.name}</TruncatedText>
              )}
            </li>
          ))}
        </ul>
      </PopoverContent>
    </Popover>
  );
}

// ObjectNameCell is the name of a secret or a config as the compose file writes it, in mono, with a muted
// version label when Bayplan hashed it into the object's name. The Tooltip carries the full object name.
function ObjectNameCell({ row }: { row: StackResourceObject }) {
  const t = useT();
  return (
    <span className="flex min-w-0 flex-col">
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="block max-w-64 truncate font-mono text-xs">{row.name}</span>
        </TooltipTrigger>
        <TooltipContent className="break-all">{row.fullName || row.name}</TooltipContent>
      </Tooltip>
      {row.hash ? (
        <span className="text-xs text-muted-foreground">
          {t("stackResources.objects.version", { hash: row.hash })}
        </span>
      ) : null}
    </span>
  );
}

function ObjectsTable({ envId, rows }: { envId: string; rows: readonly StackResourceObject[] }) {
  const t = useT();
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead scope="col">{t("stackResources.columns.name")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.usedBy")}</TableHead>
          <TableHead scope="col">{t("stackResources.columns.ownership")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => (
          <TableRow key={`${row.id}|${row.fullName}`} className={ROW}>
            <TableCell className="max-w-72">
              <ObjectNameCell row={row} />
            </TableCell>
            <TableCell className="whitespace-nowrap">
              <UsedBy envId={envId} row={row} />
            </TableCell>
            <TableCell>
              <OwnershipBadge ownership={row.ownership} />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

// StackResources is the Resources tab (STK-10, UI-SPEC "Resources tab"): everything the stack owns on its
// environment as full-width cards in a column, the Tasks card of a swarm or the Containers card of a
// standalone host first, then the Networks card. Each card has its own count, empty line, loading skeleton
// and failure, so one read that fails never hides the others. A swarm reads again every 5 s while the page
// is visible. Every name is Docker's own text, drawn as text.
export function StackResources({ envId, stackId }: StackResourcesProps) {
  const t = useT();
  const stack = useQuery(stackQuery(envId, stackId)).data;
  const query = useQuery(
    stackResourcesQuery(envId, stackId, {
      target: stack?.target ?? "swarm",
      busy: stack?.lastOperation?.state === "running",
    }),
  );
  const data = query.data;
  const swarm = (stack?.target ?? "swarm") === "swarm";
  // Nothing runs on a stack that is stopped or was never deployed, and the empty line says so.
  const idle = stack?.state === "stopped" || stack?.state === "not_deployed";
  const retry = () => void query.refetch();

  // A card is loaded when the answer has it without an error; it failed when the whole read failed or its
  // own part of the answer carries an error.
  const loadedOf = (error: string | undefined) => data !== undefined && !error;
  const failedOf = (error: string | undefined) => query.isError || Boolean(error);

  const tasks = data?.tasks;
  const containers = data?.containers;
  const volumes = data?.volumes;
  return (
    <div className="flex flex-col gap-6">
      {swarm ? (
        <CardShell
          kind="tasks"
          loaded={loadedOf(tasks?.error)}
          failed={failedOf(tasks?.error)}
          count={tasks?.items?.length ?? 0}
          note={t("stackResources.tasksNote")}
          empty={idle ? t("stackResources.empty.nothingRunning") : t("stackResources.empty.tasks")}
          onRetry={retry}
          retrying={query.isFetching}
        >
          <TasksTable envId={envId} rows={tasks?.items ?? []} />
        </CardShell>
      ) : (
        <CardShell
          kind="containers"
          loaded={loadedOf(containers?.error)}
          failed={failedOf(containers?.error)}
          count={containers?.items?.length ?? 0}
          empty={
            idle ? t("stackResources.empty.nothingRunning") : t("stackResources.empty.containers")
          }
          onRetry={retry}
          retrying={query.isFetching}
        >
          <ContainersTable envId={envId} rows={containers?.items ?? []} />
        </CardShell>
      )}
      <CardShell
        kind="networks"
        loaded={loadedOf(data?.networks.error)}
        failed={failedOf(data?.networks.error)}
        count={data?.networks.items?.length ?? 0}
        empty={t("stackResources.empty.networks")}
        onRetry={retry}
        retrying={query.isFetching}
      >
        <NetworksTable envId={envId} rows={data?.networks.items ?? []} />
      </CardShell>
      <CardShell
        kind="volumes"
        loaded={loadedOf(volumes?.error)}
        failed={failedOf(volumes?.error)}
        count={volumes?.items?.length ?? 0}
        empty={t("stackResources.empty.volumes")}
        onRetry={retry}
        retrying={query.isFetching}
        after={
          (volumes?.notReporting ?? []).length > 0 ? (
            <p className="text-sm text-status-warning-text">
              {t("stackResources.volumes.notReporting", {
                count: volumes?.notReporting?.length ?? 0,
                nodes: (volumes?.notReporting ?? []).join(", "),
              })}
            </p>
          ) : null
        }
      >
        <VolumesTable envId={envId} rows={volumes?.items ?? []} swarm={swarm} />
      </CardShell>
      <CardShell
        kind="secrets"
        loaded={loadedOf(data?.secrets?.error)}
        failed={failedOf(data?.secrets?.error)}
        count={data?.secrets?.items?.length ?? 0}
        empty={t("stackResources.empty.secrets")}
        onRetry={retry}
        retrying={query.isFetching}
      >
        <ObjectsTable envId={envId} rows={data?.secrets?.items ?? []} />
      </CardShell>
      <CardShell
        kind="configs"
        loaded={loadedOf(data?.configs?.error)}
        failed={failedOf(data?.configs?.error)}
        count={data?.configs?.items?.length ?? 0}
        empty={t("stackResources.empty.configs")}
        onRetry={retry}
        retrying={query.isFetching}
      >
        <ObjectsTable envId={envId} rows={data?.configs?.items ?? []} />
      </CardShell>
    </div>
  );
}
