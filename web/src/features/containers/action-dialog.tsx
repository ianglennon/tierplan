// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import {
  ActionRequestError,
  type BulkBody,
  type ContainerActionResult,
  useBulkAction,
} from "@/api/containers";
import type { ContainerRow } from "@/api/resources";
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
import { Spinner } from "@/components/ui/spinner";
import {
  type PendingVerb,
  pendingFingerprint,
  pendingKey,
  usePendingApi,
} from "@/features/containers/pending";
import { TargetList } from "@/features/resources/target-list";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The actions that go through a dialog.
export type DialogVerb = "stop";

type Translate = (key: string, options?: Record<string, unknown>) => string;

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
    default:
      // The engine's own message says what it refused; without one, the server
      // could not say more than that something went wrong.
      return result.detail || t("actions.refusal.generic");
  }
}

// requestReasonOf is the sentence for a request that got no answer per
// container: the permission was withdrawn, or the server failed.
export function requestReasonOf(t: Translate, err: unknown): string {
  return err instanceof ActionRequestError && err.status === 403
    ? t("actions.refusal.permission")
    : t("actions.refusal.generic");
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
  ): Promise<ContainerActionResult[]> {
    const keys = rows.map(pendingKey);
    pend.mark(
      rows.map((r) => ({ key: pendingKey(r), name: r.name, fingerprint: pendingFingerprint(r) })),
      verb,
    );
    const body = { kind: "action", action: verb, ...extra } as BulkBody;
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

// toastBulk sums a bulk up in one toast: "Stopped 28 containers.", with
// "2 skipped" after it, or when some failed "Stopped 28, 2 failed, 2 skipped".
// skippedBefore counts the rows that were never sent because they could not
// take the action.
export function toastBulk(
  t: Translate,
  verb: PendingVerb,
  results: readonly ContainerActionResult[],
  skippedBefore: number,
) {
  const done = results.filter((r) => r.result === "done").length;
  const failed = results.filter((r) => r.result === "failed").length;
  const skipped = results.filter((r) => r.result === "skipped").length + skippedBefore;
  const tail = skipped > 0 ? t("actions.toast.skipped", { count: skipped }) : "";
  if (failed === 0) {
    toastSuccess(
      [t(`actions.toast.bulk.${verb}`, { count: done }), tail].filter(Boolean).join(" "),
    );
    return;
  }
  toastError(
    [t("actions.toast.partial", { verb: t(`actions.past.${verb}`), done, failed }), tail]
      .filter(Boolean)
      .join(", "),
  );
}

interface ActionDialogProps {
  envId: string;
  verb: DialogVerb;
  // The containers chosen, one or many. Each is acted on and answered alone.
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
  // The container's own stop timeout in seconds, when it is known.
  seconds?: number;
}

// ActionDialog is the confirmation for Stop of one or many containers (D-07,
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
  seconds,
}: ActionDialogProps) {
  const t = useT();
  const runner = useBulkRunner(envId);
  const [error, setError] = useState<string | null>(null);
  // Whether the dialog is still on screen when the answer arrives.
  const shown = useRef(open);
  shown.current = open;
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = runner.isPending;

  const bulk = rows.length > 1;
  const name = rows[0]?.name ?? "";

  function fail(reason: string) {
    if (shown.current) {
      setError(reason);
      return;
    }
    const verbWord = t(`actions.verb.${verb}`);
    toastError(
      bulk
        ? t("actions.toast.failedMany", { verb: verbWord, count: rows.length, reason })
        : t("actions.toast.failed", { verb: verbWord, name, reason }),
    );
  }

  async function confirm() {
    if (inFlight.current || rows.length === 0) return;
    inFlight.current = true;
    setError(null);
    try {
      const results = await runner.run(verb, rows);
      if (!bulk) {
        const result = results[0];
        if (result?.result === "done") {
          toastSuccess(t("actions.toast.stopped", { name }));
          onClose();
          onClosed?.();
          return;
        }
        fail(
          result
            ? reasonOf(t, { name, node: rows[0]?.node ?? "" }, result)
            : t("actions.refusal.generic"),
        );
        return;
      }
      toastBulk(t, verb, results, 0);
      onClose();
      onClosed?.();
      onDone?.();
    } catch (err) {
      fail(requestReasonOf(t, err));
    } finally {
      inFlight.current = false;
    }
  }

  function change(next: boolean) {
    if (next) return;
    setError(null);
    onClose();
  }

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
          <AlertDialogTitle className="break-all">
            {bulk
              ? t("actions.stop.titleBulk", { count: rows.length })
              : t("actions.stop.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {bulk
              ? t("actions.stop.bodyBulk")
              : seconds === undefined
                ? t("actions.stop.bodyOwn")
                : t("actions.stop.body", { seconds })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {bulk ? (
          <TargetList swarm={swarm} rows={rows.map((r) => ({ name: r.name, node: r.node }))} />
        ) : null}
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel className={TOUCH_HEIGHT}>{t("actions.stop.cancel")}</AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={pending}
            className={`${TOUCH_HEIGHT} aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => {
              if (!pending) void confirm();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("actions.stop.pending")}
              </>
            ) : bulk ? (
              t("actions.stop.confirmBulk", { count: rows.length })
            ) : (
              t("actions.stop.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
