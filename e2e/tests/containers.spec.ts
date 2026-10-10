// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Golden path 4 (Phase 4): containers, logs, a shell, the event feed, networks and
// volumes, in a real browser behind the proxy at /bayplan, against the dind
// engine Bayplan manages. Containers are started on the engine behind Bayplan's
// back, so every change the page shows arrives the way a person's own `docker
// run` would, not through a click of the page's.

import type { Page } from "@playwright/test";

import { AUTH_STATE, at } from "./env";
import {
  dind,
  expect,
  importShellImage,
  openFirstEnvironment,
  runLogger,
  SHELL_IMAGE,
  test,
} from "./helpers";

test.use({ storageState: AUTH_STATE });

// The spec is one story: the later tests use the container the first one starts.
test.describe.configure({ mode: "serial" });

// The row of one container in the containers list.
function rowOf(page: Page, name: string) {
  return page.getByRole("row").filter({ has: page.getByRole("link", { name, exact: true }) });
}

// Everything this spec makes, and what the dashboard spec made under names this one
// uses. A retried attempt starts clean too.
function clearEngine(): void {
  dind("docker rm -f e2e-web e2e-a e2e-b e2e-box >/dev/null 2>&1 || true");
  dind("docker network rm e2e-net >/dev/null 2>&1 || true");
  dind("docker volume rm e2e-vol >/dev/null 2>&1 || true");
}

let envId = "";

test.beforeAll(() => {
  clearEngine();
  importShellImage();
});

test.afterAll(() => {
  clearEngine();
});

test("a container started behind Bayplan's back appears live, is stopped and started from its row, and two are restarted together", async ({
  page,
}) => {
  envId = await openFirstEnvironment(page);
  await page.goto(at(`/env/${envId}/containers`));
  await expect(page.getByRole("heading", { name: "Containers", level: 1 })).toBeVisible();

  // A flag on this document is gone if anything reloads it.
  await page.evaluate(() => {
    (window as unknown as { __noReload: boolean }).__noReload = true;
  });

  runLogger("e2e-web");
  const web = rowOf(page, "e2e-web");
  await expect(web).toBeVisible();
  await expect(web).toContainText("Running");
  expect(
    await page.evaluate(() => (window as unknown as { __noReload?: boolean }).__noReload),
    "the page was reloaded",
  ).toBe(true);

  // Stop asks first; the dialog names the container.
  await web.getByRole("button", { name: "Actions for e2e-web" }).click();
  await page.getByRole("menuitem", { name: "Stop", exact: true }).click();
  const stop = page.getByRole("alertdialog", { name: "Stop e2e-web?" });
  await stop.getByRole("button", { name: "Stop container" }).click();
  await expect(web).toContainText("Exited", { timeout: 30_000 });
  expect(dind("docker inspect -f '{{.State.Status}}' e2e-web")).toBe("exited");

  // Start runs at once, with no question.
  await web.getByRole("button", { name: "Actions for e2e-web" }).click();
  await page.getByRole("menuitem", { name: "Start", exact: true }).click();
  await expect(web).toContainText("Running", { timeout: 30_000 });
  expect(dind("docker inspect -f '{{.State.Status}}' e2e-web")).toBe("running");

  // Two more, selected together and restarted from the bulk bar with one toast.
  for (const name of ["e2e-a", "e2e-b"]) {
    dind(`docker rm -f ${name} >/dev/null 2>&1 || true`);
    dind(`docker run -d --stop-timeout 1 --name ${name} ${SHELL_IMAGE} sleep 3600`);
  }
  const startedBefore = ["e2e-a", "e2e-b"].map((n) =>
    dind(`docker inspect -f '{{.State.StartedAt}}' ${n}`),
  );
  await expect(rowOf(page, "e2e-a")).toBeVisible();
  await expect(rowOf(page, "e2e-b")).toBeVisible();
  await page.getByRole("checkbox", { name: "Select e2e-a" }).check();
  await page.getByRole("checkbox", { name: "Select e2e-b" }).check();
  const bar = page.getByRole("toolbar", { name: "Bulk actions" });
  await expect(bar).toContainText("2 selected");
  await bar.getByRole("button", { name: "Restart", exact: true }).click();
  // One toast sums up both, rather than one per container.
  const toast = page.locator("[data-sonner-toast]").filter({ hasText: /^Restarted/ });
  await expect(toast).toContainText("Restarted 2 containers.", { timeout: 30_000 });
  await expect(toast).toHaveCount(1);
  // The engine restarted both.
  const startedAfter = ["e2e-a", "e2e-b"].map((n) =>
    dind(`docker inspect -f '{{.State.StartedAt}}' ${n}`),
  );
  expect(startedAfter[0]).not.toBe(startedBefore[0]);
  expect(startedAfter[1]).not.toBe(startedBefore[1]);
});
