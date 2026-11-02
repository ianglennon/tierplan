// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useRef, useState } from "react";

import { deleteTeam, previewTeamImpact } from "@/api/teams";
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

interface DeleteTeamDialogProps {
  // The team to delete, or null while the dialog is closed.
  team: { id: string; name: string } | null;
  onClose: () => void;
  // Called once the team is deleted.
  onDone: () => void;
  // Called when the answer shows what the screen holds is out of date.
  onStale: () => void;
}

// DeleteTeamDialog is "Delete team {name}?" (D-27). A team holds no data, so the
// name is not typed: the impact list is the guard, and the button stays
// unavailable until it has loaded. Focus starts on Cancel. It cannot be
// dismissed while the request is in flight, and a failure stays inside the
// dialog.
export function DeleteTeamDialog({ team, onClose, onDone, onStale }: DeleteTeamDialogProps) {
  const t = useT();
  // Ready is remembered for the team it loaded for, so it can never carry over
  // to another one.
  const previewKey = team ? team.id : null;
  const [readyFor, setReadyFor] = useState<string | null>(null);
  const ready = previewKey !== null && readyFor === previewKey;
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const name = team?.name ?? "";

  async function remove() {
    if (!team || !ready || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = await deleteTeam(team.id);
      if (result.kind === "ok") {
        toastSuccess(t("teams.delete.done", { team: name }));
        onDone();
        return;
      }
      setError(
        result.kind === "not_found"
          ? t("teams.delete.gone", { team: name })
          : t("teams.delete.failed", { team: name }),
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
    <AlertDialog open={team !== null} onOpenChange={change}>
      <AlertDialogContent>
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("teams.delete.title", { team: name })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("teams.delete.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {team ? (
          <ImpactPreview
            key={previewKey}
            load={() => previewTeamImpact(team.id, { op: "delete" })}
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
            {t("teams.delete.cancel")}
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
                {t("teams.delete.pending")}
              </>
            ) : (
              t("teams.delete.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
