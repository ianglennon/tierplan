// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Golden path 9 (plan 09-33): a stack from a git repository, end to end through the built image behind the reverse
// proxy at /bayplan. The git server is a service of this topology (e2e/gitserver) with a certificate of its own, so the
// path is what a self-hoster with a private CA does: add the repository with its CA under Advanced, test it, create a
// stack from a folder with the From git page, review and deploy it, push a change to the repository, press Check now,
// see the stack Behind by one commit, press Review update, deploy, and see it In sync (GIT-01 to GIT-08).
//
// The engine Bayplan manages has no route to a registry, and a stack pins every image to a digest a registry gave, so it
// runs a registry of its own on its loopback (seedStackImage), as the stacks spec does. The repository's compose file
// names that image. Nothing here reaches the network outside the topology.

import type { BrowserContext, Page } from "@playwright/test";

import { AUTH_STATE, at } from "./env";
import {
  apiAs,
  dockerExec,
  dropStackImage,
  engineIds,
  environmentsOf,
  expect,
  removeStackIfPresent,
  seedStackImage,
  test,
  waitForDeployComplete,
} from "./helpers";

test.use({ storageState: AUTH_STATE });

const IMAGE = "localhost:5000/e2e/gitweb:1";
const STACK = "e2e-git";
const REPO = "e2e-stacks";
// The git service of e2e/compose.yaml, by the name its certificate carries.
const REPO_URL = "https://gitserver/git/stacks.git";

