// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { execFileSync } from "node:child_process";
import { test as base, expect, type Page, type TestInfo } from "@playwright/test";

import { COMPOSE_FILE } from "./env";

// compose runs `docker compose -f e2e/compose.yaml <args>` against the stack
// `make e2e` brought up and returns what it printed.
export function compose(...args: string[]): string {
  return execFileSync("docker", ["compose", "-f", COMPOSE_FILE, ...args], {
    encoding: "utf8",
    timeout: 120_000,
  });
}

// docker runs the docker CLI on the machine running the suite, against the
// engine that hosts the Compose stack (not the dind one Bayplan manages).
export function docker(...args: string[]): string {
  return execFileSync("docker", args, { encoding: "utf8", timeout: 60_000 }).trim();
}

// setupToken reads the first-run token the way a self-hoster does:
// `docker exec <container> bayplan setup-token`. The command runs as root in the
// container, so it also proves the CLI drops privileges and leaves /data usable
// by the server (D-17).
export function setupToken(): string {
  return compose("exec", "-T", "bayplan", "bayplan", "setup-token").trim();
}

// dind runs a shell command inside the dind service, against the engine Bayplan
// manages. Its DOCKER_HOST points the docker CLI at the shared socket.
export function dind(cmd: string): string {
  return compose("exec", "-T", "dind", "sh", "-c", cmd).trim();
}

// dindCount is the number of lines a dind command prints: `docker ps -aq` and
// its siblings print one id or name per line.
export function dindCount(cmd: string): number {
  const out = dind(cmd);
  return out === "" ? 0 : out.split("\n").length;
}

// PageTracker watches one page for the two things a unit test cannot see: a
// Content-Security-Policy violation (T-01-113) and a request that leaves the
// Bayplan origin (T-01-114, D-16).
export interface PageTracker {
  // Every request URL the page made, in order.
  readonly requests: readonly string[];
  // Every CSP violation reported, as a readable line.
  readonly cspViolations: readonly string[];
  assertNoCspViolations(): void;
  assertSameOrigin(baseURL: string): void;
}

export async function trackPage(page: Page): Promise<PageTracker> {
  const requests: string[] = [];
  const cspViolations: string[] = [];

  // A navigation throws away the page's own window.__csp, so each violation is
  // also handed to this side as it happens and survives every route change.
  await page.exposeFunction("__bayplanCspReport", (line: string) => {
    cspViolations.push(line);
  });
  await page.addInitScript(() => {
    const w = window as unknown as {
      __csp: string[];
      __bayplanCspReport?: (line: string) => void;
    };
    w.__csp = [];
    document.addEventListener("securitypolicyviolation", (e) => {
      const line = `${e.violatedDirective} blocked ${e.blockedURI || "inline"} at ${e.sourceFile}:${e.lineNumber}`;
      w.__csp.push(line);
      w.__bayplanCspReport?.(line);
    });
  });
  // Chromium also logs every enforced violation to the console. Reading that as
  // well catches one raised before the init script's listener existed.
  page.on("console", (msg) => {
    const text = msg.text();
    if (/Content Security Policy|Refused to (load|apply|execute|connect|frame)/i.test(text)) {
      cspViolations.push(`console: ${text}`);
    }
  });
  page.on("request", (req) => {
    requests.push(req.url());
  });

  return {
    requests,
    cspViolations,
    assertNoCspViolations() {
      expect(cspViolations, "Content-Security-Policy violations").toEqual([]);
    },
    assertSameOrigin(baseURL: string) {
      const origin = new URL(baseURL).origin;
      const offenders = requests.filter((u) => {
        const url = new URL(u);
        // data:, blob: and about: never leave the browser.
        if (url.protocol !== "http:" && url.protocol !== "https:") return false;
        return url.origin !== origin;
      });
      expect(offenders, `requests to an origin other than ${origin}`).toEqual([]);
    },
  };
}

// The tracker is attached to every page a spec opens through the `page`
// fixture, and checked when the test ends, so no golden path can pass with a
// CSP violation or an off-origin request in it.
export const test = base.extend<{ tracker: PageTracker }>({
  tracker: [
    async ({ page, baseURL }, use) => {
      const tracker = await trackPage(page);
      await use(tracker);
      tracker.assertSameOrigin(baseURL ?? "");
      tracker.assertNoCspViolations();
    },
    { auto: true },
  ],
});

export { expect };

// What the version footer says: "Bayplan v0.1.0" for a release, "Bayplan dev
// (abc1234)" for a build without a version.
export const VERSION_TEXT = /Bayplan (v\d|dev)/;

// expectVersionFooter asserts the AGPL section 13 footer is on screen exactly
// once: the version, and a "Source code" link that opens in a new tab (PLAT-05).
// The sidebar and the page each own the footer in turn, so a second one
// showing at once is a bug as much as a missing one (D-37).
export async function expectVersionFooter(page: Page): Promise<void> {
  const footers = page.locator("footer:visible");
  await expect(footers).toHaveCount(1);
  await expect(footers).toContainText(VERSION_TEXT);
  const source = footers.getByRole("link", { name: /^Source code/ });
  await expect(source).toBeVisible();
  await expect(source).toHaveAttribute("target", "_blank");
}

// screenshotOnFailure attaches the page as it was when a spec that drives its
// own browser context failed. Specs on the `page` fixture get Playwright's own.
export async function screenshotOnFailure(page: Page, info: TestInfo): Promise<void> {
  if (info.status === info.expectedStatus) return;
  await info.attach("failure", {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
}
