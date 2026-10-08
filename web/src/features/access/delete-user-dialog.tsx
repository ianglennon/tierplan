// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import { deleteUser } from "@/api/users";
import { NeedsTooltip } from "@/components/needs-tooltip";
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
import { useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface DeleteUserDialogProps {
  // The user to delete, or null while the dialog is closed. lastInstanceAdmin
  // says the server would refuse, so the button is unavailable with the reason.
  user: { id: string; username: string; lastInstanceAdmin?: boolean } | null;
  onClose: () => void;
  // Called once the user is deleted.
  onDone: () => void;
  // Called when the answer shows what the screen holds is out of date.
  onStale: () => void;
}

// DeleteUserDialog is "Delete {username}?" (D-22c, D-18). Deleting is permanent,
// so the username must be typed before the button works. Focus starts on Cancel.
// It cannot be dismissed while the request is in flight, and a refusal stays
// inside the dialog.
export function DeleteUserDialog({ user, onClose, onDone, onStale }: DeleteUserDialogProps) {
  const t = useT();
  const { data: me } = useMe();
  const [typed, setTyped] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const username = user?.username ?? "";
  const matched = matchesName(typed, username);
  const isSelf = user !== null && me?.user.id === user.id;

  async function remove() {
    if (!user || !matched || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = await deleteUser(user.id);
      if (result.kind === "ok") {
        toastSuccess(t("userpage.delete.done", { username }));
        setTyped("");
        onDone();
        return;
      }
      if (result.kind === "last_instance_admin") setError(t("userpage.errors.lastAdmin"));
      else if (result.kind === "not_found") setError(t("userpage.errors.gone", { username }));
      else setError(t("userpage.delete.failed", { username }));
      onStale();
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  function change(open: boolean) {
    if (open || pending) return;
    setError(null);
    setTyped("");
    onClose();
  }

  const confirm = (
    <Button
      type="button"
      aria-disabled={!matched || pending}
      className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
      onClick={() => void remove()}
    >
      {pending ? (
        <>
          <Spinner aria-hidden="true" />
          {t("userpage.delete.pending")}
        </>
      ) : (
        t("userpage.delete.confirm")
      )}
    </Button>
  );

  return (
    <AlertDialog open={user !== null} onOpenChange={change}>
      <AlertDialogContent>
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("userpage.delete.title", { username })}
          </AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {t("userpage.delete.body", { username })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {isSelf ? (
          <Alert>
            <AlertDescription>{t("userpage.delete.ownAccount")}</AlertDescription>
          </Alert>
        ) : null}
        <TypeToConfirm
          name={username}
          value={typed}
          onChange={setTyped}
          onConfirm={() => void remove()}
          disabled={pending}
        />
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("userpage.delete.cancel")}
          </AlertDialogCancel>
          <NeedsTooltip reason={user?.lastInstanceAdmin ? t("needs.lastAdmin") : undefined}>
            {confirm}
          </NeedsTooltip>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
