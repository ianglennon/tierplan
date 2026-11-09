// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import * as outcome from "@/features/backup/restore-outcome";
import { SetupWizard } from "@/features/setup/setup-wizard";
import { resetBasePathForTests } from "@/lib/base-path";
import "@/i18n";

// The poll after the restart is replaced by a promise the test settles.
vi.mock("@/features/backup/restore-outcome", async (original) => ({
  ...(await original<typeof import("@/features/backup/restore-outcome")>()),
  waitForRestoreOutcome: vi.fn(),
}));

interface Call {
  url: string;
  body: Record<string, unknown>;
}

// announceBase does what the server does when it renders index.html (plan
// 02-03): it puts the base path in a meta tag the SPA reads once.
function announceBase(base: string | null): void {
  for (const el of document.head.querySelectorAll('meta[name="tierplan-base"]')) el.remove();
  resetBasePathForTests();
  if (base === null) return;
  const meta = document.createElement("meta");
  meta.name = "tierplan-base";
  meta.content = base;
  document.head.append(meta);
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
          Response.json({
            defaultInstanceName: "lab-box",
            publicUrl: "https://tierplan.example.com",
            publicUrlLocked: false,
          })
        );
      }
      return (
        opts.complete?.() ??
        Response.json({
          user: { id: "1", username: "admin", instanceAdmin: true },
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
    expect(screen.getByText("Set up TierPlan")).toBeTruthy();
    const token = field("Setup token");
    expect(token.getAttribute("autocomplete")).toBe("off");
    expect(token.getAttribute("spellcheck")).toBe("false");
    expect(token.className).toContain("font-mono");
    expect(screen.getByText("docker exec <container> tierplan setup-token").tagName).toBe("CODE");
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
    await screen.findByText("Set up TierPlan");
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

  it("pre-fills the instance name and the server's public URL on step 3", async () => {
    stubApi({});
    await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    expect(field("Instance name").value).toBe("lab-box");
    expect(field("Public URL").value).toBe("https://tierplan.example.com");
    expect(field("Public URL").readOnly).toBe(false);
    expect(field("Instance name").getAttribute("maxlength")).toBe("64");
  });

  it("offers nothing for the public URL when the page was loaded over http", async () => {
    // jsdom's page is http://localhost:3000. The wizard must not offer an
    // address the form would refuse, so the field starts empty and an empty
    // submit shows the required error instead of sending anything.
    expect(window.location.protocol).toBe("http:");
    stubApi({
      token: () =>
        Response.json({ defaultInstanceName: "lab-box", publicUrl: "", publicUrlLocked: false }),
    });
    await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    expect(field("Public URL").value).toBe("");
    click("Create admin account");
    await screen.findByText("Enter a full https address such as https://tierplan.example.com.");
    expect(field("Public URL").getAttribute("aria-invalid")).toBe("true");
    expect(calls.filter((c) => c.url === "/api/setup/complete")).toHaveLength(0);
  });

  it("pre-fills the browser origin when the page was loaded over https and accepts it", async () => {
    vi.stubGlobal("location", {
      ...window.location,
      protocol: "https:",
      origin: "https://tierplan.example.com",
    });
    stubApi({
      token: () =>
        Response.json({ defaultInstanceName: "lab-box", publicUrl: "", publicUrlLocked: false }),
    });
    await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    expect(field("Public URL").value).toBe("https://tierplan.example.com");
    click("Create admin account");
    await waitFor(() =>
      expect(calls.filter((c) => c.url === "/api/setup/complete")).toHaveLength(1),
    );
  });

  it("pre-fills the public URL with the origin plus the base path under a base", async () => {
    announceBase("/tierplan");
    vi.stubGlobal("location", {
      ...window.location,
      protocol: "https:",
      origin: "https://tierplan.example.com",
    });
    try {
      stubApi({
        token: () =>
          Response.json({ defaultInstanceName: "lab-box", publicUrl: "", publicUrlLocked: false }),
      });
      await renderWizard();
      await passToken();
      fillAdmin();
      await screen.findByText("Name this instance");
      expect(field("Public URL").value).toBe("https://tierplan.example.com/tierplan");
    } finally {
      announceBase(null);
    }
  });

  it("refuses an http public URL with the https-only sentence and sends nothing", async () => {
    stubApi({});
    await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    type("Public URL", "http://tierplan.example.com");
    click("Create admin account");
    const err = await screen.findByText(
      "TierPlan serves HTTPS only. Use an address that starts with https://, such as https://tierplan.example.com.",
    );
    const url = field("Public URL");
    expect(url.getAttribute("aria-invalid")).toBe("true");
    expect(url.getAttribute("aria-describedby")).toContain(err.id);
    expect(err.closest('[data-slot="field-error"]')).toBeTruthy();
    expect(calls.filter((c) => c.url === "/api/setup/complete")).toHaveLength(0);
  });

  it.each([
    "tierplan.example.com",
    "https://",
    "https://admin:pw@tierplan.example.com",
    "https://tierplan.example.com/?x=1",
    "https://tierplan.example.com/#top",
  ])("shows the generic public URL error for %s", async (value) => {
    stubApi({});
    await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    type("Public URL", value);
    click("Create admin account");
    await screen.findByText("Enter a full https address such as https://tierplan.example.com.");
    expect(calls.filter((c) => c.url === "/api/setup/complete")).toHaveLength(0);
  });

  it("makes the public URL read-only with the locked helper when the environment sets it", async () => {
    stubApi({
      token: () =>
        Response.json({
          defaultInstanceName: "lab-box",
          publicUrl: "https://tierplan.example.com",
          publicUrlLocked: true,
        }),
    });
    await renderWizard();
    await passToken();
    fillAdmin();
    await screen.findByText("Name this instance");
    const url = field("Public URL");
    expect(url.readOnly).toBe(true);
    expect(url.value).toBe("https://tierplan.example.com");
    expect(screen.getByText("Set by the TIERPLAN_PUBLIC_URL environment variable.")).toBeTruthy();
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
    type("Public URL", "https://tierplan.example.com");
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
          publicUrl: "https://tierplan.example.com",
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
    await screen.findByText("Enter a full https address such as https://tierplan.example.com.");
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
    await screen.findByText("Can't reach TierPlan. Check your connection and try again.");
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
        user: { id: "1", username: "admin", instanceAdmin: true },
        csrfToken: "c",
        instanceName: "Lab",
        publicUrl: "https://b.example",
        publicUrlLocked: false,
      }),
    );
    await waitFor(() => expect(router.state.location.pathname).toBe("/welcome"));
  });
});

