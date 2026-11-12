// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import { useState } from "react";

import { ACMEControlError, httpsStatusQuery, useStopACME } from "@/api/https";
import { useNow } from "@/components/relative-time";
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
import { hstsView } from "@/features/https/card-state";
import { formatDate } from "@/features/https/copy";
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
// in the dialog; a success closes it once the status says the certificate is self-signed again, and a
// 409 (the page was out of date, or HSTS was turned on: hsts_on) closes it so the re-read card can say
// why, with no toast. While browsers may still hold an HSTS policy TierPlan sent (hsts.forgetBy is in the
// future) it warns, and the confirm stays available: with HSTS off the dialog warns and does not block.
export function StopDialog({ open, onOpenChange }: StopDialogProps) {
  const t = useT();
  const stop = useStopACME();
  const now = useNow();
  const status = useQuery(httpsStatusQuery).data;
  const forgetBy = status ? hstsView(status, now)?.forgetBy : undefined;
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
    } catch (error) {
      // A 409 means the page was out of date (a run started, or Let's Encrypt was already stopped).
      // The status has been read again and the card says what it is now, so the dialog just closes.
      if (error instanceof ACMEControlError && error.status === 409) {
        onOpenChange(false);
        return;
      }
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
        {forgetBy ? (
          <Alert variant="warning" role="note">
            <TriangleAlert aria-hidden="true" />
            <AlertDescription>
              {t("https.stop.browserWarning", { date: formatDate(forgetBy) })}
            </AlertDescription>
          </Alert>
        ) : null}
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
