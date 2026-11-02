// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Info } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { BackupRestored } from "@/features/backup/backup-restored";
import { DownloadCard } from "@/features/backup/download-card";
import { MasterKeyNote } from "@/features/backup/master-key-note";
import { RestorePanel } from "@/features/backup/restore-panel";
import { SchedulePanel } from "@/features/backup/schedule-panel";
import { useT } from "@/i18n/use-t";

// BackupPage is Settings > Backup: what a backup holds, the download and the
// master key note, in the UI-SPEC order, 32px apart. The scheduled backups card (with
// the saved backups under it) and the restore card sit between the download and the note.
// The page sits in the narrow (896px) Settings column. Once a restore has worked
// the session is gone, and the whole page gives way to "Backup restored".
export function BackupPage() {
  const t = useT();
  const [restored, setRestored] = useState(false);
  // The function that lets the app react to refusals again; it is held back until
  // this page is left, so the restart's 401s do not move the browser meanwhile.
  const release = useRef<(() => void) | null>(null);
  useEffect(() => () => release.current?.(), []);
  if (restored) return <BackupRestored />;
  return (
    <div className="flex flex-col gap-8">
      <h2 className="text-lg font-semibold">{t("backup.title")}</h2>
      {/* A note, not an alert: it is always there and must not be announced as news. */}
      <Alert role="note">
        <Info aria-hidden="true" />
        <AlertDescription className="text-foreground">{t("backup.intro")}</AlertDescription>
      </Alert>
      <DownloadCard />
      <SchedulePanel />
      <RestorePanel
        onRestored={(done) => {
          release.current = done;
          setRestored(true);
        }}
      />
      <MasterKeyNote />
    </div>
  );
}
