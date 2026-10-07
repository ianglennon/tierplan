// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { clearInviteToken, heldInviteToken } from "@/api/invite";
import { queryClient } from "@/api/query-client";
import { resetBasePathForTests } from "@/lib/base-path";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";

const TOKEN = "T".repeat(43);
const PASSWORD = "a long chosen passphrase";

interface Sent {
  path: string;
  method: string;
  body?: Record<string, unknown>;
}

let sent: Sent[] = [];

// stubApi answers the calls the invite page makes. `lookup` and `accept` decide
// what the two invite endpoints say.
function stubApi(opts: { lookup?: () => Response; accept?: () => Response } = {}) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method };
      if (req.method === "POST") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 14 });
      }
      if (path === "/api/invite/lookup") {
        return (
          opts.lookup?.() ??
          Response.json({ kind: "invite", username: "alice", instanceName: "Lab" })
        );
      }
      if (path === "/api/invite/accept") {
        return opts.accept?.() ?? new Response(null, { status: 204 });
      }
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.0.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.0.0",
        });
      }
      return Response.json({ status: 401 }, { status: 401 });
    }),
  );
}

function renderInvite() {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: ["/invite"] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

// openLink puts the address the admin handed over in the browser's location, as
// a browser does before the page's script runs.
function openLink(hash = `#token=${TOKEN}`) {
  window.history.replaceState(null, "", `/invite${hash}`);
}

beforeEach(() => {
  sent = [];
  setCsrfToken(null);
  resetBasePathForTests();
  clearInviteToken();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  window.history.replaceState(null, "", "/");
});

describe("/invite", () => {
  it("removes the fragment from the address bar, shows who the link is for and sets the password", async () => {
    stubApi();
    openLink();
    const router = renderInvite();

    expect(await screen.findByRole("heading", { name: "Set your password" })).toBeTruthy();
    expect(window.location.hash).toBe("");
    expect(window.location.href).not.toContain(TOKEN);
    expect(
      screen.getByText("alice, choose a password to finish setting up your account on Lab."),
    ).toBeTruthy();

    fireEvent.change(screen.getByLabelText("New password"), { target: { value: PASSWORD } });
    fireEvent.change(screen.getByLabelText("Confirm new password"), {
      target: { value: PASSWORD },
    });
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    expect(router.state.location.search).toEqual({ notice: "password-set" });
    const accept = sent.find((s) => s.path === "/api/invite/accept");
    expect(accept?.body).toEqual({ token: TOKEN, password: PASSWORD });
    // The token went in bodies only, never into a URL.
    expect(sent.every((s) => !s.path.includes(TOKEN))).toBe(true);
    expect(heldInviteToken()).toBeNull();
  });
});
