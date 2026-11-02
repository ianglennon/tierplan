// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type ReactNode, useState } from "react";

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
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface RestoreConfirmDialogProps {
  open: boolean;
  // This instance's name, which has to be typed (D-18). Empty while it is not
  // known yet: nothing matches then.
  name: string;
  pending: boolean;
  // What went wrong with the last attempt, shown inline.
  error: ReactNode;
  onCancel: () => void;
  onConfirm: () => void;
}

// RestoreConfirmDialog is "Restore this backup?": a restore replaces everything,
// so the instance name is typed before the destructive button works. Focus starts
// on Cancel, and nothing closes the dialog while the request is in flight.
export function RestoreConfirmDialog({
  open,
  name,
  pending,
  error,
  onCancel,
  onConfirm,
}: RestoreConfirmDialogProps) {
  const t = useT();
  const [typed, setTyped] = useState("");
  const matched = name !== "" && matchesName(typed, name);

  function change(next: boolean) {
    if (next || pending) return;
    setTyped("");
    onCancel();
  }

  function confirm() {
    if (matched && !pending) onConfirm();
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t("restore.confirm.title")}</AlertDialogTitle>
          <AlertDialogDescription>{t("restore.confirm.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        <TypeToConfirm
          name={name}
          value={typed}
          onChange={setTyped}
          onConfirm={confirm}
          disabled={pending}
        />
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("restore.confirm.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={!matched || pending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={confirm}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("restore.confirm.pending")}
              </>
            ) : (
              t("restore.confirm.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
