// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { RestoreSummary } from "@/api/backup";
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";

// BackupSummary is "What's in this backup", shared by Settings and the setup
// wizard. Counts of 0 are shown as 0.
export function BackupSummary({ summary }: { summary: RestoreSummary }) {
  const t = useT();
  const counts = [
    t("restore.summary.users", { count: summary.counts.users }),
    t("restore.summary.teams", { count: summary.counts.teams }),
    t("restore.summary.environments", { count: summary.counts.environments }),
    t("restore.summary.auditEntries", { count: summary.counts.auditEntries }),
  ].join(", ");
  return (
    <section
      aria-labelledby="restore-summary-title"
      className="flex min-w-0 flex-col gap-2 rounded-lg border p-3"
    >
      <h4 id="restore-summary-title" className="text-sm font-semibold">
        {t("restore.summary.title")}
      </h4>
      <ul className="flex min-w-0 flex-col gap-1 text-sm">
        <li className="break-words">{t("restore.summary.from", { name: summary.instanceName })}</li>
        <li>{t("restore.summary.made", { date: formatAbsolute(summary.created) })}</li>
        <li>{t("restore.summary.version", { version: summary.appVersion })}</li>
        <li>{counts}</li>
      </ul>
    </section>
  );
}