// shot attaches the page as it looks now to the test's report.
async function shot(page: Page, name: string): Promise<void> {
  await test.info().attach(name, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
}

// removeRepositoryIfPresent deletes the instance repositories of this name through the API, so a run that
// stopped halfway leaves nothing for the next to trip over. It is a no-op when there is none.
async function removeRepositoryIfPresent(context: BrowserContext, name: string): Promise<void> {
  const res = await context.request.get(at("/api/git/repos"));
  expect(res.ok(), "GET /api/git/repos").toBe(true);
  const items = ((await res.json()) as { items: { id: string; name: string }[] }).items;
  for (const repo of items.filter((r) => r.name === name)) {
    const gone = await apiAs(context, "DELETE", `/api/git/repos/${repo.id}`);
    expect([204, 404], `DELETE /api/git/repos/${repo.id}: ${await gone.text()}`).toContain(
      gone.status(),
    );
  }
}

// modeRunning is the MODE setting of the stack's one container, read from the engine.
function modeRunning(): string {
  const ids = engineIds("dind", "container", `com.docker.compose.project=${STACK}`);
  expect(ids, "the stack runs one container").toHaveLength(1);
  // The image has a shell and little else, so the value is echoed, not read with printenv.
  return dockerExec("dind", `docker exec ${ids[0]} sh -c 'echo "$MODE"'`);
}

test("add a repository, create a stack from a folder, deploy it, and pull an update", async ({
  page,
}) => {
  test.setTimeout(600_000);
  const local = (await environmentsOf(page.context())).find((e) => e.isLocalSocket);
  expect(local, "the managed local host is an environment").toBeTruthy();
  const envId = local?.id ?? "";
  // The managed engine and the instance must hold nothing from an earlier attempt.
  dockerExec(
    "dind",
    `docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=${STACK}) >/dev/null 2>&1; docker network rm ${STACK}_default >/dev/null 2>&1; true`,
  );
  await removeStackIfPresent(page.context(), envId, STACK);
  await removeRepositoryIfPresent(page.context(), REPO);
  seedStackImage("dind", IMAGE);
  // The server made its certificate when it started; it is its own authority, so its PEM is the CA to trust.
  const caPem = dockerExec("gitserver", "cat /certs/cert.pem");
  expect(caPem).toContain("BEGIN CERTIFICATE");
  // Back to the first commit, so the run starts from "mode one" whatever an earlier attempt pushed.
  dockerExec("gitserver", "commit-change one");

  try {
    await test.step("add the repository with its CA under Advanced and test it", async () => {
      await page.goto(at("/git"));
      await page.getByRole("button", { name: "Add repository" }).first().click();
      const sheet = page.getByRole("dialog", { name: "Add a repository" });
      await sheet.getByRole("textbox", { name: "Name" }).fill(REPO);
      await sheet.getByRole("textbox", { name: "Repository address" }).fill(REPO_URL);
      // Without the CA the server's certificate is not trusted, and the test says so.
      await sheet.getByRole("button", { name: "Test repository" }).click();
      await expect(sheet.getByText(/The certificate from gitserver isn't trusted/)).toBeVisible({
        timeout: 60_000,
      });
      await sheet.getByRole("button", { name: "Advanced" }).click();
      await sheet.getByRole("textbox", { name: "Custom CA certificate (PEM)" }).fill(caPem);
      await sheet.getByRole("button", { name: "Test repository" }).click();
      await expect(sheet.getByText(/Found 1 branches? and 0 tags\./)).toBeVisible({
        timeout: 60_000,
      });
      await shot(page, "1-repository-tested");
      await sheet.getByRole("button", { name: "Save repository" }).click();
      await expect(page.getByText(`Added repository ${REPO}.`).first()).toBeVisible();
      await expect(page.getByRole("row").filter({ hasText: REPO })).toBeVisible();
    });

    await test.step("create the stack from a folder of the repository", async () => {
      await page.goto(at(`/env/${envId}/stacks`));
      await page.getByRole("button", { name: "Create stack" }).first().click();
      const dialog = page.getByRole("dialog", { name: "Create a stack" });
      await dialog.getByRole("textbox", { name: /^Name/ }).fill(STACK);
      await dialog.getByRole("radio", { name: /A git repository/ }).click();
      await dialog.getByRole("button", { name: "Choose repository" }).click();
      await expect(page.getByRole("heading", { name: `Create ${STACK} from git` })).toBeVisible();

      await page.getByRole("combobox", { name: "Repository" }).click();
      await page.getByRole("option", { name: new RegExp(REPO) }).click();
      // The default branch is the starting choice.
      await expect(page.getByRole("combobox", { name: "Branch" })).toContainText("main");

      const folders = page.getByRole("list", { name: "Folder contents" });
      await folders.getByRole("button", { name: /^stacks/ }).click();
      await folders.getByRole("button", { name: /^web/ }).click();
      await page.getByRole("button", { name: "Use this folder" }).click();
      await expect(page.getByText("Found 1 service")).toBeVisible({ timeout: 60_000 });
      await shot(page, "2-from-git");
      await page.getByRole("button", { name: "Create stack" }).click();
      await expect(page).toHaveURL(new RegExp(`/bayplan/env/${envId}/stacks/[^/]+`), {
        timeout: 60_000,
      });
    });

    await test.step("review and deploy, and the strip says In sync", async () => {
      await page.getByRole("link", { name: "Deploy…" }).click();
      await expect(page.getByRole("heading", { name: `Pull and redeploy ${STACK}` })).toBeVisible({
        timeout: 60_000,
      });
      await expect(page.getByText("First deploy")).toBeVisible();
      await page.getByRole("button", { name: "Deploy stack" }).click();
      await waitForDeployComplete(page);
      await shot(page, "3-deploy-complete");
      expect(engineIds("dind", "container", `com.docker.compose.project=${STACK}`)).toHaveLength(1);
      expect(modeRunning()).toBe("one");
      await page.getByRole("link", { name: "Open stack" }).click();
      const strip = page.getByRole("region", { name: "Git sync" });
      await expect(strip.getByText("In sync")).toBeVisible({ timeout: 60_000 });
    });

    await test.step("push a change, press Check now, and the stack is Behind by one commit", async () => {
      dockerExec("gitserver", "commit-change two");
      const strip = page.getByRole("region", { name: "Git sync" });
      await strip.getByRole("button", { name: "Check now" }).click();
      await expect(strip.getByText("Behind by 1 commit")).toBeVisible({ timeout: 60_000 });
      await shot(page, "4-behind");
    });

    await test.step("Review update pulls the commit, and deploying it brings the stack In sync", async () => {
      const strip = page.getByRole("region", { name: "Git sync" });
      await strip.getByRole("button", { name: "Review update" }).click();
      await expect(page.getByRole("heading", { name: `Pull and redeploy ${STACK}` })).toBeVisible({
        timeout: 60_000,
      });
      await expect(page.getByText("1 setting changed")).toBeVisible();
      await shot(page, "5-review-update");
      await page.getByRole("button", { name: "Deploy stack" }).click();
      await waitForDeployComplete(page);
      expect(modeRunning()).toBe("two");
      await page.getByRole("link", { name: "Open stack" }).click();
      const strip2 = page.getByRole("region", { name: "Git sync" });
      await expect(strip2.getByText("In sync")).toBeVisible({ timeout: 60_000 });
      await shot(page, "6-in-sync");
    });

    await test.step("Remove with the typed name deletes the stack and leaves nothing behind", async () => {
      await page.goto(at(`/env/${envId}/stacks`));
      const row = page.getByRole("row").filter({ hasText: STACK });
      await row.getByRole("button", { name: `Actions for ${STACK}` }).click();
      await page.getByRole("menuitem", { name: /^Remove/ }).click();
      const dialog = page.getByRole("dialog", { name: `Remove ${STACK}?` });
      await dialog.getByRole("textbox").fill(STACK);
      await dialog.getByRole("button", { name: "Remove stack" }).click();
      await expect(page.getByText(`Removed ${STACK}.`).first()).toBeVisible({ timeout: 120_000 });
      expect(engineIds("dind", "container", `com.docker.compose.project=${STACK}`)).toEqual([]);
      expect(engineIds("dind", "network", `com.docker.compose.project=${STACK}`)).toEqual([]);
    });
  } finally {
    await removeStackIfPresent(page.context(), envId, STACK);
    await removeRepositoryIfPresent(page.context(), REPO);
    dropStackImage("dind", IMAGE);
  }
});
