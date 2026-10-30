// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// G-02-1a: Coaming opened over plain http by a LAN host name has no
// navigator.clipboard, so Copy uses the legacy textarea fallback. Inside a
// modal Dialog the focus trap used to steal the textarea's focus and nothing
// was copied. These tests run on http://coaming.test (an insecure context) and
// read the clipboard back from a localhost page.

import { type BrowserContext, expect, type Page, test } from "@playwright/test";

// Copied from web/harness/copy/main.tsx.
const TOKEN = "bpjt_harness_S3CRET";
const COMMAND = ` docker run -d --name coaming-agent -e COAMING_AGENT_JOIN_TOKEN='${TOKEN}' -e COAMING_AGENT_URL='wss://coaming.test/agent/connect' ghcr.io/ianglennon/coaming-agent:edge`;
const SENTINEL = "SENTINEL-not-the-command";
const COPY = "Copy the install command";
const COPIED = "Copied the install command";

// A second page on http://localhost, a secure context, to seed and read the
// clipboard. Chromium refuses the Clipboard API on an unfocused document, so
// each call brings the reader to the front first.
async function clipboardReader(context: BrowserContext, baseURL: string) {
  const origin = `http://localhost:${new URL(baseURL).port}`;
  await context.grantPermissions(["clipboard-read", "clipboard-write"], { origin });
  const reader: Page = await context.newPage();
  await reader.goto(`${origin}/`);
  return {
    async write(text: string) {
      await reader.bringToFront();
      await reader.evaluate((t) => navigator.clipboard.writeText(t), text);
    },
    async read() {
      await reader.bringToFront();
      return reader.evaluate(() => navigator.clipboard.readText());
    },
  };
}

for (const kind of ["plain", "dialog", "alertdialog"] as const) {
  test(`copies the install command on an insecure origin: ${kind}`, async ({
    page,
    context,
    baseURL,
  }) => {
    const clip = await clipboardReader(context, baseURL as string);
    await clip.write(SENTINEL);

    await page.goto(`/?case=${kind}`);
    // The test exercises the fallback, not the Clipboard API.
    expect(
      await page.evaluate(() => ({
        secure: window.isSecureContext,
        clipboard: typeof navigator.clipboard,
      })),
    ).toEqual({ secure: false, clipboard: "undefined" });
    await page.bringToFront();

    await page.getByRole("button", { name: COPY }).click();
    await expect(page.getByRole("button", { name: COPIED })).toBeVisible();
    await expect.poll(() => clip.read()).toBe(COMMAND);
  });
}

test("reports a failure when execCommand claims success but nothing was copied", async ({
  page,
  context,
  baseURL,
}) => {
  const clip = await clipboardReader(context, baseURL as string);
  await clip.write(SENTINEL);
  // Chromium answers true from execCommand("copy") even with nothing selected;
  // this stand-in answers true and fires no copy event.
  await page.addInitScript(() => {
    Document.prototype.execCommand = () => true;
  });

  await page.goto("/?case=dialog");
  await page.bringToFront();
  await page.getByRole("button", { name: COPY }).click();

  // The modal marks the toast region aria-hidden, so find the toast by its text.
  await expect(
    page.getByText("Copy failed. Select the text and copy it manually."),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: COPY })).toBeVisible();
  await expect(page.getByRole("button", { name: COPIED })).toHaveCount(0);
  // The token is revealed and the command selected for a manual copy.
  await expect.poll(() => page.evaluate(() => window.getSelection()?.toString())).toBe(COMMAND);
  expect(await clip.read()).toBe(SENTINEL);
});
