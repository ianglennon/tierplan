// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The 375px check the Phase 3 UI-SPEC flagged as needing a visual test (UI
// Considerations, "User detail, Backup and Access pages at 375px"): the page
// does not scroll sideways, the buttons a person taps are at least 44px high, and
// the grant dialog fits the screen with 16px to spare on each side. It is an
// automated guard that complements, and does not replace, a person looking at
// the pages on a phone-sized browser (docs/release-checklist.md).

import type { BrowserContext, Locator, Page } from "@playwright/test";

import { AUTH_STATE, at, ORIGIN, VIEWER_PASSWORD } from "./env";
import {
  apiAs,
  createCredentialAs,
  deleteCredentialAs,
  dind,
  environmentsOf,
  expect,
  expectChipsInsideScreen,
  removeStackIfPresent,
  retryName,
  test,
  trackPage,
  typeInEditor,
} from "./helpers";

const WIDTH = 375;
const HEIGHT = 812;

test.use({ storageState: AUTH_STATE, viewport: { width: WIDTH, height: HEIGHT } });

// The 44px touch target of P1 D-64.
const TOUCH = 44;

// expectNoSidewaysScroll asserts the page itself is no wider than the screen:
// tables are meant to scroll inside their cards instead.
async function expectNoSidewaysScroll(page: Page, what: string): Promise<void> {
  const { scroll, inner } = await page.evaluate(() => ({
    scroll: document.scrollingElement?.scrollWidth ?? 0,
    inner: window.innerWidth,
  }));
  expect(scroll, `${what}: the page scrolls sideways`).toBeLessThanOrEqual(inner);
}

// expectTouchHeight asserts that a named button, as the page shows it, is at
// least 44px high.
async function expectTouchHeight(scope: Page | Locator, name: string, what: string): Promise<void> {
  const button = scope.getByRole("button", { name, exact: true }).first();
  await expect(button, `${what}: "${name}"`).toBeVisible();
  const box = await button.boundingBox();
  expect(box?.height ?? 0, `${what}: "${name}" is ${box?.height}px high`).toBeGreaterThanOrEqual(
    TOUCH,
  );
}

test("the user page stacks its cards, keeps tables inside them and has 44px buttons", async ({
  page,
}) => {
  const res = await page.request.get(at("/api/users"));
  expect(res.ok()).toBe(true);
  const users = (await res.json()) as { id: string; username: string }[];
  const user = users.find((u) => u.username.startsWith("viewer1"));
  expect(user, "the user the access spec created").toBeDefined();
  if (!user) return;

  await page.goto(at(`/settings/access/users/${user.id}`));
  await expect(page.getByRole("heading", { name: user.username, level: 2 })).toBeVisible();
  // The cards below the header: the access card is the one with a table.
  await expect(page.getByRole("heading", { name: "Access", level: 3, exact: true })).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Sessions", level: 3, exact: true }),
  ).toBeVisible();
  await expectNoSidewaysScroll(page, "user page");
  await expectTouchHeight(page, "Add access", "user page");
  await expectTouchHeight(page, "Add to team", "user page");

  // The users list is the page people reach it from.
  await page.goto(at("/settings/access/users"));
  await expect(page.getByRole("table", { name: "Users" })).toBeVisible();
  await expectNoSidewaysScroll(page, "users list");
  await expectTouchHeight(page, "Create user", "users list");
});

test("the Backup page stacks its cards and has 44px buttons", async ({ page }) => {
  await page.goto(at("/settings/backup"));
  await expect(page.getByRole("heading", { name: "Back up and restore Bayplan" })).toBeVisible();
  for (const title of ["Download a backup", "Scheduled backups", "Restore from a backup"]) {
    await expect(page.getByRole("heading", { name: title, level: 3 })).toBeVisible();
  }
  await expectNoSidewaysScroll(page, "Backup page");
  await expectTouchHeight(page, "Download backup", "Backup page");
  await expectTouchHeight(page, "Check backup", "Backup page");
  await expectTouchHeight(page, "Save schedule", "Backup page");
});

