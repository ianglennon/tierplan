// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { execFileSync } from "node:child_process";
import {
  type APIResponse,
  type BrowserContext,
  test as base,
  expect,
  type Locator,
  type Page,
  type TestInfo,
} from "@playwright/test";

import { at, COMPOSE_FILE, ORIGIN } from "./env";

// compose runs `docker compose -f e2e/compose.yaml <args>` against the stack
// `make e2e` brought up and returns what it printed.
export function compose(...args: string[]): string {
  return execFileSync("docker", ["compose", "-f", COMPOSE_FILE, ...args], {
    encoding: "utf8",
    timeout: 120_000,
    // A proxy's access log runs past the 1 MiB default after a few page loads.
    maxBuffer: 64 * 1024 * 1024,
  });
}

// docker runs the docker CLI on the machine running the suite, against the
// engine that hosts the Compose stack (not the dind one Bayplan manages).
export function docker(...args: string[]): string {
  return execFileSync("docker", args, { encoding: "utf8", timeout: 60_000 }).trim();
}

// setupToken reads the first-run token the way a self-hoster does:
// `docker exec <container> bayplan setup-token`. The command runs as root in the
// container, so it also proves the CLI drops privileges and leaves /data usable
// by the server (D-17).
export function setupToken(): string {
  return compose("exec", "-T", "bayplan", "bayplan", "setup-token").trim();
}

// dind runs a shell command inside the dind service, against the engine Bayplan
// manages. Its DOCKER_HOST points the docker CLI at the shared socket.
export function dind(cmd: string): string {
  return compose("exec", "-T", "dind", "sh", "-c", cmd).trim();
}

// The image the containers spec runs: a shell and a few tools, built inside the
// dind with no registry (the dind has no route to one).
export const SHELL_IMAGE = "e2e-busybox:local";

// importShellImage builds SHELL_IMAGE from the dind's own busybox and the musl
// loader it links against, as a root filesystem imported with `docker import`.
// busybox is dynamically linked on Alpine, so the loader has to come with it.
// Nothing is pushed or pulled; the image exists only in the test engine. It is
// idempotent: a second call finds the image and does nothing.
export function importShellImage(): void {
  dind(
    [
      `docker image inspect ${SHELL_IMAGE} >/dev/null 2>&1 && exit 0`,
      "root=$(mktemp -d)",
      'mkdir -p "$root/bin" "$root/lib" "$root/tmp"',
      'cp /bin/busybox "$root/bin/busybox"',
      'cp /lib/ld-musl-*.so.1 "$root/lib/"',
      'chmod 1777 "$root/tmp"',
      'for c in sh echo sleep touch ls cat printf; do ln -s busybox "$root/bin/$c"; done',
      `tar -C "$root" -c . | docker import - ${SHELL_IMAGE} >/dev/null`,
      'rm -rf "$root"',
    ].join("; "),
  );
}

// runLogger starts a container that writes "line-N" to stdout and "err-N" to
// stderr once a second, removing any container of that name a retried attempt
// left behind. It is started on the engine behind Bayplan's back, the way a
// person at a prompt on the host would.
export function runLogger(name: string): void {
  dind(`docker rm -f ${name} >/dev/null 2>&1 || true`);
  dind(
    `docker run -d --stop-timeout 2 --name ${name} ${SHELL_IMAGE} sh -c ` +
      `'i=0; while true; do echo line-$i; echo err-$i >&2; i=$((i+1)); sleep 1; done'`,
  );
}

// swarmInit makes the dind engine Bayplan manages a one-node swarm, the way a person
// would on a fresh host: `docker swarm init --advertise-addr 127.0.0.1`. It leaves any
// swarm a failed earlier attempt left behind first, so it can run twice. The dind has no
// route to a registry, so a swarm service has to use an image that is already there.
export function swarmInit(): void {
  swarmLeave();
  dind("docker swarm init --advertise-addr 127.0.0.1 >/dev/null");
}

// swarmLeave takes the dind engine out of its swarm, which removes the services, secrets
// and configs the swarm held, and the bridge it made. It is safe when the engine is in no swarm.
export function swarmLeave(): void {
  dind("docker swarm leave --force >/dev/null 2>&1 || true");
  // The bridge the swarm made for its containers outlives it on some engines; a swarm made
  // again creates it afresh.
  dind("docker network rm docker_gwbridge >/dev/null 2>&1 || true");
}

