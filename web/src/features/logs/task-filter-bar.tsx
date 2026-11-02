// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Check, X } from "lucide-react";
import { type ReactNode, useId } from "react";

import type { NodeList } from "@/api/nodes";
import type { ServiceTasks } from "@/api/services";
import { StatusDot } from "@/components/status-dot";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandEmpty,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { isEndedTaskState } from "@/features/logs/log-source";
import { taskState } from "@/features/tasks/task-state";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The third row of a service's log toolbar (plan 06-30, OBS-02, D-33; UI-SPEC "Filter
// popovers", "Filter chips"): which tasks and which nodes the log is narrowed to, and whether
// it includes the tasks that have stopped.

// One task of the service the filter can pick, current or earlier.
export interface FilterTask {
  id: string;
  slot: number;
  node: string;
  // The state to show; ended says Docker's own state is one the task cannot leave.
  state: string;
  ended: boolean;
  // In a stack's log, which merges several services, the service the task belongs to.
  service?: string;
  // A container of a standalone stack is named by the engine, and its state is the engine's own
  // word, shown as it is.
  name?: string;
  stateText?: string;
}

// One node the service has tasks on: its role, and how many of those tasks have not ended.
export interface FilterNode {
  name: string;
  role?: string;
  tasks: number;
}

// What the filter popovers offer. loaded is false until the service's tasks have been read.
export interface FilterOptions {
  loaded: boolean;
  tasks: FilterTask[];
  nodes: FilterNode[];
}

export const NO_FILTER_OPTIONS: FilterOptions = { loaded: false, tasks: [], nodes: [] };

// filterOptionsFrom turns the service's tasks (slots with their history) and the swarm's nodes
// into what the popovers list. The nodes are the ones the service has tasks on, so a swarm of
// hundreds offers only the few that matter.
export function filterOptionsFrom(tasks?: ServiceTasks, nodes?: NodeList): FilterOptions {
  if (!tasks) return NO_FILTER_OPTIONS;
  const listed: FilterTask[] = [];
  for (const slot of tasks.slots ?? []) {
    for (const item of [slot.current, ...(slot.history ?? [])]) {
      listed.push({
        id: item.id,
        slot: slot.slot,
        node: item.node,
        state: item.state,
        ended: isEndedTaskState(item.dockerState),
      });
    }
  }
  const running = new Map<string, number>();
  for (const task of listed) {
    if (!task.node) continue;
    running.set(task.node, (running.get(task.node) ?? 0) + (task.ended ? 0 : 1));
  }
  const roles = new Map((nodes?.items ?? []).map((n) => [n.hostname, n.role]));
  const offered = [...running]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, count]): FilterNode => {
      const role = roles.get(name);
      return { name, ...(role ? { role } : {}), tasks: count };
    });
  return { loaded: true, tasks: listed, nodes: offered };
}

// The address names a task by its ID or by the start of one, at least eight characters of it.
const MIN_ID_PREFIX = 8;
const idMatches = (chosen: string, id: string): boolean =>
  chosen === id || (chosen.length >= MIN_ID_PREFIX && id.startsWith(chosen));
const shortId = (id: string): string => id.slice(0, MIN_ID_PREFIX);

// A task is named by its service and slot, and a task of a global service, which has none, by
// the service and its node. A container is named by the engine.
const taskLabel = (
  service: string,
  task: Pick<FilterTask, "slot" | "node" | "service" | "name">,
): string => {
  if (task.name) return task.name;
  const of = task.service ?? service;
  return task.slot > 0 ? `${of}.${task.slot}` : `${of}@${task.node}`;
};

export interface TaskFilterBarProps {
  // The service's name, which the task names carry.
  service: string;
  options: FilterOptions;
  // What the address chooses: task IDs (or the starts of them) and node names.
  tasks: readonly string[];
  nodes: readonly string[];
  stopped: boolean;
  onTasksChange: (ids: string[]) => void;
  onNodesChange: (names: string[]) => void;
  onStoppedChange: (on: boolean) => void;
  // A stack's log (plan 07-29, D-22) names what is chosen as containers on a standalone host, which
  // has no nodes to choose, and words the switch Include stopped. Controls to put before the Tasks
  // button (its Services filter) and chips after the others.
  unit?: "task" | "container";
  stack?: boolean;
  leading?: ReactNode;
  trailing?: ReactNode;
}

