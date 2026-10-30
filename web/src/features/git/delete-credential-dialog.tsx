// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useRef, useState } from "react";

import { useDeleteGitCredential } from "@/api/git";
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

// The credential a delete is about. Only its name is shown; its scope picks the route family, the
// environment's for a credential of one and the instance's otherwise.
export interface CredentialTarget {
  id: string;
  name: string;
  scope?: { envId: string | null };
}

interface DeleteCredentialDialogProps {
  // The credential to delete; ignored while the dialog is closed.
  credential: CredentialTarget | null;
  open: boolean;
  onClose: () => void;
  // Called when the credential is gone, whether this delete removed it or it already was.
  onDone?: () => void;
}

// DeleteCredentialDialog is 'Delete "{name}"?': a plain confirm with no name to type, since a credential is easy to add again.
// Focus starts on Cancel, the dialog cannot be dismissed while the request is out, and a refusal
// stays inside it. A credential that is already gone says so in a toast and the dialog closes, since
// the refreshed list no longer shows it.
export function DeleteCredentialDialog({
  credential,
  open,
  onClose,
  onDone,
}: DeleteCredentialDialogProps) {
  const t = useT();
  const mutation = useDeleteGitCredential();
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = mutation.isPending;
  const name = credential?.name ?? "";

  function finish() {
    setError(null);
    onClose();
    onDone?.();
  }

  async function confirm() {
    if (!credential || inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await mutation.mutateAsync({
        id: credential.id,
        envId: credential.scope?.envId ?? undefined,
      });
      toastSuccess(t("git.deleteCred.done", { name }));
      finish();
    } catch (e) {
      const err = e as { status?: number; type?: string; usedBy?: number };
      if (err.status === 404) {
        toastInfo(t("git.deleteCred.gone", { name }));
        finish();
      } else if (err.status === 403) {
        setError(t("git.deleteCred.permission"));
      } else if (err.status === 409 && err.usedBy !== undefined) {
        setError(t("git.deleteCred.inUse", { count: err.usedBy }));
      } else {
        setError(t("git.deleteCred.generic"));
      }
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
      <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-all">
            {t("git.deleteCred.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("git.deleteCred.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("git.deleteCred.cancel")}
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
                {t("git.deleteCred.pending")}
              </>
            ) : (
              t("git.deleteCred.confirm")
            )}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
