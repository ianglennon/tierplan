// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import createClient, { type Middleware } from "openapi-fetch";

import type { paths } from "./schema";

// Same-origin typed client generated from the committed openapi.json. The base
// URL is the page origin, so `new Request()` always gets an absolute URL
// (relative ones are rejected outside a browser, for example under Vitest).
// fetch is looked up per call, not captured at import time, so tests can stub it.
export const api = createClient<paths>({
  baseUrl: window.location.origin,
  fetch: (input) => globalThis.fetch(input),
});

// The CSRF token lives in this module variable and nowhere else: never in any
// browser storage or a cookie. A refresh loses it and the app loads it again
// from /api/me (D-36, T-01-56).
let csrfToken: string | null = null;
let onUnauthorized: ((path: string) => void) | null = null;

export function setCsrfToken(token: string | null): void {
  csrfToken = token;
}

// The handler is told the path (with its query string) the user was on, so the
// sign-in screen can send them back after they sign in again.
export function setUnauthorizedHandler(fn: ((path: string) => void) | null): void {
  onUnauthorized = fn;
}

const MUTATING = new Set(["POST", "PUT", "PATCH", "DELETE"]);

// A 401 from these calls is an answer, not an expired session: a wrong
// password, the pre-auth instance lookup and the setup wizard's own calls.
function isExemptFrom401(pathname: string): boolean {
  return (
    pathname === "/api/auth/login" ||
    pathname === "/api/instance" ||
    pathname.startsWith("/api/setup/")
  );
}

const session: Middleware = {
  onRequest({ request }) {
    if (csrfToken && MUTATING.has(request.method.toUpperCase())) {
      request.headers.set("X-CSRF-Token", csrfToken);
    }
    return request;
  },
  onResponse({ request, response }) {
    if (
      response.status === 401 &&
      onUnauthorized &&
      !isExemptFrom401(new URL(request.url).pathname)
    ) {
      onUnauthorized(window.location.pathname + window.location.search);
    }
    return response;
  },
};

api.use(session);
