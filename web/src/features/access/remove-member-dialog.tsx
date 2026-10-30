// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useRef, useState } from "react";

import { previewTeamImpact, removeTeamMember } from "@/api/teams";
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
import { ImpactPreview } from "@/features/access/impact-preview";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

export interface MembershipToRemove {
  team: { id: string; name: string };
  user: { id: string; username: string };
}

interface RemoveMemberDialogProps {
  // The membership to end, or null while the dialog is closed.
  membership: MembershipToRemove | null;
  onClose: () => void;
  // Called once the user is out of the team.
  onDone: () => void;
  // Called when the answer shows what the screen holds is out of date.
  onStale: () => void;
}

// RemoveMemberDialog is "Remove {username} from {team}?" (D-27), opened from the
// team's Members card and from the user's Teams card. The impact list says what
// the user loses and the button stays unavailable until it has loaded. Focus
// starts on Cancel. It cannot be dismissed while the request is in flight, and a
// failure stays inside the dialog.
export function RemoveMemberDialog({
  membership,
  onClose,
  onDone,
  onStale,
}: RemoveMemberDialogProps) {
  const t = useT();
  // Ready is remembered for the membership it loaded for, so it can never carry
  // over to another one.
  const previewKey = membership ? `${membership.team.id}:${membership.user.id}` : null;
  const [readyFor, setReadyFor] = useState<string | null>(null);
  const ready = previewKey !== null && readyFor === previewKey;
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const names = { username: membership?.user.username ?? "", team: membership?.team.name ?? "" };

  async function remove() {
    if (!membership || !ready || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = await removeTeamMember(membership.team.id, membership.user.id);
      if (result.kind === "ok") {
        toastSuccess(t("teams.remove.done", names));
        onDone();
        return;
      }
      setError(
        result.kind === "not_found"
          ? t("teams.remove.gone", names)
          : t("teams.remove.failed", names),
      );
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
    <AlertDialog open={membership !== null} onOpenChange={change}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle className="break-words">
            {t("teams.remove.title", names)}
          </AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {t("teams.remove.body", names)}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {membership ? (
          <ImpactPreview
            key={previewKey}
            load={() =>
              previewTeamImpact(membership.team.id, {
                op: "remove_member",
                userId: membership.user.id,
              })
            }
            onReadyChange={(r) => setReadyFor(r ? previewKey : null)}
          />
        ) : null}
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("teams.remove.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={!ready || pending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void remove()}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("teams.remove.pending")}
              </>
            ) : (
              t("teams.remove.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
