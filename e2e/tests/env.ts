// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Paths and addresses shared by the config and the specs. Nothing here imports
// Playwright, so the config can load it.

import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));

// `make e2e` serves Bayplan through Caddy at https://127.0.0.1:<port>/bayplan with
// a certificate it generated (scripts/e2e-cert.go), published on 127.0.0.1 only
// (WR-12). BAYPLAN_E2E_HTTPS_PORT moves it when something else holds 8443.
export const PORT = process.env.BAYPLAN_E2E_HTTPS_PORT ?? "8443";
// Docker publishes IPv4 loopback only, and "localhost" may resolve to ::1 first.
export const ORIGIN = `https://127.0.0.1:${PORT}`;
// Bayplan lives under a subpath, so every address the specs open has to carry it
// (D-59, RESEARCH Pitfall 14). Playwright's baseURL is the origin, because it
// resolves a path that starts with a slash against the origin and would drop a
// base path held in it; at() puts the base back.
export const BASE_PATH = "/bayplan";
// The public URL: what the setup wizard pre-fills and what BAYPLAN_PUBLIC_URL says.
export const BASE_URL = `${ORIGIN}${BASE_PATH}`;

// at turns an application path ("/audit") into the address behind the proxy.
export function at(path: string): string {
  return `${BASE_PATH}${path.startsWith("/") ? path : `/${path}`}`;
}

export const COMPOSE_FILE = resolve(HERE, "../compose.yaml");

// The SPKI pin of the proxy's certificate, written by `make e2e`. A private-CA
// proxy has no pin in the install command, so the agent spec adds this one the
// documented way, as BAYPLAN_AGENT_PIN (docs/agent.md).
// Empty when the certificate has not been generated, so that loading this file
// (the config, a type check) never fails; the agent spec insists on a value.
export const AGENT_PIN = ((): string => {
  try {
    return readFileSync(resolve(HERE, "../../.devdata/e2e-tls/pin.txt"), "utf8").trim();
  } catch {
    return "";
  }
})();

// The signed-in browser state the first spec saves for the later ones. It lives
// under .devdata/, which is git-ignored.
export const AUTH_STATE = resolve(HERE, "../../.devdata/e2e-admin.json");

export const ADMIN = { username: "admin", password: "correct horse battery staple" } as const;
