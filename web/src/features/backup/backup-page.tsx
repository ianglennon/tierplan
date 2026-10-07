// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { DownloadCard } from "@/features/backup/download-card";
import { useT } from "@/i18n/use-t";

// BackupPage is Settings > Backup: what a backup holds and the download. Later
// plans add the schedule and restore cards between the download and the note.
export function BackupPage() {
  const t = useT();
  return (
    <div className="flex flex-col gap-8">
      <h2 className="text-lg font-semibold">{t("backup.title")}</h2>
      <DownloadCard />
    </div>
  );
}
