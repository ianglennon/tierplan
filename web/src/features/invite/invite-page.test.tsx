// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { StrictMode } from "react";
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

function renderInvite(strict = false) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: ["/invite"] }),
  });
  const app = (
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
  render(strict ? <StrictMode>{app}</StrictMode> : app);
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

describe("/invite under React StrictMode (WR-C-05)", () => {
  it("validates the link and sets the password even though StrictMode runs the page's cleanup and mounts it again", async () => {
    stubApi();
    openLink();
    const router = renderInvite(true);

    expect(await screen.findByRole("heading", { name: "Set your password" })).toBeTruthy();
    expect(screen.queryByText("This link doesn't work")).toBeNull();
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: PASSWORD } });
    fireEvent.change(screen.getByLabelText("Confirm new password"), {
      target: { value: PASSWORD },
    });
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    expect(sent.find((s) => s.path === "/api/invite/accept")?.body).toEqual({
      token: TOKEN,
      password: PASSWORD,
    });
    expect(heldInviteToken()).toBeNull();
  });
});

describe("/invite states", () => {
  const dir = (id: string) => screen.getByLabelText(id) as HTMLInputElement;
  const fillPasswords = (password: string, confirm = password) => {
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: password } });
    fireEvent.change(screen.getByLabelText("Confirm new password"), { target: { value: confirm } });
  };
  const notFound = () => Response.json({ status: 404, type: "invalid_link" }, { status: 404 });

  it("shows skeleton lines and no form while the token is checked", async () => {
    let release: (r: Response) => void = () => {};
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const path = new URL(req.url).pathname;
        if (path === "/api/invite/lookup") return new Promise<Response>((r) => (release = r));
        if (path === "/api/instance") {
          return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 14 });
        }
        return Response.json({ status: 404 }, { status: 404 });
      }),
    );
    openLink();
    renderInvite();

    const checking = await screen.findByRole("status", { name: "Checking your link" });
    expect(checking.querySelectorAll("[data-slot=skeleton]").length).toBeGreaterThan(1);
    expect(screen.queryByLabelText("New password")).toBeNull();
    expect(screen.queryByRole("heading", { name: "This link doesn't work" })).toBeNull();

    release(Response.json({ kind: "invite", username: "alice", instanceName: "Lab" }));
    expect(await screen.findByLabelText("New password")).toBeTruthy();
    expect(screen.queryByRole("status", { name: "Checking your link" })).toBeNull();
  });

  it("shows one 'This link doesn't work' state when the lookup says 404", async () => {
    stubApi({ lookup: notFound });
    openLink();
    const router = renderInvite();

    expect(await screen.findByRole("heading", { name: "This link doesn't work" })).toBeTruthy();
    expect(
      screen.getByText(
        "It may have expired, been used already or been replaced by a newer link. Ask an administrator for a new one.",
      ),
    ).toBeTruthy();
    expect(screen.queryByLabelText("New password")).toBeNull();
    const button = screen.getByRole("button", { name: "Go to sign-in" });
    expect(button.getAttribute("data-variant")).toBe("ghost");
    fireEvent.click(button);
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
  });

  it("shows the same state for a page opened with no fragment, and asks the server nothing", async () => {
    stubApi();
    openLink("");
    renderInvite();

    expect(await screen.findByRole("heading", { name: "This link doesn't work" })).toBeTruthy();
    expect(sent.filter((s) => s.path === "/api/invite/lookup")).toHaveLength(0);
  });

  it("shows the same state when accepting finds the link spent", async () => {
    stubApi({ accept: notFound });
    openLink();
    renderInvite();

    await screen.findByLabelText("New password");
    fillPasswords(PASSWORD);
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    expect(await screen.findByRole("heading", { name: "This link doesn't work" })).toBeTruthy();
    expect(screen.queryByLabelText("New password")).toBeNull();
  });

  it("greets a reset link with its own heading and body", async () => {
    stubApi({
      lookup: () => Response.json({ kind: "reset", username: "alice", instanceName: "Lab" }),
    });
    openLink();
    renderInvite();

    expect(await screen.findByRole("heading", { name: "Choose a new password" })).toBeTruthy();
    expect(screen.getByText("alice, choose a new password for Lab.")).toBeTruthy();
  });

  it("refuses a short password and a mismatch under the right fields without calling the server", async () => {
    stubApi();
    openLink();
    renderInvite();
    await screen.findByLabelText("New password");

    fillPasswords("short");
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    expect(await screen.findByText("Use at least 12 characters.")).toBeTruthy();
    expect(dir("New password").getAttribute("aria-invalid")).toBe("true");

    fillPasswords(PASSWORD, `${PASSWORD}x`);
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    expect(await screen.findByText("The passwords don't match.")).toBeTruthy();
    expect(dir("Confirm new password").getAttribute("aria-invalid")).toBe("true");
    expect(sent.filter((s) => s.path === "/api/invite/accept")).toHaveLength(0);
  });

  it("shows the server's 'too common' refusal under the password and keeps the page usable", async () => {
    stubApi({
      accept: () =>
        Response.json(
          {
            status: 422,
            type: "validation_failed",
            errors: [{ location: "body.password", message: "password_common" }],
          },
          { status: 422 },
        ),
    });
    openLink();
    renderInvite();
    await screen.findByLabelText("New password");

    fillPasswords(PASSWORD);
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    expect(
      await screen.findByText("That password is too common. Choose something harder to guess."),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Set password" })).toBeTruthy();
    expect(dir("New password").matches(":disabled")).toBe(false);
  });

  it("says to wait on a 429", async () => {
    stubApi({
      accept: () => Response.json({ status: 429, type: "rate_limited" }, { status: 429 }),
    });
    openLink();
    renderInvite();
    await screen.findByLabelText("New password");

    fillPasswords(PASSWORD);
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    const alert = await screen.findByText("Too many attempts. Wait a minute and try again.");
    expect(alert.closest("[role=alert]")).toBeTruthy();
    // A rate limit is not "this link doesn't work".
    expect(screen.queryByRole("heading", { name: "This link doesn't work" })).toBeNull();
  });

  it("says to wait when the token check itself is rate limited", async () => {
    stubApi({
      lookup: () => Response.json({ status: 429, type: "rate_limited" }, { status: 429 }),
    });
    openLink();
    renderInvite();

    expect(await screen.findByText("Too many attempts. Wait a minute and try again.")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "This link doesn't work" })).toBeNull();
  });

  it("disables the form, shows Setting password… and sends one request while it runs", async () => {
    let release: (r: Response) => void = () => {};
    stubApi({ accept: () => new Promise<Response>((r) => (release = r)) as unknown as Response });
    openLink();
    renderInvite();
    await screen.findByLabelText("New password");

    fillPasswords(PASSWORD);
    const button = screen.getByRole("button", { name: "Set password" });
    fireEvent.click(button);
    expect(await screen.findByText("Setting password…")).toBeTruthy();
    expect(dir("New password").matches(":disabled")).toBe(true);
    expect(dir("Confirm new password").matches(":disabled")).toBe(true);
    expect(screen.getByText("Setting password…").closest("button")?.matches(":disabled")).toBe(
      true,
    );
    fireEvent.submit(dir("New password").closest("form") as HTMLFormElement);
    release(new Response(null, { status: 204 }));
    await waitFor(() => expect(heldInviteToken()).toBeNull());
    expect(sent.filter((s) => s.path === "/api/invite/accept")).toHaveLength(1);
  });

  it("retries once with the CSRF token from /api/me after a 403 csrf_failed", async () => {
    let attempts = 0;
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
          return Response.json({ kind: "invite", username: "alice", instanceName: "Lab" });
        }
        if (path === "/api/me") {
          return Response.json({
            user: { id: "u", username: "bob", displayName: "", instanceAdmin: false },
            csrfToken: "csrf-9",
            permissions: { global: [], environments: {} },
            access: {},
            requirements: {},
            administrators: [],
          });
        }
        if (path === "/api/invite/accept") {
          attempts += 1;
          return attempts === 1
            ? Response.json({ status: 403, type: "csrf_failed" }, { status: 403 })
            : new Response(null, { status: 204 });
        }
        return Response.json({}, { status: 404 });
      }),
    );
    openLink();
    const router = renderInvite();
    await screen.findByLabelText("New password");

    fillPasswords(PASSWORD);
    fireEvent.click(screen.getByRole("button", { name: "Set password" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    expect(attempts).toBe(2);
    expect(sent.some((s) => s.path === "/api/me")).toBe(true);
  });

  it("never puts the token in storage or cookies, and drops it when the page goes away", async () => {
    stubApi();
    openLink();
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    const { unmount } = (() => {
      renderInvite();
      return { unmount: () => cleanup() };
    })();
    await screen.findByLabelText("New password");
    expect(heldInviteToken()).toBe(TOKEN);

    const stored = JSON.stringify({ ...window.localStorage, ...window.sessionStorage });
    expect(stored).not.toContain(TOKEN);
    expect(document.cookie).not.toContain(TOKEN);
    expect(setItem.mock.calls.some(([, value]) => String(value).includes(TOKEN))).toBe(false);
    expect(window.location.href).not.toContain(TOKEN);

    unmount();
    expect(heldInviteToken()).toBeNull();
    setItem.mockRestore();
  });
});
