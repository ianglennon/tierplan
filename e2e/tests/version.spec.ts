// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The version and "Source code" footer on every route (PLAT-05, AGPL section
// 13), exactly one of it at a time on the expanded sidebar, the collapsed rail
// and a 375px viewport with the drawer shut and open (D-37). The fixture
// behind `test` also fails the spec on any Content-Security-Policy violation
// or request to another origin (T-01-113, T-01-114, D-16).

import type { Page } from "@playwright/test";

import { AUTH_STATE, at } from "./env";
import { expect, expectVersionFooter, openFirstEnvironment, test } from "./helpers";

test.describe("signed out", () => {
  test.use({ storageState: { cookies: [], origins: [] } });

  test("the setup address closes and the sign-in page shows the footer", async ({ page }) => {
    await page.goto(at("/setup"));
    await expect(page).toHaveURL(/\/login$/);
    await expectVersionFooter(page);
  });
});

test.describe("signed in", () => {
  test.use({ storageState: AUTH_STATE });

  async function envBase(page: Page): Promise<string> {
    await openFirstEnvironment(page);
    return new URL(page.url()).pathname;
  }

  test("every signed-in route shows the footer once", async ({ page }) => {
    const base = await envBase(page);
    const routes = [
      base,
      `${base}/containers`,
      `${base}/images`,
      `${base}/volumes`,
      `${base}/networks`,
      at("/audit"),
      at("/settings/general"),
      at("/settings/environments"),
      at("/settings/about"),
    ];
    for (const route of routes) {
      await page.goto(route);
      await expect(page.getByRole("main")).toBeVisible();
      await expectVersionFooter(page);
      expect(new URL(page.url()).pathname, `${route} stayed where it was`).toBe(route);
    }
  });

  test("the collapsed rail still shows exactly one footer", async ({ page }) => {
    await page.goto(await envBase(page));
    await expectVersionFooter(page);

    const toggle = page.getByRole("button", { name: "Toggle sidebar" });
    await toggle.click();
    await expectVersionFooter(page);

    // Reloading keeps the rail (the choice is remembered), and the footer too.
    await page.reload();
    await expectVersionFooter(page);

    // Put the sidebar back for whoever runs next.
    await toggle.click();
    await expectVersionFooter(page);
  });

  test("at 375px the footer is single with the drawer closed and open", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 812 });
    await page.goto(await envBase(page));
    await expectVersionFooter(page);

    await page.getByRole("button", { name: "Toggle sidebar" }).click();
    await expect(page.getByRole("dialog")).toBeVisible();
    await expectVersionFooter(page);
    await expect(page.getByRole("dialog").locator("footer:visible")).toHaveCount(1);

    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await expectVersionFooter(page);
  });
});
