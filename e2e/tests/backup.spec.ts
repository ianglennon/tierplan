// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Golden path 7 (plan 03-29, ROADMAP Phase 3 success criterion 5): an encrypted
// backup is downloaded from Settings > Backup, something is changed, and the
// backup is restored over the running instance with the typed confirmation.
// Coaming restarts into the backup, says "Backup restored", and the admin signs
// in again with an account that came from the backup; what was made after the
// backup is gone (PLAT-03).
//
// It runs last: the restore replaces the state of the server every other spec
// shares and ends every session, the stored one included.

import { AUTH_STATE, ADMIN, at } from "./env";
import { compose, createUserViaUI, expect, retryName, test } from "./helpers";

test.use({ storageState: AUTH_STATE });

const PASSPHRASE = "a backup passphrase of sixteen or more";

test("a backup is downloaded, restored over the running instance, and the admin signs in again without what came after it", async ({
  page,
}, info) => {
  test.setTimeout(300_000);
  const after = retryName("afterbackup", info);
  let file = "";

  const instance = await page.request.get(at("/api/instance"));
  expect(instance.ok()).toBe(true);
  const { instanceName } = (await instance.json()) as { instanceName: string };
  expect(instanceName, "the instance name the restore asks to be typed").not.toBe("");

  await test.step("download a backup with a passphrase", async () => {
    await page.goto(at("/settings/backup"));
    await expect(page.getByRole("heading", { name: "Back up and restore Coaming" })).toBeVisible();
    const card = page.getByRole("region", { name: "Download a backup" });
    await card.getByLabel("Passphrase", { exact: true }).fill(PASSPHRASE);
    await card.getByLabel("Confirm passphrase").fill(PASSPHRASE);

    const [download] = await Promise.all([
      page.waitForEvent("download"),
      card.getByRole("button", { name: "Download backup" }).click(),
    ]);
    expect(download.suggestedFilename()).toMatch(/^coaming-backup-\d{8}-\d{4}\.coaming$/);
    file = info.outputPath(download.suggestedFilename());
    await download.saveAs(file);
    await expect(page.getByText("Backup downloaded.")).toBeVisible();
  });

  await test.step("make a change that the backup does not hold", async () => {
    await createUserViaUI(page, after);
    await expect(
      page.getByRole("table", { name: "Users" }).getByRole("link", { name: after, exact: true }),
    ).toBeVisible();
  });

  await test.step("check the backup in the Restore card and read what is in it", async () => {
    await page.goto(at("/settings/backup"));
    const card = page.getByRole("region", { name: "Restore from a backup" });
    // Nothing uploads until Check backup.
    await card.locator('input[type="file"]').setInputFiles(file);
    await card.getByLabel("Passphrase", { exact: true }).fill(PASSPHRASE);
    await card.getByRole("button", { name: "Check backup" }).click();
    await expect(card.getByRole("heading", { name: "What's in this backup" })).toBeVisible();
    await expect(card).toContainText(`From ${instanceName}`);
    await expect(card).toContainText(/\d+ users?/);
  });

  await test.step("restore it with the instance name typed, and wait for Coaming to come back", async () => {
    const card = page.getByRole("region", { name: "Restore from a backup" });
    await card.getByRole("button", { name: "Restore backup" }).click();

    const confirm = page.getByRole("alertdialog", { name: "Restore this backup?" });
    // Focus starts on Cancel, and the button waits for the name.
    await expect(confirm.getByRole("button", { name: "Cancel" })).toBeFocused();
    const restore = confirm.getByRole("button", { name: "Restore backup" });
    await expect(restore).toHaveAttribute("aria-disabled", "true");
    await confirm.getByLabel(`Type ${instanceName} to confirm`).fill(instanceName);
    await expect(restore).toHaveAttribute("aria-disabled", "false");
    await restore.click();

    await expect(page.getByRole("heading", { name: "Backup restored" })).toBeVisible({
      timeout: 180_000,
    });
    await expect(
      page.getByText("You were signed out. Sign in with an account from the backup."),
    ).toBeVisible();
  });

  await test.step("sign in as the admin from the backup", async () => {
    await page.getByRole("link", { name: "Go to sign-in" }).click();
    await expect(page).toHaveURL(/\/coaming\/login/);
    // Right after the restart into the backup, a busy runner can answer the sign-in
    // page's first request with an error; press Retry as a person would.
    const username = page.getByLabel("Username");
    await expect(async () => {
      const retry = page.getByRole("button", { name: "Retry" });
      if (await retry.isVisible()) await retry.click();
      await expect(username).toBeVisible({ timeout: 3_000 });
    }).toPass({ timeout: 30_000 });
    await username.fill(ADMIN.username);
    await page.getByLabel("Password", { exact: true }).fill(ADMIN.password);
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByRole("button", { name: "User menu" })).toBeVisible();
  });

  await test.step("what was made after the backup is gone, what came before it is back", async () => {
    await page.goto(at("/settings/access/users"));
    const table = page.getByRole("table", { name: "Users" });
    await expect(table.getByRole("link", { name: ADMIN.username, exact: true })).toBeVisible();
    await expect(table.getByRole("link", { name: after, exact: true })).toHaveCount(0);
    // The user the access spec made before the backup is still here.
    await expect(table.getByRole("link", { name: /^viewer1/ }).first()).toBeVisible();

    // The restore is in the audit log, and the chain still verifies.
    await page.goto(at("/audit"));
    await expect(
      page
        .getByRole("table", { name: "Audit log" })
        .getByRole("cell", { name: "Restored backup", exact: true })
        .first(),
    ).toBeVisible();
    expect(compose("exec", "-T", "coaming", "coaming", "audit", "verify")).toMatch(
      /^audit chain OK: \d+ entries/m,
    );
  });
});
