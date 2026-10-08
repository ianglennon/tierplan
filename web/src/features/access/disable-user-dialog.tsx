// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import { disableUser, type UserDetail } from "@/api/users";
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

interface DisableUserDialogProps {
  // The user to disable, or null while the dialog is closed.
  user: { id: string; username: string } | null;
  onClose: () => void;
  // Called with the user as the server now has them.
  onDone: (user: UserDetail) => void;
  // Called when the answer shows what the screen holds is out of date.
  onStale: () => void;
}

// DisableUserDialog is "Disable {username}?" (D-22c). Nothing happens until the
// button is pressed. It cannot be dismissed while the request is in flight, and
// a refusal stays inside the dialog: the last active instance admin cannot be
// disabled, and the screen is read again.
export function DisableUserDialog({ user, onClose, onDone, onStale }: DisableUserDialogProps) {
  const t = useT();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const username = user?.username ?? "";

  async function disable() {
    if (!user || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = await disableUser(user.id);
      if (result.kind === "ok") {
        toastSuccess(t("userpage.disable.done", { username }));
        onDone(result.user);
        return;
      }
      if (result.kind === "last_instance_admin") {
        setError(t("userpage.errors.lastAdmin"));
      } else if (result.kind === "not_found") {
        setError(t("userpage.errors.gone", { username }));
      } else {
        setError(t("userpage.disable.failed", { username }));
      }
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
            {t("userpage.disable.title", { username })}
          </AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {t("userpage.disable.body", { username })}
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
            {t("userpage.disable.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            disabled={pending}
            className={TOUCH_HEIGHT}
            onClick={() => void disable()}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("userpage.disable.pending")}
              </>
            ) : (
              t("userpage.disable.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
