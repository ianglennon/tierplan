// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Info } from "lucide-react";

import { type KeyValueRow, KeyValueTable } from "@/components/key-value-table";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import type { UnreadableInfo } from "@/features/logs/log-stream";
import { useT } from "@/i18n/use-t";

// What the Logs tab shows, in place of the viewer, when the engine said it cannot read a
// container's log back (D-25 as amended): the container sends its logs somewhere Docker
// keeps no copy. It is information, not a fault, so it has the neutral Info icon and is
// not announced as an alert. The driver's options come with their values withheld from
// anyone who may not reveal secrets; a holder of secrets.reveal can show each one.
export function UnreadableNotice({ info }: { info: UnreadableInfo }) {
  const t = useT();
  const rows: KeyValueRow[] = info.options.map((o) =>
    o.masked || o.value === undefined
      ? { key: o.name, masked: true as const }
      : { key: o.name, masked: true as const, canReveal: true as const, value: o.value },
  );
  return (
    <div className="flex max-w-2xl flex-col gap-4">
      <Alert role="status">
        <Info aria-hidden="true" />
        <AlertTitle>{t("logs.unreadable.title")}</AlertTitle>
        <AlertDescription>
          {t("logs.unreadable.body", { driver: info.driver || "?" })}
        </AlertDescription>
      </Alert>
      {rows.length > 0 ? (
        <section className="flex flex-col gap-2">
          <h2 className="text-sm font-medium">{t("logs.unreadable.options")}</h2>
          <KeyValueTable
            rows={rows}
            label={t("logs.unreadable.options")}
            empty={t("logs.unreadable.noOptions")}
          />
        </section>
      ) : null}
    </div>
  );
}
