// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { SetupWizard } from "@/features/setup/setup-wizard";
import "@/i18n";

interface Call {
  url: string;
  body: Record<string, unknown>;
}

const PASSWORD = "correct horse battery";
let calls: Call[] = [];

function problem(status: number, body: Record<string, unknown>) {
  return Response.json(
    { title: "x", status, ...body },
    { status, headers: { "content-type": "application/problem+json" } },
  );
}

// stubApi answers /api/setup/token and /api/setup/complete from the given
// responders and records what the wizard sent.
function stubApi(opts: {
  token?: () => Response | Promise<Response>;
  complete?: () => Response | Promise<Response>;
}) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url).pathname;
      calls.push({ url, body: await req.clone().json() });
      if (url === "/api/setup/token") {
        return (
          opts.token?.() ??
          Response.json({ defaultInstanceName: "lab-box", publicUrl: "", publicUrlLocked: false })
        );
      }
      return (
        opts.complete?.() ??
        Response.json({
          user: { id: "1", username: "admin", role: "admin" },
          csrfToken: "c",
          instanceName: "Lab",
          publicUrl: "https://b.example",
          publicUrlLocked: false,
        })
      );
    }),
  );
}

async function renderWizard() {
  // The wizard is the root's own component; "/" is where a finished setup
  // would otherwise land and "/welcome" is where it navigates to (step 4).
  const root = createRootRoute({ component: SetupWizard });
  const home = createRoute({ getParentRoute: () => root, path: "/" });
  const setup = createRoute({ getParentRoute: () => root, path: "/setup" });
  const welcome = createRoute({ getParentRoute: () => root, path: "/welcome" });
  const router = createRouter({
    routeTree: root.addChildren([home, setup, welcome]),
    history: createMemoryHistory({ initialEntries: ["/setup"] }),
  });
  const client = new QueryClient();
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  await screen.findByText("Step 1 of 4");
  return { router };
}

const field = (label: string | RegExp) => screen.getByLabelText(label) as HTMLInputElement;
const type = (label: string | RegExp, value: string) =>
  fireEvent.change(field(label), { target: { value } });
const click = (name: string) => fireEvent.click(screen.getByRole("button", { name }));

async function passToken() {
  type("Setup token", "tok-123");
  click("Continue");
  await screen.findByText("Create the admin account");
}

function fillAdmin() {
  type("Username", "Admin");
  type("Password", PASSWORD);
  type("Confirm password", PASSWORD);
  click("Continue");
}

