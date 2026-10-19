// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
import { useT } from "@/i18n/use-t";

interface CancelDeployDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Called once when the person confirms. The dialog closes itself.
  onConfirm: () => void;
}

// CancelDeployDialog is "Cancel this deploy?" (UI-SPEC "Deploy console"): an alert dialog that says
// what a cancel does and does not do. Cancelling a deploy is reversible by deploying again, so the
// confirm has the primary treatment and not the destructive one. Focus starts on Keep deploying, the
// safe answer, so Enter does not cancel by accident.
export function CancelDeployDialog({ open, onOpenChange, onConfirm }: CancelDeployDialogProps) {
  const t = useT();
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          <AlertDialogTitle>{t("stackDeploy.cancel.title")}</AlertDialogTitle>
          <AlertDialogDescription>{t("stackDeploy.cancel.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel className="max-md:h-11">
            {t("stackDeploy.cancel.keep")}
          </AlertDialogCancel>
          <AlertDialogAction className="max-md:h-11" onClick={onConfirm}>
            {t("stackDeploy.cancel.confirm")}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
