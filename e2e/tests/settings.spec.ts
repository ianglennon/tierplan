// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// The Settings tabs against the real server (plans 23 and 26): saving the
// instance name, renaming and removing the managed host with its confirmation
// and adding it back, and the About tab's source links. It runs last, because
// removing the host changes the environment id the earlier specs used.

import { AUTH_STATE, at } from "./env";
import { expect, openFirstEnvironment, test } from "./helpers";

test.use({ storageState: AUTH_STATE });

test("General saves the instance name and keeps it across a reload", async ({ page }) => {
  await page.goto(at("/settings/general"));
  await expect(page.getByRole("heading", { name: "Settings", level: 1 })).toBeVisible();
  await expect(page.getByRole("tab", { name: "General" })).toHaveAttribute("aria-selected", "true");

  const name = page.getByRole("textbox", { name: "Instance name" });
  // The Shells card has a Save settings button of its own, so this one is found
  // through the group that holds the instance name.
  const save = page
    .getByRole("group")
    .filter({ has: name })
    .getByRole("button", { name: "Save settings" });
  // Nothing changed yet, so there is nothing to save.
  await expect(save).toBeDisabled();
  // TIERPLAN_PUBLIC_URL is set in this topology (the proxy's address with its
  // subpath), so the field is locked and says which variable holds it. The agent
  // URL is set too, so it is locked as well.
  await expect(page.getByRole("img", { name: "Locked" }).first()).toBeVisible();
  await expect(page.getByText("Set by the TIERPLAN_PUBLIC_URL environment variable.")).toBeVisible();

  await name.fill("E2E TierPlan");
  await expect(save).toBeEnabled();
  await save.click();
  await expect(page.getByText("Settings saved.")).toBeVisible();

  await page.reload();
  await expect(page.getByRole("textbox", { name: "Instance name" })).toHaveValue("E2E TierPlan");
});

test("Environments renames the host, asks before removing it, and adds it back", async ({
  page,
}) => {
  await page.goto(at("/settings/environments"));
  await expect(page.getByRole("tab", { name: "Environments" })).toHaveAttribute(
    "aria-selected",
    "true",
  );

  // Rename.
  await page.getByRole("button", { name: /^Rename / }).click();
  const rename = page.getByRole("dialog", { name: "Rename environment" });
  await rename.getByRole("textbox", { name: "Name" }).fill("lab-1");
  await rename.getByRole("button", { name: "Rename environment" }).click();
  await expect(rename).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Rename lab-1" })).toBeVisible();

  // Remove: the dialog starts on Cancel, and Cancel changes nothing.
  await page.getByRole("button", { name: "Remove host lab-1" }).click();
  const confirm = page.getByRole("alertdialog", { name: "Remove lab-1?" });
  await expect(confirm.getByRole("button", { name: "Cancel" })).toBeFocused();
  await confirm.getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByRole("button", { name: "Rename lab-1" })).toBeVisible();

  await page.getByRole("button", { name: "Remove host lab-1" }).click();
  await page
    .getByRole("alertdialog", { name: "Remove lab-1?" })
    .getByRole("button", { name: "Remove host" })
    .click();

  // Nothing is managed now, and the same panel as /welcome offers the host again.
  await expect(page.getByRole("button", { name: "Manage this host" })).toBeVisible();
  await page.getByRole("button", { name: "Manage this host" }).click();
  await expect(page.getByRole("button", { name: /^Rename / })).toBeVisible();

  // The dashboard works on the re-added host.
  await openFirstEnvironment(page);
  await expect(page.getByRole("region", { name: "Dashboard" })).toBeVisible();
});

test("About shows the version, the source links and the third-party notices", async ({ page }) => {
  await page.goto(at("/settings/about"));
  const main = page.getByRole("main");
  const source = main.getByRole("link", { name: "Source code (opens in a new tab)" });
  await expect(source).toBeVisible();
  await expect(source).toHaveAttribute("target", "_blank");
  await expect(source).toHaveAttribute("href", /^https:\/\/github\.com\/ianglennon\/tierplan/);
  await expect(main.getByRole("link", { name: /^Commit .* source tree/ })).toBeVisible();
  await expect(main.getByRole("region", { name: "Third-party notices list" })).toBeVisible();
});
