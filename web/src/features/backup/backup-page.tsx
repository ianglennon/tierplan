// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Info } from "lucide-react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { DownloadCard } from "@/features/backup/download-card";
import { MasterKeyNote } from "@/features/backup/master-key-note";
import { useT } from "@/i18n/use-t";

// BackupPage is Settings > Backup: what a backup holds, the download and the
// master key note, in the UI-SPEC order, 32px apart. Later plans add the schedule
// and restore cards between the download and the note. The page sits in the
// narrow (896px) Settings column.
export function BackupPage() {
  const t = useT();
  return (
    <div className="flex flex-col gap-8">
      <h2 className="text-lg font-semibold">{t("backup.title")}</h2>
      {/* A note, not an alert: it is always there and must not be announced as news. */}
      <Alert role="note">
        <Info aria-hidden="true" />
        <AlertDescription className="text-foreground">{t("backup.intro")}</AlertDescription>
      </Alert>
      <DownloadCard />
      <MasterKeyNote />
    </div>
  );
}
