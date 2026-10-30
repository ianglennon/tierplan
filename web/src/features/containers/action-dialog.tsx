// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { ChevronRight, TriangleAlert } from "lucide-react";
import { useId, useRef, useState } from "react";

import {
  ActionRequestError,
  type BulkBody,
  type BulkResults,
  type ContainerActionResult,
  useBulkAction,
} from "@/api/containers";
import type { ContainerRow } from "@/api/resources";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { matchesName, TypeToConfirm } from "@/components/type-to-confirm";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import type { BulkReport, BulkReportRow } from "@/features/containers/bulk-result-dialog";
import { eligibleFor, needsForce, ownKind } from "@/features/containers/eligibility";
import {
  type PendingVerb,
  pendingFingerprint,
  pendingKey,
  usePendingApi,
} from "@/features/containers/pending";
import { TargetList } from "@/features/resources/target-list";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastErrorWithAction, toastInfo, toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The destructive fill, used only where the UI-SPEC says: Kill, Remove, and any
// action on Coaming's own containers.
const DESTRUCTIVE = "bg-destructive-fill text-white hover:bg-destructive-fill/90";

// The actions that go through a dialog. Restart and Pause only do so for one of
// Coaming's own containers; for any other they run at once.
export type DialogVerb = "stop" | "kill" | "remove" | "restart" | "pause";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// The sentence for a single success.
const DONE_TOAST: Record<DialogVerb, string> = {
  stop: "stopped",
  kill: "killed",
  remove: "removed",
  restart: "restarted",
  pause: "paused",
};

// The timeout a stop may be given, in seconds (the server's own limit).
const MAX_TIMEOUT = 86400;

// reasonOf is the sentence for a container that failed, from its code. A code
// is never shown: the SPA words every one of them (UI-SPEC "Shared refusals").
export function reasonOf(
  t: Translate,
  who: { name: string; node: string },
  result: ContainerActionResult,
): string {
  switch (result.code) {
    case "own_container":
      return t("actions.refusal.own_container", { name: who.name });
    case "node_unreachable":
      return t("actions.refusal.node_unreachable", { node: who.node });
    case "not_found":
      return t("actions.refusal.not_found", { name: who.name });
    case "agent_policy_denied":
      return t("actions.refusal.agent_policy_denied");
    case "host_read_only":
      return t("actions.refusal.host_read_only");
    case "container_running":
      return t("actions.refusal.container_running", { name: who.name });
    default:
      // The engine's own message says what it refused; without one, the server
      // could not say more than that something went wrong.
      return result.detail || t("actions.refusal.generic");
  }
}

// What a request refused as a whole comes to: the sentence, and whether it is
// the agent's policy, which has a notice of its own.
interface Failure {
  message: string;
  agentPolicy: boolean;
}

// isLostAnswer says whether a request got no answer at all: it was sent and the
// connection dropped before the server replied, as a page does when Chromium
// abandons every request on a change of the host's network. The server sees a
// sent action through, so it may well have happened: it is not a server fault,
// and the list (refreshed after every request) shows the outcome.
export function isLostAnswer(err: unknown): boolean {
  return err instanceof ActionRequestError && err.status === 0;
}

function failureOf(t: Translate, err: unknown): Failure {
  if (isLostAnswer(err)) {
    return { message: t("actions.refusal.lost"), agentPolicy: false };
  }
  if (err instanceof ActionRequestError) {
    if (err.type === "agent_policy_denied") {
      return { message: t("actions.refusal.agent_policy_denied"), agentPolicy: true };
    }
    if (err.status === 403) {
      return { message: t("actions.refusal.permission"), agentPolicy: false };
    }
    if (err.status === 422 && err.type === "confirmation_required") {
      return { message: t("actions.refusal.confirmation"), agentPolicy: false };
    }
  }
  return { message: t("actions.refusal.generic"), agentPolicy: false };
}

// requestReasonOf is the sentence for a request that got no answer per
// container: the permission was withdrawn, or the server failed.
export function requestReasonOf(t: Translate, err: unknown): string {
  return failureOf(t, err).message;
}

