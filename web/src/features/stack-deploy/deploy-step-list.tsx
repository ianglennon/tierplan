// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Check, CircleX, Minus } from "lucide-react";

import type { StackJobItem } from "@/api/stacks";
import { Spinner } from "@/components/ui/spinner";
import { CONVERGE_TIMEOUT, DeployServiceRow } from "@/features/stack-deploy/deploy-service-row";
import { useT } from "@/i18n/use-t";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// The server's code for a service whose update the swarm paused or rolled back.
const UPDATE_STOPPED = "update_stopped";

// failureOf is why an item failed, as the console words it: the sentence for the code, and Docker's
// own words under it. Docker's rejection of a service reads as one sentence naming the service. Both
// are text from outside Bayplan and are drawn as text.
export function failureOf(
  t: Translate,
  item: StackJobItem,
): { sentence: string; words: string } | undefined {
  if (item.code === "service_rejected" && item.detail) {
    return {
      sentence: t("stackDeploy.failure.rejected", { service: item.label, message: item.detail }),
      words: "",
    };
  }
  // A service that did not settle: how long it was given, and the swarm's last words about it.
  if (item.code === CONVERGE_TIMEOUT) {
    return {
      sentence: t("stackConverge.timeout.body", {
        service: item.label,
        count: Number(item.result?.minutes) || 0,
      }),
      words: item.detail,
    };
  }
  if (item.code === UPDATE_STOPPED) {
    return {
      sentence: t("stackConverge.stopped.body", { service: item.label }),
      words: item.detail,
    };
  }
  if (!item.message && !item.detail) return undefined;
  return { sentence: item.message, words: item.detail };
}

// FailureText is the reason under a failed step or service, in offline text, wrapped.
export function FailureText({ item }: { item: StackJobItem }) {
  const t = useT();
  const why = failureOf(t, item);
  if (!why) return null;
  return (
    <div className="flex flex-col gap-1 text-sm text-status-offline-text">
      {why.sentence ? <p className="break-all">{why.sentence}</p> : null}
      {why.words ? <p className="font-mono text-xs break-all">{why.words}</p> : null}
    </div>
  );
}

type State = StackJobItem["state"];

function StepMark({ state }: { state: State }) {
  switch (state) {
    case "running":
      return <Spinner aria-hidden="true" />;
    case "done":
      return <Check aria-hidden="true" className="size-4 text-status-online-text" />;
    case "failed":
      return <CircleX aria-hidden="true" className="size-4 text-destructive" />;
    default:
      return <Minus aria-hidden="true" className="size-4 text-muted-foreground" />;
  }
}

export interface DeployStep {
  item: StackJobItem;
  // The service rows of the step (Deploy services has them).
  rows: StackJobItem[];
}

// tasksOf are the task items under a service row, in the order they appeared.
function tasksOf(items: readonly StackJobItem[], row: StackJobItem): StackJobItem[] {
  return items.filter((i) => i.parent === row.id && i.kind === "task");
}

// containersOf are the container items under a service row of a standalone host, in the order they
// appeared.
function containersOf(items: readonly StackJobItem[], row: StackJobItem): StackJobItem[] {
  return items.filter((i) => i.parent === row.id && i.kind === "container");
}

// isStandalone says whether a job is a deploy to a standalone host: only it has a volumes group.
export function isStandalone(items: readonly StackJobItem[]): boolean {
  return items.some((i) => i.id === "volumes");
}

// stepLabel is the words of a step. A standalone deploy's steps are worded here, in the standalone
// variant, from the step they are; every other step reads as the server labelled it.
function stepLabel(t: Translate, item: StackJobItem, standalone: boolean): string {
  if (!standalone) return item.label;
  if (["check", "lookup", "services", "cleanup"].includes(item.id)) {
    return t(`stackStandalone.steps.${item.id}`);
  }
  for (const [prefix, key] of [
    ["network:", "network"],
    ["volume:", "volume"],
    ["remove-container:", "removeContainer"],
  ] as const) {
    if (item.id.startsWith(prefix)) {
      return t(`stackStandalone.steps.${key}`, { name: item.id.slice(prefix.length) });
    }
  }
  return item.label;
}

// stepsOf arranges a job's items into the lines of the step list, in run order. The server adds the
// networks, secrets and configs, the services and the old objects it removes while the deploy runs,
// so the order of the items is the order they were added in, not the order of the steps: a step is
// read from its group or its parent instead. A top-level step is a line, followed by the steps under
// it (an old secret under the cleanup step); a group is not a line but its steps are, in its place; a
// service is a row inside the step it belongs to.
export function stepsOf(items: readonly StackJobItem[]): DeployStep[] {
  const kindOf = (i: StackJobItem): string => i.kind || "step";
  const under = (id: string) => items.filter((i) => i.parent === id);
  const out: DeployStep[] = [];
  for (const top of items.filter((i) => !i.parent)) {
    if (kindOf(top) !== "group") {
      out.push({ item: top, rows: under(top.id).filter((i) => kindOf(i) === "service") });
    }
    for (const child of under(top.id)) {
      if (kindOf(child) === "step") out.push({ item: child, rows: [] });
    }
  }
  return out;
}

interface DeployStepListProps {
  items: readonly StackJobItem[];
}

// DeployStepList is the ordered list of the steps of a deploy (UI-SPEC "Deploy console"): each with a
// mark and its state in words, and the services as rows inside Deploy services. The list is an ol
// and every step exposes its state as text; nothing in it announces itself, the console announces
// outcomes. Every label is text.
export function DeployStepList({ items }: DeployStepListProps) {
  const t = useT();
  const steps = stepsOf(items);
  const standalone = isStandalone(items);
  return (
    <ol aria-label={t("stackDeploy.steps.label")} className="flex flex-col gap-2">
      {steps.map(({ item, rows }) => (
        <li key={item.id} className="flex flex-col gap-1 text-sm">
          <span className="flex items-center gap-2">
            <StepMark state={item.state} />
            <span className="min-w-0 break-words">{stepLabel(t, item, standalone)}</span>
            <span className="text-muted-foreground">
              {item.result?.cleaned === "true"
                ? t("stackDeploy.cleanup.label")
                : t(`stackDeploy.state.${item.state}`)}
            </span>
          </span>
          {item.state === "failed" && rows.every((r) => r.state !== "failed") ? (
            <div className="ms-6">
              <FailureText item={item} />
            </div>
          ) : null}
          {rows.length > 0 ? (
            <ul aria-label={t("stackDeploy.steps.services")} className="ms-6 flex flex-col">
              {rows.map((row) => (
                <DeployServiceRow
                  key={row.id}
                  item={row}
                  tasks={tasksOf(items, row)}
                  containers={containersOf(items, row)}
                  defaultOpen={row.state === "failed"}
                >
                  {row.state === "failed" ? <FailureText item={row} /> : undefined}
                </DeployServiceRow>
              ))}
            </ul>
          ) : null}
        </li>
      ))}
    </ol>
  );
}
