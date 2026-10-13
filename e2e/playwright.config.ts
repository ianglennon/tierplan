// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { defineConfig, devices } from "@playwright/test";

import { ORIGIN as baseURL } from "./tests/env";

// The specs share one instance and depend on each other's state: the first
// claims it, the rest use that account. Playwright orders files alphabetically
// unless told otherwise, so each spec is its own project that depends on the
// one before it. A failure early on skips the rest, which is what a broken
// setup deserves.
const chromium = { ...devices["Desktop Chrome"], baseURL };
// The containers project follows the dashboard because it starts from the one
// environment and the engine the dashboard spec left, and it clears what that spec
// made under names it also uses.
// The registries project needs only the admin: it stores one throwaway
// credential and deletes it, so it sits anywhere after the setup.
// The agent project adds a second environment and revokes it, so the specs before
// it assume the one environment they started with. The access project follows it
// because it needs that second, revoked environment as the one a new user must not
// see. The backup project runs last: its restore replaces the state of the shared
// server, which signs everyone out and drops everything made after the backup.
const order = [
  "setup-and-login",
  "dashboard",
  "containers",
  "audit",
  "version",
  "settings",
  "agent",
  "access",
  "registries",
  "responsive",
  "backup",
] as const;

export default defineConfig({
  testDir: "./tests",
  workers: 1,
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  reporter: [
    ["list"],
    ["html", { outputFolder: "../.devdata/playwright-report", open: "never" }],
  ],
  outputDir: "../.devdata/playwright-results",
  use: {
    baseURL,
    // The proxy's certificate is the harness's own, made by scripts/e2e-cert.go.
    ignoreHTTPSErrors: true,
    // A click that cannot find its target fails in seconds, not at the end of the
    // test's own timeout.
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    // A pinned timezone and locale keep date text stable between machines.
    locale: "en-GB",
    timezoneId: "UTC",
  },
  expect: { timeout: 10_000 },
  projects: order.map((name, i) => ({
    name,
    testMatch: `${name}.spec.ts`,
    // The golden path keeps its trace even when it passes: the release check
    // looks at the waiting panel going live in it.
    use: name === "agent" ? { ...chromium, trace: "on" } : chromium,
    dependencies: i === 0 ? [] : [order[i - 1] as string],
  })),
});