// dockerExec runs a shell script inside one service of the stack with
// `docker compose exec -T <service> sh -c <script>`, as a person at a prompt on
// that machine would. For "agenthost" it is a shell on the remote Docker host:
// the golden path pastes the install command Bayplan showed into it. It returns
// what the script printed; a non-zero exit throws with the script's output.
export function dockerExec(container: string, script: string): string {
  return execFileSync(
    "docker",
    ["compose", "-f", COMPOSE_FILE, "exec", "-T", container, "sh", "-c", script],
    { encoding: "utf8", timeout: 180_000 },
  ).trim();
}

// dindCount is the number of lines a dind command prints: `docker ps -aq` and
// its siblings print one id or name per line.
export function dindCount(cmd: string): number {
  const out = dind(cmd);
  return out === "" ? 0 : out.split("\n").length;
}

// PageTracker watches one page for the two things a unit test cannot see: a
// Content-Security-Policy violation (T-01-113) and a request that leaves the
// Bayplan origin (T-01-114, D-16).
export interface PageTracker {
  // Every request URL the page made, in order.
  readonly requests: readonly string[];
  // Every CSP violation reported, as a readable line.
  readonly cspViolations: readonly string[];
  assertNoCspViolations(): void;
  assertSameOrigin(baseURL: string): void;
}

export async function trackPage(page: Page): Promise<PageTracker> {
  const requests: string[] = [];
  const cspViolations: string[] = [];

  // A navigation throws away the page's own window.__csp, so each violation is
  // also handed to this side as it happens and survives every route change.
  await page.exposeFunction("__bayplanCspReport", (line: string) => {
    cspViolations.push(line);
  });
  await page.addInitScript(() => {
    const w = window as unknown as {
      __csp: string[];
      __bayplanCspReport?: (line: string) => void;
    };
    w.__csp = [];
    document.addEventListener("securitypolicyviolation", (e) => {
      const line = `${e.violatedDirective} blocked ${e.blockedURI || "inline"} at ${e.sourceFile}:${e.lineNumber}`;
      w.__csp.push(line);
      w.__bayplanCspReport?.(line);
    });
  });
  // Chromium also logs every enforced violation to the console. Reading that as
  // well catches one raised before the init script's listener existed.
  page.on("console", (msg) => {
    const text = msg.text();
    if (/Content Security Policy|Refused to (load|apply|execute|connect|frame)/i.test(text)) {
      cspViolations.push(`console: ${text}`);
    }
  });
  page.on("request", (req) => {
    requests.push(req.url());
  });

  return {
    requests,
    cspViolations,
    assertNoCspViolations() {
      expect(cspViolations, "Content-Security-Policy violations").toEqual([]);
    },
    assertSameOrigin(baseURL: string) {
      const origin = new URL(baseURL).origin;
      const offenders = requests.filter((u) => {
        const url = new URL(u);
        // data:, blob: and about: never leave the browser.
        if (url.protocol !== "http:" && url.protocol !== "https:") return false;
        return url.origin !== origin;
      });
      expect(offenders, `requests to an origin other than ${origin}`).toEqual([]);
    },
  };
}

// The tracker is attached to every page a spec opens through the `page`
// fixture, and checked when the test ends, so no golden path can pass with a
// CSP violation or an off-origin request in it.
export const test = base.extend<{ tracker: PageTracker }>({
  tracker: [
    async ({ page, baseURL }, use) => {
      const tracker = await trackPage(page);
      await use(tracker);
      tracker.assertSameOrigin(baseURL ?? "");
      tracker.assertNoCspViolations();
    },
    { auto: true },
  ],
});

export { expect };

// openFirstEnvironment opens the first environment in the overview and returns
// its dashboard's id. A person who has opened none yet lands on the overview, not
// on a dashboard (D-31: "/" goes to the environment last opened, and a page needs
// a couple of seconds on screen to count), so the specs go through the overview
// the way that person would. Before the agent spec there is exactly one, the
// Docker host Bayplan manages through its socket.
export async function openFirstEnvironment(page: Page): Promise<string> {
  await page.goto(at("/environments"));
  // The first page a test opens can be slow on a busy runner (a cold browser and
  // a full SPA load behind the proxy), so allow more than the 10 s default here.
  await expect(page.getByRole("heading", { name: "Environments", level: 1 })).toBeVisible({
    timeout: 30_000,
  });
  // The name link carries the whole name as its title, in the table and in the
  // stacked rows under 768px alike.
  await page.getByRole("main").locator("a[title]").first().click();
  await expect(page).toHaveURL(/\/bayplan\/env\/[^/]+$/);
  const id = /\/env\/([^/]+)$/.exec(new URL(page.url()).pathname)?.[1];
  if (!id) throw new Error(`no environment id in ${page.url()}`);
  return id;
}

