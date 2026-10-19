// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ChevronRight } from "lucide-react";
import { type ReactNode, useEffect, useState } from "react";

import type { HealthState } from "@/api/environments";
import type { StackJobItem } from "@/api/stacks";
import { StatusDot } from "@/components/status-dot";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Progress } from "@/components/ui/progress";
import { taskState } from "@/features/tasks/task-state";
import { useT } from "@/i18n/use-t";

type Translate = (key: string, options?: Record<string, unknown>) => string;

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

// The server's code for a service that did not settle within the timeout.
export const CONVERGE_TIMEOUT = "converge_timeout";

// The phases of a service of a standalone host, from the containers' states (STK-07, D-13): the words are
// the Compose SDK's, shown for display. A phase the console does not know reads as Running once the
// service is done and as the working state before.
const PHASES = ["pulling", "creating", "starting", "running", "healthy"] as const;

function phaseWord(t: Translate, phase: string | undefined, fallback: string): string {
  return (PHASES as readonly string[]).includes(phase ?? "")
    ? t(`stackStandalone.service.phase.${phase}`)
    : fallback;
}

// serviceState is the word beside a service's dot: Waiting, Pulling image or Converging {k}/{n} while
// its tasks come up, Running once they do, Timed out or Failed when it did not, and Created or
// Updated for a service the swarm took that the deploy did not wait for (a job, or a deploy that
// stopped first), which is not known to run.
export function serviceState(t: Translate, item: StackJobItem): string {
  const result = item.result ?? {};
  const standalone = item.unit === "containers";
  switch (item.state) {
    case "running":
      if (standalone) {
        return phaseWord(t, result.phase, t("stackDeploy.service.state.running"));
      }
      if (result.phase === "pulling") return t("stackConverge.pulling");
      if (item.unit === "tasks" && item.total > 0) {
        return t("stackConverge.converging", { running: item.current, total: item.total });
      }
      return t("stackDeploy.service.state.running");
    case "done":
      if (standalone) return phaseWord(t, result.phase, t("stackDeploy.service.state.done"));
      if (result.settled === "false") {
        return t(result.action === "created" ? "stackConverge.created" : "stackConverge.updated");
      }
      return t("stackDeploy.service.state.done");
    case "failed":
      return item.code === CONVERGE_TIMEOUT
        ? t("stackConverge.timedOut")
        : t("stackDeploy.service.state.failed");
    default:
      return t(`stackDeploy.service.state.${item.state}`);
  }
}

interface DeployServiceRowProps {
  item: StackJobItem;
  // The tasks the swarm shows under the service (items of kind task), in the order they appeared.
  tasks?: readonly StackJobItem[];
  // The containers a standalone host shows under the service (items of kind container).
  containers?: readonly StackJobItem[];
  // What else the row can open: why it failed. A row with nothing to open has no chevron.
  children?: ReactNode;
  // Whether the row starts open. The row of a failed service does.
  defaultOpen?: boolean;
}

// ConvergenceBar is the bar under a service that is coming up: running tasks over the tasks it must
// have, with the count beside it. Its movement is not announced; the row's state word says it.
function ConvergenceBar({ item }: { item: StackJobItem }) {
  const t = useT();
  const pct = Math.min(100, Math.round((item.current / item.total) * 100));
  const containers = item.unit === "containers";
  return (
    <div className="flex items-center gap-3 ps-6">
      <Progress
        value={pct}
        aria-label={
          containers
            ? t("stackStandalone.service.progress.label", { service: item.label })
            : t("stackConverge.progress.label", { service: item.label })
        }
        className="max-w-48"
      />
      <span aria-live="off" className="text-xs whitespace-nowrap text-muted-foreground">
        {containers
          ? t("stackStandalone.service.progress.containers", {
              running: item.current,
              count: item.total,
            })
          : t("stackConverge.progress.tasks", { running: item.current, desired: item.total })}
      </span>
    </div>
  );
}

// The dot of a container's state word: the working words pulse, the running ones are online, a failure is
// offline, and anything that is not part of coming up (exited, stopped, removed) is neutral.
function containerTone(word: string): HealthState {
  switch (word) {
    case "waiting":
    case "creating":
    case "created":
    case "starting":
    case "recreating":
    case "stopping":
    case "removing":
      return "reconnecting";
    case "started":
    case "running":
    case "healthy":
    case "recreated":
    case "restarted":
      return "online";
    case "failed":
      return "offline";
    default:
      return "unknown";
  }
}

