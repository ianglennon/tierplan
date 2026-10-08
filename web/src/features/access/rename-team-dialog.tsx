// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useState } from "react";

import type { Team } from "@/api/teams";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { RenameTeamForm } from "@/features/access/rename-team-form";
import { useT } from "@/i18n/use-t";

interface RenameTeamDialogProps {
  // The team to rename, or null while the dialog is closed.
  team: Pick<Team, "id" | "name" | "updatedAt"> | null;
  onClose: () => void;
  // Called once the name is saved.
  onSaved: (team: Team) => void;
  // Called when the answer shows what the screen holds is out of date.
  onStale: () => void;
}

// RenameTeamDialog is "Rename team", opened from the Teams table row menu. It
// cannot be dismissed while the request is in flight; a failure stays inside it.
export function RenameTeamDialog({ team, onClose, onSaved, onStale }: RenameTeamDialogProps) {
  const t = useT();
  const [pending, setPending] = useState(false);
  return (
    <Dialog
      open={team !== null}
      onOpenChange={(open) => {
        if (!open && !pending) onClose();
      }}
    >
      <DialogContent showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>{t("teams.rename.title")}</DialogTitle>
          <DialogDescription>{t("teams.rename.body")}</DialogDescription>
        </DialogHeader>
        {team ? (
          <RenameTeamForm
            team={team}
            onSaved={onSaved}
            onStale={onStale}
            onPendingChange={setPending}
            cancel={
              <Button
                type="button"
                variant="outline"
                className="h-11 md:h-8"
                disabled={pending}
                onClick={onClose}
              >
                {t("teams.create.cancel")}
              </Button>
            }
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