// What the version footer says: "Bayplan v0.1.0" for a release, "Bayplan dev
// (abc1234)" for a build without a version.
export const VERSION_TEXT = /Bayplan (v\d|dev)/;

// expectVersionFooter asserts the AGPL section 13 footer is on screen exactly
// once: the version, and a "Source code" link that opens in a new tab (PLAT-05).
// The sidebar and the page each own the footer in turn, so a second one
// showing at once is a bug as much as a missing one (D-37).
export async function expectVersionFooter(page: Page): Promise<void> {
  const footers = page.locator("footer:visible");
  await expect(footers).toHaveCount(1);
  await expect(footers).toContainText(VERSION_TEXT);
  const source = footers.getByRole("link", { name: /^Source code/ });
  await expect(source).toBeVisible();
  await expect(source).toHaveAttribute("target", "_blank");
}

// The height of a filter chip under 768px, the 44px touch target of P1 D-64, and the space
// between chips and between rows of chips (the ToggleGroup's spacing={2}), in px.
export const CHIP_HEIGHT = 44;
export const CHIP_GAP = 8;

// The width from which the chips stop being 44px high (the md breakpoint), in px.
const CHIP_MAX_VIEWPORT = 768;

type Box = { x: number; y: number; width: number; height: number };

// expectChipsInsideScreen asserts that every chip of a filter-chip row is on the screen (G-06-2).
// It is for screens under 768px, where the chips are 44px high and wrap onto further rows, and it
// reads the screen's width from the page's viewport. It compares each chip with its own row as
// well as with the screen, because the Nodes list's last chip was measured cut off 8px inside its
// row while still inside the screen. A chip that is cut off has its full box as its bounding box,
// so only the comparison with the row catches it. It also asserts the row's computed style: the
// row wraps and does not scroll, so a return to a scrolling strip fails even where the chips
// happen to fit on one line (the Volumes and Images rows).
//
// It waits for the fonts and the document's finite animations, never for a fixed time, because a
// box measured before the fonts load or while something animates is not the box people see. It
// then reads the row and every chip in one evaluate, so all the boxes come from one layout even
// though the lists are live, and runs every assertion on that snapshot. It does not assert the
// page's own width: expectNoSidewaysScroll and expectPhoneWidth in responsive.spec.ts do.
export async function expectChipsInsideScreen(
  page: Page,
  group: Locator,
  what: string,
): Promise<void> {
  await expect(group).toBeVisible();
  const viewport = page.viewportSize();
  expect(viewport, `${what}: the page has a viewport`).not.toBeNull();
  if (!viewport) return;
  const width = viewport.width;
  expect(
    width,
    `${what}: the chips are 44px high only under ${CHIP_MAX_VIEWPORT}px, the viewport is ${width}px`,
  ).toBeLessThan(CHIP_MAX_VIEWPORT);

  await page.evaluate(async () => {
    await document.fonts.ready;
    // A finite animation ends; an infinite one (a spinner or pulse) never does, and a cancelled
    // one rejects, so neither may hold the wait.
    const finite = document
      .getAnimations()
      .filter((a) => Number.isFinite(a.effect?.getComputedTiming().endTime as number));
    await Promise.allSettled(finite.map((a) => a.finished));
  });

  const snap = await group.evaluate((el) => {
    const toBox = (r: DOMRect) => ({ x: r.x, y: r.y, width: r.width, height: r.height });
    const style = getComputedStyle(el);
    return {
      overflowX: style.overflowX,
      flexWrap: style.flexWrap,
      scrollWidth: el.scrollWidth,
      clientWidth: el.clientWidth,
      row: toBox(el.getBoundingClientRect()),
      chips: [...el.querySelectorAll('[data-slot="toggle-group-item"]')].map((chip) => ({
        text: (chip.textContent ?? "").trim(),
        box: toBox(chip.getBoundingClientRect()),
      })),
    };
  });

  expect(snap.overflowX, `${what}: the chip row does not scroll sideways`).toBe("visible");
  expect(snap.flexWrap, `${what}: the chip row wraps`).toBe("wrap");
  expect(
    snap.scrollWidth,
    `${what}: the chip row holds content wider than itself (scrollWidth ${snap.scrollWidth}, clientWidth ${snap.clientWidth})`,
  ).toBeLessThanOrEqual(snap.clientWidth + 1);

  const row = snap.row;
  expect(row.x, `${what}: the chip row starts inside the screen`).toBeGreaterThanOrEqual(0);
  expect(row.x + row.width, `${what}: the chip row ends inside the screen`).toBeLessThanOrEqual(
    width + 0.5,
  );

  expect(snap.chips.length, `${what}: the chip row has chips`).toBeGreaterThan(0);
  let previous: Box | null = null;
  for (const { text, box } of snap.chips) {
    expect(
      box.x,
      `${what}: chip "${text}" starts inside its row (x ${box.x}, row x ${row.x})`,
    ).toBeGreaterThanOrEqual(row.x - 0.5);
    expect(
      box.x + box.width,
      `${what}: chip "${text}" ends inside its row (right ${box.x + box.width}, row right ${row.x + row.width})`,
    ).toBeLessThanOrEqual(row.x + row.width + 0.5);
    expect(
      box.x + box.width,
      `${what}: chip "${text}" ends inside the screen (right ${box.x + box.width}, screen ${width})`,
    ).toBeLessThanOrEqual(width + 0.5);
    expect(
      Math.abs(box.height - CHIP_HEIGHT),
      `${what}: chip "${text}" is ${box.height}px high, not ${CHIP_HEIGHT}px`,
    ).toBeLessThanOrEqual(0.5);
    if (previous) {
      const sameRow = Math.abs(box.y - previous.y) <= 0.5;
      const gap = sameRow
        ? box.x - (previous.x + previous.width)
        : box.y - (previous.y + previous.height);
      expect(
        Math.abs(gap - CHIP_GAP),
        `${what}: chip "${text}" is ${gap}px from the chip before it ${sameRow ? "on its row" : "above it"}, not ${CHIP_GAP}px`,
      ).toBeLessThanOrEqual(0.5);
    }
    previous = box;
  }
}

