// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useRef, useState } from "react";

import { type OneTimeLink, regenerateInvite, type UserSummary } from "@/api/users";
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

interface RegenerateInviteDialogProps {
  // The user whose invite is replaced, or null while the dialog is closed.
  user: UserSummary | null;
  onClose: () => void;
  // Called with the new link once the server made it. The caller shows it and
  // is the only place that holds it.
  onGenerated: (user: UserSummary, link: OneTimeLink) => void;
  // Called when the answer shows the list is out of date: the user is gone or
  // has set a password meanwhile.
  onStale: () => void;
}

// RegenerateInviteDialog is "Generate a new invite link?" (D-22a). Nothing is
// generated until the button is pressed. It cannot be dismissed while the
// request is in flight, and a failure stays inside the dialog.
export function RegenerateInviteDialog({
  user,
  onClose,
  onGenerated,
  onStale,
}: RegenerateInviteDialogProps) {
  const t = useT();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);

  async function generate() {
    if (!user || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = await regenerateInvite(user.id);
      if (result.kind === "ok") {
        onGenerated(user, result.link);
        return;
      }
      if (result.kind === "not_invited") {
        setError(t("users.regenerate.notInvited", { username: user.username }));
      } else {
        setError(t("users.link.failed"));
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
          <AlertDialogTitle>{t("users.regenerate.title")}</AlertDialogTitle>
          <AlertDialogDescription>{t("users.regenerate.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("users.regenerate.cancel")}
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
                {t("users.regenerate.pending")}
              </>
            ) : (
              t("users.regenerate.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
