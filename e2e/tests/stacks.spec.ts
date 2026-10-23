// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Golden path 7 (plan 07-31): compose stacks, end to end through the built image behind the
// reverse proxy at /bayplan. The first test writes a stack on the engine Bayplan manages through
// its socket, is told about a YAML mistake and fixes it, sets a variable, reviews, deploys, watches
// the console, sees the stack running, edits a value, sees a one-service change and removes the
// stack with the typed confirm (STK-01 to STK-07, STK-09 to STK-11). The second deploys a stack
// with a secret file to a remote host through a real agent and reads the secret inside the
// container (the standalone path over the tunnel).
//
// The engines of this topology have no route to a registry, and a stack pins every image to a
// digest a registry gave, so each engine runs a registry of its own on its loopback
// (seedStackImage), as the Go integration tests do.

import type { Page } from "@playwright/test";

import { AGENT_PIN, AUTH_STATE, at } from "./env";
import {
  createStack,
  dockerExec,
  dropStackImage,
  engineIds,
  enrollAgentHost,
  environmentsOf,
  expect,
  openReview,
  removeRemoteEnvironments,
  removeStackIfPresent,
  seedStackImage,
  test,
  typeInEditor,
  waitForDeployComplete,
} from "./helpers";

test.use({ storageState: AUTH_STATE });

const WEB_IMAGE = "localhost:5000/e2e/web:1";
const APP_IMAGE = "localhost:5000/e2e/app:1";

