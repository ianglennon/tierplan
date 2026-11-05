// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Golden path 12 (plan 12-36): image updates and the fleet view, end to end through the built image
// behind the reverse proxy at /tierplan. A managed standalone stack runs an image from the registry
// service of this topology. The server's own check (a HEAD request, D-01) finds it up to date; a
// new build is pushed under the same tag; the next check shows "Update available" on the stacks
// list, the stack's services and the dashboard; "Update stack" updates it in one confirm; the
// stack's History records an "Image update" and the badge returns to "Up to date" (IMG-04, IMG-05,
// IMG-06). The second test skips a version and takes the skip back, reads the Overview's tiles and
// tabs, and finds the stack with Cmd/Ctrl+K (IMG-08, ENV-10, ENV-11).
//
// The engine reaches the registry through the dind service's --insecure-registry flag, and the
// server through a registry credential marked insecure: both are test topology only.

import type { Page } from "@playwright/test";

import { AUTH_STATE, at } from "./env";
import {
  createStack,
  deleteCredentialAs,
  dropRegistryImage,
  E2E_REGISTRY,
  engineIds,
  environmentsOf,
  expect,
  openReview,
  registryCredential,
  removeStackIfPresent,
  repushRegistryImage,
  seedRegistryImage,
  test,
  typeInEditor,
  waitForDeployComplete,
} from "./helpers";

test.use({ storageState: AUTH_STATE });

const IMAGE = `${E2E_REGISTRY}/e2e/upd:1`;

const COMPOSE = [
  "services:",
  "  web:",
  `    image: ${IMAGE}`,
  '    command: ["sleep", "3600"]',
  "    stop_grace_period: 1s",
  "",
].join("\n");

