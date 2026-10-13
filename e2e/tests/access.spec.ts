// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Golden path 6 (plan 03-29, ROADMAP Phase 3 success criteria 1 to 4): the whole
// team-access story in a real browser, behind the proxy. An instance admin
// creates a user and a team, puts the user in the team and gives the team Viewer
// on one environment. The user opens the invite link, sets a password, signs in,
// sees only that environment and sees it read only; the other environment is
// "Page not found" to them, as if it did not exist; and a direct API call for an
// action the UI hides is refused with 403 and shows up in the audit log as
// "Permission denied" (AUTH-04, AUTH-05, AUTH-06, AUTH-07, AUTH-10).
//
// It runs after the agent spec, which leaves a second, revoked environment: that
// is the one the new user must not be able to see.

import { AUTH_STATE, at, ORIGIN, VIEWER_PASSWORD } from "./env";
import {
  apiAs,
  createUserViaUI,
  environmentsOf,
  expect,
  retryName,
  test,
  trackPage,
} from "./helpers";

test.use({ storageState: AUTH_STATE });

test("an invited user gets Viewer through a team, sees only that environment, and a hidden action is refused and audited", async ({
  page,
  browser,
}, info) => {
  test.setTimeout(180_000);
  const username = retryName("viewer1", info);
  const team = retryName("readers", info);

  // The two environments the previous specs left: the Docker host Bayplan
  // manages (granted below) and the revoked agent environment (never granted).
  const envs = await environmentsOf(page.context());
  const granted = envs.find((e) => e.isLocalSocket);
  const hidden = envs.find((e) => !e.isLocalSocket);
  expect(granted, "the managed Docker host").toBeDefined();
  expect(hidden, "the environment the agent spec left behind").toBeDefined();
  if (!granted || !hidden) return;

  let invite = "";

  await test.step("the admin creates a user and reads the one-time invite link", async () => {
    invite = await createUserViaUI(page, username);
    const row = page
      .getByRole("table", { name: "Users" })
      .getByRole("row", { name: new RegExp(username) });
    await expect(row).toContainText("Invite pending");
  });

  await test.step("the admin creates a team, adds the user and gives the team Viewer on one environment", async () => {
    await page.goto(at("/settings/access/teams"));
    await page.getByRole("button", { name: "Create team", exact: true }).first().click();
    const create = page.getByRole("dialog", { name: "Create team" });
    await create.getByRole("textbox", { name: "Team name" }).fill(team);
    await create.getByRole("button", { name: "Create team", exact: true }).click();
    await expect(page.getByText(`${team} created.`)).toBeVisible();

    await page
      .getByRole("table", { name: "Teams" })
      .getByRole("link", { name: team, exact: true })
      .click();
    await expect(page.getByRole("heading", { name: team, level: 2 })).toBeVisible();

    // Members: pick the user in the popover and add them.
    await page.getByRole("button", { name: "Add members" }).click();
    await page.getByRole("option", { name: new RegExp(`^${username}`) }).click();
    await page.getByRole("button", { name: "Add 1 member" }).click();
    await expect(page.getByText(`1 member added to ${team}.`)).toBeVisible();

    // Access: Viewer is the dialog's default role, and the extras stay off.
    await page.getByRole("button", { name: "Add access" }).click();
    const grant = page.getByRole("dialog", { name: "Add access" });
    await grant.getByRole("combobox", { name: "Environment" }).click();
    await page.getByRole("option", { name: granted.name, exact: true }).click();
    await expect(grant.getByRole("radio", { name: "Viewer" })).toBeChecked();
    await grant.getByRole("button", { name: "Add access" }).click();
    await expect(page.getByText("Access saved.")).toBeVisible();
    await expect(page.getByRole("table", { name: `Direct access of ${team}` })).toContainText(
      granted.name,
    );

    // The user's own page says why they have access: through the team.
    await page.goto(at("/settings/access/users"));
    await page
      .getByRole("table", { name: "Users" })
      .getByRole("link", { name: username, exact: true })
      .click();
    const effective = page.getByRole("table", { name: `Access of ${username} by environment` });
    await expect(effective).toContainText(granted.name);
    await expect(effective).toContainText("Viewer");
    await expect(effective).toContainText(`Team ${team}`);
  });

  // A second browser that is not signed in as the admin: the person who was
  // sent the link.
  const context = await browser.newContext({
    baseURL: ORIGIN,
    ignoreHTTPSErrors: true,
    storageState: { cookies: [], origins: [] },
    locale: "en-GB",
    timezoneId: "UTC",
  });
  const viewer = await context.newPage();
  const tracker = await trackPage(viewer);
  try {
    await test.step("the user opens the link, sets a password and signs in", async () => {
      await viewer.goto(invite);
      await expect(
        viewer.getByRole("heading", { name: "Set your password", level: 2 }),
      ).toBeVisible();
      // The token is moved out of the address bar before anything renders.
      expect(viewer.url()).not.toContain("token");
      await viewer.getByLabel("New password", { exact: true }).fill(VIEWER_PASSWORD);
      await viewer.getByLabel("Confirm new password").fill(VIEWER_PASSWORD);
      await viewer.getByRole("button", { name: "Set password" }).click();

      // No automatic sign-in: the page goes to sign-in with a notice.
      await expect(viewer).toHaveURL(/\/bayplan\/login/);
      await expect(viewer.getByText("Password set. Sign in to continue.")).toBeVisible();
      await viewer.getByLabel("Username").fill(username);
      await viewer.getByLabel("Password", { exact: true }).fill(VIEWER_PASSWORD);
      await viewer.getByRole("button", { name: "Sign in" }).click();
      await expect(viewer.getByRole("button", { name: "User menu" })).toBeVisible();
    });

    await test.step("they see only the granted environment, read only", async () => {
      await viewer.goto(at("/environments"));
      await expect(viewer.getByRole("heading", { name: "Environments", level: 1 })).toBeVisible();
      await expect(viewer.getByRole("main").locator("a[title]")).toHaveCount(1);
      await expect(viewer.getByRole("main").locator("a[title]").first()).toHaveAttribute(
        "title",
        granted.name,
      );

      await viewer.getByRole("button", { name: "Switch environment" }).click();
      await expect(viewer.getByRole("option", { name: new RegExp(granted.name) })).toBeVisible();
      await expect(viewer.getByRole("option", { name: new RegExp(hidden.name) })).toHaveCount(0);
      await viewer.keyboard.press("Escape");

      // Resource lists load.
      await viewer.goto(at(`/env/${granted.id}`));
      await expect(viewer.getByRole("region", { name: "Dashboard" })).toBeVisible();
      for (const [path, title] of [
        ["containers", "Containers"],
        ["images", "Images"],
        ["volumes", "Volumes"],
        ["networks", "Networks"],
      ] as const) {
        await viewer.goto(at(`/env/${granted.id}/${path}`));
        await expect(viewer.getByRole("heading", { name: title, level: 1 })).toBeVisible();
      }

      // Nothing to administer: no Settings, no Audit, and the Access pages are
      // not available at their address either.
      await expect(viewer.getByRole("link", { name: "Settings", exact: true })).toHaveCount(0);
      await expect(viewer.getByRole("link", { name: "Audit", exact: true })).toHaveCount(0);
      await viewer.goto(at("/settings/access/users"));
      await expect(
        viewer.getByRole("heading", { name: "You don't have permission to view this page" }),
      ).toBeVisible();
      await expect(viewer.getByRole("button", { name: "Create user" })).toHaveCount(0);
    });

    await test.step("an environment they were not given is Page not found", async () => {
      await viewer.goto(at(`/env/${hidden.id}`));
      await expect(viewer.getByRole("heading", { name: "Page not found" })).toBeVisible();
      // Indistinguishable from one that does not exist.
      await viewer.goto(at("/env/00000000-0000-4000-8000-000000000000"));
      await expect(viewer.getByRole("heading", { name: "Page not found" })).toBeVisible();
    });

    await test.step("a direct API call for an action the UI hides is refused", async () => {
      const rename = await apiAs(context, "PATCH", `/api/env/${granted.id}`, {
        name: "renamed-by-a-viewer",
      });
      expect(rename.status(), "PATCH /api/env/{id} as a Viewer").toBe(403);
      // The environment they cannot see answers as if it were not there.
      const unseen = await apiAs(context, "PATCH", `/api/env/${hidden.id}`, {
        name: "renamed-by-a-viewer",
      });
      expect(unseen.status(), "PATCH /api/env/{id} on an unseen environment").toBe(404);
      // And nothing changed.
      const after = await environmentsOf(page.context());
      expect(after.find((e) => e.id === granted.id)?.name).toBe(granted.name);
    });
  } finally {
    tracker.assertSameOrigin(ORIGIN);
    tracker.assertNoCspViolations();
    await context.close();
  }

  await test.step("the admin's audit log shows the refusal", async () => {
    await page.goto(at("/audit"));
    const table = page.getByRole("table", { name: "Audit log" });
    const refused = table
      .getByRole("row")
      .filter({ hasText: username })
      .filter({ has: page.getByRole("cell", { name: "Permission denied", exact: true }) });
    await expect(refused.first()).toBeVisible();
    await expect(refused.first()).toContainText("Denied");
  });
});
