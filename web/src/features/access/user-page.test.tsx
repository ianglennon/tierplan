// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

function userBody(over: Record<string, unknown> = {}) {
  return {
    id: "u1",
    username: "alice",
    displayName: "",
    email: "",
    instanceAdmin: false,
    status: "active",
    teams: [],
    teamCount: 0,
    lastInstanceAdmin: false,
    createdAt: "2026-10-01T10:00:00Z",
    updatedAt: "2026-10-07T10:00:00.123456789Z",
    ...over,
  };
}

let current: Record<string, unknown>;
let user: Record<string, unknown>;
// What GET /api/users/u1 answers: the user, or a status for a failure.
let getUser: () => Response | Promise<Response>;
let lifecycle: (path: string) => Response | Promise<Response>;
// What the other write routes answer: PATCH, instance-admin, unlock and DELETE.
let write: (req: Request, body: unknown) => Response | Promise<Response>;
let seen: string[];
// The JSON bodies the page sent, by "METHOD path".
let sent: Record<string, unknown>;

beforeEach(() => {
  // Not the user being looked at, unless a test says so.
  current = makeMe({ instanceAdmin: true, id: "me" });
  user = userBody();
  sent = {};
  write = () => Response.json(user);
  getUser = () => Response.json(user);
  lifecycle = (path) => {
    user = { ...user, status: path.endsWith("/disable") ? "disabled" : "active" };
    return Response.json(user);
  };
  seen = [];
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      seen.push(`${req.method} ${path}`);
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(current);
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.1.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      }
      if (path === "/api/users" && req.method === "GET") return Response.json([user]);
      if (path === "/api/users/u1" && req.method === "GET") return getUser();
      if (/^\/api\/users\/u1\/(disable|enable)$/.test(path)) return lifecycle(path);
      if (/^\/api\/users\/u1(\/(instance-admin|unlock))?$/.test(path)) {
        const body =
          req.method === "GET"
            ? undefined
            : await req
                .clone()
                .json()
                .catch(() => undefined);
        sent[`${req.method} ${path}`] = body;
        return write(req, body);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
  document.title = "";
});

function renderAt(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

const PAGE = "/settings/access/users/u1";

describe("the user page", () => {
  it("shows the breadcrumb, the username as the title and the status", async () => {
    renderAt(PAGE);
    const nav = await screen.findByRole("navigation", { name: "Breadcrumb" });
    expect(
      within(nav)
        .getAllByRole("listitem")
        .map((li) => li.textContent),
    ).toEqual(["Settings", "Access", "Users", "alice"]);
    expect(within(nav).getByRole("link", { name: "Users" }).getAttribute("href")).toBe(
      "/settings/access/users",
    );
    expect(screen.getByRole("heading", { level: 2, name: "alice" })).toBeTruthy();
    expect(screen.getByText("Active")).toBeTruthy();
    expect(document.title).toBe("alice · Lab");
  });

  it("titles the page with the display name and puts the username beneath it, next to the badges", async () => {
    user = userBody({ displayName: "Alice Liddell", instanceAdmin: true, status: "disabled" });
    renderAt(PAGE);
    expect(await screen.findByRole("heading", { level: 2, name: "Alice Liddell" })).toBeTruthy();
    // The username is beneath the title as well as in the breadcrumb.
    expect(screen.getAllByText("alice")).toHaveLength(2);
    expect(screen.getByText("Instance admin")).toBeTruthy();
    expect(screen.getByText("Disabled")).toBeTruthy();
  });

  it("shows a skeleton in the final layout while the user loads", async () => {
    getUser = () => new Promise<Response>(() => undefined);
    renderAt(PAGE);
    const busy = await screen.findByText("Loading user");
    expect((busy.parentElement as HTMLElement).getAttribute("aria-busy")).toBe("true");
  });

  it("shows the list error with Retry when the load fails, and loads on Retry", async () => {
    getUser = () => Response.json({ status: 500, title: "x", detail: "x" }, { status: 500 });
    renderAt(PAGE);
    // One automatic retry a second later comes first.
    expect(await screen.findByText("Couldn't load users.", {}, { timeout: 3000 })).toBeTruthy();
    getUser = () => Response.json(user);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { level: 2, name: "alice" })).toBeTruthy();
  });

  it("shows the Page not found page for an unknown or deleted id", async () => {
    getUser = () =>
      Response.json(
        { status: 404, type: "user_not_found", title: "Not Found", detail: "x" },
        { status: 404 },
      );
    renderAt(PAGE);
    expect(await screen.findByText("Page not found")).toBeTruthy();
    expect(seen.filter((s) => s === "GET /api/users/u1")).toHaveLength(1); // a 404 is not retried
  });

  it("is refused for someone who does not hold users.manage", async () => {
    current = makeMe({ envs: { e1: "admin" } });
    renderAt(PAGE);
    await waitFor(() => expect(screen.queryByRole("heading", { name: "alice" })).toBeNull());
    expect(seen).not.toContain("GET /api/users/u1");
  });
});

