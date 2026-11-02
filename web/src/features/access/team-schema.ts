// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { z } from "@/lib/zod-config";

// The team name rule the Create team dialog and the General card share, so a
// name is judged the same way wherever it is typed. It is UX only: the server
// checks the name again and names the field at fault.

// The same limit the server holds a team name to, counted in characters.
export const MAX_TEAM_NAME = 64;

export const TEAM_KEY = {
  name: "teams.create.errors.name",
  tooLong: "teams.create.errors.tooLong",
  taken: "teams.create.errors.taken",
  server: "teams.create.errors.server",
} as const;

const chars = (s: string) => [...s].length;

// Messages are i18n keys, never literals.
export const teamNameRule = z
  .string()
  .trim()
  .min(1, { error: TEAM_KEY.name })
  .refine((v) => chars(v) <= MAX_TEAM_NAME, { error: TEAM_KEY.tooLong });

// The server's field code for the name, from POST /api/teams and PATCH
// /api/teams/{id}: an empty or too long name.
export const TEAM_NAME_FIELD_CODE = "invalid_team_name";
