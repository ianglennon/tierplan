// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { usersQuery } from "@/api/users";
import { Toaster } from "@/components/ui/sonner";
import { CreateUserDialog } from "@/features/access/create-user-dialog";
import "@/i18n";

const TOKEN = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCde";
const LINK = `/invite#token=${TOKEN}`;

interface Sent {
  path: string;
  method: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
let answer: (body: Record<string, unknown>) => Response | Promise<Response>;
let writeText: ReturnType<typeof vi.fn>;

function created(username: string): Response {
  return Response.json(
    {
      user: {
        id: "u9",
        username,
        displayName: "",
        email: "",
        instanceAdmin: false,
        status: "invite_pending",
        teams: [],
        teamCount: 0,
      },
      link: { url: LINK, expiresAt: new Date(Date.now() + 72 * 3_600_000).toISOString() },
    },
    { status: 201 },
  );
}

beforeEach(() => {
  sent = [];
  setCsrfToken("csrf");
  answer = (body) => created(String(body.username));
  writeText = vi.fn(async () => undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method };
      if (req.method === "POST") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/users" && req.method === "POST") return answer(entry.body ?? {});
      if (path === "/api/users") return Response.json([]);
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

function Harness({ onClosed }: { onClosed?: () => void }) {
  const [open, setOpen] = useState(true);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        reopen
      </button>
      <CreateUserDialog
        open={open}
        onOpenChange={(next) => {
          setOpen(next);
          if (!next) onClosed?.();
        }}
      />
    </>
  );
}

function renderDialog(onClosed?: () => void) {
  queryClient.setQueryData(usersQuery.queryKey, []);
  render(
    <QueryClientProvider client={queryClient}>
      <Harness onClosed={onClosed} />
      <Toaster />
    </QueryClientProvider>,
  );
}

const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement;
const submit = () => fireEvent.click(screen.getByRole("button", { name: "Create user" }));
const posts = () => sent.filter((s) => s.path === "/api/users" && s.method === "POST");

function fill(username: string, extra: { displayName?: string; email?: string } = {}) {
  fireEvent.change(field("Username"), { target: { value: username } });
  if (extra.displayName !== undefined) {
    fireEvent.change(field("Display name (optional)"), { target: { value: extra.displayName } });
  }
  if (extra.email !== undefined) {
    fireEvent.change(field("Email (optional)"), { target: { value: extra.email } });
  }
}

describe("CreateUserDialog", () => {
  it("shows the form with its helpers and, on submit, switches to the masked one-time link", async () => {
    renderDialog();
    expect(screen.getByRole("dialog", { name: "Create user" })).toBeTruthy();
    expect(
      screen.getByText(
        "They get a one-time link to choose their own password. Bayplan doesn't send email.",
      ),
    ).toBeTruthy();
    expect(
      screen.getByText(/3 to 32 characters: letters, numbers, dot, dash or underscore/),
    ).toBeTruthy();
    expect(screen.getByText("Nothing uses it yet. Bayplan sends no email.")).toBeTruthy();

    fill("Alice", { displayName: " Alice L ", email: "alice@example.test" });
    submit();

    expect(await screen.findByRole("dialog", { name: "alice is ready" })).toBeTruthy();
    expect(posts()).toHaveLength(1);
    expect(posts()[0]?.body).toEqual({
      username: "alice",
      displayName: "Alice L",
      email: "alice@example.test",
    });
    // The link is on screen only as bullets until Show link is pressed.
    const dialog = screen.getByRole("dialog");
    expect(dialog.textContent).not.toContain(TOKEN);
    expect(screen.getByText(/Works once\. Expires .* \(72 hours from now\)\./)).toBeTruthy();
    expect(screen.getByText(/Send this link to alice yourself/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Show link" }));
    expect(dialog.textContent).toContain(TOKEN);
    expect(screen.getByRole("button", { name: "Hide link" })).toBeTruthy();
  });

  it("copies the real, absolute link even while it is masked", async () => {
    renderDialog();
    fill("alice");
    submit();
    await screen.findByRole("dialog", { name: "alice is ready" });

    fireEvent.click(screen.getByRole("button", { name: "Copy link" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText).toHaveBeenCalledWith(`${window.location.origin}${LINK}`);
  });

  it("closes from the link step, refreshes the users list and forgets the link", async () => {
    const onClosed = vi.fn();
    renderDialog(onClosed);
    fill("alice");
    submit();
    await screen.findByRole("dialog", { name: "alice is ready" });
    // The list behind the dialog is marked stale, so the new user appears in it.
    expect(queryClient.getQueryState(usersQuery.queryKey)?.isInvalidated).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(onClosed).toHaveBeenCalledTimes(1);

    // Opening it again starts at an empty form: the link is gone.
    fireEvent.click(screen.getByText("reopen"));
    expect(await screen.findByRole("dialog", { name: "Create user" })).toBeTruthy();
    expect(field("Username").value).toBe("");
    expect(document.body.textContent).not.toContain(TOKEN);
  });
});
