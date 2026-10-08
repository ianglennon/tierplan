// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api, getCsrfToken } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import { apiUrl } from "@/lib/base-path";

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

export type RestoreSummary = components["schemas"]["RestoreSummaryBody"];

// What a stored upload answers: the ID the check and the restore name it by.
export interface RestoreUpload {
  uploadId: string;
  size: number;
  expiresAt: string;
}

// RestoreError is a restore step the server refused, or a proxy in front of it
// did. type is the problem type ("backup_unreadable", "backup_newer", ...) when
// the answer had one; detail is the server's own sentence. Neither carries the
// passphrase or the setup token.
export class RestoreError extends Error {
  readonly status: number;
  readonly type: string | undefined;
  readonly detail: string | undefined;

  constructor(status: number, type?: string, detail?: string) {
    super(`restore step failed with ${status}`);
    this.name = "RestoreError";
    this.status = status;
    this.type = type;
    this.detail = detail;
  }
}

function detailOf(body: unknown): string | undefined {
  if (typeof body !== "object" || body === null || !("detail" in body)) return undefined;
  return typeof body.detail === "string" ? body.detail : undefined;
}

// restoreErrorOf reads a failed answer into a RestoreError. body is whatever
// openapi-fetch or a plain fetch gave: a parsed problem, or a string (or nothing)
// when a proxy answered with a page of its own.
function restoreErrorOf(body: unknown, response: { status: number }): RestoreError {
  let parsed = body;
  if (typeof body === "string") {
    try {
      parsed = JSON.parse(body);
    } catch {
      parsed = undefined;
    }
  }
  return new RestoreError(response.status, readProblem(parsed, response).type, detailOf(parsed));
}

// A raw POST of the file to the upload route, which is not in the typed client:
// its body is the file itself. The CSRF token goes in the header the way the
// typed client sends it; the passphrase is not part of this call.
async function postFile(
  path: string,
  file: File,
  extraHeaders: Record<string, string> = {},
): Promise<RestoreUpload> {
  const headers: Record<string, string> = {
    "Content-Type": "application/octet-stream",
    ...extraHeaders,
  };
  const csrf = getCsrfToken();
  if (csrf) headers["X-CSRF-Token"] = csrf;
  const response = await globalThis.fetch(apiUrl(path), {
    method: "POST",
    headers,
    body: file,
    credentials: "same-origin",
  });
  if (!response.ok) {
    const body: unknown = await response.json().catch(() => undefined);
    throw restoreErrorOf(body, response);
  }
  return (await response.json()) as RestoreUpload;
}

// uploadRestore stores the file on the server. Nothing is opened yet.
export function uploadRestore(file: File): Promise<RestoreUpload> {
  return postFile("/api/backup/restore/upload", file);
}

// checkRestore opens the stored file with the passphrase and says what is in it.
export async function checkRestore(uploadId: string, passphrase: string): Promise<RestoreSummary> {
  const { data, error, response } = await api.POST("/api/backup/restore/check", {
    body: { uploadId, passphrase },
  });
  if (!data) throw restoreErrorOf(error, response);
  return data;
}

// applyRestore replaces this instance with the backup. The 202 means Bayplan is
// about to restart; confirm is the instance name the person typed.
export async function applyRestore(
  uploadId: string,
  passphrase: string,
  confirm: string,
): Promise<void> {
  const { error, response } = await api.POST("/api/backup/restore/apply", {
    body: { uploadId, passphrase, confirm },
  });
  if (!response.ok) throw restoreErrorOf(error, response);
}

// The wizard's three steps. The setup token is the credential: a header on the
// raw upload and a member of the body elsewhere, never in the address.
export function uploadSetupRestore(file: File, setupToken: string): Promise<RestoreUpload> {
  return postFile("/api/setup/restore/upload", file, { "X-Setup-Token": setupToken });
}

export async function checkSetupRestore(
  uploadId: string,
  passphrase: string,
  setupToken: string,
): Promise<RestoreSummary> {
  const { data, error, response } = await api.POST("/api/setup/restore/check", {
    body: { uploadId, passphrase, setupToken },
  });
  if (!data) throw restoreErrorOf(error, response);
  return data;
}

export async function applySetupRestore(
  uploadId: string,
  passphrase: string,
  setupToken: string,
): Promise<void> {
  const { error, response } = await api.POST("/api/setup/restore/apply", {
    body: { uploadId, passphrase, setupToken },
  });
  if (!response.ok) throw restoreErrorOf(error, response);
}

