// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { SessionsDialog } from "@/features/account/sessions-dialog";
import "@/i18n";

const CHROME =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36";
const FIREFOX = "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0";

function session(over: Record<string, unknown> = {}) {
  return {
    id: "a".repeat(32),
    userAgent: CHROME,
    ip: "203.0.113.5",
    createdAt: "2026-10-01T10:00:00Z",
    lastSeenAt: new Date().toISOString(),
    current: false,
    ...over,
  };
}

const CURRENT = session({ id: "c".repeat(32), current: true, ip: "203.0.113.9" });
const OTHER = session({ id: "b".repeat(32), userAgent: FIREFOX, ip: "198.51.100.7" });

let sessions: Array<Record<string, unknown>>;
let listStatus: number;
let revoke: (id: string) => Response | Promise<Response>;
let revokeOthers: () => Response | Promise<Response>;
let calls: string[];
let csrfSeen: Array<string | null>;

beforeEach(() => {
  sessions = [CURRENT, OTHER];
  listStatus = 200;
  calls = [];
  csrfSeen = [];
  revoke = (id) => {
    sessions = sessions.filter((s) => s.id !== id);
    return new Response(null, { status: 204 });
  };
  revokeOthers = () => {
    const ended = sessions.filter((s) => !s.current).length;
    sessions = sessions.filter((s) => s.current);
    return Response.json({ ended });
  };
  setCsrfToken("csrf-1");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      calls.push(`${req.method} ${path}`);
      if (req.method !== "GET") csrfSeen.push(req.headers.get("X-CSRF-Token"));
      if (path === "/api/me/sessions" && req.method === "GET") {
        return listStatus === 200
          ? Response.json(sessions)
          : Response.json({ title: "boom" }, { status: listStatus });
      }
      if (path === "/api/me/sessions/revoke-others") return revokeOthers();
      const one = /^\/api\/me\/sessions\/([^/]+)$/.exec(path);
      if (one?.[1] && req.method === "DELETE") return revoke(one[1]);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

function renderDialog(onOpenChange = vi.fn()) {
  render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <SessionsDialog open onOpenChange={onOpenChange} />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return onOpenChange;
}

const dialog = () => screen.findByRole("dialog", { name: "Your sessions" });

describe("the Sessions dialog (D-23)", () => {
  it("lists each session with its device, address and last activity, and marks the current one", async () => {
    renderDialog();
    const d = await dialog();
    expect(within(d).getByText("Everywhere you're signed in to Bayplan.")).toBeTruthy();
    for (const h of ["Device", "IP address", "Last active"]) {
      expect(within(d).getByRole("columnheader", { name: h })).toBeTruthy();
    }
    await within(d).findByText("Chrome on Windows");
    expect(within(d).getByText("Firefox on Linux")).toBeTruthy();
    expect(within(d).getByText("203.0.113.9")).toBeTruthy();
    expect(within(d).getByText("198.51.100.7")).toBeTruthy();

    const rows = within(d).getAllByRole("row");
    const currentRow = rows.find((r) => within(r).queryByText("203.0.113.9"));
    expect(currentRow).toBeTruthy();
    expect(within(currentRow as HTMLElement).getByText("This session")).toBeTruthy();
    // The current session cannot be revoked from its own row (D-23).
    expect(within(currentRow as HTMLElement).queryByRole("button", { name: /Revoke/ })).toBeNull();
    const otherRow = rows.find((r) => within(r).queryByText("198.51.100.7"));
    expect(
      within(otherRow as HTMLElement).getByRole("button", { name: "Revoke, Firefox on Linux" }),
    ).toBeTruthy();
    expect(within(otherRow as HTMLElement).queryByText("This session")).toBeNull();
  });

  it("revokes one other session with the CSRF token, toasts, and reads the list again", async () => {
    renderDialog();
    const d = await dialog();
    await within(d).findByText("Firefox on Linux");

    fireEvent.click(within(d).getByRole("button", { name: "Revoke, Firefox on Linux" }));

    await waitFor(() => expect(within(d).queryByText("Firefox on Linux")).toBeNull());
    expect(calls).toContain(`DELETE /api/me/sessions/${OTHER.id}`);
    expect(csrfSeen).toEqual(["csrf-1"]);
    expect(await screen.findByText("Session revoked.")).toBeTruthy();
    // The list was read again after the revoke.
    expect(calls.filter((c) => c === "GET /api/me/sessions").length).toBeGreaterThanOrEqual(2);
    // With nothing else left it says so.
    expect(await within(d).findByText("You're only signed in here.")).toBeTruthy();
  });

  it("shows one row and the only-here line when the current session is the only one", async () => {
    sessions = [CURRENT];
    renderDialog();
    const d = await dialog();
    expect(await within(d).findByText("You're only signed in here.")).toBeTruthy();
    expect(within(d).queryByRole("button", { name: /^Revoke/ })).toBeNull();
    expect(within(d).getAllByRole("row")).toHaveLength(2); // the header and the one session
    // There is nobody to sign out.
    expect(
      (within(d).getByRole("button", { name: "Sign out everywhere else" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
  });

  it("shows Unknown device for an agent it cannot read and a dash for a missing address", async () => {
    sessions = [CURRENT, session({ id: "d".repeat(32), userAgent: "curl/8.5.0", ip: "" })];
    renderDialog();
    const d = await dialog();
    await within(d).findByText("Unknown device");
    const row = within(d)
      .getAllByRole("row")
      .find((r) => within(r).queryByText("Unknown device"));
    expect(within(row as HTMLElement).getByText("—")).toBeTruthy();
  });

  it("keeps the whole user agent in a tooltip and never widens the table with it", async () => {
    const long = `${CHROME} ${"x".repeat(400)}`;
    sessions = [
      { ...CURRENT, userAgent: FIREFOX },
      session({ id: "e".repeat(32), userAgent: long }),
    ];
    renderDialog();
    const d = await dialog();
    const cell = await within(d).findByText("Chrome on Windows");
    // The cell shows the two words, cut to the column; the agent itself is not in it.
    expect(cell.textContent).toBe("Chrome on Windows");
    expect(cell.className).toContain("truncate");
    await act(async () => {
      fireEvent.focus(cell);
    });
    const tips = await screen.findAllByText(`${CHROME} ${"x".repeat(400)}`);
    expect(tips.length).toBeGreaterThan(0);
  });

  it("confirms before signing out everywhere else, and then ends the other sessions", async () => {
    renderDialog();
    const d = await dialog();
    await within(d).findByText("Firefox on Linux");

    fireEvent.click(within(d).getByRole("button", { name: "Sign out everywhere else" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Sign out everywhere else?" });
    expect(
      within(confirm).getByText("Every other session ends. You stay signed in here."),
    ).toBeTruthy();
    // Nothing happens until the button is pressed.
    expect(calls.filter((c) => c.startsWith("POST"))).toEqual([]);
    expect(document.activeElement).toBe(within(confirm).getByRole("button", { name: "Cancel" }));

    fireEvent.click(within(confirm).getByRole("button", { name: "Sign out everywhere else" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(calls).toContain("POST /api/me/sessions/revoke-others");
    expect(await screen.findByText("Signed out everywhere else.")).toBeTruthy();
    await waitFor(() => expect(within(d).queryByText("Firefox on Linux")).toBeNull());
  });

  it("cancelling the confirm sends nothing", async () => {
    renderDialog();
    const d = await dialog();
    await within(d).findByText("Firefox on Linux");
    fireEvent.click(within(d).getByRole("button", { name: "Sign out everywhere else" }));
    const confirm = await screen.findByRole("alertdialog");
    fireEvent.click(within(confirm).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(calls.filter((c) => c.startsWith("POST"))).toEqual([]);
  });

  it("shows a failed revoke as an alert inside the dialog, changes nothing and reads the list again", async () => {
    revoke = () => Response.json({ title: "boom" }, { status: 500 });
    renderDialog();
    const d = await dialog();
    await within(d).findByText("Firefox on Linux");
    const before = calls.filter((c) => c === "GET /api/me/sessions").length;

    fireEvent.click(within(d).getByRole("button", { name: "Revoke, Firefox on Linux" }));

    const alert = await within(d).findByRole("alert");
    expect(alert.textContent).toContain("Couldn't revoke that session");
    expect(within(d).getByText("Firefox on Linux")).toBeTruthy();
    await waitFor(() =>
      expect(calls.filter((c) => c === "GET /api/me/sessions").length).toBeGreaterThan(before),
    );
  });

  it("shows a failed sign-out-everywhere-else inside its own confirm and keeps it open", async () => {
    revokeOthers = () => Response.json({ title: "boom" }, { status: 500 });
    renderDialog();
    const d = await dialog();
    await within(d).findByText("Firefox on Linux");
    fireEvent.click(within(d).getByRole("button", { name: "Sign out everywhere else" }));
    const confirm = await screen.findByRole("alertdialog");
    fireEvent.click(within(confirm).getByRole("button", { name: "Sign out everywhere else" }));
    const alert = await within(confirm).findByRole("alert");
    expect(alert.textContent).toContain("Couldn't sign out the other sessions");
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(within(d).getByText("Firefox on Linux")).toBeTruthy();
  });

  it("shows a retry when the list cannot be loaded", async () => {
    listStatus = 500;
    renderDialog();
    const d = await dialog();
    expect(await within(d).findByText("Couldn't load the sessions.")).toBeTruthy();
    listStatus = 200;
    fireEvent.click(within(d).getByRole("button", { name: "Retry" }));
    expect(await within(d).findByText("Firefox on Linux")).toBeTruthy();
  });

  it("closes from Close", async () => {
    const onOpenChange = renderDialog();
    const d = await dialog();
    await within(d).findByText("Firefox on Linux");
    fireEvent.click(within(d).getByRole("button", { name: "Close" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});
