// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { ChevronRight, Ellipsis, ListTodo } from "lucide-react";
import { useId, useRef, useState } from "react";

import type { NodeTaskItem } from "@/api/nodes";
import { copyText } from "@/components/copyable-code";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { TruncatedText } from "@/features/resources/resource-table";
import {
  filterFailedOnly,
  isFailedTask,
  type TaskItem,
  type TaskSlot,
  taskState,
} from "@/features/tasks/task-state";
import { TaskStateCell } from "@/features/tasks/task-state-cell";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

// The columns of the slot table: the expander, Task, State, Desired, Node, Updated, Error and
// the row menu.
const COLUMNS = 8;
// Rows are 36px (UI-SPEC "Shared state and overflow rules").
const ROW = "h-9";
// How many characters of a task's ID the Task column shows.
const SHORT_ID = 12;

export interface TaskTableProps {
  envId: string;
  // The service whose tasks these are, to open its Logs tab narrowed to one task. A page
  // that lists tasks of several services (a node's) leaves it out and the action with it.
  serviceId?: string;
  // The name the tasks are shown under: web.2 for slot 2 of the service web. A page that lists
  // tasks of several services leaves it out and shows the slot number alone.
  serviceName?: string;
  slots: readonly TaskSlot[];
  // How many tasks per slot Docker keeps, for the line above the earlier tasks.
  retention: number;
  // True for a global service: its rows are nodes, not slots.
  global: boolean;
  showFailedOnly: boolean;
  // Called with the new value when the switch is used. A page that cannot keep the value
  // leaves it out and the switch with it.
  onShowFailedOnlyChange?: (value: boolean) => void;
  // Called by Scale… in the empty state of a service scaled to 0. Left out when the caller may
  // not scale, and then the button is not drawn.
  onScale?: () => void;
}

// taskName is what a task is called: the service and its slot, web.2, or for a global service's
// task the service alone, since the node is in its own column.
function taskName(serviceName: string | undefined, task: TaskItem, global: boolean): string {
  if (global) return serviceName ?? "";
  return serviceName ? `${serviceName}.${task.slot}` : String(task.slot);
}

// The key of a slot's row: its number, or for a global service its node.
const slotKey = (slot: TaskSlot, global: boolean) =>
  global ? `node:${slot.current.nodeId}` : `slot:${slot.slot}`;

const taskCount = (slots: readonly TaskSlot[]) =>
  slots.reduce((n, s) => n + 1 + (s.history?.length ?? 0), 0);

function NodeCell({ task }: { task: TaskItem }) {
  const t = useT();
  if (!task.nodeId) return <span>{t("tasks.table.missing")}</span>;
  if (!task.nodeKnown) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="text-muted-foreground">{t("tasks.table.removedNode")}</span>
        </TooltipTrigger>
        <TooltipContent className="break-all">
          {t("tasks.table.removedNodeTooltip", { id: task.nodeId })}
        </TooltipContent>
      </Tooltip>
    );
  }
  return <span className="break-all">{task.node || t("tasks.table.missing")}</span>;
}

// errorOf is the text of the Error column: Docker's error, else its status message.
const errorOf = (task: TaskItem) => task.error || task.message;

// ErrorCell is Docker's task error or message. In the table it is cut with the whole text in a
// tooltip; in an expanded row it wraps. A failed or rejected task's text is in the offline
// colour. Always a text node: Docker's text is never markup.
function ErrorCell({ task, wrap }: { task: TaskItem; wrap?: boolean }) {
  const t = useT();
  const text = errorOf(task);
  if (!text) return <span>{t("tasks.table.missing")}</span>;
  const tone = isFailedTask(task) ? "text-status-offline-text" : undefined;
  if (wrap) return <span className={cn("break-all whitespace-normal", tone)}>{text}</span>;
  return <TruncatedText className={cn("max-w-64 truncate", tone)}>{text}</TruncatedText>;
}

