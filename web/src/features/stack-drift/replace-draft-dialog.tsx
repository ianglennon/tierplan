// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { RelativeTime } from "@/components/relative-time";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";

interface ReplaceDraftDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Who saved the draft that is about to be replaced, and when (RFC 3339).
  savedBy: string;
  savedAt: string;
  // The rebuild is on its way: the dialog stays open until it ends and cannot be dismissed meanwhile.
  pending: boolean;
  onReplace: () => void;
}

// ReplaceDraftDialog is the confirm of Accept live when a saved draft exists (UI-SPEC "Drift page", D-23): the rebuild
// replaces the draft with its files and variables, which are shared saved work, and cannot be undone. It is the same
// destructive treatment as Discard draft: focus starts on the safe button and only the replace is destructive.
export function ReplaceDraftDialog({
  open,
  onOpenChange,
  savedBy,
  savedAt,
  pending,
  onReplace,
}: ReplaceDraftDialogProps) {
  const t = useT();
  return (
    <AlertDialog
      open={open}
      onOpenChange={(next) => {
        // A rebuild on its way is not interrupted by closing the dialog.
        if (!pending) onOpenChange(next);
      }}
    >
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          <AlertDialogTitle>{t("stackAcceptLive.replace.title")}</AlertDialogTitle>
          <AlertDialogDescription>
            {t("stackAcceptLive.replace.before", { user: savedBy })}{" "}
            <RelativeTime value={savedAt} />
            {t("stackAcceptLive.replace.after")}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel autoFocus disabled={pending}>
            {t("stackAcceptLive.replace.keep")}
          </AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            aria-disabled={pending ? true : undefined}
            onClick={(e) => {
              // The dialog stays open until the rebuild has happened, or says why it did not.
              e.preventDefault();
              if (!pending) onReplace();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("stackAcceptLive.working")}
              </>
            ) : (
              t("stackAcceptLive.replace.confirm")
            )}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
