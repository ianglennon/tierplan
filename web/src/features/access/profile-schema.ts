// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { z } from "zod";

// The profile rules the Create user dialog and the Profile card share, so a
// display name or email is judged the same way wherever it is typed. They are
// UX only: the server checks every field again and names the ones at fault.

// The same limit the server holds a display name to, counted in characters.
export const MAX_DISPLAY_NAME = 64;

export const PROFILE_KEY = {
  email: "users.create.errors.email",
  displayName: "users.create.errors.displayName",
  server: "users.create.errors.server",
} as const;

const chars = (s: string) => [...s].length;

// Messages are i18n keys, never literals.
export const displayNameRule = z
  .string()
  .trim()
  .refine((v) => chars(v) <= MAX_DISPLAY_NAME, { error: PROFILE_KEY.displayName });

export const emailRule = z
  .string()
  .trim()
  .refine((v) => v === "" || z.email().safeParse(v).success, { error: PROFILE_KEY.email });

// The server's field codes for these two fields, from POST /api/users and
// PATCH /api/users/{id}.
export const PROFILE_SERVER_FIELD = {
  invalid_email: { field: "email", messageKey: PROFILE_KEY.email },
  display_name_too_long: { field: "displayName", messageKey: PROFILE_KEY.displayName },
} as const;