// shot attaches the page as it looks now to the test's report.
async function shot(page: Page, name: string): Promise<void> {
  await test.info().attach(name, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
}

const FIRST_COMPOSE = [
  "services:",
  "  web:",
  `    image: ${WEB_IMAGE}`,
  '    command: ["sleep", "3600"]',
  "    stop_grace_period: 1s",
  "    ports:",
  // biome-ignore lint/suspicious/noTemplateCurlyInString: the placeholder is for the compose file
  '      - "${WEB_PORT}:80"',
  "",
].join("\n");

test("create, check, review, deploy and remove a stack on the local host", async ({ page }) => {
  test.setTimeout(480_000);
  const NAME = "e2e-web";
  const local = (await environmentsOf(page.context())).find((e) => e.isLocalSocket);
  expect(local, "the managed local host is an environment").toBeTruthy();
  const envId = local?.id ?? "";
  // The managed engine must hold nothing of this stack from an earlier attempt.
  dockerExec(
    "dind",
    `docker rm -f $(docker ps -aq --filter label=com.docker.compose.project=${NAME}) >/dev/null 2>&1; docker network rm ${NAME}_default >/dev/null 2>&1; true`,
  );
  await removeStackIfPresent(page.context(), envId, NAME);
  seedStackImage("dind", WEB_IMAGE);

  try {
    await test.step("create the stack from an empty file", async () => {
      await createStack(page, envId, NAME);
      await shot(page, "1-new-stack");
    });

    await test.step("a YAML mistake is marked in the editor and listed under Problems", async () => {
      await page.getByRole("tab", { name: "Editor" }).click();
      const mistake = FIRST_COMPOSE.replace('    command: ["sleep"', '\tcommand: ["sleep"');
      await typeInEditor(page, "compose.yaml, YAML", mistake);
      await expect(page.locator(".cm-lint-marker-error").first()).toBeVisible({ timeout: 30_000 });
      // The panel opens itself when the check finds an error; open it only if it has not.
      const problems = page.getByRole("button", { name: /^Problems/ });
      if ((await problems.getAttribute("aria-expanded")) !== "true") await problems.click();
      await expect(
        page.getByRole("button", { name: /^Error: .*compose\.yaml line \d+$/ }),
      ).toBeVisible();
      await shot(page, "2-yaml-mistake");
    });

    await test.step("fixing it leaves the one problem the file has: a variable with no value", async () => {
      await typeInEditor(page, "compose.yaml, YAML", FIRST_COMPOSE);
      await expect(
        page.getByRole("button", { name: /^Error: WEB_PORT has no value and no default/ }),
      ).toBeVisible({ timeout: 30_000 });
      await expect(page.getByRole("button", { name: /found a tab character/ })).toHaveCount(0);
    });

    await test.step("a variable the file uses gets its value in the Variables table", async () => {
      await page.getByRole("tab", { name: "Variables" }).click();
      const value = page.getByRole("textbox", { name: "Value of WEB_PORT" });
      await expect(value).toBeVisible({ timeout: 30_000 });
      await value.fill("18080");
      await expect(page.getByText("From the table")).toBeVisible({ timeout: 30_000 });
      // With a value the file has no problem left: back in the Files pane nothing is marked.
      await page.getByRole("tab", { name: "Files" }).click();
      await expect(
        page.getByRole("button", { name: /^Problems 0 errors|^Problems No problems/ }),
      ).toBeVisible({ timeout: 30_000 });
      await expect(page.locator(".cm-lint-marker-error")).toHaveCount(0);
      await page.getByRole("tab", { name: "Variables" }).click();
      await shot(page, "3-variable");
    });

    await test.step("the review: first deploy, every service added, a new pin", async () => {
      await openReview(page, NAME);
      await expect(page.getByText("First deploy")).toBeVisible();
      await expect(page.getByText("Nothing from this stack is running yet")).toBeVisible();
      // Every service of the file is Added, and its image gets a New pin.
      await expect(page.getByRole("row", { name: /^Services\s+1\s+0\s+0$/ })).toBeVisible();
      await expect(page.getByRole("button", { name: /^Will be pinned to sha256:/ })).toBeVisible();
      await expect(page.getByRole("row", { name: /^Images\s+0 pinned, 1 new$/ })).toBeVisible();
      await shot(page, "4-review-first-deploy");
    });

    await test.step("deploy, and watch the console reach Deploy complete", async () => {
      await page.getByRole("button", { name: "Deploy stack" }).click();
      await waitForDeployComplete(page);
      await expect(page.getByText(`${NAME} is running 1 service`)).toBeVisible();
      await shot(page, "5-deploy-complete");
      // The engine really runs it, with the labels that make the stack removable.
      expect(engineIds("dind", "container", `com.docker.compose.project=${NAME}`)).toHaveLength(1);
    });

    await test.step("the overview reads Running with a digest chip", async () => {
      await page.getByRole("link", { name: "Open stack" }).click();
      await expect(page.getByText("Running", { exact: true }).first()).toBeVisible({
        timeout: 60_000,
      });
      await expect(page.getByRole("button", { name: /^Pinned to sha256:/ })).toBeVisible();
      await shot(page, "6-overview-running");
    });

    await test.step("an edited value shows as a one-service change in the review", async () => {
      await page.getByRole("tab", { name: "Editor" }).click();
      await typeInEditor(
        page,
        "compose.yaml, YAML",
        FIRST_COMPOSE.replace('"sleep", "3600"', '"sleep", "3601"'),
      );
      await openReview(page, NAME);
      await expect(page.getByText("1 setting changed")).toBeVisible();
      await shot(page, "7-review-one-change");
    });

    await test.step("Remove with the typed name deletes it and leaves nothing behind", async () => {
      await page.goto(at(`/env/${envId}/stacks`));
      const row = page.getByRole("row").filter({ hasText: NAME });
      await row.getByRole("button", { name: `Actions for ${NAME}` }).click();
      await page.getByRole("menuitem", { name: /^Remove/ }).click();
      const dialog = page.getByRole("dialog", { name: `Remove ${NAME}?` });
      const confirm = dialog.getByRole("button", { name: "Remove stack" });
      await expect(confirm).toHaveAttribute("aria-disabled", "true");
      await dialog.getByRole("textbox").fill(NAME);
      await confirm.click();
      // The row leaves the list as the removal ends, and the dialog goes with it: the answer is
      // the toast, and the list is empty again.
      await expect(page.getByText(`Removed ${NAME}.`).first()).toBeVisible({ timeout: 120_000 });
      await expect(page.getByRole("row").filter({ hasText: NAME })).toHaveCount(0);
      expect(engineIds("dind", "container", `com.docker.compose.project=${NAME}`)).toEqual([]);
      expect(engineIds("dind", "network", `com.docker.compose.project=${NAME}`)).toEqual([]);
    });
  } finally {
    dropStackImage("dind", WEB_IMAGE);
  }
});

test("deploy a stack with a secret file to the agent host", async ({ page }) => {
  test.setTimeout(600_000);
  const NAME = "e2e-secret";
  const HOST = "e2e-stacks-host";
  const SECRET = "agent-host-secret-value";
  // The agent spec left the agent host with its revoked environment, and a host belongs to one
  // environment for good: that one goes, and the host is enrolled again under a name of this spec.
  await removeRemoteEnvironments(page.context());
  const envId = await enrollAgentHost(page, HOST, AGENT_PIN);
  seedStackImage("agenthost", APP_IMAGE);

  try {
    await test.step("create the stack and give it a secret file", async () => {
      await createStack(page, envId, NAME);
      await page.getByRole("tab", { name: "Editor" }).click();
      await typeInEditor(
        page,
        "compose.yaml, YAML",
        [
          "services:",
          "  app:",
          `    image: ${APP_IMAGE}`,
          '    command: ["sleep", "3600"]',
          "    stop_grace_period: 1s",
          "    secrets:",
          "      - tok",
          "secrets:",
          "  tok:",
          "    file: ./tok.txt",
          "",
        ].join("\n"),
      );
      await page.getByRole("button", { name: "Add file" }).click();
      const add = page.getByRole("dialog", { name: "Add a file" });
      await add.getByRole("textbox", { name: "File name" }).fill("tok.txt");
      await add.getByRole("button", { name: "Add file" }).click();
      await typeInEditor(page, "tok.txt, text", SECRET);
      await shot(page, "1-secret-file");
    });

    await test.step("review and deploy through the agent", async () => {
      await openReview(page, NAME);
      await page.getByRole("button", { name: "Deploy stack" }).click();
      await waitForDeployComplete(page);
      await shot(page, "2-deploy-complete");
    });

    await test.step("the secret is in the container, and nowhere on the screen", async () => {
      const ids = engineIds("agenthost", "container", `com.docker.compose.project=${NAME}`);
      expect(ids).toHaveLength(1);
      expect(dockerExec("agenthost", `docker exec ${ids[0]} cat /run/secrets/tok`)).toBe(SECRET);
      await expect(page.getByText(SECRET)).toHaveCount(0);
    });

    await test.step("Remove deletes it from the agent host", async () => {
      await page.goto(at(`/env/${envId}/stacks`));
      const row = page.getByRole("row").filter({ hasText: NAME });
      await row.getByRole("button", { name: `Actions for ${NAME}` }).click();
      await page.getByRole("menuitem", { name: /^Remove/ }).click();
      const dialog = page.getByRole("dialog", { name: `Remove ${NAME}?` });
      await dialog.getByRole("textbox").fill(NAME);
      await dialog.getByRole("button", { name: "Remove stack" }).click();
      await expect(page.getByText(`Removed ${NAME}.`).first()).toBeVisible({ timeout: 120_000 });
      expect(engineIds("agenthost", "container", `com.docker.compose.project=${NAME}`)).toEqual([]);
    });
  } finally {
    dropStackImage("agenthost", APP_IMAGE);
    // The environment goes again, so the specs after this one find what the agent spec left less
    // one revoked environment, and no agent of this host connected to the server.
    await removeRemoteEnvironments(page.context());
    dockerExec(
      "agenthost",
      "docker rm -f bayplan-agent >/dev/null 2>&1; docker volume rm -f bayplan-agent >/dev/null 2>&1; true",
    );
  }
});
