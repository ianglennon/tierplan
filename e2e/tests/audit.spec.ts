// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Golden path 4 (D-80): the audit log holds what the first two specs did, can
// be filtered by action, exports CSV, and its hash chain verifies from inside
// the container (AUTH-08, AUTH-09).

import { readFileSync } from "node:fs";

import { AUTH_STATE, at, ORIGIN } from "./env";
import { compose, expect, test } from "./helpers";

test.use({ storageState: AUTH_STATE });

// A password typed into the username box, as people do (CR-01). It is not a
// valid username (it holds a space), and no account matches it either way.
const TYPED = "Tr0ub4dor&3 hunter";

test("the audit log lists what happened, filters by action and exports CSV", async ({ page }) => {
  await page.goto(at("/audit"));
  await expect(page.getByRole("heading", { name: "Audit log", level: 1 })).toBeVisible();

  const table = page.getByRole("table", { name: "Audit log" });
  for (const label of ["Completed setup", "Failed sign-in", "Signed in", "Added environment"]) {
    await expect(
      table.getByRole("cell", { name: label, exact: true }).first(),
      `a row labelled "${label}"`,
    ).toBeVisible();
  }

  // Filter to one action: the other rows go, and the address holds the filter.
  await page.getByRole("button", { name: "Action Any action" }).click();
  // A plain click, not .check(): the box is controlled by the address, so it
  // only turns on once the router has navigated.
  const signedIn = page.getByRole("checkbox", { name: "Signed in", exact: true });
  await signedIn.click();
  await expect(signedIn).toBeChecked();
  await expect(page).toHaveURL(/auth\.login/);
  await page.keyboard.press("Escape");
  await expect(table.getByRole("cell", { name: "Signed in", exact: true }).first()).toBeVisible();
  await expect(table.getByRole("cell", { name: "Failed sign-in", exact: true })).toHaveCount(0);
  await expect(table.getByRole("cell", { name: "Completed setup", exact: true })).toHaveCount(0);

  // The export is the filtered view, as a CSV with the documented header.
  await page.getByRole("button", { name: "Export" }).click();
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("menuitem", { name: "Export CSV" }).click(),
  ]);
  expect(download.suggestedFilename()).toMatch(/\.csv$/);
  // saveAs, not path(): the browser may be a remote one, whose files are not on this disk.
  const path = test.info().outputPath("audit.csv");
  await download.saveAs(path);
  const csv = readFileSync(path, "utf8");
  expect(csv.split("\n")[0]).toMatch(/^id,time,actor_type/);
  expect(csv).toContain("auth.login");
  expect(csv).not.toContain("auth.login_failed");
});

test("the audit chain verifies from inside the container", async () => {
  // The CLI path again: root in the container, dropped to UID 1000, reading the
  // same database the server wrote.
  const out = compose("exec", "-T", "coaming", "coaming", "audit", "verify");
  expect(out).toMatch(/^audit chain OK: \d+ entries/m);
});

test("a password typed as the username never reaches the audit log, its export or the container log", async ({
  page,
  playwright,
}) => {
  // Anonymous, like a visitor at the sign-in page: no session cookie and no CSRF
  // token travel with this request. The empty storage state matters, because
  // test.use() above would otherwise hand this context the admin's cookie.
  const anonymous = await playwright.request.newContext({
    baseURL: ORIGIN,
    ignoreHTTPSErrors: true,
    storageState: { cookies: [], origins: [] },
  });
  try {
    const refused = await anonymous.post(at("/api/auth/login"), {
      data: { username: TYPED, password: "wrong wrong wrong", remember: false },
    });
    expect(refused.status()).toBe(401);
  } finally {
    await anonymous.dispose();
  }

  // The signed-in admin reads the audit log back in both export formats.
  const exports = [
    { format: "json", url: at("/api/audit/export?format=json") },
    { format: "csv", url: at("/api/audit/export?format=csv") },
  ];
  for (const { format, url } of exports) {
    const res = await page.request.get(url);
    expect(res.status(), `GET ${url}`).toBe(200);
    const body = await res.text();
    expect(body, `the ${format} export`).toContain("auth.login_failed");
    expect(body, `the ${format} export`).toContain("(unknown user)");
    expect(body, `the ${format} export`).not.toContain(TYPED);
  }

  // The same string must not be in what the container wrote to stdout either.
  const logs = compose("logs", "--no-color", "coaming");
  expect(logs).not.toContain(TYPED);
});
