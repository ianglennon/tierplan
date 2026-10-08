// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { type BackupFileItem, backupFilesQuery, downloadBackupFile, saveBlob } from "@/api/backup";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Truncated } from "@/features/access/truncated";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";
import { toastError } from "@/lib/toast";

// SavedBackupsTable lists the scheduled backups on disk, newest first, each with a
// Download. The names are monospace and truncate with the whole name in a tooltip.
export function SavedBackupsTable() {
  const t = useT();
  const files = useQuery(backupFilesQuery);
  const [busy, setBusy] = useState<string | null>(null);

  async function download(name: string) {
    if (busy) return;
    setBusy(name);
    try {
      const file = await downloadBackupFile(name);
      saveBlob(file.blob, file.name);
    } catch {
      toastError(t("schedule.files.downloadFailed"));
    } finally {
      setBusy(null);
    }
  }

  const items: BackupFileItem[] = files.data ?? [];
  return (
    <>
      <Separator />
      <h4 className="text-sm font-semibold">{t("schedule.files.title")}</h4>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t("schedule.files.file")}</TableHead>
            <TableHead>{t("schedule.files.size")}</TableHead>
            <TableHead>{t("schedule.files.created")}</TableHead>
            <TableHead className="w-0">
              <span className="sr-only">{t("schedule.files.actions")}</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((f) => (
            <TableRow key={f.name}>
              <TableCell>
                <Truncated text={f.name} className="font-mono text-xs" />
              </TableCell>
              <TableCell>{formatBytes(f.size)}</TableCell>
              <TableCell>
                <RelativeTime value={f.created} />
              </TableCell>
              <TableCell className="text-right">
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  className="max-md:h-11"
                  aria-label={t("schedule.files.downloadAria", { name: f.name })}
                  disabled={busy !== null}
                  onClick={() => void download(f.name)}
                >
                  {t("schedule.files.download")}
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </>
  );
}
