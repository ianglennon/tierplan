// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useState } from "react";

import { useRemoveACMEDNSToken } from "@/api/https";
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

interface RemoveTokenDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // How many test TXT records TierPlan failed to delete. It needs the token to delete them, so the
  // dialog says they stay when the token goes (D-18).
  leftOverCount: number;
}

// RemoveTokenDialog is "Remove the Cloudflare token?" (D-05). Nothing needs typing: the token can only
// be removed while HTTP-01 is the saved method, so no renewal depends on it. Focus starts on "Keep
// token", and nothing closes the dialog while the request is in flight. A failure, including a 409
// from a page that was out of date, stays in the dialog.
export function RemoveTokenDialog({ open, onOpenChange, leftOverCount }: RemoveTokenDialogProps) {
  const t = useT();
  const remove = useRemoveACMEDNSToken();
  const [failed, setFailed] = useState(false);
  const pending = remove.isPending;

  function change(next: boolean) {
    if (!next && pending) return;
    if (!next) setFailed(false);
    onOpenChange(next);
  }

  async function confirm() {
    if (pending) return;
    setFailed(false);
    try {
      await remove.mutateAsync();
    } catch {
      setFailed(true);
      return;
    }
    toastSuccess(t("https.dns.remove.done"));
    onOpenChange(false);
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t("https.dns.remove.title")}</AlertDialogTitle>
          <AlertDialogDescription>{t("https.dns.remove.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {leftOverCount > 0 ? (
          <p className="text-sm text-muted-foreground">
            {t("https.dns.remove.leftOver", { count: leftOverCount })}
          </p>
        ) : null}
        {failed ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{t("common.mutationFailed")}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("https.dns.remove.cancel")}
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
                {t("https.dns.remove.pending")}
              </>
            ) : (
              t("https.dns.remove.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
