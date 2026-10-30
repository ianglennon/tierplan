// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { resetBasePathForTests } from "@/lib/base-path";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

const ME = makeMe({
  instanceAdmin: true,
  permissions: { global: ["audit.read"], environments: { "*": [] } },
});

interface Sent {
  path: string;
  method: string;
  csrf: string | null;
  body?: Record<string, unknown>;
}

let sent: Sent[] = [];

// stubApi answers the pre-auth calls the sign-in screen makes. `signedIn`
// decides whether /api/me knows the caller, and `login` answers the sign-in.
function stubApi(opts: {
  signedIn?: boolean;
  // /api/me knows the caller once the sign-in call has been answered, so the
  // guarded screen the user is sent to does not bounce them back to /login.
  signedInAfterLogin?: boolean;
  needsSetup?: boolean;
  login?: () => Response | Promise<Response>;
}) {
  let loggedIn = false;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method, csrf: req.headers.get("X-CSRF-Token") };
      if (req.method === "POST") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/instance") {
        return Response.json({
          instanceName: "Lab",
          needsSetup: opts.needsSetup ?? false,
          rememberDays: 14,
        });
      }
      if (path === "/api/me") {
        return opts.signedIn || (opts.signedInAfterLogin && loggedIn)
          ? Response.json(ME)
          : Response.json({ status: 401 }, { status: 401 });
      }
      if (path === "/api/auth/login") {
        loggedIn = true;
        return (
          opts.login?.() ?? Response.json({ user: ME.user, csrfToken: "csrf-1" }, { status: 200 })
        );
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
      return Response.json({}, { status: 404 });
    }),
  );
}

