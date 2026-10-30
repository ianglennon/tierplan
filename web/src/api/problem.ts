// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

export interface ProblemField {
  location: string;
  message: string;
}

export interface Problem {
  status: number;
  type?: string;
  fields: ProblemField[];
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

// readProblem reads a huma problem+json body into the three things screens
// need: the HTTP status, the problem `type` and the per-field codes. `body` is
// whatever openapi-fetch returned as `error`, which is a string when the server
// (or a proxy in front of it) answered with something other than JSON, and
// `response` supplies the status. It never throws, and it copies only the
// `location` and `message` members: a field's `value` is the submitted input
// (a password, for one), which must not travel on into logs or state.
export function readProblem(body: unknown, response?: { status: number }): Problem {
  const rec = isRecord(body) ? body : {};
  const status = response?.status ?? (typeof rec.status === "number" ? rec.status : 0);
  const out: Problem = { status, fields: [] };
  if (typeof rec.type === "string") out.type = rec.type;
  if (Array.isArray(rec.errors)) {
    for (const e of rec.errors) {
      if (isRecord(e) && typeof e.location === "string" && typeof e.message === "string") {
        out.fields.push({ location: e.location, message: e.message });
      }
    }
  }
  return out;
}