test("the environment Access tab fits, and its grant dialog fits the screen", async ({ page }) => {
  const envs = await environmentsOf(page.context());
  const env = envs.find((e) => e.isLocalSocket);
  expect(env, "the managed Docker host").toBeDefined();
  if (!env) return;

  await page.goto(at(`/settings/environments/${env.id}/access`));
  await expect(page.getByRole("heading", { name: `Access to ${env.name}` })).toBeVisible();
  await expect(
    page.getByRole("table", { name: `People with access to ${env.name}` }),
  ).toBeVisible();
  await expectNoSidewaysScroll(page, "environment Access tab");
  await expectTouchHeight(page, "Add access", "environment Access tab");

  await page.getByRole("button", { name: "Add access", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Add access" });
  await expect(dialog).toBeVisible();
  // The three roles are listed in full inside it.
  await expect(dialog.getByRole("radio", { name: "Viewer" })).toBeVisible();
  // The dialog zooms in over 100ms, and a box measured meanwhile is scaled down
  // by a few percent: wait for it to settle before measuring anything.
  await dialog.evaluate(async (el) => {
    await Promise.all(el.getAnimations().map((a) => a.finished));
  });

  const box = await dialog.boundingBox();
  expect(box, "the dialog has a box").not.toBeNull();
  expect(
    box?.width ?? Number.POSITIVE_INFINITY,
    "the dialog is no wider than the screen minus 32px",
  ).toBeLessThanOrEqual(WIDTH - 32 + 0.5);
  expect(box?.x ?? -1, "the dialog starts inside the screen").toBeGreaterThanOrEqual(0);
  expect(
    (box?.x ?? 0) + (box?.width ?? 0),
    "the dialog ends inside the screen",
  ).toBeLessThanOrEqual(WIDTH);
  // Its own buttons are touch sized.
  await expectTouchHeight(dialog, "Cancel", "grant dialog");
  await expectNoSidewaysScroll(page, "grant dialog");
});

// expectPhoneWidth asserts the page is exactly as wide as the phone. On a phone a
// document that is too wide grows window.innerWidth itself (the layout viewport
// expands to fit it), so expectNoSidewaysScroll, which compares the two, cannot
// catch it: the width has to be compared with the screen's own 375.
async function expectPhoneWidth(page: Page, what: string): Promise<void> {
  const { scroll, inner } = await page.evaluate(() => ({
    scroll: document.scrollingElement?.scrollWidth ?? 0,
    inner: window.innerWidth,
  }));
  expect(inner, `${what}: the layout viewport grew to ${inner}px`).toBe(WIDTH);
  expect(scroll, `${what}: the document is ${scroll}px wide`).toBeLessThanOrEqual(WIDTH);
}

// expectInsideScreen asserts a box lies wholly inside the 375x812 window.
async function expectInsideScreen(target: Locator, what: string): Promise<void> {
  const box = await target.boundingBox();
  expect(box, `${what} has a box`).not.toBeNull();
  if (!box) return;
  expect(box.x, `${what} starts at x ${box.x}`).toBeGreaterThanOrEqual(0);
  expect(box.y, `${what} starts at y ${box.y}`).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width, `${what} ends at x ${box.x + box.width}`).toBeLessThanOrEqual(WIDTH);
  expect(box.y + box.height, `${what} ends at y ${box.y + box.height}`).toBeLessThanOrEqual(HEIGHT);
}

// A real phone is what UAT 7 used. A desktop browser resized to 375px does not grow
// its layout viewport for a too-wide page, so this needs mobile emulation (isMobile),
// and a Viewer: the instance admin can manage every credential and so never sees the
// visually hidden reason that makes the page too wide (G-05-7a, G-05-7b).
test("a Viewer on a phone sees Registries, Volumes and Networks at the width of the screen, and View details fits it", async ({
  browser,
  page,
}, info) => {
  test.setTimeout(180_000);
  const envs = await environmentsOf(page.context());
  const env = envs.find((e) => e.isLocalSocket);
  expect(env, "the managed Docker host").toBeDefined();
  if (!env) return;

  const name = retryName("e2e-mobile", info);
  const volume = "e2e-mobile-vol";
  const credential = await createCredentialAs(page.context(), {
    name,
    type: "generic",
    host: "registry.e2e.invalid:5000",
    username: "bot",
    secret: "e2e-throwaway-secret-1",
  });
  let context: BrowserContext | undefined;
  try {
    dind(`docker volume create ${volume}`);

    // The Viewer the access spec made: a retried spec makes viewer1-r1 as well, and
    // only the last one is the user of the attempt that passed.
    const res = await page.request.get(at("/api/users"));
    expect(res.ok()).toBe(true);
    const users = (await res.json()) as { username: string }[];
    const viewerName = users
      .map((u) => u.username)
      .filter((u) => u.startsWith("viewer1"))
      .sort()
      .at(-1);
    expect(viewerName, "the user the access spec created").toBeDefined();
    if (!viewerName) throw new Error("the access spec made no Viewer");

    context = await browser.newContext({
      baseURL: ORIGIN,
      ignoreHTTPSErrors: true,
      storageState: { cookies: [], origins: [] },
      locale: "en-GB",
      timezoneId: "UTC",
      viewport: { width: WIDTH, height: HEIGHT },
      isMobile: true,
      hasTouch: true,
      deviceScaleFactor: 2,
    });
    const phone = await context.newPage();
    await trackPage(phone);
    await phone.goto(at("/login"));
    await phone.getByLabel("Username").fill(viewerName);
    await phone.getByLabel("Password", { exact: true }).fill(VIEWER_PASSWORD);
    await phone.getByRole("button", { name: "Sign in" }).click();
    await expect(phone.getByRole("button", { name: "User menu" })).toBeVisible();

    // The main menu: 18rem wide with the page visible beside it, a Close menu
    // button, closing when a page is chosen, and closing on a tap beside it
    // (G-05-7a, D-09).
    const toggle = phone.getByRole("button", { name: "Toggle sidebar" });
    const menu = phone.getByRole("dialog", { name: "Navigation" });
    const openMenu = async (what: string): Promise<void> => {
      await toggle.click();
      await expect(menu, what).toBeVisible();
      // The drawer slides in; a box measured meanwhile is off to one side.
      await menu.evaluate(async (el) => {
        await Promise.all(el.getAnimations().map((a) => a.finished));
      });
    };
    const expectNarrowMenu = async (what: string): Promise<void> => {
      const box = await menu.boundingBox();
      expect(box, `${what}: the menu has a box`).not.toBeNull();
      expect(box?.width ?? 0, `${what}: the menu is 18rem wide at most`).toBeLessThanOrEqual(288.5);
      expect((box?.x ?? 0) + (box?.width ?? 0), `${what}: the page shows beside it`).toBeLessThan(
        WIDTH,
      );
    };

    await openMenu("the menu opens");
    await expectNarrowMenu("before Registries");
    await menu.getByRole("button", { name: "Close menu" }).click();
    await expect(menu).toBeHidden();

    await openMenu("the menu opens again");
    await menu.getByRole("link", { name: "Registries", exact: true }).click();
    await expect(phone).toHaveURL(/\/bayplan\/registries/);
    await expect(menu).toBeHidden();
    await expect(phone.getByRole("button", { name: `Actions for ${name}` })).toBeVisible();
    await expectPhoneWidth(phone, "Registries");

    await openMenu("the menu opens on Registries");
    await expectNarrowMenu("on Registries");
    await phone.touchscreen.tap(360, 400);
    await expect(menu).toBeHidden();

    await phone.getByRole("button", { name: `Actions for ${name}` }).click();
    await phone.getByRole("menuitem", { name: "View details" }).click();
    const sheet = phone.getByRole("dialog");
    await expect(sheet).toBeVisible();
    // The sheet slides in; a box measured meanwhile is off to one side.
    await sheet.evaluate(async (el) => {
      await Promise.all(el.getAnimations().map((a) => a.finished));
    });
    await expectInsideScreen(sheet, "the View details sheet");
    const closes = sheet.getByRole("button", { name: "Close", exact: true });
    await expect(closes).toHaveCount(2);
    for (const close of await closes.all()) {
      await expectInsideScreen(close, "a Close button of View details");
    }
    await phone.keyboard.press("Escape");
    await expect(sheet).toBeHidden();

    for (const list of ["volumes", "networks"]) {
      await phone.goto(at(`/env/${env.id}/${list}`));
      await expect(phone.getByRole("table").first()).toBeVisible();
      await expectPhoneWidth(phone, `${list} list`);
    }
  } finally {
    await context?.close();
    await deleteCredentialAs(page.context(), credential.id);
    try {
      dind(`docker volume rm ${volume}`);
    } catch {
      // Already gone.
    }
  }
});

// The Containers, Volumes and Images lists keep every filter chip on the screen (G-06-2). They are
// covered here, on the standalone host Bayplan manages, and services.spec.ts covers Services and
// Nodes, because those two lists need the swarm that only the services story creates. The chips'
// counts arrive with each list's data and widen the chips, so a row is measured only once its last
// chip reads its final text.
test("the Containers, Volumes and Images lists keep every filter chip on a 375px screen", async ({
  page,
}) => {
  const envs = await environmentsOf(page.context());
  const env = envs.find((e) => e.isLocalSocket);
  expect(env, "the managed Docker host").toBeDefined();
  if (!env) return;

  await page.goto(at(`/env/${env.id}/containers`));
  const status = page.getByRole("group", { name: "Filter by status" });
  await expect(status.locator('[data-slot="toggle-group-item"]').last()).toHaveText(
    /^Unhealthy [\d,]+$/,
  );
  await expectChipsInsideScreen(page, status, "Containers list");
  await expectNoSidewaysScroll(page, "Containers list");

  // The Volumes chips carry no counts, so the row is measured once the list has loaded: its table,
  // or its empty state when no earlier test left a volume on the host.
  await page.goto(at(`/env/${env.id}/volumes`));
  await expect(
    page
      .getByRole("table")
      .or(page.getByRole("heading", { name: "No volumes" }))
      .first(),
  ).toBeVisible();
  const usage = page.getByRole("group", { name: "Filter by usage" });
  await expect(usage.locator('[data-slot="toggle-group-item"]').last()).toHaveText("Unused");
  await expectChipsInsideScreen(page, usage, "Volumes list");
  await expectNoSidewaysScroll(page, "Volumes list");

  await page.goto(at(`/env/${env.id}/images`));
  const images = page.getByRole("group", { name: "Filter images" });
  await expect(images.locator('[data-slot="toggle-group-item"]').last()).toHaveText(
    /^Dangling [\d,]+$/,
  );
  await expectChipsInsideScreen(page, images, "Images list");
  await expectNoSidewaysScroll(page, "Images list");
});

// G-09-10 (.planning/debug/stack-editor-footer-overlap-375.md): the editor's footer bar was one
// non-wrapping row of a fixed height, with the save status and three buttons that cannot shrink in
// it. At 375px the buttons alone needed 368px of a 343px bar, so the status box collapsed to 0px,
// "Unsaved changes" spilled out under the ghost Discard draft button and Discard draft sat 9px past
// the left edge. The decision (option a) is that the bar wraps at every width: what does not fit
// beside the buttons takes its own row, and what does not fit in a row goes to the next.

const FOOTER_STACK = "e2e-footer";
// A compose file whose image line is indented with a tab does not parse, so the review of it is
// answered at once with its problems and no registry lookups ("Fix 1 error first.").
const FOOTER_COMPOSE = "services:\n  web:\n\timage: busybox:1.36\n";

// expectFooterClear asserts that a footer's text lies inside the screen and inside the bar, that
// every control in the bar is inside the screen and the bar and (on a phone) 44px high, and that
// the text overlaps no control and no control overlaps another. It measures the text with a Range
// and not the element's box, because the defect is text spilling out of a 0px box.
async function expectFooterClear(
  page: Page,
  text: Locator,
  width: number,
  what: string,
): Promise<void> {
  const { scroll, inner } = await page.evaluate(() => ({
    scroll: document.scrollingElement?.scrollWidth ?? 0,
    inner: window.innerWidth,
  }));
  expect(inner, `${what} at ${width}px: the layout viewport is ${inner}px`).toBe(width);
  expect(scroll, `${what} at ${width}px: the document is ${scroll}px wide`).toBeLessThanOrEqual(
    width,
  );

  const bar = page.locator('[data-slot="editor-footer"]');
  const barBox = await bar.boundingBox();
  expect(barBox, `${what} at ${width}px: the bar has a box`).not.toBeNull();
  if (!barBox) return;
  const barLine = `bar x ${barBox.x}..${barBox.x + barBox.width}, y ${barBox.y}..${barBox.y + barBox.height}`;

  const tolerance = 0.5;
  const textBox = await text.evaluate((el) => {
    const range = document.createRange();
    range.selectNodeContents(el);
    const r = range.getBoundingClientRect();
    return { x: r.x, y: r.y, width: r.width, height: r.height };
  });
  const textLine = `text x ${textBox.x}..${textBox.x + textBox.width}, y ${textBox.y}..${textBox.y + textBox.height}`;
  expect(textBox.width, `${what} at ${width}px: the text has width (${textLine})`).toBeGreaterThan(
    0,
  );
  expect(
    textBox.x,
    `${what} at ${width}px: the text starts on the screen (${textLine})`,
  ).toBeGreaterThanOrEqual(0);
  expect(
    textBox.x + textBox.width,
    `${what} at ${width}px: the text ends on the screen (${textLine})`,
  ).toBeLessThanOrEqual(width);
  expect(
    textBox.y,
    `${what} at ${width}px: the text starts inside the bar (${textLine}; ${barLine})`,
  ).toBeGreaterThanOrEqual(barBox.y - tolerance);
  expect(
    textBox.y + textBox.height,
    `${what} at ${width}px: the text ends inside the bar (${textLine}; ${barLine})`,
  ).toBeLessThanOrEqual(barBox.y + barBox.height + tolerance);

  const boxes: { name: string; x: number; y: number; width: number; height: number }[] = [];
  for (const control of await bar.locator("button, a").all()) {
    if (!(await control.isVisible())) continue;
    const box = await control.boundingBox();
    const name =
      (await control.innerText()).trim() ||
      (await control.getAttribute("aria-label")) ||
      "a control";
    expect(box, `${what} at ${width}px: "${name}" has a box`).not.toBeNull();
    if (!box) continue;
    const line = `"${name}" x ${box.x}..${box.x + box.width}, y ${box.y}..${box.y + box.height}; ${barLine}`;
    expect(box.x, `${what} at ${width}px: ${line} starts on the screen`).toBeGreaterThanOrEqual(0);
    expect(
      box.x + box.width,
      `${what} at ${width}px: ${line} ends on the screen`,
    ).toBeLessThanOrEqual(width);
    expect(box.y, `${what} at ${width}px: ${line} starts inside the bar`).toBeGreaterThanOrEqual(
      barBox.y - tolerance,
    );
    expect(
      box.y + box.height,
      `${what} at ${width}px: ${line} ends inside the bar`,
    ).toBeLessThanOrEqual(barBox.y + barBox.height + tolerance);
    if (width < 768) {
      expect(box.height, `${what} at ${width}px: ${line} is touch sized`).toBeGreaterThanOrEqual(
        TOUCH,
      );
    }
    const overX =
      Math.min(box.x + box.width, textBox.x + textBox.width) - Math.max(box.x, textBox.x);
    const overY =
      Math.min(box.y + box.height, textBox.y + textBox.height) - Math.max(box.y, textBox.y);
    expect(
      overX > tolerance && overY > tolerance,
      `${what} at ${width}px: the text (${textLine}) is drawn over ${line}`,
    ).toBe(false);
    boxes.push({ name, ...box });
  }
  for (const [i, a] of boxes.entries()) {
    for (const b of boxes.slice(i + 1)) {
      const overX = Math.min(a.x + a.width, b.x + b.width) - Math.max(a.x, b.x);
      const overY = Math.min(a.y + a.height, b.y + b.height) - Math.max(a.y, b.y);
      expect(
        overX > tolerance && overY > tolerance,
        `${what} at ${width}px: "${a.name}" overlaps "${b.name}"`,
      ).toBe(false);
    }
  }
}

test("the stack editor footer keeps Unsaved changes off Discard draft on a 375px screen (G-09-10)", async ({
  page,
}) => {
  test.setTimeout(180_000);
  const envs = await environmentsOf(page.context());
  const env = envs.find((e) => e.isLocalSocket);
  expect(env, "the managed Docker host").toBeDefined();
  if (!env) return;

  await removeStackIfPresent(page.context(), env.id, FOOTER_STACK);
  const created = await apiAs(page.context(), "POST", `/api/env/${env.id}/stacks`, {
    name: FOOTER_STACK,
    start: "upload",
    compose: FOOTER_COMPOSE,
  });
  expect(created.status(), `create ${FOOTER_STACK}: ${await created.text()}`).toBe(201);
  const { id } = (await created.json()) as { id: string };

  try {
    await page.goto(at(`/env/${env.id}/stacks/${id}/editor`));
    const footer = page.locator('[data-slot="editor-footer"]');
    const status = footer.getByRole("status");
    // Nothing is saved yet: one row, so the bar keeps the 56px it had before the wrap.
    await expect(status).toHaveText("No draft yet");
    await expect(footer.getByRole("button", { name: "Discard draft" })).toHaveCount(0);
    const oneRow = await footer.boundingBox();
    expect(oneRow?.height ?? 0, "the bar of an empty draft is one 56px row").toBeCloseTo(56, 0);

    await typeInEditor(page, "compose.yaml, YAML", `${FOOTER_COMPOSE}# saved\n`);
    await footer.getByRole("button", { name: "Save draft" }).click();
    await expect(status).toHaveText(/^Saved .+ by /);

    await typeInEditor(page, "compose.yaml, YAML", `${FOOTER_COMPOSE}# edited\n`);
    await expect(status).toHaveText("Unsaved changes");
    await expect(footer.getByRole("button", { name: "Discard draft" })).toBeVisible();
    await expectFooterClear(page, status, 375, "editor, unsaved");
  } finally {
    await removeStackIfPresent(page.context(), env.id, FOOTER_STACK);
  }
});