// The wizard's restore branch (D-30, path 1).
describe("SetupWizard restore branch", () => {
  const SUMMARY = {
    appVersion: "v0.1.0",
    chainVerified: true,
    counts: { users: 2, teams: 1, environments: 3, auditEntries: 40 },
    created: "2026-10-07T10:30:00Z",
    instanceName: "Old lab",
    older: false,
    publicUrl: "",
    publicUrlDiffers: false,
    schema: 12,
  };
  const BACKUP_PASS = "correct horse battery";
  const file = () => new File(["bytes"], "lab.tierplan");

  interface Seen {
    method: string;
    path: string;
    token: string | null;
    body: unknown;
    csrf: string | null;
  }
  let seen: Seen[];
  let answers: Record<string, () => Response | Promise<Response>>;

  beforeEach(() => {
    seen = [];
    answers = {
      "/api/setup/token": () =>
        Response.json({ defaultInstanceName: "lab-box", publicUrl: "", publicUrlLocked: false }),
      "/api/setup/restore/upload": () =>
        Response.json({ uploadId: "up-1", size: 5, expiresAt: "x" }, { status: 201 }),
      "/api/setup/restore/check": () => Response.json(SUMMARY),
      "/api/setup/restore/apply": () => Response.json({ restarting: true }, { status: 202 }),
    };
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        if (typeof input === "string") {
          const path = new URL(input, window.location.origin).pathname;
          const headers = new Headers(init?.headers);
          const method = init?.method ?? "GET";
          seen.push({
            method,
            path,
            token: headers.get("X-Setup-Token"),
            body: init?.body,
            csrf: headers.get("X-CSRF-Token"),
          });
          const answer = answers[`${method} ${path}`] ?? answers[path];
          return (answer ?? (() => Response.json({}, { status: 404 })))();
        }
        const req = input as Request;
        const path = new URL(req.url).pathname;
        seen.push({
          method: req.method,
          path,
          token: req.headers.get("X-Setup-Token"),
          body: await req.clone().json(),
          csrf: req.headers.get("X-CSRF-Token"),
        });
        return (answers[path] ?? (() => Response.json({}, { status: 404 })))();
      }),
    );
    vi.mocked(outcome.waitForRestoreOutcome).mockReset();
  });

  async function renderRestoreWizard() {
    const root = createRootRoute({ component: SetupWizard });
    const home = createRoute({ getParentRoute: () => root, path: "/" });
    const setup = createRoute({ getParentRoute: () => root, path: "/setup" });
    const login = createRoute({ getParentRoute: () => root, path: "/login" });
    const router = createRouter({
      routeTree: root.addChildren([home, setup, login]),
      history: createMemoryHistory({ initialEntries: ["/setup"] }),
    });
    render(
      <QueryClientProvider client={new QueryClient()}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    );
    await screen.findByText("Step 1 of 4");
    return { router };
  }

  async function openRestore() {
    const view = await renderRestoreWizard();
    await passToken();
    fireEvent.click(screen.getByRole("button", { name: "Restore from a backup" }));
    await screen.findByRole("heading", { name: "Restore from a backup" });
    return view;
  }

  function chooseAndCheck() {
    fireEvent.change(field("Backup file"), { target: { files: [file()] } });
    fireEvent.change(field("Passphrase"), { target: { value: BACKUP_PASS } });
    click("Check backup");
  }

  it("offers 'Restoring an existing TierPlan? Restore from a backup' under the admin form, the last words a link", async () => {
    stubApi({});
    await renderRestoreWizard();
    await passToken();
    const link = screen.getByRole("button", { name: "Restore from a backup" });
    expect(link.closest("p")?.textContent).toBe(
      "Restoring an existing TierPlan? Restore from a backup",
    );
    expect(link.textContent).toBe("Restore from a backup");
    expect(screen.queryByText("Restoring an existing TierPlan?")).not.toBeNull();
  });

  it("switches step 2 to the restore form without leaving the route, reading Step 2 of 2", async () => {
    const { router } = await openRestore();
    expect(router.state.location.pathname).toBe("/setup");
    expect(screen.getByText("Step 2 of 2")).toBeTruthy();
    expect(
      screen.getByText(
        "Upload a backup file and enter its passphrase. TierPlan checks it before changing anything.",
      ),
    ).toBeTruthy();
    expect(field("Backup file")).toBeTruthy();
    expect(field("Passphrase")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Back" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Check backup" })).toBeTruthy();
    // The admin form is gone while the restore form shows.
    expect(screen.queryByLabelText("Username")).toBeNull();
  });

  it("goes Back to the admin form with the values typed there still in place", async () => {
    await renderRestoreWizard();
    await passToken();
    type("Username", "Admin");
    type("Password", PASSWORD);
    fireEvent.click(screen.getByRole("button", { name: "Restore from a backup" }));
    await screen.findByRole("heading", { name: "Restore from a backup" });
    click("Back");
    await screen.findByText("Create the admin account");
    expect(screen.getByText("Step 2 of 4")).toBeTruthy();
    expect(field("Username").value).toBe("Admin");
    expect(field("Password").value).toBe(PASSWORD);
  });

  it("uploads with the setup token in a header, checks with it in the body, and shows the summary with Restore backup as the primary action", async () => {
    await openRestore();
    expect(seen.filter((s) => s.path.includes("/restore/"))).toHaveLength(0);
    chooseAndCheck();
    await screen.findByText("What's in this backup");

    const upload = seen.find((s) => s.path === "/api/setup/restore/upload");
    expect(upload?.token).toBe("tok-123");
    expect(upload?.csrf).toBeNull();
    expect(seen.find((s) => s.path === "/api/setup/restore/check")?.body).toEqual({
      uploadId: "up-1",
      passphrase: BACKUP_PASS,
      setupToken: "tok-123",
    });
    expect(screen.getByText("From Old lab")).toBeTruthy();
    expect(screen.getByText("2 users, 1 team, 3 environments, 40 audit entries")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Choose another file" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Restore backup" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Check backup" })).toBeNull();
    // A fresh install has nothing to lose: no type-to-confirm.
    expect(screen.queryByLabelText(/^Type .* to confirm$/)).toBeNull();
  });

  it("restores on one press: waits for the restart, then goes to /login with the restored notice", async () => {
    let settle: (o: outcome.RestoreOutcome) => void = () => {};
    vi.mocked(outcome.waitForRestoreOutcome).mockImplementation(
      () => new Promise((resolve) => (settle = resolve)),
    );
    const { router } = await openRestore();
    chooseAndCheck();
    await screen.findByText("What's in this backup");
    click("Restore backup");

    const status = await screen.findByRole("status");
    expect(within(status).getByText("Restoring…")).toBeTruthy();
    expect(seen.find((s) => s.path === "/api/setup/restore/apply")?.body).toEqual({
      uploadId: "up-1",
      passphrase: BACKUP_PASS,
      setupToken: "tok-123",
    });

    settle("restored");
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    expect(router.state.location.search).toEqual({ notice: "backup-restored" });
  });

  it("shows the same messages as the Backup page for a bad backup, and keeps the form", async () => {
    answers["/api/setup/restore/check"] = () =>
      problem(422, { type: "backup_unreadable", detail: "x" });
    await openRestore();
    chooseAndCheck();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "Couldn't open this backup. Check the passphrase, and that the file isn't damaged.",
    );
    expect(field("Backup file")).toBeTruthy();
    expect(screen.getByText("Step 2 of 2")).toBeTruthy();
  });

  it("reads any 413 as a cut-off upload", async () => {
    answers["/api/setup/restore/upload"] = () => new Response("too big", { status: 413 });
    await openRestore();
    chooseAndCheck();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "The upload was cut off. If TierPlan runs behind a reverse proxy, check its upload size limit.",
    );
  });

  it("says there were too many attempts when the wizard's check or restore is rate limited", async () => {
    answers["/api/setup/restore/check"] = () => problem(429, { type: "rate_limited", detail: "x" });
    await openRestore();
    chooseAndCheck();
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Too many attempts. Wait a minute and try again.",
    );

    answers["/api/setup/restore/check"] = () => Response.json(SUMMARY);
    answers["/api/setup/restore/apply"] = () => problem(429, { type: "rate_limited", detail: "x" });
    click("Check backup");
    await screen.findByText("What's in this backup");
    click("Restore backup");
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toBe(
        "The restore didn't finish and nothing was changed. Too many attempts. Wait a minute and try again.",
      ),
    );
  });

  it("treats a 409 setup_complete as setup already done and leaves the wizard", async () => {
    answers["/api/setup/restore/upload"] = () => problem(409, { type: "setup_complete" });
    const { router } = await openRestore();
    chooseAndCheck();
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
  });

  it("says nothing was changed, with the server's reason, when apply is refused", async () => {
    answers["/api/setup/restore/apply"] = () =>
      problem(500, { type: "internal", detail: "The database could not be prepared." });
    await openRestore();
    chooseAndCheck();
    await screen.findByText("What's in this backup");
    click("Restore backup");
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("The restore didn't finish and nothing was changed.");
    expect(alert.textContent).toContain("The database could not be prepared.");
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("says TierPlan went back to the snapshot when it came back still asking for setup", async () => {
    vi.mocked(outcome.waitForRestoreOutcome).mockResolvedValue("rolled_back");
    const { router } = await openRestore();
    chooseAndCheck();
    await screen.findByText("What's in this backup");
    click("Restore backup");
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("The restore failed and TierPlan went back to the snapshot.");
    expect(router.state.location.pathname).toBe("/setup");
  });

  it("holds the setup token and the passphrase in memory only", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    await openRestore();
    chooseAndCheck();
    await screen.findByText("What's in this backup");
    expect(setItem).not.toHaveBeenCalled();
    expect(window.location.href).not.toContain("tok-123");
    expect(window.location.href).not.toContain(BACKUP_PASS);
    for (const s of seen) expect(s.path).not.toContain("tok-123");
  });

  it("clears the file and passphrase on Choose another file", async () => {
    await openRestore();
    chooseAndCheck();
    await screen.findByText("What's in this backup");
    click("Choose another file");
    expect(field("Passphrase").value).toBe("");
    expect(screen.getByText("No file chosen")).toBeTruthy();
  });

  it("discards the staged upload with the setup token when the file is given up, ignoring a 404", async () => {
    answers["DELETE /api/setup/restore/upload"] = () => problem(404, { type: "not_found" });
    await openRestore();
    chooseAndCheck();
    await screen.findByText("What's in this backup");
    expect(seen.filter((s) => s.method === "DELETE")).toHaveLength(0);
    click("Choose another file");
    await waitFor(() => expect(seen.filter((s) => s.method === "DELETE")).toHaveLength(1));
    const discard = seen.find((s) => s.method === "DELETE");
    expect(discard?.path).toBe("/api/setup/restore/upload");
    expect(discard?.token).toBe("tok-123");
    expect(field("Passphrase").value).toBe("");
  });

  it("discards the staged upload when the wizard's check fails", async () => {
    answers["/api/setup/restore/check"] = () =>
      problem(422, { type: "backup_unreadable", detail: "x" });
    await openRestore();
    chooseAndCheck();
    await screen.findByRole("alert");
    await waitFor(() => expect(seen.filter((s) => s.method === "DELETE")).toHaveLength(1));
  });
});