function TaskActions({
  envId,
  serviceId,
  task,
  name,
}: {
  envId: string;
  serviceId?: string;
  task: TaskItem;
  name: string;
}) {
  const t = useT();
  const host = useRef<HTMLSpanElement>(null);
  // Why the container cannot be opened, in the words of the disabled-with-reason pattern.
  let reason: string | undefined;
  if (!task.containerId) reason = t("tasks.actions.noContainer");
  else if (!task.targetNodeId) {
    if (!task.nodeKnown) reason = t("tasks.actions.nodeGone");
    else if (task.node) reason = t("tasks.actions.nodeNotReporting", { node: task.node });
    else reason = t("tasks.actions.nodeNotReportingUnnamed");
  }

  async function copyId() {
    if (await copyText(task.id, host.current)) toastSuccess(t("tasks.actions.copied"));
    else toastError(t("tasks.actions.copyFailed"));
  }

  return (
    <span ref={host}>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            className="max-md:size-11"
            aria-label={t("actions.menu.label", { name })}
          >
            <Ellipsis aria-hidden="true" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          {reason ? (
            <DropdownMenuItem disabled className="max-md:min-h-11 flex-col items-start gap-0">
              <span>{t("tasks.actions.open")}</span>
              <span className="text-xs text-muted-foreground">{reason}</span>
            </DropdownMenuItem>
          ) : (
            <DropdownMenuItem asChild className="max-md:min-h-11">
              <Link
                to="/env/$envId/containers/$containerId"
                params={{ envId, containerId: task.containerId }}
                search={{ node: task.targetNodeId }}
              >
                {t("tasks.actions.open")}
              </Link>
            </DropdownMenuItem>
          )}
          {serviceId ? (
            <DropdownMenuItem asChild className="max-md:min-h-11">
              <Link
                to="/env/$envId/services/$serviceId/logs"
                params={{ envId, serviceId }}
                search={{ task: task.id }}
              >
                {t("tasks.actions.logs")}
              </Link>
            </DropdownMenuItem>
          ) : null}
          <DropdownMenuItem className="max-md:min-h-11" onSelect={() => void copyId()}>
            {t("tasks.actions.copyId")}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </span>
  );
}

// TaskCell is the Task column: the name in mono with the short ID beside it on a narrow
// screen and below it on a wide one.
function TaskCell({ name, id }: { name: string; id: string }) {
  return (
    <div className="flex flex-col max-md:flex-row max-md:flex-wrap max-md:items-baseline max-md:gap-x-2">
      {name ? <span className="font-mono text-xs break-all">{name}</span> : null}
      <span className="font-mono text-xs text-muted-foreground">{id.slice(0, SHORT_ID)}</span>
    </div>
  );
}

const desiredLabel = (t: ReturnType<typeof useT>, state: string) => t(taskState(state).labelKey);

interface HistoryProps {
  envId: string;
  serviceId?: string;
  serviceName?: string;
  slot: TaskSlot;
  slotName: string;
  retention: number;
  global: boolean;
}