beforeEach(() => {
  calls = [];
  // jsdom has no scrolling; the router asks for it on every navigation.
  vi.stubGlobal("scrollTo", vi.fn());
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("SetupWizard", () => {
  it("opens on step 1 with the token field set up for a secret", async () => {
    stubApi({});
    await renderWizard();
    expect(screen.getByText("Set up Bayplan")).toBeTruthy();
    const token = field("Setup token");
    expect(token.getAttribute("autocomplete")).toBe("off");
    expect(token.getAttribute("spellcheck")).toBe("false");
    expect(token.className).toContain("font-mono");
    expect(screen.getByText("docker exec <container> bayplan setup-token").tagName).toBe("CODE");
    expect(token.getAttribute("aria-describedby")).toContain("setup-token-helper");
  });

  it("shows the wrong-token copy inline and keeps the user on step 1", async () => {
    stubApi({ token: () => problem(401, { type: "invalid_setup_token" }) });
    await renderWizard();
    type("Setup token", "nope");
    click("Continue");
    const err = await screen.findByText(
      "That setup token isn't valid. Copy it again from the container logs.",
    );
    const token = field("Setup token");
    expect(token.getAttribute("aria-invalid")).toBe("true");
    expect(token.getAttribute("aria-describedby")).toContain(err.id);
    expect(screen.getByText("Step 1 of 4")).toBeTruthy();
  });

  it("shows the rate-limited copy as a form-level alert", async () => {
    stubApi({ token: () => problem(429, { type: "rate_limited" }) });
    await renderWizard();
    type("Setup token", "tok");
    click("Continue");
    const alert = await screen.findByText("Too many attempts. Wait a minute and try again.");
    expect(alert.closest('[role="alert"]')).toBeTruthy();
    expect(field("Setup token").getAttribute("aria-invalid")).toBe("false");
  });

  it("validates the token before sending anything", async () => {
    stubApi({});
    await renderWizard();
    click("Continue");
    await screen.findByText("Enter the setup token.");
    expect(calls).toHaveLength(0);
  });

  it("validates the admin step before Continue and keeps values on Back", async () => {
    stubApi({});
    await renderWizard();
    await passToken();
    expect(screen.getByText("Step 2 of 4")).toBeTruthy();

    type("Username", "ab");
    type("Password", "short");
    type("Confirm password", "different");
    click("Continue");
    await screen.findByText("Use 3 to 32 characters: letters, numbers, dot, dash or underscore.");
    expect(screen.getByText("Use at least 12 characters.")).toBeTruthy();
    expect(screen.getByText("Create the admin account")).toBeTruthy();

    type("Username", "Admin");
    type("Password", PASSWORD);
    type("Confirm password", "something else entirely");
    click("Continue");
    await screen.findByText("The passwords don't match.");

    // Back to step 1 and forward again with the token unchanged: no new request.
    click("Back");
    await screen.findByText("Set up Bayplan");
    expect(field("Setup token").value).toBe("tok-123");
    click("Continue");
    await screen.findByText("Create the admin account");
    expect(field("Username").value).toBe("admin");
    expect(calls.filter((c) => c.url === "/api/setup/token")).toHaveLength(1);
  });

  it("clears the mismatch error as soon as the confirmation matches, without a blur", async () => {
    // Leaving the password field checks the confirmation too, and an empty one
    // does not match. The error then has to go as the right text is typed: if it
    // waited for the blur it would vanish between mouse-down and mouse-up on
    // Continue, move the buttons and lose the click.
    stubApi({});
    await renderWizard();
    await passToken();
    type("Password", PASSWORD);
    fireEvent.blur(field("Password"));
    await screen.findByText("The passwords don't match.");
    type("Confirm password", PASSWORD);
    await waitFor(() => expect(screen.queryByText("The passwords don't match.")).toBeNull());
  });

  it("lower-cases the username on blur", async () => {
    stubApi({});
    await renderWizard();
    await passToken();
    type("Username", "Admin.User");
    fireEvent.blur(field("Username"));
    await waitFor(() => expect(field("Username").value).toBe("admin.user"));
  });

  it("gives the new password fields their autocomplete and a named show/hide toggle", async () => {
    stubApi({});
    await renderWizard();
    await passToken();
    expect(field("Username").getAttribute("autocomplete")).toBe("username");
    expect(field("Password").getAttribute("autocomplete")).toBe("new-password");
    expect(field("Confirm password").getAttribute("autocomplete")).toBe("new-password");
    expect(field("Password").type).toBe("password");
    click("Show password");
    expect(field("Password").type).toBe("text");
    expect(field("Confirm password").type).toBe("text");
    expect(screen.getByRole("button", { name: "Hide password" })).toBeTruthy();
  });

  it("pre-fills the instance name and the browser origin on step 3", async () => {
    stubApi({});
    await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    expect(field("Instance name").value).toBe("lab-box");
    expect(field("Public URL").value).toBe(window.location.origin);
    expect(field("Public URL").readOnly).toBe(false);
    expect(field("Instance name").getAttribute("maxlength")).toBe("64");
  });

  it("makes the public URL read-only with the locked helper when the environment sets it", async () => {
    stubApi({
      token: () =>
        Response.json({
          defaultInstanceName: "lab-box",
          publicUrl: "https://bayplan.example.com",
          publicUrlLocked: true,
        }),
    });
    await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    const url = field("Public URL");
    expect(url.readOnly).toBe(true);
    expect(url.value).toBe("https://bayplan.example.com");
    expect(screen.getByText("Set by the BAYPLAN_PUBLIC_URL environment variable.")).toBeTruthy();
    expect(screen.getByRole("img", { name: "Locked" })).toBeTruthy();
  });

  it("creates the admin in one request, stores nothing in the browser and goes to step 4", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    stubApi({});
    const { router } = await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    type("Instance name", "  Home lab ");
    type("Public URL", "https://bayplan.example.com");
    click("Create admin account");
    await waitFor(() => expect(router.state.location.pathname).toBe("/welcome"));

    const complete = calls.filter((c) => c.url === "/api/setup/complete");
    expect(complete).toEqual([
      {
        url: "/api/setup/complete",
        body: {
          token: "tok-123",
          username: "admin",
          password: PASSWORD,
          instanceName: "Home lab",
          publicUrl: "https://bayplan.example.com",
        },
      },
    ]);
    expect(router.state.location.pathname).toBe("/welcome");
    expect(router.state.location.search).toEqual({});
    expect(setItem).not.toHaveBeenCalled();
    expect(window.localStorage.length).toBe(0);
    expect(window.sessionStorage.length).toBe(0);
    expect(document.cookie).toBe("");
  });

  it("sends a server password error back to step 2 and focuses the field", async () => {
    stubApi({
      complete: () =>
        problem(422, {
          type: "validation_failed",
          errors: [{ location: "body.password", message: "password_common" }],
        }),
    });
    await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    click("Create admin account");
    await screen.findByText("That password is too common. Choose something harder to guess.");
    expect(screen.getByText("Create the admin account")).toBeTruthy();
    const password = field("Password");
    expect(password.getAttribute("aria-invalid")).toBe("true");
    await waitFor(() => expect(document.activeElement).toBe(password));
  });

  it("keeps a public URL error on step 3", async () => {
    stubApi({
      complete: () =>
        problem(422, {
          type: "validation_failed",
          errors: [{ location: "body.publicUrl", message: "invalid_public_url" }],
        }),
    });
    await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    click("Create admin account");
    await screen.findByText("Enter a full address such as https://bayplan.example.com.");
    expect(screen.getByText("Name this instance")).toBeTruthy();
  });

  it("shows the server-failure copy as a form-level alert on a 500", async () => {
    stubApi({ complete: () => problem(500, { type: "about:blank" }) });
    await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    click("Create admin account");
    const msg = await screen.findByText(/Something went wrong on the server/);
    expect(msg.closest('[role="alert"]')).toBeTruthy();
  });

  it("shows the unreachable copy when the request cannot be made", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("network down");
      }),
    );
    await renderWizard();
    type("Setup token", "tok");
    click("Continue");
    await screen.findByText("Can't reach Bayplan. Check your connection and try again.");
  });

  it("disables the form while pending and never sends a double submit", async () => {
    let release: (r: Response) => void = () => {};
    stubApi({
      complete: () => new Promise<Response>((resolve) => (release = resolve)),
    });
    const { router } = await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");

    const form = field("Instance name").closest("form") as HTMLFormElement;
    click("Create admin account");
    await screen.findByText("Creating account…");
    expect(field("Instance name").matches(":disabled")).toBe(true);
    expect(screen.getByRole("button", { name: /Creating account/ }).matches(":disabled")).toBe(
      true,
    );
    fireEvent.submit(form);
    fireEvent.submit(form);
    expect(calls.filter((c) => c.url === "/api/setup/complete")).toHaveLength(1);

    release(
      Response.json({
        user: { id: "1", username: "admin", role: "admin" },
        csrfToken: "c",
        instanceName: "Lab",
        publicUrl: "https://b.example",
        publicUrlLocked: false,
      }),
    );
    await waitFor(() => expect(router.state.location.pathname).toBe("/welcome"));
  });
});