function renderAt(path: string, basepath?: string) {
  const router = createRouter({
    routeTree,
    basepath,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

// announceBase does what the server does when it renders index.html (plan
// 02-03): it puts the base path in a meta tag the SPA reads once.
function announceBase(base: string | null): void {
  for (const el of document.head.querySelectorAll('meta[name="coaming-base"]')) el.remove();
  resetBasePathForTests();
  if (base === null) return;
  const meta = document.createElement("meta");
  meta.name = "coaming-base";
  meta.content = base;
  document.head.append(meta);
}

const invalid = () => Response.json({ status: 401, type: "invalid_credentials" }, { status: 401 });

beforeEach(() => {
  sent = [];
  setCsrfToken(null);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  window.localStorage.clear();
  window.sessionStorage.clear();
});

async function fill(username: string, password: string) {
  fireEvent.change(await screen.findByLabelText("Username"), { target: { value: username } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: password } });
}

describe("/login", () => {
  it("shows the instance name, the fields and the remember length", async () => {
    stubApi({});
    renderAt("/login");
    expect(await screen.findByRole("heading", { name: "Lab" })).toBeTruthy();
    expect(screen.getByText("Sign in to continue")).toBeTruthy();
    expect(screen.getByLabelText("Username").getAttribute("autocomplete")).toBe("username");
    expect(screen.getByLabelText("Password").getAttribute("autocomplete")).toBe("current-password");
    expect(screen.getByRole("checkbox", { name: "Keep me signed in for 14 days" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeTruthy();
    expect(document.title).toBe("Sign in · Lab");
  });

  it("lowercases the username when the field loses focus", async () => {
    stubApi({});
    renderAt("/login");
    const field = (await screen.findByLabelText("Username")) as HTMLInputElement;
    fireEvent.change(field, { target: { value: "AdMin" } });
    fireEvent.blur(field);
    expect(field.value).toBe("admin");
  });

  it("shows only the generic error for a failed sign-in and keeps the values", async () => {
    stubApi({ login: invalid });
    renderAt("/login");
    await fill("admin", "wrong password");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    const alert = await screen.findByText("Invalid username or password.");
    expect(alert.closest("[role=alert]")).toBeTruthy();
    expect((screen.getByLabelText("Username") as HTMLInputElement).value).toBe("admin");
    expect((screen.getByLabelText("Password") as HTMLInputElement).value).toBe("wrong password");
  });

  it("shows the same error for a locked account or a server error", async () => {
    stubApi({ login: () => Response.json({ status: 429 }, { status: 429 }) });
    renderAt("/login");
    await fill("admin", "x");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByText("Invalid username or password.")).toBeTruthy();
  });

  it("shows the unreachable message when the request itself fails", async () => {
    stubApi({
      login: () => {
        throw new TypeError("network down");
      },
    });
    renderAt("/login");
    await fill("admin", "x");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(
      await screen.findByText("Can't reach Coaming. Check your connection and try again."),
    ).toBeTruthy();
  });

  it("disables the form and shows Signing in… while the request runs, and sends it once", async () => {
    let release: (r: Response) => void = () => {};
    stubApi({ login: () => new Promise<Response>((resolve) => (release = resolve)) });
    renderAt("/login");
    await fill("admin", "pw");
    const button = screen.getByRole("button", { name: "Sign in" });
    fireEvent.click(button);
    expect(await screen.findByText("Signing in…")).toBeTruthy();
    expect(screen.getByLabelText("Username").matches(":disabled")).toBe(true);
    expect(screen.getByLabelText("Password").matches(":disabled")).toBe(true);
    expect(screen.getByText("Signing in…").closest("button")?.matches(":disabled")).toBe(true);
    fireEvent.submit(screen.getByLabelText("Username").closest("form") as HTMLFormElement);
    release(invalid());
    await screen.findByText("Invalid username or password.");
    expect(sent.filter((s) => s.path === "/api/auth/login")).toHaveLength(1);
  });

  it("sends the remember choice and goes to a safe next path", async () => {
    stubApi({});
    const router = renderAt("/login?next=%2Fsomewhere%3Fa%3D1");
    await fill("admin", "pw");
    fireEvent.click(screen.getByRole("checkbox", { name: "Keep me signed in for 14 days" }));
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/somewhere"));
    expect(router.state.location.search).toEqual({ a: 1 });
    const login = sent.find((s) => s.path === "/api/auth/login");
    expect(login?.body).toEqual({ username: "admin", password: "pw", remember: true });
  });

  it("empties the whole query cache on a successful sign-in", async () => {
    stubApi({});
    const router = renderAt("/login?next=%2Fsomewhere");
    await screen.findByLabelText("Username");
    // What a previous user's session left behind (WR-15).
    queryClient.setQueryData(["environments"], { items: [{ id: "e1" }] });
    queryClient.setQueryData(["audit", {}], { items: [] });
    await fill("admin", "pw");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/somewhere"));
    expect(queryClient.getQueryData(["environments"])).toBeUndefined();
    expect(queryClient.getQueryData(["audit", {}])).toBeUndefined();
    expect(queryClient.getQueryData(["me"])).toBeUndefined();
  });

  it.each(["https%3A%2F%2Fevil.example%2F", "%2F%2Fevil.example", "%2F%5Cevil.example"])(
    "ignores an unsafe next (%s) and goes to /",
    async (next) => {
      stubApi({});
      const router = renderAt(`/login?next=${next}`);
      await fill("admin", "pw");
      fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
      // "/" is the home page, which moves on to the last-used environment or
      // the overview as soon as it knows which.
      await waitFor(() => expect(["/", "/environments"]).toContain(router.state.location.pathname));
      expect(router.state.location.href).not.toContain("evil");
    },
  );

  it("shows the expired-session notice", async () => {
    stubApi({});
    renderAt("/login?expired=true");
    expect(await screen.findByText("Your session expired. Sign in again.")).toBeTruthy();
  });

  it("shows the password-set notice after a link was used", async () => {
    stubApi({});
    renderAt("/login?notice=password-set");
    const notice = await screen.findByText("Password set. Sign in to continue.");
    expect(notice.closest("[role=status]")).toBeTruthy();
    // The form is still there: the notice does not sign anyone in.
    expect(screen.getByLabelText("Username")).toBeTruthy();
  });

  it("shows the backup-restored notice after a restore from the setup wizard", async () => {
    stubApi({});
    renderAt("/login?notice=backup-restored");
    const notice = await screen.findByText(
      "Backup restored. Sign in with an account from the backup.",
    );
    expect(notice.closest("[role=status]")).toBeTruthy();
    // The form is still there: the notice does not sign anyone in.
    expect(screen.getByLabelText("Username")).toBeTruthy();
    expect(screen.queryByText("Password set. Sign in to continue.")).toBeNull();
  });

  it.each(["other", "", "Password-Set", "password-set,x"])(
    "shows no notice for the unknown notice value %j",
    async (value) => {
      stubApi({});
      renderAt(`/login?notice=${encodeURIComponent(value)}`);
      await screen.findByLabelText("Username");
      expect(screen.queryByText("Password set. Sign in to continue.")).toBeNull();
      expect(screen.queryByRole("status")).toBeNull();
    },
  );

  it("shows no expired notice by default", async () => {
    stubApi({});
    renderAt("/login");
    await screen.findByLabelText("Username");
    expect(screen.queryByText("Your session expired. Sign in again.")).toBeNull();
  });

  it("sends a signed-in visitor on to next without showing the form", async () => {
    stubApi({ signedIn: true });
    const router = renderAt("/login?next=%2Felsewhere");
    await waitFor(() => expect(router.state.location.pathname).toBe("/elsewhere"));
    expect(screen.queryByLabelText("Username")).toBeNull();
  });

  it("sends a visitor to /setup while no admin exists", async () => {
    stubApi({ needsSetup: true });
    const router = renderAt("/login");
    await waitFor(() => expect(router.state.location.pathname).toBe("/setup"));
  });

  it("never puts the CSRF token or the password in browser storage", async () => {
    stubApi({});
    renderAt("/login");
    await fill("admin", "hunter2-hunter2");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(sent.some((s) => s.path === "/api/auth/login")).toBe(true));
    const stored = JSON.stringify({ ...window.localStorage, ...window.sessionStorage });
    expect(stored).not.toContain("csrf-1");
    expect(stored).not.toContain("hunter2");
  });

  it("keeps the version and source link on the sign-in page", async () => {
    stubApi({});
    renderAt("/login");
    expect(await screen.findByText("Coaming v0.0.0")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Source code (opens in a new tab)" })).toBeTruthy();
  });
});

describe("/login under a base path", () => {
  beforeEach(() => announceBase("/coaming"));
  afterEach(() => announceBase(null));

  it("sends the browser to the base plus next after sign-in", async () => {
    stubApi({ signedInAfterLogin: true });
    const router = renderAt("/coaming/login?next=%2Fenv%2Fa%2Fcontainers", "/coaming");
    await fill("admin", "pw");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(router.history.location.pathname).toBe("/coaming/env/a/containers"));
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/a/containers"));
  });

  it.each(["https%3A%2F%2Fevil.example%2F", "%2F%2Fevil.example", "%2F%5Cevil.example"])(
    "ignores an unsafe next (%s) and lands on the base root",
    async (next) => {
      stubApi({ signedInAfterLogin: true });
      const router = renderAt(`/coaming/login?next=${next}`, "/coaming");
      await fill("admin", "pw");
      fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
      await waitFor(() => expect(router.history.location.pathname).toMatch(/^\/coaming\/?$/));
      await waitFor(() => expect(["/", "/environments"]).toContain(router.state.location.pathname));
      expect(router.history.location.href).not.toContain("evil");
    },
  );

  it("skips the form for a signed-in visitor and lands under the base", async () => {
    stubApi({ signedIn: true });
    const router = renderAt("/coaming/login?next=%2Fenv%2Fa", "/coaming");
    await waitFor(() => expect(router.history.location.pathname).toBe("/coaming/env/a"));
    expect(router.state.location.pathname).toBe("/env/a");
  });
});
