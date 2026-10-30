// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Golden path 5 (D-59, plan 02-32): a remote host joins through the reverse proxy.
// Coaming is behind Caddy at https://127.0.0.1:<port>/coaming; the admin adds an
// environment, copies the Standalone command, and the test pastes it into a shell
// on a second Docker host (agenthost) the way a person would on a server. The
// agent dials Caddy by IP and trusts its certificate through COAMING_AGENT_PIN,
// the documented private-CA proxy setting. The waiting panel goes live without a
// reload and opens the environment, the overview and the switcher show it online,
// its dashboard is read through the tunnel, and Revoke node ends it (ENV-01,
// ENV-03, ENV-05, ENV-06, PLAT-04).
//
// The page, the event stream and the agent's WebSocket all pass through the
// proxy: nothing but Caddy is published, and the agent's address is Caddy's.

import type { Page } from "@playwright/test";

import { AGENT_PIN, AUTH_STATE, at } from "./env";
import { compose, dockerExec, expect, test } from "./helpers";

test.use({
  storageState: AUTH_STATE,
  // The Copy button writes the command to the clipboard, and the test reads it
  // back, so the command it runs is the one a person would paste.
  permissions: ["clipboard-read", "clipboard-write"],
});

const REMOTE = "e2e-remote";

function tileValue(page: Page, label: string) {
  return page
    .getByRole("region", { name: "Dashboard" })
    .getByRole("link", { name: new RegExp(`^${label}\\b`) })
    .locator("span")
    .nth(1);
}

