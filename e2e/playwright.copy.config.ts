// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { defineConfig, devices } from "@playwright/test";

// The copy fallback only breaks on an insecure context (plain http by a name
// that is not localhost), and jsdom cannot show it: its select() never moves
// focus. So this suite runs a real Chromium against a test page served by Vite
// at http://coaming.test, a name the browser maps to 127.0.0.1. It is separate
// from the ordered Docker-stack suite in playwright.config.ts.
const PORT = process.env.COAMING_COPY_HARNESS_PORT ?? "5179";

export default defineConfig({
  testDir: "./copy",
  workers: 1,
  forbidOnly: Boolean(process.env.CI),
  // D-15: never retried. D-22: a hang ends in Playwright's own report; the e2e
  // job's timeout-minutes (30) is the outer bound.
  retries: 0,
  globalTimeout: 5 * 60_000,
  reporter: [
    ["list"],
    // D-21: scripts/test-summary.sh names the failing specs from this file.
    ["json", { outputFile: "../.devdata/test/e2e-copy.json" }],
    ...(process.env.CI ? ([["github"]] as const) : []),
  ],
  outputDir: "../.devdata/playwright-copy-results",
  use: {
    ...devices["Desktop Chrome"],
    baseURL: `http://coaming.test:${PORT}`,
    launchOptions: {
      args: ["--host-resolver-rules=MAP coaming.test 127.0.0.1"],
    },
  },
  webServer: {
    command: `pnpm -C ../web exec vite --config vite.harness.config.ts --host 127.0.0.1 --port ${PORT} --strictPort`,
    url: `http://127.0.0.1:${PORT}/`,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
  },
});
