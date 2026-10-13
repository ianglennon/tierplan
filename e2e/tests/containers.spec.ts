// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Golden path 4 (Phase 4): containers, logs, a shell, the event feed, networks and
// volumes, in a real browser behind the proxy at /bayplan, against the dind
// engine Bayplan manages. Containers are started on the engine behind Bayplan's
// back, so every change the page shows arrives the way a person's own `docker
// run` would, not through a click of the page's.

import { readFileSync } from "node:fs";

import type { CDPSession, Page } from "@playwright/test";

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

// openContainerTab opens a container's page from the list, as a person clicking its
// name would, and then one of its tabs.
async function openContainerTab(page: Page, name: string, tab: string): Promise<void> {
  await page.goto(at(`/env/${envId}/containers`));
  await page.getByRole("link", { name, exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/bayplan/env/${envId}/containers/[^/]+`));
  await page.getByRole("tab", { name: tab, exact: true }).click();
}

// The highest N among the "line-N" lines a log surface shows, or -1 when there are none.
async function latestLine(log: ReturnType<Page["getByRole"]>): Promise<number> {
  const texts = await log.locator('[data-stream="stdout"]').allInnerTexts();
  const ns = texts.flatMap((t) => [...t.matchAll(/line-(\d+)/g)].map((m) => Number(m[1])));
  return ns.length === 0 ? -1 : Math.max(...ns);
}

// What one page has open to the server right now, read from the browser's own network
// events: a connection that has been started and has neither finished nor failed.
interface OpenConnection {
  url: string;
  type: string;
  protocol?: string;
  status?: number;
}

interface NetworkProbe {
  open(): OpenConnection[];
  // Every WebSocket handshake that was answered, with its status.
  handshakes(): Array<{ url: string; status: number }>;
}

async function probeNetwork(page: Page): Promise<NetworkProbe> {
  const cdp: CDPSession = await page.context().newCDPSession(page);
  await cdp.send("Network.enable");
  const open = new Map<string, OpenConnection>();
  const handshakes: Array<{ url: string; status: number }> = [];
  cdp.on("Network.requestWillBeSent", (e) => {
    open.set(e.requestId, { url: e.request.url, type: e.type ?? "Other" });
  });
  cdp.on("Network.responseReceived", (e) => {
    const c = open.get(e.requestId);
    if (c) {
      c.protocol = e.response.protocol;
      c.status = e.response.status;
    }
  });
  cdp.on("Network.loadingFinished", (e) => open.delete(e.requestId));
  cdp.on("Network.loadingFailed", (e) => open.delete(e.requestId));
  cdp.on("Network.webSocketCreated", (e) => {
    open.set(e.requestId, { url: e.url, type: "WebSocket" });
  });
  cdp.on("Network.webSocketHandshakeResponseReceived", (e) => {
    const c = open.get(e.requestId);
    if (c) c.status = e.response.status;
    handshakes.push({ url: c?.url ?? "", status: e.response.status });
  });
  cdp.on("Network.webSocketClosed", (e) => open.delete(e.requestId));
  return { open: () => [...open.values()], handshakes: () => handshakes };
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
  // A row can show up while its container is still created, before the start event
  // has been read, and a bulk leaves out a row its action does not apply to.
  await expect(rowOf(page, "e2e-a")).toContainText("Running");
  await expect(rowOf(page, "e2e-b")).toContainText("Running");
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

test("the Logs tab streams stdout and stderr, searches, downloads, and pops out beside the tab", async ({
  page,
  context,
}, info) => {
  // Watched from the first request, so the streams the app keeps open on every page
  // (health, events) are counted along with the log's own.
  const probe = await probeNetwork(page);
  await openContainerTab(page, "e2e-web", "Logs");
  const log = page.getByRole("log", { name: "Logs for e2e-web" });

  // Both streams, live; a stderr line carries the visible marker.
  await expect(
    log.locator('[data-stream="stdout"]').filter({ hasText: "line-" }).first(),
  ).toBeVisible();
  const err = log.locator('[data-stream="stderr"]').filter({ hasText: "err-" }).first();
  await expect(err).toBeVisible();
  await expect(err).toContainText("stderr");
  // New lines keep arriving: the highest number grows while nobody touches the page.
  const first = await latestLine(log);
  await expect.poll(() => latestLine(log), { timeout: 20_000 }).toBeGreaterThan(first);

  // Search works on the loaded lines and says how many it found.
  await expect(
    log.locator('[data-stream="stdout"]').filter({ hasText: "line-3" }).first(),
  ).toBeVisible();
  await page.getByRole("searchbox", { name: "Search logs" }).fill("line-3");
  await expect(page.getByText(/^\d+ of \d+$/).first()).toBeVisible();

  // Download logs: the browser is handed a .log file named after the container.
  await page.getByRole("button", { name: "Download…" }).click();
  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: "Download logs", exact: true }).click();
  const file = await download;
  expect(file.suggestedFilename()).toMatch(/^e2e-web-\d{8}-\d{4}\.log$/);
  const body = readFileSync((await file.path()) as string, "utf8");
  expect(body, "the downloaded file holds the container's lines").toMatch(/line-\d+/);
  await page.keyboard.press("Escape");
  await page.getByRole("searchbox", { name: "Search logs" }).fill("");

  // Pop-out: a second window streams while the tab does. Every page of the one
  // origin shares the browser's connections to it, so both being live at once is
  // what RESEARCH A3 asks about.
  const cspInPopout: string[] = [];
  context.on("page", (p) => {
    p.on("console", (msg) => {
      if (
        /Content Security Policy|Refused to (load|apply|execute|connect|frame)/i.test(msg.text())
      ) {
        cspInPopout.push(msg.text());
      }
    });
  });
  const popupEvent = context.waitForEvent("page");
  await page.getByRole("button", { name: "Open in new window" }).click();
  const popup = await popupEvent;
  const popupProbe = await probeNetwork(popup);
  await popup.waitForLoadState("domcontentloaded");
  expect(new URL(popup.url()).pathname).toMatch(/^\/bayplan\/popout\/.+\/logs$/);
  const popLog = popup.getByRole("log", { name: "Logs for e2e-web" });
  await expect(
    popLog.locator('[data-stream="stdout"]').filter({ hasText: "line-" }).first(),
  ).toBeVisible();

  const tabBefore = await latestLine(log);
  const popBefore = await latestLine(popLog);
  await expect.poll(() => latestLine(log), { timeout: 20_000 }).toBeGreaterThan(tabBefore);
  await expect.poll(() => latestLine(popLog), { timeout: 20_000 }).toBeGreaterThan(popBefore);

  // RESEARCH A3, as observed: what each page holds open to the one origin.
  const sse = (c: OpenConnection) => /\/logs\/stream/.test(c.url);
  const tabOpen = probe.open();
  const popOpen = popupProbe.open();
  const report = {
    tab: tabOpen.map((c) => `${c.type} ${new URL(c.url).pathname} ${c.protocol ?? ""}`),
    popout: popOpen.map((c) => `${c.type} ${new URL(c.url).pathname} ${c.protocol ?? ""}`),
  };
  console.log(`A3 open connections: ${JSON.stringify(report)}`);
  info.annotations.push({ type: "A3", description: JSON.stringify(report) });
  expect(tabOpen.filter(sse), "the tab's own log stream is open").toHaveLength(1);
  expect(popOpen.filter(sse), "the pop-out's own log stream is open").toHaveLength(1);
  expect(cspInPopout, "CSP violations in the pop-out").toEqual([]);
  await popup.close();
});

test("a shell from the Shell tab runs a command in the container, over a WebSocket through the proxy, with no CSP violation", async ({
  page,
  tracker,
}, info) => {
  const probe = await probeNetwork(page);
  // What the server sends down the shell's WebSocket, as text.
  let received = "";
  page.on("websocket", (ws) => {
    ws.on("framereceived", ({ payload }) => {
      received += typeof payload === "string" ? payload : new TextDecoder().decode(payload);
    });
  });

  await openContainerTab(page, "e2e-web", "Shell");
  await page.getByRole("button", { name: "Open shell" }).click();
  const terminal = page.getByRole("textbox", { name: "Terminal in e2e-web" });
  await expect(terminal).toBeAttached();
  await expect(page.getByText("Connected", { exact: true })).toBeVisible();

  // Typed into the terminal as a person would, ending in Enter. The file is made by
  // the container's own shell, and the sum is printed back through the WebSocket.
  await terminal.focus();
  await page.keyboard.type("touch /tmp/e2e-shell-ok; echo out-$((6*7))");
  await page.keyboard.press("Enter");

  await expect
    .poll(
      () => {
        try {
          return dind("docker exec e2e-web ls /tmp/e2e-shell-ok");
        } catch {
          return "";
        }
      },
      { timeout: 20_000 },
    )
    .toBe("/tmp/e2e-shell-ok");
  await expect.poll(() => received, { timeout: 10_000 }).toContain("out-42");

  // The upgrade was answered 101 at the subpath, over TLS, and is still open.
  const shell = probe.handshakes().filter((h) => /\/bayplan\/api\/.+shell/i.test(h.url));
  console.log(`A4 shell handshakes: ${JSON.stringify(shell)}`);
  expect(shell.length, "the shell's WebSocket upgrade").toBeGreaterThanOrEqual(1);
  expect(shell.every((h) => h.status === 101 && h.url.startsWith("wss://127.0.0.1"))).toBe(true);
  const socket = probe.open().filter((c) => c.type === "WebSocket");
  expect(socket, "the shell's WebSocket is still open").toHaveLength(1);

  // RESEARCH A4: the policy that serves the page lets the shell's socket open and
  // reports nothing against it, while the socket is open and after it was used.
  const page_ = await page.request.get(at("/"));
  const policy = page_.headers()["content-security-policy"] ?? "";
  console.log(`A4 policy: ${policy}`);
  expect(policy, "the page is served with a Content-Security-Policy").toContain("connect-src");
  info.annotations.push({ type: "A4", description: JSON.stringify({ handshakes: shell, policy }) });
  expect(tracker.cspViolations, "Content-Security-Policy violations with the shell open").toEqual(
    [],
  );
});

test("the Events page lists the container's start event", async ({ page }) => {
  await page.goto(at(`/env/${envId}`));
  await page.getByRole("link", { name: "Events", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Events", level: 1 })).toBeVisible();
  // The newest events are other containers' by now, so narrow the feed to this one.
  await page.getByRole("searchbox", { name: "Search events" }).fill("e2e-web");
  const table = page.getByRole("table", { name: "Docker events, newest first" });
  const start = table
    .getByRole("row")
    .filter({ has: page.getByRole("cell", { name: "e2e-web", exact: true }) })
    .filter({ has: page.getByRole("cell", { name: "start", exact: true }) });
  await expect(start.first()).toBeVisible();
});

test("a network is created, joined with an alias, refused deletion while attached, and deleted once empty", async ({
  page,
}) => {
  await page.goto(at(`/env/${envId}/networks`));
  await expect(page.getByRole("heading", { name: "Networks", level: 1 })).toBeVisible();

  await page.getByRole("button", { name: "Create network" }).first().click();
  const create = page.getByRole("dialog", { name: "Create network" });
  await create.getByRole("textbox", { name: "Name" }).fill("e2e-net");
  await create.getByRole("button", { name: "Create network", exact: true }).click();
  await expect(page.getByText("Created network e2e-net.")).toBeVisible();
  expect(dind("docker network inspect -f '{{.Driver}}' e2e-net")).toBe("bridge");

  await page.getByRole("link", { name: "e2e-net", exact: true }).click();
  await page.getByRole("tab", { name: "Attachments", exact: true }).click();
  await expect(page.getByText("Nothing is attached to this network.")).toBeVisible();

  // Connect the container with an alias, and the alias shows in Attachments.
  await page.getByRole("button", { name: "Connect container", exact: true }).click();
  const connect = page.getByRole("dialog", { name: "Connect a container to e2e-net" });
  await connect.getByRole("combobox", { name: "Container" }).click();
  await page.getByRole("option", { name: /e2e-web/ }).click();
  await connect.getByRole("textbox", { name: "Alias (optional)" }).fill("e2e-alias");
  await connect.getByRole("button", { name: "Connect container", exact: true }).click();
  await expect(page.getByText("Connected e2e-web to e2e-net.")).toBeVisible();
  const attachments = page.getByRole("table", { name: "Attachments" });
  const attached = attachments.getByRole("row").filter({
    has: page.getByRole("cell", { name: "e2e-web", exact: true }),
  });
  await expect(attached.getByText("e2e-alias", { exact: true })).toBeVisible();
  expect(dind("docker inspect -f '{{json .NetworkSettings.Networks}}' e2e-web")).toContain(
    "e2e-alias",
  );

  // Delete is refused while something is attached: it says why and opens nothing.
  const del = page.getByRole("button", { name: "Delete network e2e-net" });
  await expect(del).toHaveAttribute("aria-disabled", "true");
  await expect(del).toHaveAccessibleDescription(/In use by 1 container/);
  // The button is aria-disabled, which Playwright will not click without force; a
  // person's click shows the reason as a toast and opens nothing.
  await del.click({ force: true });
  await expect(
    page.locator("[data-sonner-toast]").filter({ hasText: /In use by 1 container/ }),
  ).toBeVisible();
  await expect(page.getByRole("alertdialog")).toHaveCount(0);
  await expect(page.getByRole("dialog")).toHaveCount(0);
  expect(dind("docker network ls --filter name=e2e-net -q")).not.toBe("");

  // Disconnect, confirm, and the network can go.
  await attached.getByRole("button", { name: "Disconnect e2e-web from e2e-net" }).click();
  await page
    .getByRole("alertdialog", { name: "Disconnect e2e-web from e2e-net?" })
    .getByRole("button", { name: "Disconnect container" })
    .click();
  await expect(page.getByText("Disconnected e2e-web from e2e-net.")).toBeVisible();
  await expect(page.getByText("Nothing is attached to this network.")).toBeVisible();

  await expect(del).not.toHaveAttribute("aria-disabled", "true");
  await del.click();
  await page
    .getByRole("alertdialog", { name: "Delete e2e-net?" })
    .getByRole("button", { name: "Delete network" })
    .click();
  await expect(page.getByText("Deleted network e2e-net.")).toBeVisible();
  expect(dind("docker network ls --filter name=e2e-net -q")).toBe("");

  await page.goto(at(`/env/${envId}/networks`));
  await expect(page.getByRole("heading", { name: "Networks", level: 1 })).toBeVisible();
  await expect(page.getByRole("link", { name: "e2e-net", exact: true })).toHaveCount(0);
});

test("a volume is created and deleted with its name typed", async ({ page }) => {
  await page.goto(at(`/env/${envId}/volumes`));
  await expect(page.getByRole("heading", { name: "Volumes", level: 1 })).toBeVisible();

  await page.getByRole("button", { name: "Create volume" }).first().click();
  const create = page.getByRole("dialog", { name: "Create volume" });
  await create.getByRole("textbox", { name: "Name" }).fill("e2e-vol");
  await create.getByRole("button", { name: "Create volume", exact: true }).click();
  await expect(page.getByText("Created volume e2e-vol.")).toBeVisible();
  expect(dind("docker volume ls -q --filter name=e2e-vol")).toBe("e2e-vol");

  await page.getByRole("link", { name: "e2e-vol", exact: true }).click();
  await page.getByRole("button", { name: "Delete e2e-vol" }).click();
  const confirm = page.getByRole("alertdialog", { name: "Delete e2e-vol?" });
  const submit = confirm.getByRole("button", { name: "Delete volume" });
  // The name has to be typed first.
  await expect(submit).toBeDisabled();
  await confirm.getByRole("textbox").fill("e2e-vol");
  await submit.click();
  await expect(page.getByText("Deleted e2e-vol.")).toBeVisible();
  expect(dind("docker volume ls -q --filter name=e2e-vol")).toBe("");

  await page.goto(at(`/env/${envId}/volumes`));
  await expect(page.getByRole("heading", { name: "Volumes", level: 1 })).toBeVisible();
  await expect(page.getByRole("link", { name: "e2e-vol", exact: true })).toHaveCount(0);
});
