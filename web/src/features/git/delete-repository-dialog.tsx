// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import { useDeleteGitRepo } from "@/api/git";
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

// The repository a delete is about. Only its name is shown; its scope picks the route family, the
// environment's for a repository of one and the instance's otherwise.
export interface RepositoryTarget {
  id: string;
  name: string;
  scope?: { envId: string | null };
}

interface DeleteRepositoryDialogProps {
  // The repository to delete; ignored while the dialog is closed.
  repository: RepositoryTarget | null;
  open: boolean;
  onClose: () => void;
  // Called when the repository is gone, whether this delete removed it or it already was.
  onDone?: () => void;
}

// DeleteRepositoryDialog is 'Delete "{name}"?': a plain confirm with no name to type, since a repository can be added again in a moment.
// Focus starts on Cancel, the dialog cannot be dismissed while the request is out, and a refusal
// stays inside it. A repository that is already gone says so in a toast and the dialog closes, since
// the refreshed list no longer shows it.
export function DeleteRepositoryDialog({
  repository,
  open,
  onClose,
  onDone,
}: DeleteRepositoryDialogProps) {
  const t = useT();
  const mutation = useDeleteGitRepo();
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = mutation.isPending;
  const name = repository?.name ?? "";

  function finish() {
    setError(null);
    onClose();
    onDone?.();
  }

  async function confirm() {
    if (!repository || inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await mutation.mutateAsync({
        id: repository.id,
        envId: repository.scope?.envId ?? undefined,
      });
      toastSuccess(t("git.deleteRepo.done", { name }));
      finish();
    } catch (e) {
      const err = e as { status?: number; type?: string; usedBy?: number };
      if (err.status === 404) {
        toastInfo(t("git.deleteRepo.gone", { name }));
        finish();
      } else if (err.status === 403) {
        setError(t("git.deleteRepo.permission"));
      } else if (err.status === 409 && err.usedBy !== undefined) {
        setError(t("git.deleteRepo.inUse", { count: err.usedBy }));
      } else {
        setError(t("git.deleteRepo.generic"));
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
            {t("git.deleteRepo.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("git.deleteRepo.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("git.deleteRepo.cancel")}
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
                {t("git.deleteRepo.pending")}
              </>
            ) : (
              t("git.deleteRepo.confirm")
            )}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
