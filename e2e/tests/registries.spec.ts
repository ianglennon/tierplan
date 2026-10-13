// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The credential sheet after a failed test or a refused save (UAT G-05-4a and
// G-05-4b). A box that appeared next to the form's FieldGroup (an inline-size
// container) in the same commit that grew the FieldGroup's content made Chromium
// drop the layout of the whole FieldGroup: every field and the specific reason
// (sign-in refused, certificate not trusted) vanished, and only the generic
// warning stayed on screen. Only a real browser can see that: jsdom finds text
// that has no layout, so a unit test passes on the broken page. Every flow here
// ends with the same check, that the form still has a box and its buttons work.

import type { Locator, Page, Route } from "@playwright/test";

import { AUTH_STATE, at } from "./env";
import { createCredentialAs, deleteCredentialAs, expect, retryName, test } from "./helpers";

test.use({ storageState: AUTH_STATE });

const HOST = "registry.e2e.invalid:5000";
const NO_ROUTE = "The reserved .invalid name never resolves, so the server's own test fails fast.";
// A fixed throwaway value in the loopback-only e2e stack, never a real secret.
const SECRET = "e2e-throwaway-secret-1";

// expectFormLaidOut asserts the form is on screen and usable: the FieldGroup has a
// real height (it was 0 when the layout collapsed), the fields are visible, and the
// buttons a person needs next are enabled.
async function expectFormLaidOut(dialog: Locator, buttons: string[]): Promise<void> {
  const group = dialog.locator('[data-slot="field-group"]');
  await expect(group).toBeVisible();
  await expect
    .poll(async () => (await group.boundingBox())?.height ?? 0, {
      message: "the FieldGroup of the credential form has no height",
    })
    .toBeGreaterThan(200);
  await expect(dialog.getByRole("textbox", { name: "Name", exact: true })).toBeVisible();
  await expect(dialog.getByRole("textbox", { name: "Username", exact: true })).toBeVisible();
  for (const name of buttons) {
    await expect(dialog.getByRole("button", { name, exact: true })).toBeEnabled();
  }
}

// stub makes one request of the sheet (the given method, to a path ending in
// `suffix`) answer with a canned reply instead of the server's own. Other methods
// and paths go on. It returns the function that removes the stub, which has to be
// given the same predicate and handler it was set with.
async function stub(
  page: Page,
  method: string,
  suffix: string,
  status: number,
  contentType: string,
  body: unknown,
): Promise<() => Promise<void>> {
  const match = (url: URL) => url.pathname.endsWith(suffix);
  const handler = async (route: Route) => {
    if (route.request().method() !== method) return route.fallback();
    await route.fulfill({ status, contentType, body: JSON.stringify(body) });
  };
  await page.route(match, handler);
  return () => page.unroute(match, handler);
}

type Step = readonly [state: string, code: string];

// answerTest makes the sheet's test request answer with the given steps.
function answerTest(
  page: Page,
  steps: { reachable: Step; signedIn: Step; canPull: Step },
): Promise<() => Promise<void>> {
  const step = ([state, code]: Step) => ({ state, code });
  return stub(page, "POST", "/registries/test", 200, "application/json", {
    reachable: step(steps.reachable),
    signedIn: step(steps.signedIn),
    canPull: step(steps.canPull),
    testedAt: new Date().toISOString(),
  });
}

const REFUSED = {
  reachable: ["passed", ""],
  signedIn: ["failed", "sign_in_refused"],
  canPull: ["not_checked", ""],
} as const;
const REFUSED_SENTENCE = `${HOST} refused this username and secret.`;
const WARNING = "The last test failed. You can still save this credential.";

test("a failed test leaves the credential form on screen", async ({
  page,
  context,
}, info) => {
  test.setTimeout(180_000);
  const name = retryName("e2e-sheet", info);
  const credential = await createCredentialAs(context, {
    name,
    type: "generic",
    host: HOST,
    username: "bot",
    secret: SECRET,
  });

  try {
    await page.goto(at(`/registries?credential=${credential.id}`));
    const edit = page.getByRole("dialog", { name: `Edit ${name}` });
    await expect(edit).toBeVisible();
    const secret = edit.getByLabel("New secret (optional)", { exact: true });

    // A: the server's real test of a name that never resolves.
    await edit.getByRole("button", { name: "Test credential", exact: true }).click();
    await expect(edit.getByRole("status")).toContainText("Test failed:", { timeout: 30_000 });
    await expect(
      edit.getByText(`Couldn't reach ${HOST}. Check the address, DNS and firewall.`, {
        exact: true,
      }),
      NO_ROUTE,
    ).toBeVisible();
    await expect(edit.getByText(WARNING)).toBeVisible();
    await expectFormLaidOut(edit, ["Save changes", "Cancel"]);

    // B: the registry refuses the sign-in (G-05-4a).
    const unstubRefused = await answerTest(page, REFUSED);
    await secret.fill("wrong-secret-1");
    await edit.getByRole("button", { name: "Test credential", exact: true }).click();
    await expect(edit.getByText(REFUSED_SENTENCE, { exact: true })).toBeVisible();
    await expect(edit.getByText(WARNING)).toBeVisible();
    await expectFormLaidOut(edit, ["Save changes", "Cancel"]);
    await unstubRefused();
  } finally {
    await page.unrouteAll({ behavior: "ignoreErrors" });
    await deleteCredentialAs(context, credential.id);
  }
});
