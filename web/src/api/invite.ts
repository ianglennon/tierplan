// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import { queryClient } from "@/api/query-client";
import type { components } from "@/api/schema";
import { meQueryOptions } from "@/hooks/use-me";

export type InviteInfo = components["schemas"]["LookupInviteBody"];

// The one-time token lives in this module variable and nowhere else: never in
// a URL, a query key, any browser storage or a cookie. The invite route moves
// it here from the address fragment and clears the fragment (UI-SPEC "One-time
// secrets"); the page clears it when it goes away.
let held: string | null = null;

export function holdInviteToken(token: string): void {
  held = token;
}

export function heldInviteToken(): string | null {
  return held;
}

export function clearInviteToken(): void {
  held = null;
}

// tokenFromHash reads "#token=..." from a location hash, or null.
export function tokenFromHash(hash: string): string | null {
  const params = new URLSearchParams(hash.startsWith("#") ? hash.slice(1) : hash);
  const token = params.get("token");
  return token ? token : null;
}

export type LookupResult =
  | { kind: "ok"; info: InviteInfo }
  | { kind: "invalid" }
  | { kind: "rate_limited" }
  | { kind: "error" };

export type AcceptResult =
  | { kind: "ok" }
  | { kind: "invalid" }
  | { kind: "rate_limited" }
  | { kind: "fields"; fields: ProblemField[] }
  | { kind: "error" };

// withCsrfRetry runs a POST and, when a browser that holds a session made it
// without a current CSRF token (403 csrf_failed), fetches /api/me for the token
// and tries once more. The invite page is public but a session in the same
// browser still makes the guard ask for the token (Phase 1 rule, 01-12).
async function withCsrfRetry<T extends { error?: unknown; response: Response }>(
  send: () => Promise<T>,
): Promise<T> {
  const first = await send();
  if (
    first.response.status !== 403 ||
    readProblem(first.error, first.response).type !== "csrf_failed"
  ) {
    return first;
  }
  try {
    await queryClient.fetchQuery({ ...meQueryOptions, staleTime: 0 });
  } catch {
    return first;
  }
  return send();
}

// lookupInvite asks what the link is for. The token travels in the body of a POST.
export async function lookupInvite(token: string): Promise<LookupResult> {
  try {
    const { data, error, response } = await withCsrfRetry(() =>
      api.POST("/api/invite/lookup", { body: { token } }),
    );
    if (data) return { kind: "ok", info: data };
    const status = readProblem(error, response).status;
    if (status === 404) return { kind: "invalid" };
    if (status === 429) return { kind: "rate_limited" };
    return { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}

// acceptInvite spends the link and sets the password.
export async function acceptInvite(token: string, password: string): Promise<AcceptResult> {
  try {
    const { error, response } = await withCsrfRetry(() =>
      api.POST("/api/invite/accept", { body: { token, password } }),
    );
    if (response.ok) return { kind: "ok" };
    const problem = readProblem(error, response);
    if (problem.status === 404) return { kind: "invalid" };
    if (problem.status === 429) return { kind: "rate_limited" };
    if (problem.status === 422 && problem.fields.length > 0) {
      return { kind: "fields", fields: problem.fields };
    }
    return { kind: "error" };
  } catch {
    return { kind: "error" };
  }
}
