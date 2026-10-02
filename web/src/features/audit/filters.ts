// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { z } from "zod";

export const RANGES = ["1h", "24h", "7d", "30d", "all", "custom"] as const;
export type Range = (typeof RANGES)[number];

export const KNOWN_ACTIONS: readonly string[] = [];

export const auditSearchSchema = z.object({
  user: z.string().optional(),
  action: z.array(z.string()).optional(),
  range: z.enum(RANGES).default("24h"),
  from: z.string().optional(),
  to: z.string().optional(),
});

export type AuditSearch = z.input<typeof auditSearchSchema>;

type Translate = (key: string, options?: Record<string, unknown>) => string;

export function rangeFor(_search: AuditSearch, _now: Date): { from?: string; to?: string } {
  return {};
}

export function actionLabel(key: string, _t: Translate): string {
  return key;
}

export function listParams(_search: AuditSearch, _now: Date): URLSearchParams {
  return new URLSearchParams();
}

export function exportUrl(_format: "csv" | "json", _search: AuditSearch, _now: Date): string {
  return "";
}