// shot attaches the page as it looks now to the test's report.
async function shot(page: Page, name: string): Promise<void> {
  await test.info().attach(name, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
}

// localEnvId is the id of the Docker host TierPlan manages through its socket.
async function localEnvId(page: Page): Promise<string> {
  const local = (await environmentsOf(page.context())).find((e) => e.isLocalSocket);
  expect(local, "the managed local host is an environment").toBeTruthy();
  return local?.id ?? "";
}

// stackRow is the row of the stack in the environment's Stacks list.
function stackRow(page: Page, name: string) {
  return page.getByRole("row").filter({ hasText: name });
}

// deployStack makes the stack from COMPOSE, deploys it and returns on its overview.
async function deployStack(page: Page, envId: string, name: string): Promise<void> {
  await createStack(page, envId, name);
  await page.getByRole("tab", { name: "Editor" }).click();
  await typeInEditor(page, "compose.yaml, YAML", COMPOSE);
  await openReview(page, name);
  await page.getByRole("button", { name: "Deploy stack" }).click();
  await waitForDeployComplete(page);
  expect(engineIds("dind", "container", `com.docker.compose.project=${name}`)).toHaveLength(1);
}

// checkNow asks the registries again for the images of the environment with Check now in the row
// menu of the stack's container, and waits for the server's answer to the request, which the
// toast then reads: "Checked 1 image. 1 update available." `found` is the part of that answer the
// caller expects. The server lets a caller check an environment once in 10 seconds and answers
// 429 to a press in between, so a press the server turned away is made again until it takes: the
// wait is for the server's own answer, never for a stretch of time.
async function checkNow(
  page: Page,
  envId: string,
  container: string,
  found: RegExp,
): Promise<void> {
  await page.goto(at(`/env/${envId}/containers`));
  const row = page.getByRole("row").filter({ hasText: container });
  await expect(async () => {
    await row.getByRole("button", { name: `Actions for ${container}` }).click();
    const [answer] = await Promise.all([
      page.waitForResponse(
        (r) =>
          r.request().method() === "POST" && r.url().endsWith(`/api/env/${envId}/updates/check`),
      ),
      page.getByRole("menuitem", { name: "Check now" }).click(),
    ]);
    expect(answer.status(), "the server asked to wait before checking again").toBe(200);
  }).toPass({ timeout: 60_000, intervals: [1_000, 2_000, 5_000] });
  await expect(page.getByText(/^Checked \d+ images?\./)).toContainText(found);
}

test("a new build under the tag shows as an update on every surface and Update stack applies it", async ({
  page,
}) => {
  test.setTimeout(480_000);
  const NAME = "e2e-upd";
  const envId = await localEnvId(page);
  await removeStackIfPresent(page.context(), envId, NAME);
  seedRegistryImage(IMAGE);
  const credential = await registryCredential(page.context(), "e2e-updates-registry");

  try {
    await test.step("deploy a stack whose image is on the registry", async () => {
      await deployStack(page, envId, NAME);
      await shot(page, "1-deployed");
    });

    await test.step("Check now finds the image up to date", async () => {
      await checkNow(page, envId, `${NAME}-web-1`, /0 updates available\./);
      await page.goto(at(`/env/${envId}/stacks`));
      await expect(stackRow(page, NAME).getByText("Up to date", { exact: true })).toBeVisible();
      await shot(page, "2-up-to-date");
    });

    await test.step("a new build is pushed and Check now shows the update on the stacks list, the stack and the dashboard", async () => {
      repushRegistryImage(IMAGE, "build-2");
      await checkNow(page, envId, `${NAME}-web-1`, /1 update available\./);
      await page.goto(at(`/env/${envId}/stacks`));
      await expect(stackRow(page, NAME).getByText("1 update")).toBeVisible();
      await stackRow(page, NAME).getByRole("link", { name: NAME }).click();
      await expect(page.getByText("Update available").first()).toBeVisible();
      await page.goto(at(`/env/${envId}`));
      await expect(
        page
          .getByRole("region", { name: "Dashboard" })
          .getByRole("link", { name: /^Updates available\b/ }),
      ).toContainText("1");
      await shot(page, "3-update-available");
    });

    await test.step("Update stack names the service with two digests and applies the update", async () => {
      await page.goto(at(`/env/${envId}/stacks`));
      await stackRow(page, NAME).getByRole("link", { name: NAME }).click();
      await page.getByRole("button", { name: "Update stack (1)" }).click();
      const dialog = page.getByRole("dialog", { name: `Update stack ${NAME}` });
      const rows = dialog.getByRole("list", { name: "What will change" }).getByRole("listitem");
      await expect(rows).toHaveCount(1);
      await expect(rows.first()).toContainText("web");
      await expect(rows.first().getByRole("button", { name: /^Pinned to sha256:/ })).toBeVisible();
      await expect(
        rows.first().getByRole("button", { name: /^Will be pinned to sha256:/ }),
      ).toBeVisible();
      await shot(page, "4-confirm");
      await dialog.getByRole("button", { name: "Update 1 service" }).click();
      await waitForDeployComplete(page);
      await shot(page, "5-console-done");
    });

    await test.step("History records the Image update and the badge is Up to date again", async () => {
      await page.goto(at(`/env/${envId}/stacks`));
      await stackRow(page, NAME).getByRole("link", { name: NAME }).click();
      await page.getByRole("tab", { name: "History" }).click();
      await expect(page.getByText("Image update").first()).toBeVisible();
      await page.getByRole("tab", { name: "Overview" }).click();
      await expect(page.getByText("Up to date", { exact: true }).first()).toBeVisible();
      await shot(page, "6-history-and-badge");
    });
  } finally {
    await removeStackIfPresent(page.context(), envId, NAME);
    await deleteCredentialAs(page.context(), credential.id);
    dropRegistryImage(IMAGE);
  }
});

test("skip a version and take it back, read the Overview and find the stack with Cmd/Ctrl+K", async ({
  page,
}) => {
  test.setTimeout(480_000);
  const NAME = "e2e-upd-skip";
  const CONTAINER = `${NAME}-web-1`;
  const envId = await localEnvId(page);
  await removeStackIfPresent(page.context(), envId, NAME);
  seedRegistryImage(IMAGE);
  const credential = await registryCredential(page.context(), "e2e-updates-registry-2");

  try {
    await test.step("an update is on offer for a deployed stack", async () => {
      await deployStack(page, envId, NAME);
      repushRegistryImage(IMAGE, "build-3");
      await checkNow(page, envId, CONTAINER, /1 update available\./);
      await page.goto(at(`/env/${envId}/containers`));
      const row = page.getByRole("row").filter({ hasText: CONTAINER });
      await expect(row.getByText("Update available")).toBeVisible();
    });

    await test.step("Skip this version reads Version skipped, and Undo brings the update back", async () => {
      const row = page.getByRole("row").filter({ hasText: CONTAINER });
      await row.getByRole("button", { name: `Actions for ${CONTAINER}` }).click();
      await page.getByRole("menuitem", { name: "Skip this version" }).click();
      await expect(row.getByText("Version skipped")).toBeVisible();
      await expect(
        page.getByText(`for ${CONTAINER}. A newer version will show again.`),
      ).toBeVisible();
      await shot(page, "7-skipped");
      await page.getByRole("button", { name: "Undo" }).click();
      await expect(row.getByText("Update available")).toBeVisible();
    });

    await test.step("the Overview shows its four tiles and the Updates tab lists the workload with its environment", async () => {
      await page.goto(at("/environments"));
      const tiles = page.getByRole("region", { name: "Fleet summary" });
      for (const label of [
        "Environments",
        "Failing services",
        "Updates available",
        "Pools near full",
      ]) {
        await expect(tiles.getByRole("link", { name: new RegExp(`^${label}\\b`) })).toBeVisible();
      }
      await tiles.getByRole("link", { name: /^Updates available\b/ }).click();
      await expect(page).toHaveURL(/[?&]tab=updates/);
      const table = page.getByRole("table", { name: "Image updates" });
      const row = table.getByRole("row").filter({ hasText: CONTAINER });
      await expect(row).toBeVisible();
      await expect(row.getByText("Update available")).toBeVisible();
      await expect(row.getByText(`in ${NAME}`)).toBeVisible();
      await shot(page, "8-overview-updates");
    });

    await test.step("the Failing services tab has an empty state of its own", async () => {
      await page.getByRole("tab", { name: "Failing services" }).click();
      await expect(page.getByText("Nothing is failing")).toBeVisible();
    });

    await test.step("Cmd/Ctrl+K finds the stack, highlighted, and Enter opens it", async () => {
      await page.goto(at(`/env/${envId}`));
      await expect(page.getByRole("region", { name: "Dashboard" })).toBeVisible();
      await page.keyboard.press("ControlOrMeta+K");
      const palette = page.getByRole("dialog", { name: "Search TierPlan" });
      await palette.getByRole("combobox").fill(NAME);
      const hit = palette.getByRole("option").filter({ has: page.locator("mark") });
      await expect(hit.first()).toBeVisible();
      await expect(palette.getByText("Stacks", { exact: true })).toBeVisible();
      await expect(palette.locator("mark").first()).toHaveText(NAME);
      await shot(page, "9-search");
      // The first row is "Search all results"; the stack hit is the one below it.
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(page).toHaveURL(new RegExp(`/tierplan/env/${envId}/stacks/[^/]+`));
      await expect(page.getByRole("tab", { name: "History" })).toBeVisible();
      await expect(page.getByText(NAME, { exact: true }).first()).toBeVisible();
    });
  } finally {
    await removeStackIfPresent(page.context(), envId, NAME);
    await deleteCredentialAs(page.context(), credential.id);
    dropRegistryImage(IMAGE);
  }
});
