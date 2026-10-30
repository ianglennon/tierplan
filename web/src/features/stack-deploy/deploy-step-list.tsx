// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Check, CircleX, Minus } from "lucide-react";

import type { StackJobItem } from "@/api/stacks";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { Spinner } from "@/components/ui/spinner";
import { failureCopy } from "@/features/pulls/failure-copy";
import { useEnvName } from "@/features/resources/use-env-name";
import { CONVERGE_TIMEOUT, DeployServiceRow } from "@/features/stack-deploy/deploy-service-row";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// The ID prefix of the step that pulls an image (plan 07-21), and the words it is shown in.
const PULL_PREFIX = "pull:";

// The codes of a host whose agent refuses a request by its own policy and of a host its owner made
// read-only: both are the host's, not the stack's, so the console words them as the actions do.
export const AGENT_POLICY_DENIED = "agent_policy_denied";
const HOST_READ_ONLY = "host_read_only";

// refusedRequest is the request an agent refused, read from the engine's words ("... agent_policy_denied:
// POST /v1.52/containers/create"): the method and the path, without the API version.
function refusedRequest(detail: string): string {
  const after = detail.split(`${AGENT_POLICY_DENIED}:`)[1]?.trim().split("\n")[0] ?? "";
  const match = /^([A-Z]+) (\/\S*)/.exec(after);
  return match ? `${match[1]} ${(match[2] ?? "").replace(/^\/v\d+\.\d+(?=\/)/, "")}` : "";
}

// The server's code for a service whose update the swarm paused or rolled back.
export const UPDATE_STOPPED = "update_stopped";

// The server's code for a service whose tasks the swarm will not start again.
export const TASKS_FAILED = "tasks_failed";

// The server's code for a service whose network has no free addresses left in its subnet.
export const NETWORK_FULL = "network_full";

// The codes whose row reason the result alert repeats: a service that never settled, or that the
// swarm gave up on, has a sentence of its own that says why and what to do.
export const CONVERGE_SENTENCE_CODES: ReadonlySet<string> = new Set([
  CONVERGE_TIMEOUT,
  TASKS_FAILED,
  NETWORK_FULL,
]);

// FailureContext is where an item failed: the environment's name, and whether it is a standalone host,
// whose agent refusals are worded with the host's and the service's names.
export interface FailureContext {
  envName?: string;
  standalone?: boolean;
}

// failureOf is why an item failed, as the console words it: the sentence for the code, and Docker's
// own words under it. A service has four convergence outcomes with a sentence of their own: it did
// not settle in time (and how many tasks still waited for an address), the swarm stopped its update,
// the swarm will not start its tasks again, and its network has no free addresses.
// Docker's rejection of a service reads as one sentence naming the service. Both are text from
// outside Coaming and are drawn as text.
export function failureOf(
  t: Translate,
  item: StackJobItem,
  ctx: FailureContext = {},
): { sentence: string; words: string } | undefined {
  const { envName, standalone } = ctx;
  // A pull the registry or the host refused reads as the pull dialog words it, naming the credential
  // the pull used. The sentence is the SPA's own; the server's words and the code are not shown.
  if (item.id.startsWith(PULL_PREFIX) && item.code && item.code !== "not_run") {
    const credential = item.result?.credentialId
      ? {
          id: item.result.credentialId,
          name: item.result.credential ?? "",
          layer:
            item.result.credentialLayer === "instance"
              ? ("instance" as const)
              : ("environment" as const),
        }
      : null;
    const ref = item.result?.ref || item.result?.image || item.label;
    return {
      sentence: failureCopy(t, item.code, { credential, envName, ref, node: t("stackPull.host") })
        .text,
      words: "",
    };
  }
  if (item.code === HOST_READ_ONLY && standalone) {
    return { sentence: t("actions.readOnlyHost"), words: "" };
  }
  // An agent that refuses a request: which host's agent, which service and what it would not take.
  if (item.code === AGENT_POLICY_DENIED && standalone && envName !== undefined) {
    return {
      sentence: t("stackPull.refusal.sentence", {
        host: envName,
        service: item.label,
        reason: refusedRequest(item.detail) || t("stackPull.refusal.generic"),
      }),
      words: "",
    };
  }
  if (item.code === "service_rejected" && item.detail) {
    return {
      sentence: t("stackDeploy.failure.rejected", { service: item.label, message: item.detail }),
      words: "",
    };
  }
  // A service that did not settle: how long it was given, and the swarm's last words about it.
  if (item.code === CONVERGE_TIMEOUT) {
    const body = t("stackConverge.timeout.body", {
      service: item.label,
      count: Number(item.result?.minutes) || 0,
    });
    // Tasks that never got an address are the usual reason a service does not settle: say how many.
    const waiting = Number(item.result?.waiting) || 0;
    return {
      sentence:
        waiting > 0 ? `${body} ${t("stackConverge.timeout.waiting", { count: waiting })}` : body,
      words: item.detail,
    };
  }
  if (item.code === UPDATE_STOPPED) {
    return {
      sentence: t("stackConverge.stopped.body", { service: item.label }),
      words: item.detail,
    };
  }
  if (item.code === NETWORK_FULL) {
    const network = item.result?.network;
    const subnet = item.result?.subnet;
    return {
      sentence:
        network && subnet
          ? t("stackConverge.networkFull.body", { service: item.label, network, subnet })
          : t("stackConverge.networkFull.bodyUnnamed", { service: item.label }),
      words: item.detail,
    };
  }
  if (item.code === TASKS_FAILED) {
    return {
      sentence: t("stackConverge.exhausted.body", { service: item.label }),
      words: item.detail,
    };
  }
  if (!item.message && !item.detail) return undefined;
  return { sentence: item.message, words: item.detail };
}

