// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useState } from "react";

import { useStopACME } from "@/api/https";
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
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface StopDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// StopDialog is "Stop using Let's Encrypt?" (D-14). Nothing is lost by stopping (the account stays,
// issuing again is one click, nobody is signed out), so there is nothing to type. Focus starts on
// "Keep Let's Encrypt", and nothing closes the dialog while the request is in flight. A failure stays
// in the dialog; a success closes it once the status says the certificate is self-signed again.
export function StopDialog({ open, onOpenChange }: StopDialogProps) {
  const t = useT();
  const stop = useStopACME();
  const [failed, setFailed] = useState(false);
  const pending = stop.isPending;

  function change(next: boolean) {
    if (!next && pending) return;
    if (!next) setFailed(false);
    onOpenChange(next);
  }

  async function confirm() {
    if (pending) return;
    setFailed(false);
    try {
      await stop.mutateAsync();
    } catch {
      setFailed(true);
      return;
    }
    toastSuccess(t("https.stop.done"));
    onOpenChange(false);
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t("https.stop.title")}</AlertDialogTitle>
          <AlertDialogDescription>{t("https.stop.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {failed ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{t("common.mutationFailed")}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("https.stop.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={pending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void confirm()}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" role="presentation" />
                {t("https.stop.pending")}
              </>
            ) : (
              t("https.stop.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
