// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackLink, StackSentence } from "@/features/stacks/stack-link";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// StackLink (plan 07-30, STK-10, UI-SPEC "Links on other pages"; plan 08-09, STK-12, D-09): a stack name on
// another page is a link to the Coaming stack when the server says Coaming manages it, and a link to the read-only
// page of the stack when it does not, for a user who can read stacks on the environment. Otherwise it is text.

let client: QueryClient;
let role: "viewer" | "none";

beforeEach(() => {
  role = "viewer";
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      if (new URL(req.url).pathname === "/api/me") {
        return Response.json(
          role === "viewer"
            ? makeMe({ envs: { "env-1": "viewer" } })
            : makeMe({ permissions: { global: [], environments: {} } }),
        );
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

// permissionsKnown waits for the user's permissions to be read, so a name that stays text is text because they do
// not allow a link and not because they have not arrived.
async function permissionsKnown() {
  await waitFor(() => expect(client.getQueryData(["me"])).toBeTruthy());
}

function shell(children: React.ReactNode) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <p data-testid="host">{children}</p>
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

function mount(props: { name: string; coamingStackId?: string }) {
  shell(<StackLink envId="env-1" {...props} />);
}

describe("StackLink", () => {
  it("links the name to the Coaming stack when the server names it", () => {
    mount({ name: "shop", coamingStackId: "stk-1" });
    const link = screen.getByRole("link", { name: "shop" });
    expect(link.getAttribute("href")).toBe("/env/env-1/stacks/stk-1");
  });

  it("links a name the server confirms no ID for to the unmanaged page, with the Tooltip Not managed by Coaming.", async () => {
    mount({ name: "legacy" });
    const link = await screen.findByRole("link", { name: "legacy" });
    expect(link.getAttribute("href")).toBe("/env/env-1/unmanaged-stacks/legacy");
    fireEvent.focus(link);
    expect((await screen.findByRole("tooltip")).textContent).toBe("Not managed by Coaming.");
  });

  it.each([
    ["has no Coaming stack ID", undefined],
    ["has an empty Coaming stack ID", ""],
  ])("links the unmanaged page when the stack %s", async (_name, coamingStackId) => {
    mount({ name: "legacy", coamingStackId });
    expect((await screen.findByRole("link", { name: "legacy" })).getAttribute("href")).toBe(
      "/env/env-1/unmanaged-stacks/legacy",
    );
  });

  it("stays text for a user who cannot read stacks on the environment", async () => {
    role = "none";
    mount({ name: "legacy" });
    // The permissions have to be known before the answer can be a link, and here they never allow it.
    await permissionsKnown();
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByTestId("host").textContent).toBe("legacy");
  });

  it("draws a name from Docker as text, never as markup", async () => {
    mount({ name: "<b>shop</b>", coamingStackId: "stk-1" });
    expect(document.querySelector("b")).toBeNull();
    expect(screen.getByRole("link", { name: "<b>shop</b>" })).toBeTruthy();
    cleanup();
    mount({ name: "<b>legacy</b>" });
    expect(await screen.findByRole("link", { name: "<b>legacy</b>" })).toBeTruthy();
    expect(document.querySelector("b")).toBeNull();
  });

  it("builds the address from the ID by the router, never from the label's text", () => {
    mount({ name: "shop", coamingStackId: "stk-1" });
    // The link goes to the stack page of this environment; the name never takes part in it.
    expect(screen.getByRole("link").getAttribute("href")).not.toContain("shop");
  });

  it("encodes the name of an unmanaged stack in the address", async () => {
    mount({ name: "a/b c" });
    expect((await screen.findByRole("link", { name: "a/b c" })).getAttribute("href")).toBe(
      "/env/env-1/unmanaged-stacks/a%2Fb%20c",
    );
  });
});

describe("StackSentence", () => {
  it("keeps the words and makes only the name a link to the unmanaged page", async () => {
    shell(
      <StackSentence
        i18nKey="stackOverview.notDeployed.body"
        stack="legacy"
        variable="name"
        envId="env-1"
      />,
    );
    const link = await screen.findByRole("link", { name: "legacy" });
    expect(link.getAttribute("href")).toBe("/env/env-1/unmanaged-stacks/legacy");
    expect(screen.getByTestId("host").textContent).toBe(
      "legacy has a definition, but nothing is running. Review and deploy it when you're ready.",
    );
  });

  it("is the plain sentence for a user who cannot read stacks", async () => {
    role = "none";
    shell(
      <StackSentence
        i18nKey="stackOverview.notDeployed.body"
        stack="legacy"
        variable="name"
        envId="env-1"
      />,
    );
    await permissionsKnown();
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByTestId("host").textContent).toContain("legacy has a definition");
  });
});