// useBulkRunner sends one action for any number of containers and keeps the
// rows' transitional labels honest: they are marked when the request goes out,
// and a container that did not get it done is taken off at once (D-13). It
// returns the results in the order asked, or throws when the request as a whole
// was refused.
export function useBulkRunner(envId: string) {
  const mutation = useBulkAction(envId);
  const pend = usePendingApi();
  async function run(
    verb: PendingVerb,
    rows: readonly ContainerRow[],
    extra: Record<string, unknown> = {},
  ): Promise<BulkResults> {
    const keys = rows.map(pendingKey);
    pend.mark(
      rows.map((r) => ({ key: pendingKey(r), name: r.name, fingerprint: pendingFingerprint(r) })),
      verb,
    );
    const body = (
      verb === "remove" ? { kind: "remove", ...extra } : { kind: "action", action: verb, ...extra }
    ) as BulkBody;
    try {
      const results = await mutation.mutateAsync({
        body,
        targets: rows.map((r) => ({ nodeId: r.nodeId, id: r.id })),
      });
      // A container that was not done goes back to its real state at once.
      pend.release(keys.filter((_, i) => results[i]?.result !== "done"));
      return results;
    } catch (err) {
      pend.release(keys);
      throw err;
    }
  }
  return { run, isPending: mutation.isPending };
}

// reportOf lines the results up with the rows that were asked about, in the
// order they were chosen. A row that could not take the action was never sent
// and reads as skipped.
export function reportOf(
  t: Translate,
  verb: PendingVerb,
  rows: readonly ContainerRow[],
  sent: readonly ContainerRow[],
  results: readonly ContainerActionResult[] & { bulkId?: string },
): BulkReport {
  const answered = new Map<ContainerRow, ContainerActionResult | undefined>();
  sent.forEach((r, i) => {
    answered.set(r, results[i]);
  });
  return {
    verb,
    bulkId: results.bulkId,
    rows: rows.map((r): BulkReportRow => {
      if (!answered.has(r)) {
        return {
          name: r.name,
          node: r.node,
          result: "skipped",
          reason: t("actions.targets.skipped"),
        };
      }
      const result = answered.get(r);
      if (!result) {
        return {
          name: r.name,
          node: r.node,
          result: "failed",
          reason: t("actions.refusal.generic"),
        };
      }
      return {
        name: r.name,
        node: r.node,
        result: result.result,
        reason: result.result === "done" ? "" : reasonOf(t, r, result),
        code: result.code,
      };
    }),
  };
}

// toastBulk sums a bulk up in one toast: "Stopped 28 containers.", with
// "2 skipped" after it, or when some failed "Stopped 28, 2 failed, 2 skipped"
// with a Details button that opens the results.
export function toastBulk(
  t: Translate,
  report: BulkReport,
  onDetails?: (report: BulkReport) => void,
) {
  const count = (r: BulkReportRow["result"]) => report.rows.filter((x) => x.result === r).length;
  const done = count("done");
  const failed = count("failed");
  const skipped = count("skipped");
  const tail = skipped > 0 ? t("actions.toast.skipped", { count: skipped }) : "";
  if (failed === 0) {
    toastSuccess(
      [t(`actions.toast.bulk.${report.verb}`, { count: done }), tail].filter(Boolean).join(" "),
    );
    return;
  }
  const message = [
    t("actions.toast.partial", { verb: t(`actions.past.${report.verb}`), done, failed }),
    tail,
  ]
    .filter(Boolean)
    .join(", ");
  if (onDetails) {
    toastErrorWithAction(message, t("actions.toast.details"), () => onDetails(report));
  } else {
    toastError(message);
  }
}

// noteOf is what a target's row says about it: the reason acting on it matters,
// or that it is left out.
function noteOf(t: Translate, row: ContainerRow, skipped: boolean): string | undefined {
  if (skipped) return t("actions.targets.skipped");
  if (row.service) return t("actions.targets.task", { service: row.service });
  if (row.project) return t("actions.targets.project", { project: row.project });
  return undefined;
}

interface ActionDialogProps {
  envId: string;
  verb: DialogVerb;
  // The containers chosen, one or many. Each is acted on and answered alone; in
  // a bulk the ones the action does not apply to are listed as skipped.
  rows: readonly ContainerRow[];
  // On a swarm the target list says which node each one is on.
  swarm?: boolean;
  open: boolean;
  onClose: () => void;
  // Called when the dialog has closed on its own account (an action that
  // worked), so the menu button can take focus back.
  onClosed?: () => void;
  // Called when a bulk has been answered, so the list can clear its selection.
  onDone?: () => void;
  // Called with the report when the person asks for the details of a bulk that
  // partly failed.
  onDetails?: (report: BulkReport) => void;
  // The container's own stop timeout in seconds, when it is known.
  seconds?: number;
}