describe("Disable user and Enable user", () => {
  it("asks first, with focus on Cancel, then disables, says so and shows the Disabled badge", async () => {
    renderAt(PAGE);
    fireEvent.click(await screen.findByRole("button", { name: "Disable user" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Disable alice?" });
    expect(
      within(confirm).getByText(
        "alice is signed out everywhere and can't sign in until you enable them again. Their access and team memberships are kept.",
      ),
    ).toBeTruthy();
    expect(document.activeElement).toBe(within(confirm).getByRole("button", { name: "Cancel" }));
    expect(seen).not.toContain("POST /api/users/u1/disable");

    fireEvent.click(within(confirm).getByRole("button", { name: "Disable user" }));
    expect(await screen.findByText("alice is disabled.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(seen.filter((s) => s === "POST /api/users/u1/disable")).toHaveLength(1);
    // The button now offers the opposite and the badge shows the new state.
    expect(await screen.findByRole("button", { name: "Enable user" })).toBeTruthy();
    expect(screen.getByText("Disabled")).toBeTruthy();
  });

  it("enables with one press, toasts and shows Active", async () => {
    user = userBody({ status: "disabled" });
    renderAt(PAGE);
    fireEvent.click(await screen.findByRole("button", { name: "Enable user" }));
    expect(await screen.findByText("alice is enabled.")).toBeTruthy();
    expect(await screen.findByRole("button", { name: "Disable user" })).toBeTruthy();
    expect(screen.getByText("Active")).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("keeps the dialog open with the reason when the server refuses the last admin", async () => {
    lifecycle = () =>
      Response.json(
        { status: 409, type: "last_instance_admin", title: "Conflict", detail: "x" },
        { status: 409 },
      );
    renderAt(PAGE);
    fireEvent.click(await screen.findByRole("button", { name: "Disable user" }));
    const confirm = await screen.findByRole("alertdialog");
    fireEvent.click(within(confirm).getByRole("button", { name: "Disable user" }));
    const alert = await within(confirm).findByRole("alert");
    expect(alert.textContent).toBe(
      "Bayplan needs at least one active instance admin. Make someone else an instance admin first.",
    );
    // The page reads the user again.
    await waitFor(() =>
      expect(seen.filter((s) => s === "GET /api/users/u1").length).toBeGreaterThan(1),
    );
  });
});

// Profile, Instance administrator and Account (plan 03-14 Task 3).

const conflict = (type: string) =>
  Response.json({ status: 409, type, title: "Conflict", detail: "x" }, { status: 409 });

describe("the Profile card", () => {
  it("shows the username read-only with the reason and the editable fields", async () => {
    user = userBody({ displayName: "Alice L", email: "a@example.test" });
    renderAt(PAGE);
    const card = (await screen.findByRole("heading", { level: 3, name: "Profile" })).closest(
      "section",
    ) as HTMLElement;
    expect(within(card).getByText("Usernames can't be changed.")).toBeTruthy();
    expect(within(card).queryByRole("textbox", { name: "Username" })).toBeNull();
    expect(within(card).getByText("alice")).toBeTruthy();
    expect((within(card).getByLabelText("Display name") as HTMLInputElement).value).toBe("Alice L");
    expect((within(card).getByLabelText("Email") as HTMLInputElement).value).toBe("a@example.test");
  });

  it("keeps Save changes off until something changes, then sends the version it read and says Profile saved", async () => {
    write = (_req, body) => {
      user = { ...user, ...(body as object), updatedAt: "2026-10-08T10:00:00.5Z" };
      return Response.json(user);
    };
    renderAt(PAGE);
    const save = await screen.findByRole("button", { name: "Save changes" });
    expect((save as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "  Alice L " } });
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "alice@example.test" } });
    expect((save as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(save);

    expect(await screen.findByText("Profile saved.")).toBeTruthy();
    expect(sent["PATCH /api/users/u1"]).toEqual({
      displayName: "Alice L",
      email: "alice@example.test",
      updatedAt: "2026-10-07T10:00:00.123456789Z",
    });
    // The title now shows the display name and Save is off again.
    expect(await screen.findByRole("heading", { level: 2, name: "Alice L" })).toBeTruthy();
    await waitFor(() => expect((save as HTMLButtonElement).disabled).toBe(true));
  });

  it("shows the stale-change alert when the server says the user changed meanwhile", async () => {
    write = () => conflict("stale_change");
    renderAt(PAGE);
    fireEvent.change(await screen.findByLabelText("Display name"), { target: { value: "Other" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "This changed while you were looking at it. Reload the page and try again.",
    );
  });

  it("shows the field message under the email when the server refuses it", async () => {
    write = () =>
      Response.json(
        {
          status: 422,
          type: "validation_failed",
          title: "Unprocessable Entity",
          detail: "validation failed",
          errors: [{ location: "body.email", message: "invalid_email" }],
        },
        { status: 422 },
      );
    renderAt(PAGE);
    fireEvent.change(await screen.findByLabelText("Email"), { target: { value: "nope" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("Enter a valid email address.")).toBeTruthy();
  });
});

describe("the Instance administrator card", () => {
  const adminSwitch = () => screen.findByRole("switch", { name: "Instance administrator" });

  it("asks before making someone an instance admin, then does it", async () => {
    write = (_req, body) => {
      user = { ...user, instanceAdmin: (body as { enabled: boolean }).enabled };
      return Response.json(user);
    };
    renderAt(PAGE);
    expect(
      await screen.findByText(
        "Instance admins manage users, teams, settings, backups and every environment, with every extra permission. This can't be given through a team.",
      ),
    ).toBeTruthy();
    fireEvent.click(await adminSwitch());
    const confirm = await screen.findByRole("alertdialog", {
      name: "Make alice an instance admin?",
    });
    expect(
      within(confirm).getByText(
        "They'll be able to manage users and teams, change settings, take backups and use every environment, including container shells and secrets.",
      ),
    ).toBeTruthy();
    expect(document.activeElement).toBe(within(confirm).getByRole("button", { name: "Cancel" }));
    expect(sent["POST /api/users/u1/instance-admin"]).toBeUndefined();

    fireEvent.click(within(confirm).getByRole("button", { name: "Make instance admin" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(sent["POST /api/users/u1/instance-admin"]).toEqual({ enabled: true });
    expect(await screen.findByText("Instance admin")).toBeTruthy();
    await waitFor(() =>
      expect(screen.getByRole("switch").getAttribute("aria-checked")).toBe("true"),
    );
  });

  it("asks before removing instance admin and says they keep their other access", async () => {
    user = userBody({ instanceAdmin: true });
    renderAt(PAGE);
    fireEvent.click(await adminSwitch());
    const confirm = await screen.findByRole("alertdialog", {
      name: "Remove instance admin from alice?",
    });
    expect(
      within(confirm).getByText("They keep any access they have through roles and teams."),
    ).toBeTruthy();
    fireEvent.click(within(confirm).getByRole("button", { name: "Remove instance admin" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(sent["POST /api/users/u1/instance-admin"]).toEqual({ enabled: false });
  });

  it("does nothing when the dialog is cancelled", async () => {
    renderAt(PAGE);
    fireEvent.click(await adminSwitch());
    const confirm = await screen.findByRole("alertdialog");
    fireEvent.click(within(confirm).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(sent["POST /api/users/u1/instance-admin"]).toBeUndefined();
    expect(screen.getByRole("switch").getAttribute("aria-checked")).toBe("false");
  });

  it("guards the last active instance admin: the switch, Disable user and Delete user are aria-disabled with the reason", async () => {
    user = userBody({ instanceAdmin: true, lastInstanceAdmin: true });
    renderAt(PAGE);
    const reason = "Bayplan needs at least one active instance admin";
    const sw = await adminSwitch();
    const disable = screen.getByRole("button", { name: "Disable user" });
    const del = screen.getByRole("button", { name: "Delete user…" });
    for (const control of [sw, disable, del]) {
      expect(control.getAttribute("aria-disabled")).toBe("true");
      expect(control.hasAttribute("disabled")).toBe(false);
      const described = document.getElementById(control.getAttribute("aria-describedby") ?? "");
      expect(described?.textContent).toBe(reason);
    }
    fireEvent.click(sw);
    fireEvent.click(disable);
    fireEvent.click(del);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(seen.some((s) => s.startsWith("POST") || s.startsWith("DELETE"))).toBe(false);
  });

  it("shows the server's refusal inline and reads the user again when the guard was out of date", async () => {
    user = userBody({ instanceAdmin: true });
    write = () => conflict("last_instance_admin");
    renderAt(PAGE);
    fireEvent.click(await adminSwitch());
    fireEvent.click(await screen.findByRole("button", { name: "Remove instance admin" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "Bayplan needs at least one active instance admin. Make someone else an instance admin first.",
    );
    await waitFor(() =>
      expect(seen.filter((s) => s === "GET /api/users/u1").length).toBeGreaterThan(1),
    );
  });
});

describe("the Account card", () => {
  it("offers Unlock only while the account is locked, with when the lock ends, and says they can sign in", async () => {
    user = userBody({ status: "locked", lockedUntil: "2026-10-08T12:30:00Z" });
    write = () => {
      user = userBody();
      return Response.json(user);
    };
    renderAt(PAGE);
    expect(await screen.findByText(/^Locked until /)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
    expect(await screen.findByText("alice can sign in again.")).toBeTruthy();
    expect(seen).toContain("POST /api/users/u1/unlock");
    await waitFor(() => expect(screen.queryByRole("button", { name: "Unlock" })).toBeNull());
    expect(screen.queryByText(/^Locked until /)).toBeNull();
  });

  it("has no Unlock for an account that is not locked", async () => {
    renderAt(PAGE);
    await screen.findByRole("button", { name: "Disable user" });
    expect(screen.queryByRole("button", { name: "Unlock" })).toBeNull();
  });
});

describe("Delete user", () => {
  const openDelete = async () => {
    fireEvent.click(await screen.findByRole("button", { name: "Delete user…" }));
    return screen.findByRole("alertdialog", { name: "Delete alice?" });
  };

  it("explains what goes, starts on Cancel, and keeps the button off until the username is typed", async () => {
    renderAt(PAGE);
    const dialog = await openDelete();
    expect(
      within(dialog).getByText(
        "This can't be undone. alice's access, team memberships, sessions and pending links are removed. The username stays reserved, so the audit log still reads “alice (deleted)”.",
      ),
    ).toBeTruthy();
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
    const confirm = within(dialog).getByRole("button", { name: "Delete user" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(seen).not.toContain("DELETE /api/users/u1");

    const input = within(dialog).getByLabelText("Type alice to confirm");
    fireEvent.change(input, { target: { value: "Alice" } });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.change(input, { target: { value: "alice " } });
    expect(confirm.getAttribute("aria-disabled")).toBe("false");
  });

  it("deletes, says so and goes back to the users list", async () => {
    write = () => new Response(null, { status: 204 });
    renderAt(PAGE);
    const dialog = await openDelete();
    fireEvent.change(within(dialog).getByLabelText("Type alice to confirm"), {
      target: { value: "alice" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete user" }));
    expect(await screen.findByText("alice was deleted.")).toBeTruthy();
    expect(seen).toContain("DELETE /api/users/u1");
    // The page is gone; the list it came from is shown.
    expect(await screen.findByRole("table", { name: "Users" })).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("warns when the person is deleting their own account", async () => {
    current = makeMe({ instanceAdmin: true, id: "u1", username: "alice" });
    renderAt(PAGE);
    const dialog = await openDelete();
    expect(
      within(dialog).getByText("You're deleting your own account. You'll be signed out."),
    ).toBeTruthy();
  });

  it("does not warn about the account of someone else", async () => {
    renderAt(PAGE);
    const dialog = await openDelete();
    expect(within(dialog).queryByText(/your own account/)).toBeNull();
  });

  it("shows the server's refusal inside the dialog", async () => {
    write = () => conflict("last_instance_admin");
    renderAt(PAGE);
    const dialog = await openDelete();
    fireEvent.change(within(dialog).getByLabelText("Type alice to confirm"), {
      target: { value: "alice" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete user" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe(
      "Bayplan needs at least one active instance admin. Make someone else an instance admin first.",
    );
  });

  it("wraps a long title instead of running off the dialog", async () => {
    user = userBody({ username: "a".repeat(32) });
    renderAt(PAGE);
    fireEvent.click(await screen.findByRole("button", { name: "Delete user…" }));
    const title = await screen.findByRole("heading", { name: `Delete ${"a".repeat(32)}?` });
    expect(title.className).toContain("break-words");
  });
});