// TaskFilterBar shows the Tasks and Nodes buttons, each opening a searchable list with check
// marks, the Include stopped tasks switch and a removable chip for everything chosen. The
// choices are the address's: this component keeps nothing.
export function TaskFilterBar(p: TaskFilterBarProps) {
  const t = useT();
  const switchId = useId();
  const { options } = p;
  const container = p.unit === "container";
  // The words of the Tasks button and what hangs from it: a stack on a host chooses containers.
  const words = container
    ? {
        label: "stackLogs.filter.containers",
        count: "stackLogs.filter.containersCount",
        list: "stackLogs.filter.containerList",
        find: "stackLogs.filter.findContainer",
        none: "stackLogs.filter.noContainers",
        chip: "stackLogs.filter.containerChip",
        remove: "stackLogs.filter.removeContainer",
      }
    : {
        label: "logs.filter.tasks",
        count: "logs.filter.tasksCount",
        list: "logs.filter.taskList",
        find: "logs.filter.findTask",
        none: "logs.filter.noTasks",
        chip: "logs.filter.taskChip",
        remove: "logs.filter.removeTask",
      };

  const picked = (task: FilterTask): boolean => p.tasks.some((c) => idMatches(c, task.id));
  const offeredTasks = options.tasks
    .filter((task) => p.stopped || !task.ended || picked(task))
    .sort(
      (a, b) =>
        (a.service ?? "").localeCompare(b.service ?? "") ||
        a.slot - b.slot ||
        Number(a.ended) - Number(b.ended) ||
        a.node.localeCompare(b.node) ||
        a.id.localeCompare(b.id),
    );
  const offeredNodes = options.nodes.filter(
    (n) => p.stopped || n.tasks > 0 || p.nodes.includes(n.name),
  );

  const toggleTask = (task: FilterTask) =>
    p.onTasksChange(
      picked(task)
        ? p.tasks.filter((c) => !idMatches(c, task.id))
        : [...p.tasks.filter((c) => !idMatches(c, task.id)), task.id],
    );
  const toggleNode = (name: string) =>
    p.onNodesChange(
      p.nodes.includes(name) ? p.nodes.filter((n) => n !== name) : [...p.nodes, name],
    );

  return (
    <>
      {p.leading}
      <FilterPopover
        label={p.tasks.length > 0 ? t(words.count, { count: p.tasks.length }) : t(words.label)}
        listLabel={t(words.list)}
        search={t(words.find)}
        none={t(words.none)}
        empty={offeredTasks.length === 0}
        canClear={p.tasks.length > 0}
        onClear={() => p.onTasksChange([])}
      >
        {offeredTasks.map((task) => {
          const state = taskState(task.state);
          return (
            <FilterItem
              key={task.id}
              value={`${taskLabel(p.service, task)} ${task.node} ${task.stateText ?? t(state.labelKey)} ${task.id}`}
              checked={picked(task)}
              onSelect={() => toggleTask(task)}
            >
              <span className="font-medium">{taskLabel(p.service, task)}</span>
              <span className="min-w-0 truncate text-muted-foreground">{task.node}</span>
              <span className="ms-auto flex shrink-0 items-center gap-2">
                {task.stateText !== undefined ? (
                  <span className="text-xs text-muted-foreground">{task.stateText}</span>
                ) : (
                  <StatusDot state={state.tone} label={t(state.labelKey)} />
                )}
                <span className="font-mono text-xs text-muted-foreground">{shortId(task.id)}</span>
              </span>
            </FilterItem>
          );
        })}
      </FilterPopover>

      {container ? null : (
        <FilterPopover
          label={
            p.nodes.length > 0
              ? t("logs.filter.nodesCount", { count: p.nodes.length })
              : t("logs.filter.nodes")
          }
          listLabel={t("logs.filter.nodeList")}
          search={t("logs.filter.findNode")}
          none={t("logs.filter.noNodes")}
          empty={offeredNodes.length === 0}
          canClear={p.nodes.length > 0}
          onClear={() => p.onNodesChange([])}
        >
          {offeredNodes.map((n) => (
            <FilterItem
              key={n.name}
              value={`${n.name} ${n.role ?? ""}`}
              checked={p.nodes.includes(n.name)}
              onSelect={() => toggleNode(n.name)}
            >
              <span className="min-w-0 truncate font-medium">{n.name}</span>
              {n.role ? (
                <span className="shrink-0 rounded-sm border px-1 text-xs text-muted-foreground">
                  {t(`logs.filter.role.${n.role}`)}
                </span>
              ) : null}
              <span className="ms-auto shrink-0 text-xs text-muted-foreground">
                {t("logs.filter.taskCount", { count: n.tasks })}
              </span>
            </FilterItem>
          ))}
        </FilterPopover>
      )}

      <Tooltip>
        <TooltipTrigger asChild>
          <span className="inline-flex items-center gap-2 max-md:min-h-11">
            <Switch id={switchId} checked={p.stopped} onCheckedChange={p.onStoppedChange} />
            <Label htmlFor={switchId} className="text-sm font-normal">
              {t(p.stack ? "stackLogs.filter.stopped" : "logs.filter.stopped")}
            </Label>
          </span>
        </TooltipTrigger>
        <TooltipContent>
          {t(p.stack ? "stackLogs.filter.stoppedTooltip" : "logs.filter.stoppedTooltip")}
        </TooltipContent>
      </Tooltip>

      {p.tasks.map((chosen) => {
        const task = options.tasks.find((x) => idMatches(chosen, x.id));
        const name = task ? taskLabel(p.service, task) : shortId(chosen);
        return (
          <Chip
            key={`task-${chosen}`}
            text={task || !options.loaded ? t(words.chip, { name }) : t("logs.filter.taskNotFound")}
            muted={!task && options.loaded}
            removeLabel={t(words.remove, { name })}
            onRemove={() => p.onTasksChange(p.tasks.filter((c) => c !== chosen))}
          />
        );
      })}
      {p.nodes.map((name) => (
        <Chip
          key={`node-${name}`}
          text={t("logs.filter.nodeChip", { name })}
          removeLabel={t("logs.filter.removeNode", { name })}
          onRemove={() => p.onNodesChange(p.nodes.filter((n) => n !== name))}
        />
      ))}
      {p.trailing}
    </>
  );
}

