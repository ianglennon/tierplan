// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useRef, useState } from "react";

import { StackGitAutoDeployError, useSetStackGitAutoDeploy } from "@/api/stack-git";
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

interface AutoDeployConfirmDialogProps {
  envId: string;
  stackId: string;
  stackName: string;
  // The ref and folder the stack follows, and the environment it deploys to: what the dialog says will happen.
  gitRef: string;
  folder: string;
  environmentName: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// AutoDeployConfirmDialog is "Turn on auto-deploy?" (UI-SPEC "Git tab", D-18, T-09-81). Turning auto-deploy on gives
// the stack's automatic deploys the authority of whoever confirms, so the consequence is said before the buttons and
// focus starts on "Keep waiting for review". "Turn on auto-deploy" is the accent button; the dialog stays open, with a
// refusal inside it, until the server answers. Nothing is applied until then, so the switch behind it reads the saved
// state the whole time.
export function AutoDeployConfirmDialog({
  envId,
  stackId,
  stackName,
  gitRef,
  folder,
  environmentName,
  open,
  onOpenChange,
}: AutoDeployConfirmDialogProps) {
  const t = useT();
  const set = useSetStackGitAutoDeploy(envId, stackId);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = set.isPending;

  async function confirm() {
    if (inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await set.mutateAsync(true);
      toastSuccess(t("stackGit.autoDeploy.turnedOn", { name: stackName }));
      onOpenChange(false);
    } catch (e) {
      const status = e instanceof StackGitAutoDeployError ? e.status : 0;
      setError(
        status === 403 ? t("stackGit.autoDeploy.permission") : t("stackGit.autoDeploy.failed"),
      );
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
          <AlertDialogTitle>{t("stackGit.autoDeploy.confirm.title")}</AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {t("stackGit.autoDeploy.confirm.body", {
              ref: gitRef,
              folder: folder || t("stackGit.autoDeploy.folderRoot"),
              environment: environmentName,
            })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens: the safe choice. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("stackGit.autoDeploy.confirm.cancel")}
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
                {t("stackGit.autoDeploy.confirm.pending")}
              </>
            ) : (
              t("stackGit.autoDeploy.confirm.confirm")
            )}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