// screenshotOnFailure attaches the page as it was when a spec that drives its
// own browser context failed. Specs on the `page` fixture get Playwright's own.
export async function screenshotOnFailure(page: Page, info: TestInfo): Promise<void> {
  if (info.status === info.expectedStatus) return;
  await info.attach("failure", {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
}

// retryName keeps a name the spec creates unique across a CI retry: the first
// attempt uses the plain name, a retry adds its number, so a user or team the
// failed attempt left behind does not make the retry fail on "already taken".
export function retryName(base: string, info: TestInfo): string {
  return info.retry === 0 ? base : `${base}-r${info.retry}`;
}

// createUserViaUI creates a user in Settings > Access > Users the way an
// instance admin does, reads the one-time invite link out of the dialog (Show
// link, then the text of the block) and closes the dialog. The link is what the
// admin would send to the person: it is returned whole, token included, and
// must be opened in a browser that is not signed in.
export async function createUserViaUI(page: Page, username: string): Promise<string> {
  await page.goto(at("/settings/access/users"));
  await expect(page.getByRole("table", { name: "Users" })).toBeVisible();
  await page.getByRole("button", { name: "Create user", exact: true }).click();

  const dialog = page.getByRole("dialog", { name: "Create user" });
  await dialog.getByRole("textbox", { name: "Username" }).fill(username);
  await dialog.getByRole("button", { name: "Create user", exact: true }).click();

  // The same dialog moves on to the one-time link.
  await expect(
    page.getByRole("dialog").getByRole("heading", { name: `${username} is ready` }),
  ).toBeVisible();
  const ready = page.getByRole("dialog");
  // The token is hidden until it is asked for.
  await expect(ready.locator("code")).toContainText("\u2022\u2022\u2022\u2022");
  await ready.getByRole("button", { name: "Show link" }).click();
  const link = ((await ready.locator("code").textContent()) ?? "").trim();
  expect(link, "the invite link carries its token in the fragment").toMatch(/\/invite#token=.+/);
  await ready.getByRole("button", { name: "Close" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  return link;
}

// apiAs sends a mutating API call as whoever the context is signed in as, the
// way a script holding that person's cookie would: with the Origin the server
// expects and the CSRF token /api/me hands out. It goes round the UI, so it
// reaches actions the UI hides from that person.
export async function apiAs(
  context: BrowserContext,
  method: "POST" | "PUT" | "PATCH" | "DELETE",
  path: string,
  body?: unknown,
): Promise<APIResponse> {
  const me = await context.request.get(at("/api/me"));
  expect(me.ok(), "GET /api/me").toBe(true);
  const { csrfToken } = (await me.json()) as { csrfToken: string };
  return context.request.fetch(at(path), {
    method,
    data: body,
    headers: { Origin: ORIGIN, "X-CSRF-Token": csrfToken },
  });
}

// createCredentialAs stores a registry credential through the API as whoever the
// context is signed in as, and returns its id and name. The body is the
// CredentialRequest of POST /api/registries; an instance admin's credential is an
// instance one.
export async function createCredentialAs(
  context: BrowserContext,
  body: Record<string, unknown>,
): Promise<{ id: string; name: string }> {
  const res = await apiAs(context, "POST", "/api/registries", body);
  expect(res.status(), `POST /api/registries: ${await res.text()}`).toBe(201);
  const credential = (await res.json()) as { id: string; name: string };
  return { id: credential.id, name: credential.name };
}

// deleteCredentialAs removes a credential through the API. A credential that is
// already gone (404) is fine, so a cleanup in a finally block can run twice.
export async function deleteCredentialAs(context: BrowserContext, id: string): Promise<void> {
  const res = await apiAs(context, "DELETE", `/api/registries/${id}`);
  expect([204, 404], `DELETE /api/registries/${id}`).toContain(res.status());
}

interface EnvironmentRow {
  id: string;
  name: string;
  isLocalSocket: boolean;
}

// environmentsOf lists the environments the signed-in context can see, through
// the API: the specs that follow the agent spec need the id of the Docker host
// Bayplan manages and of the agent environment it left revoked.
export async function environmentsOf(context: BrowserContext): Promise<EnvironmentRow[]> {
  const res = await context.request.get(at("/api/environments"));
  expect(res.ok(), "GET /api/environments").toBe(true);
  return ((await res.json()) as { items: EnvironmentRow[] }).items;
}

// ---------------------------------------------------------------------------
// Stacks (Phase 7)
// ---------------------------------------------------------------------------

// The registry the stacks spec pins images against. It runs inside the engine it serves, on that
// engine's own loopback, the way the Go integration tests do (internal/dockerx/dockertest): a
// stack's image is pinned to a digest a registry gave, the engines of this topology have no route
// to a real one, and Docker treats localhost:5000 as insecure without any daemon setting.
const REGISTRY_IMAGE = "registry:2";
const REGISTRY_NAME = "e2e-registry";
export const REGISTRY_HOST = "localhost:5000";

// StackHost names the engine a stack is deployed to: the dind Bayplan reaches through its socket,
// or the agent host behind a real agent.
export type StackHost = "dind" | "agenthost";

// seedStackImage starts a registry on the engine of the service, loading registry:2 into it from
// the machine running the suite when the engine does not hold it, and pushes an image made from
// the engine's own busybox as `ref` (a localhost:5000/... reference). The image has sh and sleep,
// which is all a stack of this spec runs. It is idempotent, so a retried attempt can run it again,
// and nothing is pulled from outside the machine except registry:2 on the suite's own engine when
// it is missing there.
export function seedStackImage(host: StackHost, ref: string): void {
  if (!ref.startsWith(`${REGISTRY_HOST}/`)) throw new Error(`${ref} is not on ${REGISTRY_HOST}`);
  const held = dockerExec(
    host,
    `docker image inspect ${REGISTRY_IMAGE} >/dev/null 2>&1 && echo yes || echo no`,
  );
  if (held !== "yes") {
    try {
      docker("image", "inspect", REGISTRY_IMAGE);
    } catch {
      docker("pull", REGISTRY_IMAGE);
    }
    execFileSync(
      "sh",
      [
        "-c",
        `docker save ${REGISTRY_IMAGE} | docker compose -f '${COMPOSE_FILE}' exec -T ${host} docker load`,
      ],
      { encoding: "utf8", timeout: 180_000, maxBuffer: 64 * 1024 * 1024 },
    );
  }
  dockerExec(
    host,
    [
      `docker inspect ${REGISTRY_NAME} >/dev/null 2>&1 || docker run -d --name ${REGISTRY_NAME} -p 127.0.0.1:5000:5000 ${REGISTRY_IMAGE} >/dev/null`,
      // Wait on the registry answering, not on the clock.
      "for i in $(seq 1 60); do wget -q -O /dev/null http://127.0.0.1:5000/v2/ 2>/dev/null && break; sleep 1; done",
      "wget -q -O /dev/null http://127.0.0.1:5000/v2/",
      "root=$(mktemp -d)",
      'mkdir -p "$root/bin" "$root/lib" "$root/tmp"',
      'cp /bin/busybox "$root/bin/busybox"',
      'cp /lib/ld-musl-*.so.1 "$root/lib/"',
      'chmod 1777 "$root/tmp"',
      'for c in sh echo sleep touch ls cat printf; do ln -s busybox "$root/bin/$c"; done',
      `tar -C "$root" -c . | docker import - ${ref} >/dev/null`,
      'rm -rf "$root"',
      `docker push ${ref} >/dev/null`,
    ].join("; "),
  );
}

// dropStackImage takes the registry and the images of seedStackImage off the engine again, so the
// specs that follow find the engine as they expect it. It is safe when none is there.
export function dropStackImage(host: StackHost, ...refs: string[]): void {
  dockerExec(
    host,
    [
      `docker rm -f ${REGISTRY_NAME} >/dev/null 2>&1`,
      ...refs.map((r) => `docker rmi -f ${r} >/dev/null 2>&1`),
      `docker rmi -f ${REGISTRY_IMAGE} >/dev/null 2>&1`,
      "true",
    ].join("; "),
  );
}

// enginePs lists the ids of the containers, networks, volumes or secrets of an engine that carry a
// label (docker <kind> ls), one per line, as an array: how a spec proves a removal left nothing.
export function engineIds(
  host: StackHost,
  kind: "container" | "network" | "volume",
  label: string,
): string[] {
  const list =
    kind === "container"
      ? `docker ps -aq --filter label=${label}`
      : `docker ${kind} ls -q --filter label=${label}`;
  const out = dockerExec(host, list);
  return out === "" ? [] : out.split("\n");
}

// createStack opens an environment's Stacks list, makes a stack from an empty file with the
// Create stack dialog and returns once the new stack's page is open. The stack is not deployed.
export async function createStack(page: Page, envId: string, name: string): Promise<void> {
  await page.goto(at(`/env/${envId}/stacks`));
  await expect(page.getByRole("heading", { name: "Stacks", level: 1 })).toBeVisible();
  await page.getByRole("button", { name: "Create stack" }).first().click();
  const dialog = page.getByRole("dialog", { name: "Create a stack" });
  await dialog.getByRole("textbox", { name: /^Name/ }).fill(name);
  await dialog.getByRole("radio", { name: /Empty file/ }).click();
  await dialog.getByRole("button", { name: "Create stack" }).click();
  await expect(page.getByRole("dialog", { name: "Create a stack" })).toHaveCount(0);
  await expect(page).toHaveURL(new RegExp(`/bayplan/env/${envId}/stacks/[^/]+`));
}

// typeInEditor replaces the whole content of the open file's editor with `text`. The editor is a
// CodeMirror surface, so the text goes in as one insertion: typing it key by key would let the
// editor indent each new line again and shift the YAML. `file` is the name the editor carries
// ("compose.yaml, YAML").
export async function typeInEditor(page: Page, file: string, text: string): Promise<void> {
  const editor = page.getByRole("textbox", { name: file, exact: true });
  await editor.click();
  await page.keyboard.press("ControlOrMeta+A");
  await page.keyboard.insertText(text);
}

// openReview presses Review and deploy (which saves the draft first) and waits for the review of
// the stack to load.
export async function openReview(page: Page, name: string): Promise<void> {
  await page.getByRole("button", { name: "Review and deploy" }).click();
  await expect(page.getByRole("heading", { name: `Review ${name}` })).toBeVisible({
    timeout: 60_000,
  });
}

// waitForDeployComplete waits for the deploy console to reach its result: the heading
// "Deploy complete". A failed deploy shows "Deploy failed" instead, and the wait names it at once
// rather than running out its clock.
export async function waitForDeployComplete(page: Page, timeout = 180_000): Promise<void> {
  const done = page.getByRole("heading", { name: "Deploy complete" });
  const failed = page.getByRole("heading", { name: /^(Deploy failed|Deploy cancelled)$/ });
  await expect(done.or(failed)).toBeVisible({ timeout });
  await expect(failed, "the deploy did not complete").toHaveCount(0);
}

// enrollAgentHost adds an environment through the UI, runs the Standalone command it shows on
// the agent host as a person would, and waits for the agent to come online, as the agent spec
// does. It starts from a clean host (the agent an earlier spec left revoked is removed) and
// returns the new environment's id. The proxy's certificate pin goes in front of the image name
// the documented way (docs/agent.md).
export async function enrollAgentHost(page: Page, name: string, pin: string): Promise<string> {
  expect(pin, "make e2e writes .devdata/e2e-tls/pin.txt").toMatch(/^sha256\/.{44}$/);
  dockerExec(
    "agenthost",
    "docker rm -f bayplan-agent >/dev/null 2>&1; docker volume rm -f bayplan-agent >/dev/null 2>&1; true",
  );
  await page.goto(at("/environments"));
  await expect(page.getByRole("heading", { name: "Environments", level: 1 })).toBeVisible();
  await page.getByRole("button", { name: "Add environment" }).first().click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("textbox", { name: /^Name/ }).fill(name);
  await dialog.getByRole("button", { name: "Generate install command" }).click();
  await expect(dialog.getByRole("heading", { name: "Install the agent" })).toBeVisible();
  await dialog.getByRole("tab", { name: "Standalone" }).click();
  await dialog.getByRole("button", { name: "Show token" }).click();
  const code = dialog.getByRole("region", { name: "command" }).locator("code");
  await expect(code).toContainText("docker run -d --name bayplan-agent");
  const command = ((await code.textContent()) ?? "").trim();
  const image = command.split(/\s+/).at(-1) ?? "";
  expect(image, "the last word of the command is the agent image").toMatch(/bayplan-agent:/);
  const cut = command.lastIndexOf(image);
  const pinned = `${command.slice(0, cut)}-e 'BAYPLAN_AGENT_PIN=${pin}' ${command.slice(cut)}`;
  const out = dockerExec("agenthost", pinned);
  expect(out, "docker run prints the container id").toMatch(/^[0-9a-f]{64}$/m);
  await expect(dialog.getByText(`${name} is online`)).toBeVisible({ timeout: 90_000 });
  await expect(page).toHaveURL(/\/bayplan\/env\/[^/]+$/, { timeout: 15_000 });
  const id = /\/env\/([^/]+)$/.exec(new URL(page.url()).pathname)?.[1] ?? "";
  expect(id).not.toBe("");
  return id;
}

// removeRemoteEnvironments removes every environment that is not the managed local host, through
// the API, the way Remove environment does. A host that an environment owns, revoked or not, is
// refused by every other ("This host is already managed by the environment ..."), and the agent
// spec leaves its host with its revoked environment, so a spec that enrols the agent host again
// starts here. It leaves the local host and Docker as they are.
export async function removeRemoteEnvironments(context: BrowserContext): Promise<void> {
  for (const env of await environmentsOf(context)) {
    if (env.isLocalSocket) continue;
    const res = await apiAs(context, "DELETE", `/api/environments/${env.id}`);
    expect([204, 404], `DELETE /api/environments/${env.id}: ${await res.text()}`).toContain(
      res.status(),
    );
  }
}

// removeStackIfPresent removes a stack of this name from an environment through the API, the way
// the Remove button does, and waits until the list no longer has it. A stack an earlier attempt of
// the spec left behind would otherwise make the name taken. It is a no-op when there is none.
export async function removeStackIfPresent(
  context: BrowserContext,
  envId: string,
  name: string,
): Promise<void> {
  const list = async () => {
    const res = await context.request.get(at(`/api/env/${envId}/stacks`));
    expect(res.ok(), `GET /api/env/${envId}/stacks`).toBe(true);
    return ((await res.json()) as { items: { id: string; name: string }[] }).items;
  };
  const found = (await list()).find((s) => s.name === name);
  if (!found) return;
  const res = await apiAs(context, "POST", `/api/env/${envId}/stacks/${found.id}/remove`, {
    confirmName: name,
    volumes: [],
    keepDefinition: false,
  });
  expect([202, 404], `remove ${name}: ${await res.text()}`).toContain(res.status());
  await expect.poll(async () => (await list()).some((s) => s.name === name)).toBe(false);
}