// A message to show: an i18n key and what fills it.
export interface RestoreMessage {
  key: string;
  values?: Record<string, string>;
}

// The version a "newer Bayplan" answer names, as in "made by a newer Bayplan (v1.2.3)".
function newerVersion(detail: string | undefined): string | undefined {
  return detail?.match(/newer Bayplan \(([^)]+)\)/)?.[1];
}

// restoreProblem turns whatever a restore step threw into the message to show.
// Any 413 is a cut-off upload, whoever answered it: the server's own limit and a
// reverse proxy's both mean the file did not arrive whole. A thrown error that is
// not a RestoreError is a connection that failed.
export function restoreProblem(err: unknown): RestoreMessage {
  if (!(err instanceof RestoreError)) return { key: "common.errors.unreachable" };
  if (err.status === 413) return { key: "restore.errors.cutOff" };
  if (err.status === 429 || err.type === "rate_limited")
    return { key: "restore.errors.rateLimited" };
  switch (err.type) {
    case "backup_unreadable":
      return { key: "restore.errors.unreadable" };
    case "not_a_backup":
      return { key: "restore.errors.notABackup" };
    case "upload_incomplete":
    case "upload_too_large":
      return { key: "restore.errors.cutOff" };
    case "restore_busy":
      return { key: "restore.errors.busy" };
    case "backup_newer": {
      const version = newerVersion(err.detail);
      return version
        ? { key: "restore.errors.newer", values: { version } }
        : { key: "restore.errors.newerUnnamed" };
    }
    default:
      return { key: "restore.errors.check" };
  }
}

export type BackupSchedule = components["schemas"]["BackupScheduleBody"];
export type BackupFileItem = components["schemas"]["BackupFileBody"];

export const SCHEDULE_KEY = ["backup", "schedule"] as const;
export const BACKUP_FILES_KEY = ["backup", "files"] as const;

// The ["backup", "schedule"] query: whether scheduled backups are on, when, where
// they go and how the last runs went. The passphrase is never in it.
export const scheduleQuery = queryOptions({
  queryKey: SCHEDULE_KEY,
  queryFn: async () => {
    const { data } = await api.GET("/api/backup/schedule");
    if (!data) throw new Error("GET /api/backup/schedule failed");
    return data;
  },
});

// The ["backup", "files"] query: the scheduled backups on disk, newest first.
export const backupFilesQuery = queryOptions({
  queryKey: BACKUP_FILES_KEY,
  queryFn: async () => {
    const { data } = await api.GET("/api/backup/files");
    if (!data) throw new Error("GET /api/backup/files failed");
    return data.items ?? [];
  },
});

// What the schedule form sends. An empty passphrase keeps the one already saved.
export interface ScheduleValues {
  enabled: boolean;
  frequency: BackupSchedule["frequency"];
  weekday: number;
  timeUtc: string;
  keep: number;
  passphrase: string;
}

// ScheduleSaveError is a schedule the server did not save. type is the problem type
// ("passphrase_too_short", "passphrase_required"), when it sent one. It never
// carries the passphrase.
export class ScheduleSaveError extends Error {
  readonly status: number;
  readonly type: string | undefined;

  constructor(status: number, type?: string) {
    super(`PUT /api/backup/schedule failed with ${status}`);
    this.name = "ScheduleSaveError";
    this.status = status;
    this.type = type;
  }
}

// saveSchedule stores the schedule. The passphrase travels in the JSON body of a
// PUT, is left out when empty, and is not kept anywhere here.
export async function saveSchedule(values: ScheduleValues): Promise<BackupSchedule> {
  const { passphrase, ...rest } = values;
  const { data, error, response } = await api.PUT("/api/backup/schedule", {
    body: passphrase ? { ...rest, passphrase } : rest,
  });
  if (!data) throw new ScheduleSaveError(response.status, readProblem(error, response).type);
  return data;
}

// downloadBackupFile fetches one saved backup so the browser can save it.
export async function downloadBackupFile(name: string): Promise<BackupFile> {
  const { data, response } = await api.GET("/api/backup/files/{name}", {
    params: { path: { name } },
    parseAs: "blob",
  });
  if (!response.ok || data === undefined) {
    throw new Error(`GET /api/backup/files/${name} failed with ${response.status}`);
  }
  return { blob: data, name: fileNameOf(response.headers.get("Content-Disposition")) };
}
