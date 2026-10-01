// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { z } from "zod";

import type { Problem } from "@/api/problem";

export type SetupField = "token" | "username" | "password" | "instanceName" | "publicUrl";

export interface RoutedError {
  step: 1 | 2 | 3 | null;
  field?: SetupField;
  messageKey: string;
}

export const tokenSchema = z.string();
export const adminSchema = z.object({
  username: z.string(),
  password: z.string(),
  confirm: z.string(),
});
export const instanceSchema = z.object({ instanceName: z.string(), publicUrl: z.string() });

export function routeServerFieldErrors(_problem: Problem): RoutedError[] {
  return [];
}

export function routeServerError(_problem: Problem): RoutedError {
  return { step: null, messageKey: "" };
}
