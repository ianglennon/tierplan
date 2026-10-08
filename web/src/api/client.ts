// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import createClient, { type Middleware } from "openapi-fetch";

import { basePath, stripBase } from "@/lib/base-path";
import type { paths } from "./schema";

// Same-origin typed client generated from the committed openapi.json. The base
// URL is the page origin plus the base path Bayplan is served under (D-50), so
// `new Request()` always gets an absolute URL (relative ones are rejected
// outside a browser, for example under Vitest).
// fetch is looked up per call, not captured at import time, so tests can stub it.
export const api = createClient<paths>({
  baseUrl: window.location.origin + basePath(),
  fetch: (input) => globalThis.fetch(input),
});

// The CSRF token lives in this module variable and nowhere else: never in any
// browser storage or a cookie. A refresh loses it and the app loads it again
// from /api/me (D-36, T-01-56).
let csrfToken: string | null = null;
let onUnauthorized: ((path: string) => void) | null = null;
let onAccessChange: ((kind: AccessChangeKind, path: string) => void) | null = null;

// What a refusal says about the caller's access (D-10): "unseen" is an
// environment the caller can no longer see (a 404 that looks like a missing
// one, or a 403 on a read), and "refused" is a change they tried to make that
// the server would not allow.
export type AccessChangeKind = "unseen" | "refused";

// The signed-in layout registers the one handler that reacts to a change of
// access while it is mounted; nothing else does, so nothing outside the signed-in
// app reacts. The handler is told the path with its query string removed and
// without the base path.
export function setAccessChangeHandler(
  fn: ((kind: AccessChangeKind, path: string) => void) | null,
): void {
  onAccessChange = fn;
}

export function setCsrfToken(token: string | null): void {
  csrfToken = token;
}

// getCsrfToken is for the calls that cannot go through the typed client, such as
// the raw upload of a backup file. The token is still read only from here.
export function getCsrfToken(): string | null {
  return csrfToken;
}

// While a restore restarts Bayplan, every request is expected to be refused: the
// session it ran under is gone. The caller holds the reactions to that off until
// it has decided what the refusals mean, so the 401s of the restart do not send
// the browser to sign-in or refetch an access list. It returns the function that
// lets them react again; calling it twice does nothing the second time.
let suspended = 0;
export function suspendSessionReactions(): () => void {
  suspended += 1;
  let released = false;
  return () => {
    if (released) return;
    released = true;
    suspended -= 1;
  };
}

// The handler is told the path (with its query string, without the base path)
// the user was on, so the sign-in screen can send them back after they sign in
// again.
export function setUnauthorizedHandler(fn: ((path: string) => void) | null): void {
  onUnauthorized = fn;
}

// getUnauthorizedHandler returns the handler now armed, so a caller that
// disarms it for a moment (sign-out) can put the same one back.
export function getUnauthorizedHandler(): ((path: string) => void) | null {
  return onUnauthorized;
}

const MUTATING = new Set(["POST", "PUT", "PATCH", "DELETE"]);

// A 401 from these calls is an answer, not an expired session: a wrong
// password, the pre-auth instance lookup and the setup wizard's own calls.
function isExemptFrom401(requestPath: string): boolean {
  const pathname = stripBase(requestPath);
  return (
    pathname === "/api/auth/login" ||
    pathname === "/api/instance" ||
    pathname.startsWith("/api/setup/")
  );
}

// The 403s the server answers for reasons that are not about what the caller may
// do: a request from the wrong origin, or without a valid CSRF token.
const NOT_ABOUT_ACCESS: ReadonlySet<string> = new Set(["csrf_failed", "origin_mismatch"]);

// The pages whose 401, 403 and 404 answers are their own business, not a change
// of access: sign-in, setup, the instance lookup and /api/me itself.
function isExemptFromAccessChange(pathname: string): boolean {
  return (
    pathname === "/api/me" ||
    pathname === "/api/instance" ||
    pathname.startsWith("/api/auth/") ||
    pathname.startsWith("/api/setup/")
  );
}

// problemType reads the `type` of a problem+json answer from a copy, so the
// caller still gets an unread body.
async function problemType(response: Response): Promise<string | undefined> {
  try {
    const body: unknown = await response.clone().json();
    if (typeof body === "object" && body !== null && "type" in body) {
      return typeof body.type === "string" ? body.type : undefined;
    }
  } catch {
    // Not JSON: it says nothing about access.
  }
  return undefined;
}

// accessChangeKind says whether a 403 or 404 means the caller's access changed
// (D-10). A 404 counts only when it says the environment is not found; one about
// a container or a stack is just that. A 403 on a change is a refusal; a 403 on
// a read under an environment means they can no longer see it.
async function accessChangeKind(
  request: Request,
  response: Response,
): Promise<AccessChangeKind | undefined> {
  const pathname = stripBase(new URL(request.url).pathname);
  if (isExemptFromAccessChange(pathname)) return undefined;
  const type = await problemType(response);
  if (type !== undefined && NOT_ABOUT_ACCESS.has(type)) return undefined;
  const mutating = MUTATING.has(request.method.toUpperCase());
  if (response.status === 404) return type === "environment_not_found" ? "unseen" : undefined;
  if (mutating) return "refused";
  return pathname.startsWith("/api/env/") ? "unseen" : undefined;
}

const session: Middleware = {
  onRequest({ request }) {
    if (csrfToken && MUTATING.has(request.method.toUpperCase())) {
      request.headers.set("X-CSRF-Token", csrfToken);
    }
    return request;
  },
  async onResponse({ request, response }) {
    if (suspended > 0) return response;
    if (
      response.status === 401 &&
      onUnauthorized &&
      !isExemptFrom401(new URL(request.url).pathname)
    ) {
      onUnauthorized(stripBase(window.location.pathname) + window.location.search);
    }
    if ((response.status === 403 || response.status === 404) && onAccessChange) {
      const kind = await accessChangeKind(request, response);
      if (kind) onAccessChange(kind, stripBase(new URL(request.url).pathname));
    }
    return response;
  },
};

api.use(session);
