// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import createClient from "openapi-fetch";

import type { paths } from "./schema";

// Same-origin typed client generated from the committed openapi.json. The base
// URL is the page origin, so `new Request()` always gets an absolute URL
// (relative ones are rejected outside a browser, for example under Vitest).
// fetch is looked up per call, not captured at import time, so tests can stub it.
export const api = createClient<paths>({
  baseUrl: window.location.origin,
  fetch: (input) => globalThis.fetch(input),
});
