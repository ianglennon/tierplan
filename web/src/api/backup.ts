// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";

// The passphrase rule the server states, used until (or unless) it is read.
export const DEFAULT_MIN_PASSPHRASE = 16;

const FALLBACK_NAME = "bayplan-backup.bayplan";

// The ["backup", "info"] query: the shortest passphrase a backup accepts and the
// file extension a backup file has. The server checks the rule again.
export const backupInfoQuery = queryOptions({
  queryKey: ["backup", "info"],
  queryFn: async () => {
    const { data } = await api.GET("/api/backup/info");
    if (!data) throw new Error("GET /api/backup/info failed");
    return data;
  },
  staleTime: 5 * 60_000,
});

// BackupExportError is a backup the server refused or could not make. type is the
// problem type ("passphrase_too_short", "backup_busy", "audit_chain_broken"), when
// it sent one. It never carries the passphrase.
export class BackupExportError extends Error {
  readonly status: number;
  readonly type: string | undefined;

  constructor(status: number, type?: string) {
    super(`POST /api/backup/export failed with ${status}`);
    this.name = "BackupExportError";
    this.status = status;
    this.type = type;
  }
}

export interface BackupFile {
  blob: Blob;
  name: string;
}

// fileNameOf reads the file name from a Content-Disposition header.
function fileNameOf(header: string | null): string {
  const match = header?.match(/filename="([^"\\]+)"/);
  return match?.[1] ?? FALLBACK_NAME;
}

// exportBackup asks the server for an encrypted backup. The passphrase travels in
// the JSON body of a POST, never in the address, and is not kept anywhere here.
export async function exportBackup(passphrase: string): Promise<BackupFile> {
  const { data, error, response } = await api.POST("/api/backup/export", {
    body: { passphrase },
    parseAs: "blob",
  });
  if (!response.ok || data === undefined) {
    // A failed answer is read as text, not JSON, with parseAs "blob": the type is
    // taken from it when it parses.
    let type: string | undefined;
    if (typeof error === "string") {
      try {
        type = readProblem(JSON.parse(error), response).type;
      } catch {
        // Not JSON: a proxy's page. The status is all there is.
      }
    } else {
      type = readProblem(error, response).type;
    }
    throw new BackupExportError(response.status, type);
  }
  return { blob: data, name: fileNameOf(response.headers.get("Content-Disposition")) };
}

// saveBlob hands a file to the browser: an object URL on a temporary link that is
// clicked and then removed, and the URL released.
export function saveBlob(blob: Blob, name: string): void {
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.hidden = true;
  document.body.append(link);
  link.click();
  link.remove();
  // Let the click start the download before the URL goes.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