// FailureText is the reason under a failed step or service, in offline text, wrapped.
export function FailureText({
  item,
  envId,
  envName,
  standalone,
}: {
  item: StackJobItem;
  envId?: string;
  envName?: string;
  standalone?: boolean;
}) {
  const t = useT();
  const why = failureOf(t, item, { envName, standalone });
  if (!why) return null;
  return (
    <div className="flex flex-col gap-2 text-sm">
      <div className="flex flex-col gap-1 text-status-offline-text">
        {why.sentence ? <p className="break-all">{why.sentence}</p> : null}
        {why.words ? <p className="font-mono text-xs break-all">{why.words}</p> : null}
      </div>
      {item.code === AGENT_POLICY_DENIED && standalone && envId ? (
        <AgentUpdateNotice envId={envId} />
      ) : null}
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
  if (item.id.startsWith(PULL_PREFIX)) {
    return t("stackPull.steps.pull", { image: item.result?.image || item.label });
  }
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
  // The environment the deploy is on: its name is said as the host of an agent that refuses a request and
  // as the scope of an environment credential in a pull's failure.
  envId: string;
}

// DeployStepList is the ordered list of the steps of a deploy (UI-SPEC "Deploy console"): each with a
// mark and its state in words, and the services as rows inside Deploy services. The list is an ol
// and every step exposes its state as text; nothing in it announces itself, the console announces
// outcomes. Every label is text.
export function DeployStepList({ items, envId }: DeployStepListProps) {
  const t = useT();
  const envName = useEnvName(envId);
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
                : item.id.startsWith(PULL_PREFIX) && item.state === "running"
                  ? t("stackPull.phase.pulling")
                  : t(`stackDeploy.state.${item.state}`)}
            </span>
            {item.id.startsWith(PULL_PREFIX) && item.total > 0 ? (
              <span className="text-muted-foreground tabular-nums">
                {t("stackPull.progress", {
                  done: formatBytes(item.current),
                  total: formatBytes(item.total),
                })}
              </span>
            ) : null}
          </span>
          {item.state === "failed" && rows.every((r) => r.state !== "failed") ? (
            <div className="ms-6">
              <FailureText item={item} envId={envId} envName={envName} standalone={standalone} />
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
                  {row.state === "failed" ? (
                    <FailureText
                      item={row}
                      envId={envId}
                      envName={envName}
                      standalone={standalone}
                    />
                  ) : undefined}
                </DeployServiceRow>
              ))}
            </ul>
          ) : null}
        </li>
      ))}
    </ol>
  );
}
