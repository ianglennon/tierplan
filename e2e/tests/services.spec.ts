// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Golden path 6 (Phase 6): a swarm service, in a real browser behind the proxy at
// /bayplan, against the dind engine Bayplan manages once that engine is made a
// one-node swarm (swarmInit in helpers.ts runs `docker swarm init`). The service is started on the engine behind Bayplan's back, from
// the dind's own local busybox with `--no-resolve-image`: creating a service through
// Bayplan pins the image's digest, and this engine has no registry to ask. The story
// is the one an operator follows: scale it, edit it (review, then apply), roll the
// edit back, create a secret, find the service's task on its node, and read the
// service's merged log.

import type { Page } from "@playwright/test";

import { AUTH_STATE, at } from "./env";
import {
  dind,
  environmentsOf,
  expect,
  importShellImage,
  SHELL_IMAGE,
  swarmInit,
  swarmLeave,
  test,
} from "./helpers";

test.use({ storageState: AUTH_STATE });

// One story: each test uses what the one before it left. The first test waits for Bayplan to
// notice the new swarm, which it probes for every 30 seconds, so a test may take minutes.
test.describe.configure({ mode: "serial", timeout: 240_000 });

// Names are unique to a run, so a swarm object a failed attempt left on a shared runner
// can never be taken for this run's.
const RUN = Math.random().toString(36).slice(2, 8);
const SERVICE = `e2e-svc-${RUN}`;
const SECRET = `e2e-secret-${RUN}`;
const ENV_NAME = "E2E_FLAG";

let envId = "";

// The service, as the engine itself reports it.
function inspect(format: string): string {
  return dind(`docker service inspect --format '${format}' ${SERVICE}`);
}

// The number of tasks of the service that the engine says are running.
function runningTasks(): number {
  const out = dind(
    `docker service ps ${SERVICE} --filter desired-state=running --format '{{.CurrentState}}'`,
  );
  return out === "" ? 0 : out.split("\n").filter((l) => l.startsWith("Running")).length;
}

// Everything this spec makes, and nothing else. Leaving the swarm removes its services and
// secrets; the containers of its tasks go with it, and the explicit removal covers an
// engine that is slow to do that.
function cleanUp(): void {
  swarmLeave();
  dind(
    `docker ps -aq --filter label=com.docker.swarm.service.name=${SERVICE} | xargs -r docker rm -f >/dev/null 2>&1 || true`,
  );
}

// waitForSwarmEnvironment waits until Bayplan has noticed that the engine it manages is a
// swarm. It probes the engine from time to time, so this can take a little while.
async function waitForSwarmEnvironment(page: Page): Promise<void> {
  await expect
    .poll(
      async () => {
        const res = await page.context().request.get(at("/api/environments"));
        if (!res.ok()) return `HTTP ${res.status()}`;
        const items = ((await res.json()) as { items: Array<{ id: string; envType?: string }> })
          .items;
        return items.find((e) => e.id === envId)?.envType ?? "unknown";
      },
      { message: "the environment becomes a swarm", timeout: 120_000, intervals: [1_000, 2_000] },
    )
    .toBe("swarm");
}

// openService opens the service's page from the services list, as a person clicking its
// name would.
async function openService(page: Page): Promise<void> {
  await page.goto(at(`/env/${envId}/services`));
  await expect(page.getByRole("heading", { name: "Services", level: 1 })).toBeVisible();
  // Bayplan reads the swarm again every few seconds and on the engine's own events, so a service
  // started behind its back, or a swarm made a moment ago, can take a little while to appear.
  const link = page.getByRole("link", { name: SERVICE, exact: true });
  await expect(link).toBeVisible({ timeout: 90_000 });
  await link.click();
  await expect(page).toHaveURL(new RegExp(`/bayplan/env/${envId}/services/[^/]+$`));
  await expect(page.getByRole("heading", { name: SERVICE, level: 2 })).toBeVisible();
}

test.beforeAll(async () => {
  cleanUp();
  importShellImage();
  swarmInit();
  // A service that writes a line a second. --no-resolve-image keeps the manager from asking a
  // registry for a digest this engine cannot give, and the short grace period keeps a rolling update
  // from waiting ten seconds for each task, because a shell that is PID 1 ignores SIGTERM.
  dind(
    `docker service create --detach --no-resolve-image --name ${SERVICE} --replicas 1 --stop-grace-period 1s ${SHELL_IMAGE} ` +
      `sh -c 'i=0; while true; do echo svc-line-$i; i=$((i+1)); sleep 1; done' >/dev/null`,
  );
  await expect.poll(runningTasks, { message: "the service's task runs", timeout: 60_000 }).toBe(1);
});

test.afterAll(() => {
  // The secret is removed with the swarm, but say so if it is not.
  cleanUp();
});

