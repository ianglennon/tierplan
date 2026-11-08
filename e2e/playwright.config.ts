// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { defineConfig, devices } from "@playwright/test";

import { ORIGIN as baseURL, SPKI_HASH } from "./tests/env";

// The specs share one instance and depend on each other's state: the first
// claims it, the rest use that account. Playwright orders files alphabetically
// unless told otherwise, so each spec is its own project that depends on the
// one before it. A failure early on skips the rest, which is what a broken
// setup deserves.
// `make e2e` runs the browser in a container that cannot see the host's network
// change (see the browser service in compose.yaml) and names its server here.
const wsEndpoint = process.env.TIERPLAN_E2E_BROWSER_WS;
// The proxy's certificate is the harness's own, made by scripts/e2e-cert.go, and no public CA
// signed it. The browser trusts it by the hash of its public key and trusts no other
// certificate it cannot verify: nothing here switches certificate checking off. The remote
// run-server takes the launch options from the runner, so the flag reaches that Chromium.
const chromium = {
  ...devices["Desktop Chrome"],
  baseURL,
  launchOptions: { args: [`--ignore-certificate-errors-spki-list=${SPKI_HASH}`] },
  ...(wsEndpoint ? { connectOptions: { wsEndpoint } } : {}),
};
// The containers project follows the dashboard because it starts from the one
// environment and the engine the dashboard spec left, and it clears what that spec
// made under names it also uses.
// The registries project needs only the admin: it stores one throwaway
// credential and deletes it, so it sits anywhere after the setup.
// The agent project adds a second environment and revokes it, so the specs before
// it assume the one environment they started with. The access project follows it
// because it needs that second, revoked environment as the one a new user must not
// see. The services project turns the engine TierPlan manages into a one-node swarm,
// runs one service on it and leaves the swarm again, so it comes after every spec
// that assumes a standalone host and before the restore. It finds the local
// environment by what it is, not by its position, because the agent spec left a
// second one. The backup project runs last: its restore replaces the state of the shared
// server, which signs everyone out and drops everything made after the backup.
// The stacks project deploys compose stacks to the engine TierPlan manages and to an agent host of
// its own. It enrols that host afresh, because the agent spec leaves its host revoked, and it
// leaves the engine as it found it. It sits before the services project because it assumes a
// standalone host, as the specs before it do.
// The git-stacks project makes a stack from a repository on the git service of this topology and
// deploys it to the engine TierPlan manages, so it follows the stacks project and, like it, comes
// before the services project, which turns that engine into a swarm.
// The updates project deploys a stack of an image on the registry service of this topology to the
// engine TierPlan manages, pushes a new build of the tag and updates the stack; like the stacks
// projects it assumes a standalone host and leaves the engine as it found it, so it sits before the
// services project.
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
  "stacks",
  "git-stacks",
  "updates",
  "services",
  "backup",
] as const;

export default defineConfig({
  testDir: "./tests",
  workers: 1,
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  // D-15: a failing e2e test is a defect, never retried. A retry would also hide
  // it: a trace taken only on a first retry would never record, so failures keep
  // theirs with "retain-on-failure" below.
  retries: 0,
  // D-22: a hang ends in Playwright's own report, not in the job's silent kill.
  // The e2e job's timeout-minutes (45, in .github/workflows/ci.yml) covers the setup
  // that runs before this timer starts (preflight, e2e-copy, the image and agent
  // builds, compose up) plus these 20 minutes plus a margin, so the job is never
  // killed before Playwright has written its report. Keep the two in step.
  globalTimeout: 20 * 60_000,
  reporter: [
    ["list"],
    ["html", { outputFolder: "../.devdata/playwright-report", open: "never" }],
    // D-21: scripts/test-summary.sh names the failing specs from this file.
    ["json", { outputFile: "../.devdata/test/e2e.json" }],
    // In CI a failing spec is annotated on the run.
    ...(process.env.CI ? ([["github"]] as const) : []),
  ],
  outputDir: "../.devdata/playwright-results",
  use: {
    baseURL,
    // A click that cannot find its target fails in seconds, not at the end of the
    // test's own timeout.
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
    trace: "retain-on-failure",
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
