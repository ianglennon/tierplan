// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useRef, useState } from "react";

import { type OneTimeLink, resetUserPassword } from "@/api/users";
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

interface ResetPasswordDialogProps {
  // The user whose password is reset, or null while the dialog is closed.
  user: { id: string; username: string } | null;
  onClose: () => void;
  // Called with the link once the server made it. The caller shows it and is the
  // only place that holds it.
  onGenerated: (user: { id: string; username: string }, link: OneTimeLink) => void;
  // Called when the answer shows what the screen holds is out of date: the user
  // is disabled or gone.
  onStale: () => void;
}

// ResetPasswordDialog is "Reset password for {username}?" (D-22b). It says what
// happens first: the sessions end now, and the admin gets a one-time link.
// Nothing is generated until the button is pressed. It cannot be dismissed while
// the request is in flight, and a failure stays inside the dialog.
export function ResetPasswordDialog({
  user,
  onClose,
  onGenerated,
  onStale,
}: ResetPasswordDialogProps) {
  const t = useT();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const username = user?.username ?? "";

  async function generate() {
    if (!user || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = await resetUserPassword(user.id);
      if (result.kind === "ok") {
        onGenerated(user, result.link);
        return;
      }
      if (result.kind === "disabled") setError(t("userpage.reset.disabled", { username }));
      else if (result.kind === "not_found") setError(t("userpage.errors.gone", { username }));
      else setError(t("userpage.reset.failed"));
      onStale();
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  function change(open: boolean) {
    if (open || pending) return;
    setError(null);
    onClose();
  }

  return (
    <AlertDialog open={user !== null} onOpenChange={change}>
      <AlertDialogContent>
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("userpage.reset.title", { username })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("userpage.reset.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription className="break-words">{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("userpage.reset.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            disabled={pending}
            className={TOUCH_HEIGHT}
            onClick={() => void generate()}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("userpage.reset.pending")}
              </>
            ) : (
              t("userpage.reset.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
