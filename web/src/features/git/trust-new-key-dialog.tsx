// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useRef, useState } from "react";

import { useTrustGitHostKey } from "@/api/git";
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
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The repository whose host key is being trusted: its layer picks the route family, the
// environment's for a repository of one and the instance's otherwise.
export interface TrustTarget {
  id: string;
  host: string;
  scope?: { envId: string | null };
}

// The key the host showed, as the test answered it.
export interface PresentedKey {
  // authorized_keys form: what is sent to the server.
  key: string;
  fingerprint: string;
}

interface TrustNewKeyDialogProps {
  repository: TrustTarget | null;
  // The fingerprint of the key that was trusted until now; empty when the answer did not name it.
  savedFingerprint: string;
  presented: PresentedKey | null;
  open: boolean;
  onClose: () => void;
  // Called when the new key is trusted.
  onDone?: () => void;
}

// TrustNewKeyDialog is 'Trust the new key for {host}?' (D-03): accepting a changed host key can send
// a credential to the wrong machine, so it is a destructive confirm whose focus starts on the safe
// button, "Keep the old key". It cannot be dismissed while the request is out, a refusal stays
// inside it, and the key sent is the one that was shown, never one typed.
export function TrustNewKeyDialog({
  repository,
  savedFingerprint,
  presented,
  open,
  onClose,
  onDone,
}: TrustNewKeyDialogProps) {
  const t = useT();
  const mutation = useTrustGitHostKey();
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = mutation.isPending;
  const host = repository?.host ?? "";

  async function confirm() {
    if (!repository || !presented || inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await mutation.mutateAsync({
        id: repository.id,
        envId: repository.scope?.envId ?? undefined,
        hostKey: presented.key,
      });
      toastSuccess(t("git.hostKey.retrust.done", { host }));
      onClose();
      onDone?.();
    } catch (e) {
      const err = e as { status?: number };
      setError(
        err.status === 403
          ? t("git.hostKey.retrust.permission")
          : err.status === 404
            ? t("git.hostKey.retrust.gone")
            : err.status === 422
              ? t("git.hostKey.retrust.invalid")
              : t("git.hostKey.retrust.generic"),
      );
    } finally {
      inFlight.current = false;
    }
  }

  function change(next: boolean) {
    if (next || pending) return;
    setError(null);
    onClose();
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-140">
        <AlertDialogHeader>
          <AlertDialogTitle className="break-all">
            {t("git.hostKey.retrust.title", { host })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("git.hostKey.retrust.body", { host })}</AlertDialogDescription>
        </AlertDialogHeader>
        {/* Fingerprints are text only, mono, and wrap anywhere (T-09-28). */}
        <dl className="flex flex-col gap-2 text-sm">
          {savedFingerprint ? (
            <div className="flex flex-col gap-0.5">
              <dt className="text-xs text-muted-foreground">{t("git.hostKey.retrust.saved")}</dt>
              <dd>
                <code className="font-mono text-xs break-all">{savedFingerprint}</code>
              </dd>
            </div>
          ) : null}
          <div className="flex flex-col gap-0.5">
            <dt className="text-xs text-muted-foreground">{t("git.hostKey.retrust.new")}</dt>
            <dd>
              <code className="font-mono text-xs break-all">{presented?.fingerprint ?? ""}</code>
            </dd>
          </div>
        </dl>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens: the safe choice. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("git.hostKey.retrust.keep")}
          </AlertDialogCancel>
          <AlertDialogAction
            aria-disabled={pending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90`}
            onClick={(event) => {
              // The dialog stays open until the answer, so a refusal can be shown in it.
              event.preventDefault();
              void confirm();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("git.hostKey.retrust.pending")}
              </>
            ) : (
              t("git.hostKey.retrust.confirm")
            )}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