test("a service started on the swarm shows in the list and is scaled to 2 from its page", async ({
  page,
}) => {
  const local = (await environmentsOf(page.context())).find((e) => e.isLocalSocket);
  if (!local) throw new Error("the Docker host Bayplan manages is not among the environments");
  envId = local.id;
  await waitForSwarmEnvironment(page);

  await openService(page);
  await expect(page.getByText("1 of 1 task running")).toBeVisible({ timeout: 30_000 });

  await page.getByRole("button", { name: "Scale…" }).click();
  const dialog = page.getByRole("dialog", { name: `Scale ${SERVICE}` });
  await dialog.getByRole("textbox", { name: "Replicas" }).fill("2");
  await dialog.getByRole("button", { name: "Scale service" }).click();
  await expect(page.getByText(`Scaled ${SERVICE} to 2.`)).toBeVisible();

  // The engine took the new count, and the page shows both tasks running.
  expect(inspect("{{.Spec.Mode.Replicated.Replicas}}")).toBe("2");
  await expect(page.getByText("2 of 2 tasks running")).toBeVisible({ timeout: 60_000 });

  // The list agrees.
  await page.goto(at(`/env/${envId}/services`));
  const row = page
    .getByRole("row")
    .filter({ has: page.getByRole("link", { name: SERVICE, exact: true }) });
  await expect(row).toContainText("2/2", { timeout: 30_000 });
});

test("an edit adds an environment variable: review, then apply, and the update completes", async ({
  page,
}) => {
  await openService(page);
  await page.getByRole("link", { name: "Edit…" }).click();
  await expect(page).toHaveURL(/\/services\/[^/]+\/edit/);
  await expect(page.getByRole("heading", { name: `Edit ${SERVICE}` })).toBeVisible();

  await page.getByRole("button", { name: "Add variable" }).click();
  await page.getByRole("textbox", { name: "Variable name" }).last().fill(ENV_NAME);
  await page.getByRole("textbox", { name: "Variable value" }).last().fill("on");
  await page.getByRole("button", { name: "Review changes" }).click();

  // The review lists the change before anything is sent.
  await expect(page.getByRole("heading", { name: "Review changes" })).toBeVisible();
  await expect(page.getByText(`Environment variable ${ENV_NAME}`)).toBeVisible();
  expect(inspect("{{json .Spec.TaskTemplate.ContainerSpec.Env}}")).not.toContain(ENV_NAME);

  await page.getByRole("button", { name: "Apply changes" }).click();
  await expect(page.getByRole("heading", { name: "Update complete" })).toBeVisible({
    timeout: 90_000,
  });
  expect(inspect("{{json .Spec.TaskTemplate.ContainerSpec.Env}}")).toContain(`${ENV_NAME}=on`);

  await page.getByRole("link", { name: "View service" }).click();
  await expect(page.getByRole("heading", { name: SERVICE, level: 2 })).toBeVisible();
});

test("the edit is rolled back from the More menu, after a look at what changes", async ({
  page,
}) => {
  await openService(page);
  await page.getByRole("button", { name: "More actions" }).click();
  await page.getByRole("menuitem", { name: "Roll back…" }).click();

  const dialog = page.getByRole("dialog", { name: `Roll back ${SERVICE}?` });
  // The dialog shows the current settings against the earlier ones.
  await expect(dialog.getByText(ENV_NAME).first()).toBeVisible();
  await dialog.getByRole("button", { name: "Roll back service" }).click();
  await expect(page.getByText(`Rolling back ${SERVICE}.`)).toBeVisible();

  // The swarm puts the earlier settings back and says the rollback finished.
  await expect
    .poll(() => inspect("{{.UpdateStatus.State}}"), {
      message: "the rollback completes",
      timeout: 90_000,
    })
    .toBe("rollback_completed");
  expect(inspect("{{json .Spec.TaskTemplate.ContainerSpec.Env}}")).not.toContain(ENV_NAME);
});

test("a secret is created from the Secrets page and is never shown again", async ({ page }) => {
  await page.goto(at(`/env/${envId}/secrets`));
  await expect(page.getByRole("heading", { name: "Secrets", level: 1 })).toBeVisible();

  await page.getByRole("button", { name: "Create secret" }).first().click();
  const sheet = page.getByRole("dialog", { name: "Create a secret" });
  await sheet.getByRole("textbox", { name: "Name", exact: true }).fill(SECRET);
  await sheet.getByLabel("Value", { exact: true }).fill("correct-horse-battery");
  await sheet.getByRole("button", { name: "Create secret" }).click();
  await expect(page.getByText(`Created secret ${SECRET}.`)).toBeVisible();

  await expect(page.getByRole("link", { name: SECRET, exact: true })).toBeVisible();
  expect(dind(`docker secret ls --filter name=${SECRET} --format '{{.Name}}'`)).toBe(SECRET);

  // The page for the secret has no value to show.
  await page.getByRole("link", { name: SECRET, exact: true }).click();
  await expect(page.getByRole("heading", { name: SECRET })).toBeVisible();
  await expect(page.getByText("correct-horse-battery")).toHaveCount(0);
});

test("the node page lists the service's tasks, and the service's Logs tab shows its lines", async ({
  page,
}) => {
  await page.goto(at(`/env/${envId}/nodes`));
  await expect(page.getByRole("heading", { name: "Nodes", level: 1 })).toBeVisible();
  // One node: the engine itself.
  await page.getByRole("main").getByRole("table").getByRole("link").first().click();
  await expect(page).toHaveURL(new RegExp(`/bayplan/env/${envId}/nodes/[^/]+$`));
  await page.getByRole("tab", { name: "Tasks", exact: true }).click();
  await expect(page.getByRole("link", { name: SERVICE, exact: true }).first()).toBeVisible({
    timeout: 30_000,
  });

  await openService(page);
  await page.getByRole("tab", { name: "Logs", exact: true }).click();
  const log = page.getByRole("log", { name: `Logs for ${SERVICE}` });
  await expect(
    log.locator('[data-stream="stdout"]').filter({ hasText: "svc-line-" }).first(),
  ).toBeVisible({ timeout: 30_000 });
});