// ServiceContainers is the table under an expanded service of a standalone host: each container the deploy
// touched, its state in the Compose SDK's last word and, for one that failed, the SDK's words. The name, the
// state and the message are text.
function ServiceContainers({
  service,
  containers,
}: {
  service: StackJobItem;
  containers: readonly StackJobItem[];
}) {
  const t = useT();
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-xs">
        <caption className="sr-only">
          {t("stackStandalone.containers.caption", { service: service.label })}
        </caption>
        <thead>
          <tr className="text-muted-foreground">
            <th scope="col" className="py-1 pe-3 font-normal">
              {t("stackStandalone.containers.container")}
            </th>
            <th scope="col" className="py-1 pe-3 font-normal">
              {t("stackStandalone.containers.state")}
            </th>
            <th scope="col" className="py-1 font-normal">
              {t("stackStandalone.containers.message")}
            </th>
          </tr>
        </thead>
        <tbody>
          {containers.map((c) => {
            const word = c.result?.state ?? "";
            return (
              <tr key={c.id} className="align-top">
                <td className="py-1 pe-3 font-mono break-all">{c.label}</td>
                <td className="py-1 pe-3 whitespace-nowrap">
                  <StatusDot
                    state={containerTone(word)}
                    label={t(`stackStandalone.container.state.${word}`, { defaultValue: word })}
                  />
                </td>
                <td className="py-1 font-mono break-all text-status-offline-text">{c.detail}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// ServiceTasks is the table under an expanded service: each task the swarm wants running, and the
// newest that failed, with its state, the node and the error. The state, the node and the error are
// the swarm's words and are drawn as text.
function ServiceTasks({
  service,
  tasks,
}: {
  service: StackJobItem;
  tasks: readonly StackJobItem[];
}) {
  const t = useT();
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-xs">
        <caption className="sr-only">
          {t("stackConverge.tasks.caption", { service: service.label })}
        </caption>
        <thead>
          <tr className="text-muted-foreground">
            <th scope="col" className="py-1 pe-3 font-normal">
              {t("stackConverge.tasks.task")}
            </th>
            <th scope="col" className="py-1 pe-3 font-normal">
              {t("stackConverge.tasks.state")}
            </th>
            <th scope="col" className="py-1 pe-3 font-normal">
              {t("stackConverge.tasks.node")}
            </th>
            <th scope="col" className="py-1 font-normal">
              {t("stackConverge.tasks.error")}
            </th>
          </tr>
        </thead>
        <tbody>
          {tasks.map((task) => {
            const state = taskState(task.result?.state ?? "");
            return (
              <tr key={task.id} className="align-top">
                <td className="py-1 pe-3 font-mono break-all">{task.label}</td>
                <td className="py-1 pe-3 whitespace-nowrap">
                  <StatusDot state={state.tone} label={t(state.labelKey)} />
                </td>
                <td className="py-1 pe-3 break-all">{task.result?.node ?? ""}</td>
                <td className="py-1 font-mono break-all text-status-offline-text">{task.detail}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// DeployServiceRow is one service of the deploy: the dot and the state in words, the service's name
// in mono (long names wrap, never overflow), the bar of its tasks coming up, and a chevron when there
// is something under it (why it failed, its tasks). The name and everything under the row are text.
export function DeployServiceRow({
  item,
  tasks = [],
  containers = [],
  children,
  defaultOpen = false,
}: DeployServiceRowProps) {
  const t = useT();
  const [open, setOpen] = useState(defaultOpen);
  // A service that fails opens its row, whether it was open to begin with or it failed in front of
  // the person; one they close stays closed.
  const failed = item.state === "failed";
  useEffect(() => {
    if (failed) setOpen(true);
  }, [failed]);
  const head = (
    <span className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
      <StatusDot state={TONE[item.state]} label={serviceState(t, item)} />
      <span className="min-w-0 font-mono text-sm break-all">{item.label}</span>
    </span>
  );
  const bar =
    (item.unit === "tasks" || item.unit === "containers") && item.total > 0 ? (
      <ConvergenceBar item={item} />
    ) : null;
  const content = children || tasks.length > 0 || containers.length > 0;
  if (!content) {
    return (
      <li data-slot="deploy-service-row" className="flex flex-col gap-1 py-1.5">
        {head}
        {bar}
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
        {bar}
        <CollapsibleContent className="flex flex-col gap-2 ps-6">
          {children}
          {tasks.length > 0 ? <ServiceTasks service={item} tasks={tasks} /> : null}
          {containers.length > 0 ? (
            <ServiceContainers service={item} containers={containers} />
          ) : null}
        </CollapsibleContent>
      </Collapsible>
    </li>
  );
}