// ActionDialog is the confirmation for Stop, Kill and Remove of one or many
// containers, and for any action on one of Coaming's own (D-07 to D-12,
// UI-SPEC "Container action dialogs"). Focus starts on Cancel. A refusal or
// failure stays inside the dialog as an alert and never raises a toast over it;
// if the dialog was closed while the request was out, the answer arrives as a
// toast instead (UI-SPEC pending state). A bulk closes when it is answered and
// sums up in one toast.
export function ActionDialog({
  envId,
  verb,
  rows,
  swarm = false,
  open,
  onClose,
  onClosed,
  onDone,
  onDetails,
  seconds,
}: ActionDialogProps) {
  const t = useT();
  const runner = useBulkRunner(envId);
  const volumesNeed = useNeeds("volumes.delete", envId);
  const ids = useId();
  const [error, setError] = useState<Failure | null>(null);
  const [fieldError, setFieldError] = useState<string | null>(null);
  const [typed, setTyped] = useState("");
  const [force, setForce] = useState(false);
  const [removeVolumes, setRemoveVolumes] = useState(false);
  const [advanced, setAdvanced] = useState(false);
  const [timeout, setTimeoutText] = useState("");
  const [signal, setSignal] = useState("");
  // Whether the dialog is still on screen when the answer arrives.
  const shown = useRef(open);
  shown.current = open;
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = runner.isPending;

  const bulk = rows.length > 1;
  const first = rows[0];
  const name = first?.name ?? "";
  const own = !bulk && first !== undefined ? ownKind(first) : "";
  // In a bulk, the rows the action does not apply to are left out of the request
  // and the count. One chosen alone is always sent: the server has the last word.
  const targets = bulk ? rows.filter((r) => eligibleFor(verb, r, { bulk: true })) : rows;
  const count = targets.length;

  const forceMissing = verb === "remove" && !force && targets.some(needsForce);
  const nameMissing = own !== "" && !matchesName(typed, name);
  const blocked = pending || forceMissing || nameMissing;

  const tasks = targets.filter((r) => r.service);
  const projects = targets.filter((r) => r.project);

  // The options of a single stop or kill, empty meaning the container's own.
  function optionsOf(): Record<string, unknown> | string {
    const out: Record<string, unknown> = {};
    if (bulk || own !== "") return out;
    const text = timeout.trim();
    if (verb === "stop" && text !== "") {
      const n = Number(text);
      if (!/^\d+$/.test(text) || n > MAX_TIMEOUT) return t("actions.advanced.timeoutInvalid");
      out.timeout = n;
    }
    const sig = signal.trim();
    if ((verb === "stop" || verb === "kill") && sig !== "") {
      if (!/^[A-Za-z0-9+]{1,16}$/.test(sig)) return t("actions.advanced.signalInvalid");
      out.signal = sig.toUpperCase();
    }
    return out;
  }

  function fail(failure: Failure) {
    if (shown.current) {
      setError(failure);
      return;
    }
    const verbWord = t(`actions.verb.${verb}`);
    toastError(
      bulk
        ? t("actions.toast.failedMany", { verb: verbWord, count, reason: failure.message })
        : t("actions.toast.failed", { verb: verbWord, name, reason: failure.message }),
    );
  }

  function reset() {
    setError(null);
    setFieldError(null);
    setTyped("");
    setForce(false);
    setRemoveVolumes(false);
    setAdvanced(false);
    setTimeoutText("");
    setSignal("");
  }

  async function confirm() {
    if (inFlight.current || blocked || count === 0) return;
    const options = optionsOf();
    if (typeof options === "string") {
      setFieldError(options);
      return;
    }
    inFlight.current = true;
    setError(null);
    setFieldError(null);
    const extra: Record<string, unknown> = { ...options };
    if (verb === "remove") {
      extra.force = force;
      extra.removeVolumes = removeVolumes && volumesNeed.allowed;
    }
    if (own !== "") extra.confirm = name;
    try {
      const results = await runner.run(verb, targets, extra);
      if (!bulk) {
        const result = results[0];
        if (result?.result === "done") {
          toastSuccess(t(`actions.toast.${DONE_TOAST[verb]}`, { name }));
          reset();
          onClose();
          onClosed?.();
          return;
        }
        fail(
          result
            ? {
                message: reasonOf(t, { name, node: first?.node ?? "" }, result),
                agentPolicy: result.code === "agent_policy_denied",
              }
            : { message: t("actions.refusal.generic"), agentPolicy: false },
        );
        return;
      }
      toastBulk(t, reportOf(t, verb, rows, targets, results), onDetails);
      reset();
      onClose();
      onClosed?.();
      onDone?.();
    } catch (err) {
      if (isLostAnswer(err)) {
        // Nothing in the dialog can say more than the list will: it closes, and a
        // notice says the outcome is not known.
        const verbWord = t(`actions.verb.${verb}`);
        toastInfo(
          bulk
            ? t("actions.toast.lostMany", { verb: verbWord, count })
            : t("actions.toast.lost", { verb: verbWord, name }),
        );
        reset();
        onClose();
        onClosed?.();
        return;
      }
      fail(failureOf(t, err));
    } finally {
      inFlight.current = false;
    }
  }

  function change(next: boolean) {
    if (next) return;
    setError(null);
    setFieldError(null);
    onClose();
  }

  const title = bulk
    ? t(`actions.${verb}.titleBulk`, { count })
    : t(`actions.${verb}.title`, { name });
  const body = bulk
    ? verb === "stop"
      ? t("actions.stop.bodyBulk")
      : t(`actions.${verb}.body`)
    : verb === "stop"
      ? seconds === undefined
        ? t("actions.stop.bodyOwn")
        : t("actions.stop.body", { seconds })
      : t(`actions.${verb}.body`);
  const confirmLabel =
    own !== ""
      ? t(`actions.own.confirm.${own}.${verb}`)
      : bulk
        ? t(`actions.${verb}.confirmBulk`, { count })
        : t(`actions.${verb}.confirm`);
  const destructive = verb === "kill" || verb === "remove" || own !== "";
  const advancedVerb = (verb === "stop" || verb === "kill") && !bulk && own === "";
  const forceId = `${ids}-force`;
  const volumesId = `${ids}-volumes`;
  const describedBy = forceMissing
    ? `${ids}-force-required`
    : nameMissing
      ? `${ids}-name`
      : undefined;

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent
        className="max-h-[90svh] overflow-y-auto data-[size=default]:sm:max-w-lg"
        onCloseAutoFocus={(event) => {
          // Focus goes back to the row's menu button, not to the page.
          event.preventDefault();
          onClosed?.();
        }}
      >
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-all">{title}</AlertDialogTitle>
          <AlertDialogDescription className="break-words">{body}</AlertDialogDescription>
        </AlertDialogHeader>

        {own !== "" ? (
          <Alert variant="warning" role="note">
            <TriangleAlert aria-hidden="true" />
            <AlertDescription className="text-foreground">
              {t(`actions.own.${own}`, {
                gerund: t(`actions.own.gerund.${verb}`),
                node: first?.node ?? "",
              })}
            </AlertDescription>
          </Alert>
        ) : null}
        {tasks.length > 0 ? (
          <Alert variant="warning" role="note">
            <TriangleAlert aria-hidden="true" />
            <AlertDescription className="text-foreground">
              {count === 1
                ? t("actions.warn.task", { service: tasks[0]?.service })
                : t("actions.warn.tasks", { count: tasks.length })}
            </AlertDescription>
          </Alert>
        ) : null}
        {projects.length > 0 ? (
          <Alert variant="warning" role="note">
            <TriangleAlert aria-hidden="true" />
            <AlertDescription className="text-foreground">
              {count === 1
                ? t("actions.warn.project", { project: projects[0]?.project })
                : t("actions.warn.projects", { count: projects.length })}
            </AlertDescription>
          </Alert>
        ) : null}

        {bulk ? (
          <TargetList
            swarm={swarm}
            rows={rows.map((r) => {
              const skipped = !targets.includes(r);
              return { name: r.name, node: r.node, note: noteOf(t, r, skipped), skipped };
            })}
          />
        ) : null}

        {verb === "remove" ? (
          <div className="flex flex-col gap-3">
            <div className="flex items-start gap-2">
              <Checkbox
                id={forceId}
                checked={force}
                disabled={pending}
                aria-describedby={`${forceId}-help`}
                onCheckedChange={(c) => setForce(c === true)}
                className="mt-0.5 max-md:size-5"
              />
              <div className="flex flex-col gap-0.5">
                <Label htmlFor={forceId}>{t("actions.remove.force")}</Label>
                <p id={`${forceId}-help`} className="text-xs text-muted-foreground">
                  {t("actions.remove.forceHelp")}
                </p>
              </div>
            </div>
            <div className="flex items-start gap-2">
              <NeedsTooltip
                reason={
                  volumesNeed.allowed
                    ? undefined
                    : (volumesNeed.reason ?? t("needs.extra.volumes.delete"))
                }
              >
                <Checkbox
                  id={volumesId}
                  checked={removeVolumes}
                  disabled={pending}
                  aria-describedby={`${volumesId}-help`}
                  onCheckedChange={(c) => setRemoveVolumes(c === true)}
                  className="mt-0.5 max-md:size-5"
                />
              </NeedsTooltip>
              <div className="flex flex-col gap-0.5">
                <Label htmlFor={volumesId}>{t("actions.remove.volumes")}</Label>
                <p id={`${volumesId}-help`} className="text-xs text-muted-foreground">
                  {t("actions.remove.volumesHelp")}
                </p>
              </div>
            </div>
          </div>
        ) : null}

        {advancedVerb ? (
          <Collapsible open={advanced} onOpenChange={setAdvanced} className="flex flex-col gap-2">
            <CollapsibleTrigger asChild>
              <Button type="button" variant="ghost" size="sm" className="self-start max-md:h-11">
                <ChevronRight
                  aria-hidden="true"
                  className={cn(
                    "transition-transform motion-reduce:transition-none",
                    advanced && "rotate-90",
                  )}
                />
                {t("actions.advanced.trigger")}
              </Button>
            </CollapsibleTrigger>
            <CollapsibleContent className="flex flex-col gap-3">
              {verb === "stop" ? (
                <div className="flex flex-col gap-1">
                  <Label htmlFor={`${ids}-timeout`}>{t("actions.advanced.timeout")}</Label>
                  <Input
                    id={`${ids}-timeout`}
                    inputMode="numeric"
                    autoComplete="off"
                    disabled={pending}
                    value={timeout}
                    placeholder={
                      seconds === undefined ? t("actions.advanced.own") : String(seconds)
                    }
                    aria-invalid={fieldError === t("actions.advanced.timeoutInvalid") || undefined}
                    className="h-11 md:h-8"
                    onChange={(e) => setTimeoutText(e.target.value)}
                  />
                </div>
              ) : null}
              <div className="flex flex-col gap-1">
                <Label htmlFor={`${ids}-signal`}>{t("actions.advanced.signal")}</Label>
                <Input
                  id={`${ids}-signal`}
                  autoComplete="off"
                  spellCheck={false}
                  autoCapitalize="characters"
                  disabled={pending}
                  value={signal}
                  placeholder={
                    verb === "kill" ? t("actions.advanced.killSignal") : t("actions.advanced.own")
                  }
                  aria-invalid={fieldError === t("actions.advanced.signalInvalid") || undefined}
                  className="h-11 md:h-8"
                  onChange={(e) => setSignal(e.target.value)}
                />
              </div>
              <p className="text-xs text-muted-foreground">{t("actions.advanced.help")}</p>
              {fieldError ? (
                <p role="alert" className="text-xs text-destructive">
                  {fieldError}
                </p>
              ) : null}
            </CollapsibleContent>
          </Collapsible>
        ) : null}

        {own !== "" ? (
          <>
            <TypeToConfirm
              name={name}
              value={typed}
              onChange={setTyped}
              onConfirm={() => void confirm()}
              disabled={pending}
            />
            <span id={`${ids}-name`} className="sr-only">
              {t("needs.typeToConfirm.hint", { name })}
            </span>
          </>
        ) : null}

        {forceMissing ? (
          <p id={`${ids}-force-required`} className="text-sm text-muted-foreground">
            {t("actions.remove.forceRequired")}
          </p>
        ) : null}

        {error ? (
          error.agentPolicy ? (
            <AgentUpdateNotice envId={envId} />
          ) : (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error.message}</AlertDescription>
            </Alert>
          )
        ) : null}

        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel className={TOUCH_HEIGHT}>
            {t("actions.dialog.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={blocked}
            aria-describedby={describedBy}
            className={cn(
              TOUCH_HEIGHT,
              destructive && DESTRUCTIVE,
              "aria-disabled:cursor-not-allowed aria-disabled:opacity-50",
            )}
            onClick={() => {
              if (!blocked) void confirm();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t(`actions.${verb}.pending`)}
              </>
            ) : (
              confirmLabel
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
