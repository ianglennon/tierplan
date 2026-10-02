// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { defineConfig, devices } from "@playwright/test";

import { BASE_URL as baseURL } from "./tests/env";

// The specs share one instance and depend on each other's state: the first
// claims it, the rest use that account. Playwright orders files alphabetically
// unless told otherwise, so each spec is its own project that depends on the
// one before it. A failure early on skips the rest, which is what a broken
// setup deserves.
const chromium = { ...devices["Desktop Chrome"], baseURL };
const order = ["setup-and-login", "dashboard", "audit", "version"] as const;

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
    use: chromium,
    dependencies: i === 0 ? [] : [order[i - 1] as string],
  })),
});
