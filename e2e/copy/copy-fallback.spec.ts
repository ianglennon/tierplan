// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// G-02-1a: Bayplan opened over plain http by a LAN host name has no
// navigator.clipboard, so Copy uses the legacy textarea fallback. Inside a
// modal Dialog the focus trap used to steal the textarea's focus and nothing
// was copied. These tests run on http://bayplan.test (an insecure context) and
// read the clipboard back from a localhost page.

import { type BrowserContext, expect, type Page, test } from "@playwright/test";

// Copied from web/harness/copy/main.tsx.
const TOKEN = "bpjt_harness_S3CRET";
const COMMAND = ` docker run -d --name bayplan-agent -e BAYPLAN_AGENT_JOIN_TOKEN='${TOKEN}' -e BAYPLAN_AGENT_URL='wss://bayplan.test/agent/connect' ghcr.io/ianglennon/bayplan-agent:edge`;
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
