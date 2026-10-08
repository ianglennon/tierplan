// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The 375px check the Phase 3 UI-SPEC flagged as needing a visual test (UI
// Considerations, "User detail, Backup and Access pages at 375px"): the page
// does not scroll sideways, the buttons a person taps are at least 44px high, and
// the grant dialog fits the screen with 16px to spare on each side. It is an
// automated guard that complements, and does not replace, a person looking at
// the pages on a phone-sized browser (docs/release-checklist.md).

import type { Locator, Page } from "@playwright/test";

import { AUTH_STATE, at } from "./env";
import { environmentsOf, expect, test } from "./helpers";

const WIDTH = 375;
const HEIGHT = 812;

test.use({ storageState: AUTH_STATE, viewport: { width: WIDTH, height: HEIGHT } });

// The 44px touch target of P1 D-64.
const TOUCH = 44;

// expectNoSidewaysScroll asserts the page itself is no wider than the screen:
// tables are meant to scroll inside their cards instead.
async function expectNoSidewaysScroll(page: Page, what: string): Promise<void> {
  const { scroll, inner } = await page.evaluate(() => ({
    scroll: document.scrollingElement?.scrollWidth ?? 0,
    inner: window.innerWidth,
  }));
  expect(scroll, `${what}: the page scrolls sideways`).toBeLessThanOrEqual(inner);
}

// expectTouchHeight asserts that a named button, as the page shows it, is at
// least 44px high.
async function expectTouchHeight(scope: Page | Locator, name: string, what: string): Promise<void> {
  const button = scope.getByRole("button", { name, exact: true }).first();
  await expect(button, `${what}: "${name}"`).toBeVisible();
  const box = await button.boundingBox();
  expect(box?.height ?? 0, `${what}: "${name}" is ${box?.height}px high`).toBeGreaterThanOrEqual(
    TOUCH,
  );
}

test("the user page stacks its cards, keeps tables inside them and has 44px buttons", async ({
  page,
}) => {
  const res = await page.request.get(at("/api/users"));
  expect(res.ok()).toBe(true);
  const users = (await res.json()) as { id: string; username: string }[];
  const user = users.find((u) => u.username.startsWith("viewer1"));
  expect(user, "the user the access spec created").toBeDefined();
  if (!user) return;

  await page.goto(at(`/settings/access/users/${user.id}`));
  await expect(page.getByRole("heading", { name: user.username, level: 2 })).toBeVisible();
  // The cards below the header: the access card is the one with a table.
  await expect(page.getByRole("heading", { name: "Access", level: 3, exact: true })).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Sessions", level: 3, exact: true }),
  ).toBeVisible();
  await expectNoSidewaysScroll(page, "user page");
  await expectTouchHeight(page, "Add access", "user page");
  await expectTouchHeight(page, "Add to team", "user page");

  // The users list is the page people reach it from.
  await page.goto(at("/settings/access/users"));
  await expect(page.getByRole("table", { name: "Users" })).toBeVisible();
  await expectNoSidewaysScroll(page, "users list");
  await expectTouchHeight(page, "Create user", "users list");
});

test("the Backup page stacks its cards and has 44px buttons", async ({ page }) => {
  await page.goto(at("/settings/backup"));
  await expect(page.getByRole("heading", { name: "Back up and restore Bayplan" })).toBeVisible();
  for (const title of ["Download a backup", "Scheduled backups", "Restore from a backup"]) {
    await expect(page.getByRole("heading", { name: title, level: 3 })).toBeVisible();
  }
  await expectNoSidewaysScroll(page, "Backup page");
  await expectTouchHeight(page, "Download backup", "Backup page");
  await expectTouchHeight(page, "Check backup", "Backup page");
  await expectTouchHeight(page, "Save schedule", "Backup page");
});

test("the environment Access tab fits, and its grant dialog fits the screen", async ({ page }) => {
  const envs = await environmentsOf(page.context());
  const env = envs.find((e) => e.isLocalSocket);
  expect(env, "the managed Docker host").toBeDefined();
  if (!env) return;

  await page.goto(at(`/settings/environments/${env.id}/access`));
  await expect(page.getByRole("heading", { name: `Access to ${env.name}` })).toBeVisible();
  await expect(
    page.getByRole("table", { name: `People with access to ${env.name}` }),
  ).toBeVisible();
  await expectNoSidewaysScroll(page, "environment Access tab");
  await expectTouchHeight(page, "Add access", "environment Access tab");

  await page.getByRole("button", { name: "Add access", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Add access" });
  await expect(dialog).toBeVisible();
  // The three roles are listed in full inside it.
  await expect(dialog.getByRole("radio", { name: "Viewer" })).toBeVisible();
  // The dialog zooms in over 100ms, and a box measured meanwhile is scaled down
  // by a few percent: wait for it to settle before measuring anything.
  await dialog.evaluate(async (el) => {
    await Promise.all(el.getAnimations().map((a) => a.finished));
  });

  const box = await dialog.boundingBox();
  expect(box, "the dialog has a box").not.toBeNull();
  expect(
    box?.width ?? Number.POSITIVE_INFINITY,
    "the dialog is no wider than the screen minus 32px",
  ).toBeLessThanOrEqual(WIDTH - 32 + 0.5);
  expect(box?.x ?? -1, "the dialog starts inside the screen").toBeGreaterThanOrEqual(0);
  expect(
    (box?.x ?? 0) + (box?.width ?? 0),
    "the dialog ends inside the screen",
  ).toBeLessThanOrEqual(WIDTH);
  // Its own buttons are touch sized.
  await expectTouchHeight(dialog, "Cancel", "grant dialog");
  await expectNoSidewaysScroll(page, "grant dialog");
});
