// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Paths and addresses shared by the config and the specs. Nothing here imports
// Playwright, so the config can load it.

import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));

// `make e2e` publishes the container on 7480, the port Bayplan serves on by
// default, on 127.0.0.1 only (WR-12). BAYPLAN_E2E_PORT moves it when something
// else holds 7480.
export const PORT = process.env.BAYPLAN_E2E_PORT ?? "7480";
// Docker publishes IPv4 loopback only, and "localhost" may resolve to ::1 first.
// The public URL the wizard pre-fills follows the browser origin.
export const BASE_URL = `http://127.0.0.1:${PORT}`;

export const COMPOSE_FILE = resolve(HERE, "../compose.yaml");

// The signed-in browser state the first spec saves for the later ones. It lives
// under .devdata/, which is git-ignored.
export const AUTH_STATE = resolve(HERE, "../../.devdata/e2e-admin.json");

export const ADMIN = { username: "admin", password: "correct horse battery staple" } as const;
