// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { ProfileDialog } from "@/features/account/profile-dialog";
import { meQueryOptions } from "@/hooks/use-me";
import "@/i18n";
import { makeMe } from "@/test-me";

let profile: Record<string, unknown>;
let getStatus: number;
let save: (body: Record<string, unknown>) => Response | Promise<Response>;
let calls: string[];
let patches: Array<Record<string, unknown>>;
let csrfSeen: Array<string | null>;

beforeEach(() => {
  profile = {
    username: "alice",
    displayName: "Alice",
    email: "alice@example.test",
    updatedAt: "2026-10-07T10:00:00.123456789Z",
  };
  getStatus = 200;
  calls = [];
  patches = [];
  csrfSeen = [];
  save = (body) => {
    profile = { ...profile, ...body, updatedAt: "2026-10-08T10:00:00.5Z" };
    return Response.json(profile);
  };
  setCsrfToken("csrf-1");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      calls.push(`${req.method} ${path}`);
      if (path === "/api/me/profile" && req.method === "GET") {
        return getStatus === 200
          ? Response.json(profile)
          : Response.json({ title: "boom" }, { status: getStatus });
      }
      if (path === "/api/me/profile" && req.method === "PATCH") {
        const body = (await req.clone().json()) as Record<string, unknown>;
        patches.push(body);
        csrfSeen.push(req.headers.get("X-CSRF-Token"));
        return save({ displayName: body.displayName, email: body.email });
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
});

function renderDialog(onOpenChange = vi.fn()) {
  queryClient.setQueryData(meQueryOptions.queryKey, makeMe({ id: "id-alice", username: "alice" }));
  render(
    <QueryClientProvider client={queryClient}>
      <ProfileDialog open onOpenChange={onOpenChange} />
      <Toaster />
    </QueryClientProvider>,
  );
  return onOpenChange;
}

const dialog = () => screen.findByRole("dialog", { name: "Profile" });
const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement;

describe("the Profile dialog (D-24)", () => {
  it("shows the username read-only with its reason, the editable fields filled in, and the note about the password", async () => {
    renderDialog();
    const d = await dialog();
    await waitFor(() => expect(field("Display name").value).toBe("Alice"));
    expect(field("Email").value).toBe("alice@example.test");
    // The username is text, not a field.
    expect(within(d).getByText("alice")).toBeTruthy();
    expect(within(d).queryByLabelText("Username")).toBeNull();
    expect(within(d).getByText("Usernames can't be changed.")).toBeTruthy();
    expect(
      within(d).getByText("To change your password, use Change password in the user menu."),
    ).toBeTruthy();
  });

  it("keeps Save changes off until something changes, then sends the version it read, toasts and closes", async () => {
    const onOpenChange = renderDialog();
    const d = await dialog();
    await waitFor(() => expect(field("Display name").value).toBe("Alice"));
    const saveButton = within(d).getByRole("button", { name: "Save changes" }) as HTMLButtonElement;
    expect(saveButton.disabled).toBe(true);

    fireEvent.change(field("Display name"), { target: { value: "  Alice L  " } });
    expect(saveButton.disabled).toBe(false);
    fireEvent.click(saveButton);

    expect(await screen.findByText("Profile saved.")).toBeTruthy();
    expect(patches).toEqual([
      {
        displayName: "Alice L",
        email: "alice@example.test",
        updatedAt: "2026-10-07T10:00:00.123456789Z",
      },
    ]);
    expect(csrfSeen).toEqual(["csrf-1"]);
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    // The caller's own record is marked out of date, so the name shown follows the save.
    await waitFor(() =>
      expect(queryClient.getQueryState(meQueryOptions.queryKey)?.isInvalidated).toBe(true),
    );
  });

  it("refuses an email that is not an address before asking the server", async () => {
    renderDialog();
    const d = await dialog();
    await waitFor(() => expect(field("Email").value).toBe("alice@example.test"));
    fireEvent.change(field("Email"), { target: { value: "not an email" } });
    fireEvent.click(within(d).getByRole("button", { name: "Save changes" }));
    expect(await within(d).findByText("Enter a valid email address.")).toBeTruthy();
    expect(patches).toEqual([]);
  });

  it("shows the stale-change alert inline when the profile changed meanwhile, and stays open", async () => {
    save = () =>
      Response.json({ type: "stale_change", title: "Conflict", status: 409 }, { status: 409 });
    const onOpenChange = renderDialog();
    const d = await dialog();
    await waitFor(() => expect(field("Display name").value).toBe("Alice"));
    fireEvent.change(field("Display name"), { target: { value: "Other" } });
    fireEvent.click(within(d).getByRole("button", { name: "Save changes" }));
    const alert = await within(d).findByRole("alert");
    expect(alert.textContent).toContain("This changed while you were looking at it.");
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("shows the server's field code under the email", async () => {
    save = () =>
      Response.json(
        {
          type: "validation_failed",
          title: "Unprocessable Entity",
          status: 422,
          errors: [{ message: "invalid_email", location: "body.email" }],
        },
        { status: 422 },
      );
    renderDialog();
    const d = await dialog();
    await waitFor(() => expect(field("Display name").value).toBe("Alice"));
    fireEvent.change(field("Display name"), { target: { value: "Other" } });
    fireEvent.click(within(d).getByRole("button", { name: "Save changes" }));
    expect(await within(d).findByText("Enter a valid email address.")).toBeTruthy();
  });

  it("says so inline when the save fails for any other reason", async () => {
    save = () => Response.json({ title: "boom" }, { status: 500 });
    renderDialog();
    const d = await dialog();
    await waitFor(() => expect(field("Display name").value).toBe("Alice"));
    fireEvent.change(field("Display name"), { target: { value: "Other" } });
    fireEvent.click(within(d).getByRole("button", { name: "Save changes" }));
    const alert = await within(d).findByRole("alert");
    expect(alert.textContent).toContain("Couldn't save the profile.");
  });

  it("shows a retry when the profile cannot be loaded", async () => {
    getStatus = 500;
    renderDialog();
    const d = await dialog();
    // The shared query client retries once after a one-second delay, so the
    // error appears just past findByText's default one-second timeout.
    expect(
      await within(d).findByText("Couldn't load your profile.", undefined, { timeout: 5000 }),
    ).toBeTruthy();
    getStatus = 200;
    fireEvent.click(within(d).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(field("Display name").value).toBe("Alice"));
  });

  it("closes from Cancel without saving", async () => {
    const onOpenChange = renderDialog();
    const d = await dialog();
    await waitFor(() => expect(field("Display name").value).toBe("Alice"));
    fireEvent.click(within(d).getByRole("button", { name: "Cancel" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(patches).toEqual([]);
  });

  // CR-C-01: the dialog stays mounted, so a cached pre-save profile used to survive
  // a close and the next save sent old values with a fresh version.
  it("reopens on the saved values and sends them with the matching version", async () => {
    let setOpen: (open: boolean) => void = () => undefined;
    function Harness() {
      const [open, set] = useState(true);
      setOpen = set;
      return <ProfileDialog open={open} onOpenChange={set} />;
    }
    queryClient.setQueryData(
      meQueryOptions.queryKey,
      makeMe({ id: "id-alice", username: "alice" }),
    );
    render(
      <QueryClientProvider client={queryClient}>
        <Harness />
        <Toaster />
      </QueryClientProvider>,
    );
    let d = await dialog();
    await waitFor(() => expect(field("Display name").value).toBe("Alice"));
    fireEvent.change(field("Display name"), { target: { value: "Alice L" } });
    fireEvent.click(within(d).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(patches).toHaveLength(1);

    setOpen(true);
    d = await dialog();
    // Never the pre-save copy, not even for a moment.
    expect(screen.queryByLabelText("Display name")).toBeNull();
    await waitFor(() => expect(field("Display name").value).toBe("Alice L"));

    fireEvent.change(field("Email"), { target: { value: "new@example.test" } });
    fireEvent.click(within(d).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(patches).toHaveLength(2));
    expect(patches[1]).toEqual({
      displayName: "Alice L",
      email: "new@example.test",
      updatedAt: "2026-10-08T10:00:00.5Z",
    });
  });
});
