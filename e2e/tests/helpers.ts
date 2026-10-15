// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { execFileSync } from "node:child_process";
import {
  type APIResponse,
  type BrowserContext,
  test as base,
  expect,
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