// History is the earlier tasks of one slot, in a table of its own under the slot's row, on the
// muted surface: the same columns, and the exit code, the start and the finish of each.
function History({
  envId,
  serviceId,
  serviceName,
  slot,
  slotName,
  retention,
  global,
}: HistoryProps) {
  const t = useT();
  const history = slot.history ?? [];
  return (
    <div className="flex flex-col gap-2 py-2 ps-10 pe-2">
      <p className="text-xs text-muted-foreground">
        {t("tasks.table.history.header", { limit: retention })}
      </p>
      {history.length === 0 ? (
        <p className="text-sm">{t("tasks.table.history.empty")}</p>
      ) : (
        <Table aria-label={t("tasks.table.history.label", { slot: slotName })}>
          <TableHeader>
            <TableRow>
              <TableHead>{t("tasks.table.columns.task")}</TableHead>
              <TableHead>{t("tasks.table.columns.state")}</TableHead>
              <TableHead>{t("tasks.table.columns.desired")}</TableHead>
              <TableHead>{t("tasks.table.columns.node")}</TableHead>
              <TableHead>{t("tasks.table.columns.exitCode")}</TableHead>
              <TableHead>{t("tasks.table.columns.started")}</TableHead>
              <TableHead>{t("tasks.table.columns.finished")}</TableHead>
              <TableHead>{t("tasks.table.columns.error")}</TableHead>
              <TableHead>
                <span className="sr-only">{t("tasks.table.columns.actions")}</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {history.map((task) => {
              const name = taskName(serviceName, task, global);
              return (
                <TableRow key={task.id} className={cn(ROW, "align-top")}>
                  <TableCell>
                    <TaskCell name="" id={task.id} />
                  </TableCell>
                  <TableCell>
                    <TaskStateCell task={task} />
                  </TableCell>
                  <TableCell>{desiredLabel(t, task.desiredState)}</TableCell>
                  <TableCell>
                    <NodeCell task={task} />
                  </TableCell>
                  <TableCell>
                    {task.exitCode === null || task.exitCode === undefined
                      ? t("tasks.table.missing")
                      : String(task.exitCode)}
                  </TableCell>
                  <TableCell>
                    <RelativeTime value={task.createdAt} />
                  </TableCell>
                  <TableCell>
                    <RelativeTime value={task.stateSince} />
                  </TableCell>
                  <TableCell className="max-w-96">
                    <ErrorCell task={task} wrap />
                  </TableCell>
                  <TableCell>
                    <TaskActions
                      envId={envId}
                      serviceId={serviceId}
                      task={task}
                      name={name || slotName}
                    />
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}
    </div>
  );
}

function HeaderRow() {
  const t = useT();
  return (
    <TableHeader>
      <TableRow>
        <TableHead className="w-10">
          <span className="sr-only">{t("tasks.table.columns.expand")}</span>
        </TableHead>
        <TableHead>{t("tasks.table.columns.task")}</TableHead>
        <TableHead>{t("tasks.table.columns.state")}</TableHead>
        <TableHead>{t("tasks.table.columns.desired")}</TableHead>
        <TableHead>{t("tasks.table.columns.node")}</TableHead>
        <TableHead>{t("tasks.table.columns.updated")}</TableHead>
        <TableHead>{t("tasks.table.columns.error")}</TableHead>
        <TableHead>
          <span className="sr-only">{t("tasks.table.columns.actions")}</span>
        </TableHead>
      </TableRow>
    </TableHeader>
  );
}

// TasksSkeleton is the table while the tasks load: the header and eight rows at 36px, with
// the words a screen reader needs.
export function TasksSkeleton() {
  const t = useT();
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t("tasks.loading")}
      </span>
      <Table aria-label={t("tasks.table.label")}>
        <HeaderRow />
        <TableBody>
          {Array.from({ length: 8 }, (_, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: placeholder rows have no identity
            <TableRow key={i} className={ROW}>
              <TableCell colSpan={COLUMNS}>
                <Skeleton className="h-4 w-full" />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

// TaskTable lists a service's tasks by slot (UI-SPEC "Tasks tab"): each slot's current task is a
// row, and its earlier tasks, as many as Docker keeps, open under it. Show only failed keeps the
// slots with a failed or rejected task and, expanded, only those tasks. A task on a node that
// is down reads Unknown, never Running (the server decides the state). The node page reuses it.
export function TaskTable({
  envId,
  serviceId,
  serviceName,
  slots,
  retention,
  global,
  showFailedOnly,
  onShowFailedOnlyChange,
  onScale,
}: TaskTableProps) {
  const t = useT();
  const switchId = useId();
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const visible = showFailedOnly ? filterFailedOnly(slots) : slots;

  function toggle(key: string) {
    setOpen((prev) => {
      const next = new Set(prev);
      if (!next.delete(key)) next.add(key);
      return next;
    });
  }

  const shown = taskCount(visible);
  const total = taskCount(slots);
  const tasksText = showFailedOnly
    ? t("tasks.caption.filtered", { shown, count: total })
    : t("tasks.caption.tasks", { count: shown });
  const caption = global
    ? t("tasks.caption.onNodes", {
        tasks: tasksText,
        nodes: t("tasks.caption.nodes", { count: visible.length }),
      })
    : t("tasks.caption.inSlots", {
        tasks: tasksText,
        slots: t("tasks.caption.slots", { count: visible.length }),
      });

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        {onShowFailedOnlyChange ? (
          <div className="flex items-center gap-2 text-sm">
            <Switch
              id={switchId}
              checked={showFailedOnly}
              onCheckedChange={onShowFailedOnlyChange}
              className="max-md:my-2"
            />
            <label htmlFor={switchId}>{t("tasks.failedOnly")}</label>
          </div>
        ) : (
          <span />
        )}
        {slots.length > 0 ? <p className="text-sm text-muted-foreground">{caption}</p> : null}
      </div>
      {visible.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <ListTodo aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle role="heading" aria-level={2}>
              {slots.length === 0 ? t("tasks.empty.none.title") : t("tasks.empty.failed.title")}
            </EmptyTitle>
            <EmptyDescription>
              {slots.length === 0 ? t("tasks.empty.none.body") : t("tasks.empty.failed.body")}
            </EmptyDescription>
          </EmptyHeader>
          {slots.length === 0 && onScale ? (
            <EmptyContent>
              <Button type="button" variant="outline" className="max-md:h-11" onClick={onScale}>
                {t("tasks.empty.none.scale")}
              </Button>
            </EmptyContent>
          ) : null}
        </Empty>
      ) : (
        <Table aria-label={t("tasks.table.label")}>
          <HeaderRow />
          <TableBody>
            {visible.map((slot) => {
              const key = slotKey(slot, global);
              const expanded = open.has(key);
              const task = slot.current;
              const name = taskName(serviceName, task, global);
              const slotName = name || String(slot.slot);
              return (
                <SlotRows
                  key={key}
                  expanded={expanded}
                  row={
                    <TableRow className={ROW}>
                      <TableCell>
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon-sm"
                          className="max-md:size-11"
                          aria-expanded={expanded}
                          aria-label={t("tasks.table.expand", { slot: slotName })}
                          onClick={() => toggle(key)}
                        >
                          <ChevronRight
                            aria-hidden="true"
                            className={cn("transition-transform", expanded && "rotate-90")}
                          />
                        </Button>
                      </TableCell>
                      <TableCell>
                        <TaskCell name={name} id={task.id} />
                      </TableCell>
                      <TableCell>
                        <TaskStateCell task={task} />
                      </TableCell>
                      <TableCell>{desiredLabel(t, task.desiredState)}</TableCell>
                      <TableCell>
                        <NodeCell task={task} />
                      </TableCell>
                      <TableCell>
                        <RelativeTime value={task.updatedAt} />
                      </TableCell>
                      <TableCell>
                        <ErrorCell task={task} />
                      </TableCell>
                      <TableCell>
                        <TaskActions
                          envId={envId}
                          serviceId={serviceId}
                          task={task}
                          name={slotName}
                        />
                      </TableCell>
                    </TableRow>
                  }
                  history={
                    <TableRow className="bg-muted/50 hover:bg-muted/50">
                      <TableCell colSpan={COLUMNS} className="p-0 whitespace-normal">
                        <History
                          envId={envId}
                          serviceId={serviceId}
                          serviceName={serviceName}
                          slot={slot}
                          slotName={slotName}
                          retention={retention}
                          global={global}
                        />
                      </TableCell>
                    </TableRow>
                  }
                />
              );
            })}
          </TableBody>
        </Table>
      )}
    </div>
  );
}

// SlotRows is a slot's row and, when it is open, the row of its earlier tasks under it. A
// fragment so the table's body keeps its rows as direct children.
function SlotRows({
  row,
  history,
  expanded,
}: {
  row: React.ReactNode;
  history: React.ReactNode;
  expanded: boolean;
}) {
  return (
    <>
      {row}
      {expanded ? history : null}
    </>
  );
}

// The columns of the node's task table: Task, Service, State, Desired, Updated, Error and the row
// menu.
const NODE_COLUMNS = 7;

function NodeHeaderRow() {
  const t = useT();
  return (
    <TableHeader>
      <TableRow>
        <TableHead>{t("tasks.table.columns.task")}</TableHead>
        <TableHead>{t("nodes.tasks.columns.service")}</TableHead>
        <TableHead>{t("tasks.table.columns.state")}</TableHead>
        <TableHead>{t("tasks.table.columns.desired")}</TableHead>
        <TableHead>{t("tasks.table.columns.updated")}</TableHead>
        <TableHead>{t("tasks.table.columns.error")}</TableHead>
        <TableHead>
          <span className="sr-only">{t("tasks.table.columns.actions")}</span>
        </TableHead>
      </TableRow>
    </TableHeader>
  );
}

// NodeTasksSkeleton is the node's task table while the tasks load: the header and eight rows at
// 36px, with the words a screen reader needs.
export function NodeTasksSkeleton() {
  const t = useT();
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t("nodes.tasks.loading")}
      </span>
      <Table aria-label={t("tasks.table.label")}>
        <NodeHeaderRow />
        <TableBody>
          {Array.from({ length: 8 }, (_, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: placeholder rows have no identity
            <TableRow key={i} className={ROW}>
              <TableCell colSpan={NODE_COLUMNS}>
                <Skeleton className="h-4 w-full" />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

export interface NodeTaskTableProps {
  envId: string;
  // The tasks the swarm placed on the node: those that should be running, then the newest of the
  // others.
  tasks: readonly NodeTaskItem[];
  // Every task the swarm holds for the node, and whether some are left out of `tasks`.
  total: number;
  capped: boolean;
}

// NodeTaskTable is TaskTable on the node's page (UI-SPEC "Nodes", Tasks tab): the tasks of one
// node, one row each, with the service they belong to as a link in place of the Node column, which
// is the page itself. It has no slot history: a node's tasks are a flat list. Docker's service
// names and error texts are text only.
export function NodeTaskTable({ envId, tasks, total, capped }: NodeTaskTableProps) {
  const t = useT();
  if (tasks.length === 0) {
    return (
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <ListTodo aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("nodes.tasks.empty.title")}
          </EmptyTitle>
          <EmptyDescription>{t("nodes.tasks.empty.body")}</EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  return (
    <div className="flex flex-col gap-3">
      <p className="text-sm text-muted-foreground">
        {t("nodes.tasks.caption", { count: tasks.length })}
      </p>
      <Table aria-label={t("tasks.table.label")}>
        <NodeHeaderRow />
        <TableBody>
          {tasks.map((task) => {
            // A global service's task has no slot: the service names it.
            const name = task.slot > 0 ? `${task.service}.${task.slot}` : task.service;
            return (
              <TableRow key={task.id} className={ROW}>
                <TableCell>
                  <TaskCell name={name} id={task.id} />
                </TableCell>
                <TableCell>
                  {task.serviceId && task.service ? (
                    <Link
                      to="/env/$envId/services/$serviceId"
                      params={{ envId, serviceId: task.serviceId }}
                      className="rounded-sm text-accent-ink break-all hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                    >
                      {task.service}
                    </Link>
                  ) : (
                    <span>{t("tasks.table.missing")}</span>
                  )}
                </TableCell>
                <TableCell>
                  <TaskStateCell task={task} />
                </TableCell>
                <TableCell>{desiredLabel(t, task.desiredState)}</TableCell>
                <TableCell>
                  <RelativeTime value={task.updatedAt} />
                </TableCell>
                <TableCell>
                  <ErrorCell task={task} />
                </TableCell>
                <TableCell>
                  <TaskActions envId={envId} task={task} name={name || task.id} />
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
      {capped ? (
        <p className="text-sm text-muted-foreground">{t("nodes.tasks.capped", { total })}</p>
      ) : null}
    </div>
  );
}