// One popover button with a searchable list below it and a Clear button at the foot. A stack's
// Services filter words the button for itself.
export function FilterPopover(props: {
  label: string;
  listLabel: string;
  search: string;
  none: string;
  empty: boolean;
  canClear: boolean;
  onClear: () => void;
  clearLabel?: string;
  children: React.ReactNode;
}) {
  const t = useT();
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button type="button" variant="outline" size="sm" className="max-md:h-11">
          {props.label}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-96 max-w-[calc(100vw-2rem)] gap-0 p-0">
        <Command label={props.listLabel} loop vimBindings={false} className="p-0">
          <CommandInput placeholder={props.search} />
          <CommandList label={props.listLabel} className="max-h-72">
            {props.empty ? (
              <p className="px-3 py-3 text-sm text-muted-foreground">{props.none}</p>
            ) : (
              <>
                <CommandEmpty>{t("logs.filter.noMatch")}</CommandEmpty>
                {props.children}
              </>
            )}
          </CommandList>
        </Command>
        <div className="flex justify-end border-t p-1">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            disabled={!props.canClear}
            onClick={props.onClear}
            className="max-md:h-11"
          >
            {props.clearLabel ?? t("logs.filter.clear")}
          </Button>
        </div>
      </PopoverContent>
    </Popover>
  );
}

// One row of a popover list: a check box that is ticked for what is chosen. The row says
// "Selected" to a screen reader as well, since the tick is a picture.
export function FilterItem(props: {
  value: string;
  checked: boolean;
  onSelect: () => void;
  children: React.ReactNode;
}) {
  const t = useT();
  return (
    <CommandItem
      value={props.value}
      data-checked={props.checked ? "true" : undefined}
      aria-checked={props.checked}
      onSelect={props.onSelect}
      className="max-md:min-h-11"
    >
      <span
        aria-hidden="true"
        className={cn(
          "flex size-4 shrink-0 items-center justify-center rounded-sm border",
          props.checked && "border-primary bg-primary text-primary-foreground",
        )}
      >
        {props.checked ? <Check className="size-3" /> : null}
      </span>
      {props.children}
      {props.checked ? <span className="sr-only">{t("logs.filter.selected")}</span> : null}
    </CommandItem>
  );
}

// A removable chip for one thing the log is narrowed to.
export function Chip(props: {
  text: string;
  removeLabel: string;
  muted?: boolean;
  onRemove: () => void;
}) {
  return (
    <span
      data-slot="filter-chip"
      className="inline-flex h-8 max-w-full items-center gap-1 rounded-full border bg-muted ps-2.5 text-xs max-md:h-11"
    >
      <span className={cn("min-w-0 truncate", props.muted && "text-muted-foreground")}>
        {props.text}
      </span>
      <Button
        type="button"
        variant="ghost"
        size="icon-xs"
        aria-label={props.removeLabel}
        onClick={props.onRemove}
        className="rounded-full max-md:size-9"
      >
        <X aria-hidden="true" />
      </Button>
    </span>
  );
}