// shot attaches the page as it looks now to the test's report. The agent project
// keeps its trace and these pictures even when it passes: the release checklist
// has someone look at the waiting panel going live and at the revoked page.
async function shot(page: Page, name: string): Promise<void> {
  await test.info().attach(name, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
}

// withPin puts the proxy certificate's pin in front of the image name, the way
// docs/agent.md tells an operator with a private-CA proxy to.
function withPin(command: string, pin: string): string {
  const image = command.trim().split(/\s+/).at(-1) ?? "";
  expect(image, "the last word of the command is the agent image").toMatch(/coaming-agent:/);
  const at = command.lastIndexOf(image);
  return `${command.slice(0, at)}-e 'COAMING_AGENT_PIN=${pin}' ${command.slice(at)}`;
}

test("a remote host joins through the proxy, shows online, opens, and is revoked", async ({
  page,
}) => {
  test.setTimeout(300_000);
  expect(AGENT_PIN, "make e2e writes .devdata/e2e-tls/pin.txt").toMatch(/^sha256\/.{44}$/);

  let envId = "";
  let command = "";

  // A retry starts from a clean host: the first attempt's agent, its state (the
  // identity it enrolled with) and the box it created are gone.
  dockerExec(
    "agenthost",
    "docker rm -f coaming-agent e2e-remote-box >/dev/null 2>&1; docker volume rm -f coaming-agent >/dev/null 2>&1; true",
  );

  await test.step("add an environment and read the Standalone command", async () => {
    await page.goto(at("/environments"));
    await expect(page.getByRole("heading", { name: "Environments", level: 1 })).toBeVisible();
    await page.getByRole("button", { name: "Add environment" }).first().click();

    const dialog = page.getByRole("dialog");
    await dialog.getByRole("textbox", { name: /^Name/ }).fill(REMOTE);
    await dialog.getByRole("button", { name: "Generate install command" }).click();

    // The same dialog moves to its second step and renames itself.
    await expect(dialog.getByRole("heading", { name: "Install the agent" })).toBeVisible();
    await dialog.getByRole("tab", { name: "Standalone" }).click();
    // The token is hidden until it is asked for.
    const block = dialog.getByRole("region", { name: "command" });
    await expect(block).toContainText("COAMING_AGENT_JOIN_TOKEN=\u2022\u2022\u2022\u2022");
    await dialog.getByRole("button", { name: "Show token" }).click();
    const code = block.locator("code");
    await expect(code).toContainText("docker run -d --name coaming-agent");
    // The agent dials the proxy by its compose address, under the subpath.
    await expect(code).toContainText("COAMING_AGENT_URL=wss://172.30.0.10/coaming/agent/connect");
    await expect(code).toContainText("COAMING_AGENT_JOIN_TOKEN=");

    // The Copy button's text is what is run, not a transcription of the screen.
    await dialog.getByRole("button", { name: "Copy command" }).click();
    await expect(dialog.getByRole("button", { name: "Copied command" })).toBeVisible();
    command = await page.evaluate(() => navigator.clipboard.readText());
    expect(command.trim()).toBe(((await code.textContent()) ?? "").trim());
    expect(command).toContain("docker run -d --name coaming-agent");
    // A private-CA proxy's pin is not in the command Coaming shows.
    expect(command).not.toContain("COAMING_AGENT_PIN");

    await expect(dialog.getByRole("status").filter({ hasText: "Waiting for the first agent" })).toBeVisible();
    await shot(page, "1-waiting-for-the-first-agent");
  });

  await test.step("run it on the remote host: the waiting panel goes live and opens the environment", async () => {
    // The page must live through the arrival: a flag on this document is gone if
    // anything reloads it.
    await page.evaluate(() => {
      (window as unknown as { __noReload: boolean }).__noReload = true;
    });

    const out = dockerExec("agenthost", withPin(command, AGENT_PIN));
    expect(out, "docker run prints the container id").toMatch(/^[0-9a-f]{64}$/m);

    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText(`${REMOTE} is online`)).toBeVisible({ timeout: 90_000 });
    await expect(dialog.getByText(/Detected a standalone host with 1 node/)).toBeVisible();
    await shot(page, "2-agent-online-panel");

    // Two seconds later the environment opens by itself.
    await expect(page).toHaveURL(/\/coaming\/env\/[^/]+$/, { timeout: 15_000 });
    envId = /\/env\/([^/]+)$/.exec(new URL(page.url()).pathname)?.[1] ?? "";
    expect(envId).not.toBe("");
    expect(
      await page.evaluate(() => (window as unknown as { __noReload?: boolean }).__noReload),
      "the page was reloaded",
    ).toBe(true);
  });

  await test.step("its dashboard is read through the tunnel", async () => {
    // What the remote Docker holds: the agent's own container, and nothing else.
    const containers = dockerExec("agenthost", "docker ps -aq | wc -l");
    const images = dockerExec("agenthost", "docker image ls -q | sort -u | wc -l");
    expect(Number(containers)).toBeGreaterThanOrEqual(1);

    await expect(page.getByRole("region", { name: "Dashboard" })).toBeVisible();
    await expect(tileValue(page, "Containers")).toHaveText(containers);
    await expect(tileValue(page, "Images")).toHaveText(images);

    // The engine card shows the remote Engine's own version, so the run says which
    // Engine it proved (COAMING_E2E_AGENTHOST_IMAGE picks another, 24 to 29).
    const engine = dockerExec("agenthost", "docker version -f '{{.Server.Version}}'");
    expect(engine).toMatch(/^\d+\.\d+\.\d+/);
    await expect(page.getByText(engine, { exact: true }).first()).toBeVisible();
    test.info().annotations.push({ type: "remote engine", description: engine });

    // A container started on the remote host shows without a reload: the event
    // stream is read through the tunnel and the proxy as well.
    dockerExec(
      "agenthost",
      "docker create --name e2e-remote-box --entrypoint /nothing ghcr.io/ianglennon/coaming-agent:edge",
    );
    await expect(tileValue(page, "Containers")).toHaveText(String(Number(containers) + 1), {
      timeout: 15_000,
    });
    await shot(page, "3-remote-dashboard");
  });

  await test.step("the overview and the switcher show it online", async () => {
    await page.getByRole("button", { name: "Switch environment" }).click();
    await page.getByRole("option", { name: "All environments" }).click();
    await expect(page).toHaveURL(/\/coaming\/environments$/);

    const row = page.getByRole("row").filter({ hasText: REMOTE });
    await expect(row).toBeVisible();
    await expect(row).toContainText("Online");
    await expect(row).toContainText("Standalone");

    // The switcher lists it with a health dot that carries the state as its name.
    await page.getByRole("button", { name: "Switch environment" }).click();
    const option = page.getByRole("option").filter({ hasText: REMOTE }).first();
    await expect(option.getByRole("img", { name: "Online" })).toBeVisible();
    await shot(page, "4-switcher-with-health-dot");

    // Switch to the managed local host and back, by the switcher alone.
    await page
      .getByRole("option")
      .filter({ hasNotText: REMOTE })
      .filter({ hasNotText: "All environments" })
      .filter({ hasNotText: "Manage environments" })
      .first()
      .click();
    await expect(page).toHaveURL(/\/coaming\/env\/[^/]+$/);
    expect(page.url()).not.toContain(envId);

    await page.getByRole("button", { name: "Switch environment" }).click();
    await page.getByRole("option").filter({ hasText: REMOTE }).first().click();
    await expect(page).toHaveURL(new RegExp(`/coaming/env/${envId}$`));
    await expect(page.getByRole("region", { name: "Dashboard" })).toBeVisible();
  });

  await test.step("Revoke node ends it and the environment says so", async () => {
    await page.goto(at("/settings/environments"));
    await page.getByRole("link", { name: `Manage ${REMOTE}` }).click();
    await expect(page).toHaveURL(new RegExp(`/coaming/settings/environments/${envId}$`));

    const nodes = page.getByRole("table", { name: /nodes/i });
    await expect(nodes).toBeVisible();
    await expect(nodes.getByRole("row").filter({ hasText: "Connected" })).toHaveCount(1);
    await nodes.getByRole("button", { name: /^Actions for / }).click();
    await page.getByRole("menuitem", { name: "Revoke node" }).click();

    const confirm = page.getByRole("alertdialog", { name: /^Revoke .+\?$/ });
    await expect(confirm).toBeVisible();
    await confirm.getByRole("button", { name: "Revoke node" }).click();
    await expect(page.getByText(/ was revoked\.$/)).toBeVisible();

    await page.goto(at(`/env/${envId}`));
    await expect(
      page.getByRole("heading", { name: "This environment was revoked" }),
    ).toBeVisible({ timeout: 30_000 });
    await shot(page, "5-environment-revoked");
  });

  await test.step("the proxy carried the page, the event stream and the agent's WebSocket", async () => {
    // Caddy writes an access line when a request ends. The revoke closed the
    // agent's session, so its upgrade is in the log now.
    await expect
      .poll(() => compose("logs", "--no-color", "caddy"), { timeout: 30_000 })
      .toMatch(/"uri":"\/coaming\/agent\/connect"[^\n]*"status":101|"status":101[^\n]*"uri":"\/coaming\/agent\/connect"/);
    const log = compose("logs", "--no-color", "caddy");
    expect(log).toContain('"uri":"/coaming/api/environments/stream"');
    expect(log).toContain('"uri":"/coaming/assets/');

    // And Coaming found nothing wrong with the proxy through all of it: no
    // untrusted forwarder, no host or scheme mismatch, no failing WebSocket.
    const warnings = await page.request.get(at("/api/warnings"));
    expect(warnings.ok()).toBe(true);
    const ids = ((await warnings.json()) as { items: { id: string }[] }).items.map((w) => w.id);
    expect(ids.filter((id) => id.startsWith("proxy_")), "proxy warnings").toEqual([]);
  });
});
