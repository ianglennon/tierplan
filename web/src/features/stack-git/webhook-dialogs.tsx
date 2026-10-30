// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useRef, useState } from "react";

import {
  type StackGitWebhookSecret,
  useRemoveStackGitWebhook,
  useRotateStackGitWebhook,
} from "@/api/stack-git";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// What a refused request is worded as: the webhook is already gone, the person may not, or anything else.
type Refusal = "gone" | "permission" | "failed";

function refusalOf(e: unknown): Refusal {
  const { status, type } = e as { status?: number; type?: string };
  if (status === 404 && type === "webhook_not_found") return "gone";
  if (status === 403) return "permission";
  return "failed";
}

interface RotateSecretDialogProps {
  envId: string;
  stackId: string;
  // The name of the git host the webhook is for, as the dialog names it to update.
  host: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Called with the new address and secret. The caller shows them once in its own state; this dialog has closed.
  onRotated: (answer: StackGitWebhookSecret) => void;
}

// RotateSecretDialog is "Rotate the webhook secret?" (UI-SPEC "Git tab", D-15): the old secret and address stop
// working at once. Focus starts on "Keep current secret"; "Rotate secret" is the accent button and the dialog stays
// open, with a refusal inside it, until the server answers. The new values are handed on, not kept.
export function RotateSecretDialog({
  envId,
  stackId,
  host,
  open,
  onOpenChange,
  onRotated,
}: RotateSecretDialogProps) {
  const t = useT();
  const rotate = useRotateStackGitWebhook(envId, stackId);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = rotate.isPending;

  async function confirm() {
    if (inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      const answer = await rotate.mutateAsync();
      // The answer is in this call and nowhere else: the mutation forgets it before the caller keeps it in state.
      rotate.reset();
      toastSuccess(t("stackGit.webhook.rotate.done"));
      onOpenChange(false);
      onRotated(answer);
    } catch (e) {
      const refusal = refusalOf(e);
      if (refusal === "gone") {
        // The git summary is read again by the mutation, which brings the card back to "No webhook yet."
        toastInfo(t("stackGit.webhook.rotate.gone"));
        onOpenChange(false);
      } else {
        setError(t(`stackGit.webhook.rotate.${refusal}`));
      }
    } finally {
      inFlight.current = false;
    }
  }

  function change(next: boolean) {
    if (!next && pending) return;
    if (!next) setError(null);
    onOpenChange(next);
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
        <AlertDialogHeader>
          <AlertDialogTitle>{t("stackGit.webhook.rotate.title")}</AlertDialogTitle>
          <AlertDialogDescription>
            {t("stackGit.webhook.rotate.body", { host })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("stackGit.webhook.rotate.cancel")}
          </AlertDialogCancel>
          <AlertDialogAction
            aria-disabled={pending}
            className={TOUCH_HEIGHT}
            onClick={(event) => {
              // The dialog stays open until the answer, so a refusal can be shown in it.
              event.preventDefault();
              void confirm();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("stackGit.webhook.rotate.pending")}
              </>
            ) : (
              t("stackGit.webhook.rotate.confirm")
            )}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

interface RemoveWebhookDialogProps {
  envId: string;
  stackId: string;
  stackName: string;
  // Whether the stack is polled, and every how many minutes: what still tells Coaming about a push once the webhook
  // is gone.
  pollEnabled: boolean;
  pollMinutes: number;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Called once the webhook is gone, so the caller can drop anything it held for it.
  onRemoved: () => void;
}

// RemoveWebhookDialog is "Remove the webhook?" (UI-SPEC "Git tab"): its address answers 404 from then on. Focus starts
// on "Keep webhook"; "Remove webhook" is destructive. A webhook that is already gone is treated as removed.
export function RemoveWebhookDialog({
  envId,
  stackId,
  stackName,
  pollEnabled,
  pollMinutes,
  open,
  onOpenChange,
  onRemoved,
}: RemoveWebhookDialogProps) {
  const t = useT();
  const remove = useRemoveStackGitWebhook(envId, stackId);
  const [error, setError] = useState<string | null>(null);
  const inFlight = useRef(false);
  const pending = remove.isPending;

  async function confirm() {
    if (inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await remove.mutateAsync();
      toastSuccess(t("stackGit.webhook.remove.done"));
      onOpenChange(false);
      onRemoved();
    } catch (e) {
      const refusal = refusalOf(e);
      if (refusal === "gone") {
        toastInfo(t("stackGit.webhook.remove.gone"));
        onOpenChange(false);
        onRemoved();
      } else {
        setError(t(`stackGit.webhook.remove.${refusal}`));
      }
    } finally {
      inFlight.current = false;
    }
  }

  function change(next: boolean) {
    if (!next && pending) return;
    if (!next) setError(null);
    onOpenChange(next);
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
        <AlertDialogHeader>
          <AlertDialogTitle>{t("stackGit.webhook.remove.title")}</AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {pollEnabled
              ? t("stackGit.webhook.remove.body", { name: stackName, minutes: pollMinutes })
              : t("stackGit.webhook.remove.bodyNoPolling", { name: stackName })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("stackGit.webhook.remove.cancel")}
          </AlertDialogCancel>
          <AlertDialogAction
            aria-disabled={pending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90`}
            onClick={(event) => {
              event.preventDefault();
              void confirm();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("stackGit.webhook.remove.pending")}
              </>
            ) : (
              t("stackGit.webhook.remove.confirm")
            )}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
