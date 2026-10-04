// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Golden paths 1 and 2 (D-80): the setup wizard claims the instance with the
// token read through `docker compose exec`, /welcome opts the dind host in,
// then sign out, a wrong password, signing in and a refresh (AUTH-01, AUTH-02,
// AUTH-03, ENV-04).
//
// The tests share one browser page and one server, and run in order: each
// builds on the one before.

import { type Page, test as base } from "@playwright/test";

import { ADMIN, AUTH_STATE, at, BASE_URL, ORIGIN } from "./env";
import {
  expect,
  expectVersionFooter,
  type PageTracker,
  screenshotOnFailure,
  setupToken,
  trackPage,
} from "./helpers";

base.describe.configure({ mode: "serial" });

let page: Page;
let tracker: PageTracker;

base.beforeAll(async ({ browser }) => {
  // A context of its own: the browser must keep its session cookie from one
  // test to the next.
  const context = await browser.newContext({
    baseURL: ORIGIN,
    ignoreHTTPSErrors: true,
    locale: "en-GB",
  });
  page = await context.newPage();
  tracker = await trackPage(page);
});

base.afterEach(async ({}, info) => {
  await screenshotOnFailure(page, info);
});

base.afterAll(async () => {
  try {
    tracker.assertSameOrigin(ORIGIN);
    tracker.assertNoCspViolations();
  } finally {
    await page.context().close();
  }
});

base("a fresh instance sends every address to the setup wizard", async () => {
  await page.goto(at("/"));
  await expect(page).toHaveURL(/\/bayplan\/setup$/);
  await expect(page.getByRole("heading", { name: "Set up Bayplan", level: 2 })).toBeVisible();
  await expectVersionFooter(page);
});

base("a wrong setup token is refused", async () => {
  await page.getByLabel("Setup token").fill("aaaaaaaa-bbbbbbbb-cccccccc-dddddddd");
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(
    page.getByText("That setup token isn't valid. Copy it again from the container logs."),
  ).toBeVisible();
  await expect(page.getByRole("heading", { name: "Create the admin account" })).toHaveCount(0);
});

base("the right token, an admin account and the defaults create the instance", async () => {
  await page.getByLabel("Setup token").fill(setupToken());
  await page.getByRole("button", { name: "Continue" }).click();

  await expect(page.getByRole("heading", { name: "Create the admin account" })).toBeVisible();
  await page.getByLabel("Username").fill(ADMIN.username);
  await page.getByLabel("Password", { exact: true }).fill(ADMIN.password);
  await page.getByLabel("Confirm password").fill(ADMIN.password);
  await page.getByRole("button", { name: "Continue" }).click();

  // The instance name and the public URL keep the defaults the wizard offered.
  await expect(page.getByRole("heading", { name: "Name this instance" })).toBeVisible();
  await expect(page.getByLabel("Instance name")).not.toHaveValue("");
  await expect(page.getByLabel("Public URL")).toHaveValue(BASE_URL);
  await page.getByRole("button", { name: "Create admin account" }).click();

  await expect(page).toHaveURL(/\/welcome$/);
});

base("/welcome offers the dind host and managing it opens the dashboard", async () => {
  await expect(page.getByRole("heading", { name: "Manage this Docker host?" })).toBeVisible();
  // The engine behind the shared socket is a standalone Docker, not a swarm.
  await expect(page.getByText("Standalone")).toBeVisible();
  await expect(page.getByText(/root-equivalent control of that host/)).toBeVisible();
  await expectVersionFooter(page);

  await page.getByRole("button", { name: "Manage this host" }).click();
  // Nothing has been opened yet, so home is the overview (D-31), with the one
  // host it just added; its dashboard is a click away.
  await expect(page).toHaveURL(/\/bayplan\/environments$/);
  await page.getByRole("main").locator("a[title]").first().click();
  await expect(page).toHaveURL(/\/bayplan\/env\/[^/]+$/);
  await expect(page.getByRole("region", { name: "Dashboard" })).toBeVisible();
  await expectVersionFooter(page);
});

base("Log out ends the session and returns to the sign-in page", async () => {
  await page.getByRole("button", { name: "User menu" }).click();
  await page.getByRole("menuitem", { name: "Log out" }).click();
  await expect(page).toHaveURL(/\/login/);
  await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
  await expectVersionFooter(page);

  // The session is really gone, not only hidden: a signed-in address bounces back.
  await page.goto(at("/audit"));
  await expect(page).toHaveURL(/\/login\?next=/);
});

base("a wrong password shows one message that does not say which half was wrong", async () => {
  await page.goto(at("/login"));
  await page.getByLabel("Username").fill(ADMIN.username);
  await page.getByLabel("Password", { exact: true }).fill("not the right passphrase");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByText("Invalid username or password.")).toBeVisible();
  await expect(page).toHaveURL(/\/login/);

  // Same message for a user that does not exist.
  await page.getByLabel("Username").fill("nobody-here");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByText("Invalid username or password.")).toBeVisible();
});

base("the right password signs in with Keep me signed in and a refresh keeps the session", async () => {
  await page.getByLabel("Username").fill(ADMIN.username);
  await page.getByLabel("Password", { exact: true }).fill(ADMIN.password);
  await page.getByLabel(/Keep me signed in/).check();
  await page.getByRole("button", { name: "Sign in" }).click();

  // Home is the environment this admin last opened, else the overview (D-31);
  // either way the signed-in shell is up.
  const home = /\/bayplan\/(environments|env\/[^/]+)$/;
  await expect(page).toHaveURL(home);
  await expect(page.getByRole("button", { name: "User menu" })).toBeVisible();

  await page.reload();
  await expect(page).toHaveURL(home);
  await expect(page.getByRole("button", { name: "User menu" })).toBeVisible();

  // Behind a subpath over https the session cookie is __Secure- (a __Host- cookie
  // must be at the root) and is sent to /bayplan only, so another application on
  // the same host never receives it (D-50, D-59).
  const cookies = await page.context().cookies();
  const session = cookies.find((c) => c.name.endsWith("bayplan_session"));
  expect(session?.name).toBe("__Secure-bayplan_session");
  expect(session?.path).toBe("/bayplan");
  expect(session?.secure).toBe(true);
  expect(session?.httpOnly).toBe(true);
  expect(session?.sameSite).toBe("Lax");

  // The later specs start from this signed-in browser.
  await page.context().storageState({ path: AUTH_STATE });
});
