// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { Problem } from "@/api/problem";
import { z } from "@/lib/zod-config";

// Validation here is UX only. The server enforces every rule again and answers
// with the field codes routeServerError maps. Messages are i18n keys in the
// `setup` namespace, never literals (D-65).

export type SetupField = "token" | "username" | "password" | "instanceName" | "publicUrl";

export interface RoutedError {
  step: 1 | 2 | 3 | null;
  field?: SetupField;
  messageKey: string;
}

// MIN_PASSWORD_LENGTH mirrors auth.MinPasswordLength on the server. The
// common-password list is server-only.
export const MIN_PASSWORD_LENGTH = 12;
export const MAX_TOKEN_LENGTH = 128;
export const MAX_USERNAME_LENGTH = 32;
export const MAX_INSTANCE_NAME_LENGTH = 64;

const KEY = {
  tokenRequired: "setup.errors.tokenRequired",
  tokenInvalid: "setup.errors.tokenInvalid",
  rateLimited: "setup.errors.rateLimited",
  username: "setup.errors.username",
  passwordShort: "setup.errors.passwordShort",
  passwordCommon: "setup.errors.passwordCommon",
  passwordMismatch: "setup.errors.passwordMismatch",
  instanceNameRequired: "setup.errors.instanceNameRequired",
  instanceNameTooLong: "setup.errors.instanceNameTooLong",
  publicUrl: "setup.errors.publicUrl",
  server: "setup.errors.server",
} as const;

// chars counts characters the way the server does (runes), not UTF-16 units.
const chars = (s: string) => [...s].length;

export const tokenSchema = z
  .string()
  .trim()
  .min(1, { error: KEY.tokenRequired })
  .max(MAX_TOKEN_LENGTH, { error: KEY.tokenInvalid });

export const tokenFormSchema = z.object({ token: tokenSchema });
export type TokenValues = z.output<typeof tokenFormSchema>;

// lowerAscii lower-cases A to Z only, never Unicode, so what the username field
// shows on blur is exactly what the schema and the server go on to check.
export function lowerAscii(s: string): string {
  return s.replace(/[A-Z]/g, (c) => c.toLowerCase());
}

// D-35: 3 to 32 of a-z, 0-9, dot, dash, underscore, not case-sensitive. The
// characters are checked as typed and only then lower-cased by ASCII range, so
// a Unicode lookalike (the Kelvin sign) is refused instead of folding to "k".
const usernameSchema = z
  .string()
  .regex(new RegExp(`^[A-Za-z0-9._-]{3,${MAX_USERNAME_LENGTH}}$`), { error: KEY.username })
  .transform(lowerAscii);

// passwordSchema is the length rule of D-07, shared with the change-password
// dialog. The common-password and contains-the-username rules are server-only.
export const passwordSchema = z
  .string()
  .refine((s) => chars(s) >= MIN_PASSWORD_LENGTH, { error: KEY.passwordShort });

// The i18n keys the change-password dialog shares with the wizard.
export const PASSWORD_KEYS = {
  short: KEY.passwordShort,
  common: KEY.passwordCommon,
  mismatch: KEY.passwordMismatch,
} as const;

export const adminSchema = z
  .object({
    username: usernameSchema,
    password: passwordSchema,
    confirm: z.string(),
  })
  .refine((v) => v.password === v.confirm, { path: ["confirm"], error: KEY.passwordMismatch });

export type AdminInput = z.input<typeof adminSchema>;
export type AdminValues = z.output<typeof adminSchema>;

// isPublicUrl mirrors config.NormalisePublicURL: an absolute http or https
// address with a host, no credentials, no query and no fragment.
export function isPublicUrl(raw: string): boolean {
  if (/[?#]/.test(raw)) return false;
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return false;
  }
  return (
    (u.protocol === "http:" || u.protocol === "https:") &&
    u.host !== "" &&
    u.username === "" &&
    u.password === ""
  );
}

export const instanceSchema = z.object({
  instanceName: z
    .string()
    .trim()
    .refine((s) => chars(s) >= 1, { error: KEY.instanceNameRequired })
    .refine((s) => chars(s) <= MAX_INSTANCE_NAME_LENGTH, { error: KEY.instanceNameTooLong }),
  publicUrl: z.string().trim().refine(isPublicUrl, { error: KEY.publicUrl }),
});

export type InstanceInput = z.input<typeof instanceSchema>;
export type InstanceValues = z.output<typeof instanceSchema>;

// The settings form is the instance form plus the agent URL (plan 02-26, D-18).
// An agent URL is checked like the public URL, and an empty one is valid: it
// means agents dial the public URL.
export const settingsSchema = instanceSchema.extend({
  agentUrl: z
    .string()
    .trim()
    .refine((s) => s === "" || isPublicUrl(s), { error: KEY.publicUrl }),
});

export type SettingsInput = z.input<typeof settingsSchema>;
export type SettingsValues = z.output<typeof settingsSchema>;

// Which step owns which field. The key order is the order the fields appear on
// screen, which routeServerFieldErrors sorts by.
const FIELD_STEP: Record<SetupField, 1 | 2 | 3> = {
  token: 1,
  username: 2,
  password: 2,
  instanceName: 3,
  publicUrl: 3,
};
const FIELD_ORDER = Object.keys(FIELD_STEP);

const LOCATION_FIELD: Record<string, SetupField> = {
  "body.token": "token",
  "body.username": "username",
  "body.password": "password",
  "body.instanceName": "instanceName",
  "body.publicUrl": "publicUrl",
};

const CODE: Record<string, { field: SetupField; messageKey: string }> = {
  invalid_username: { field: "username", messageKey: KEY.username },
  password_too_short: { field: "password", messageKey: KEY.passwordShort },
  password_common: { field: "password", messageKey: KEY.passwordCommon },
  instance_name_required: { field: "instanceName", messageKey: KEY.instanceNameRequired },
  instance_name_too_long: { field: "instanceName", messageKey: KEY.instanceNameTooLong },
  invalid_public_url: { field: "publicUrl", messageKey: KEY.publicUrl },
};

const SERVER_ERROR: RoutedError = { step: null, messageKey: KEY.server };

// routeServerFieldErrors turns a server problem into everything the wizard has
// to show: field errors for the steps that own them, in screen order so the
// first entry is where to jump, or one form-level error. The list is never
// empty. A code this build does not know still lands on its field (found from
// the error location) with the generic server message, so a newer server never
// leaves the user on a step with no visible reason.
export function routeServerFieldErrors(problem: Problem): RoutedError[] {
  if (problem.status >= 500) return [SERVER_ERROR];
  if (problem.type === "invalid_setup_token") {
    return [{ step: 1, field: "token", messageKey: KEY.tokenInvalid }];
  }
  if (problem.type === "rate_limited") return [{ step: null, messageKey: KEY.rateLimited }];

  const routed: RoutedError[] = [];
  for (const f of problem.fields) {
    const known = CODE[f.message];
    const field = known?.field ?? LOCATION_FIELD[f.location];
    if (!field) continue;
    routed.push({ step: FIELD_STEP[field], field, messageKey: known?.messageKey ?? KEY.server });
  }
  if (routed.length === 0) return [SERVER_ERROR];
  // Screen order: earliest step first, then the field order within the step.
  return routed.sort(
    (a, b) => FIELD_ORDER.indexOf(a.field ?? "") - FIELD_ORDER.indexOf(b.field ?? ""),
  );
}

// routeServerError is the single most useful error: the first one of
// routeServerFieldErrors.
export function routeServerError(problem: Problem): RoutedError {
  return routeServerFieldErrors(problem)[0] ?? SERVER_ERROR;
}
