// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { OwnedItem } from "@/api/owned";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { TransferOwnershipDialog } from "@/features/access/transfer-ownership-dialog";
import "@/i18n";

// The transfer dialog (plan 03-22, D-14, UI-SPEC "Owned automations").

const ITEM: OwnedItem = {
  kind: "git.sync",
  label: "Git sync",
  resourceId: "r1",
  name: "Sync r1",
  envId: "env-a",
  envName: "Alpha",
  state: "paused_disabled",
  needsEnvAdmin: false,
};

let eligible: Array<Record<string, unknown>>;
let eligibleStatus: number;
let transfer: (body: Record<string, unknown>) => Response | Promise<Response>;
let sent: Array<{ method: string; path: string; body?: Record<string, unknown> }>;
const defaultQueryOptions = queryClient.getDefaultOptions();
let onClose: Mock<() => void>;
let onDone: Mock<(item: OwnedItem) => void>;
let onStale: Mock<() => void>;

beforeEach(() => {
  // A failed read is retried after a second in the app; a test of the error state should not wait.
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  sent = [];
  eligible = [
    { id: "u2", username: "bob", displayName: "" },
    { id: "u3", username: "dana", displayName: "Dana D" },
  ];
  eligibleStatus = 200;
  transfer = () => Response.json({ ...ITEM, state: "active" });
  onClose = vi.fn<() => void>();
  onDone = vi.fn<(item: OwnedItem) => void>();
  onStale = vi.fn<() => void>();
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: (typeof sent)[number] = { method: req.method, path };
      if (req.method !== "GET") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/owned/git.sync/r1/eligible-owners") {
        return eligibleStatus === 200
          ? Response.json(eligible)
          : Response.json({ title: "boom" }, { status: eligibleStatus });
      }
      if (path === "/api/owned/git.sync/r1/transfer") return transfer(entry.body ?? {});
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  queryClient.setDefaultOptions(defaultQueryOptions);
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

function renderDialog(item: OwnedItem | null = ITEM) {
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TransferOwnershipDialog item={item} onClose={onClose} onDone={onDone} onStale={onStale} />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

async function choose(dialog: HTMLElement, name: string) {
  const select = await within(dialog).findByRole("combobox", { name: "New owner" });
  await waitFor(() => expect((select as HTMLButtonElement).disabled).toBe(false));
  fireEvent.keyDown(select, { key: "Enter" });
  fireEvent.keyDown(await screen.findByRole("option", { name }), { key: "Enter" });
}

const transferred = () => sent.filter((s) => s.method === "POST");

describe("TransferOwnershipDialog", () => {
  it("says what transferring does, lists the eligible users and keeps the confirm unavailable until one is chosen", async () => {
    renderDialog();
    const dialog = await screen.findByRole("dialog", { name: "Transfer Sync r1" });
    expect(
      within(dialog).getByText("Choose who owns it. It runs with their permissions from now on."),
    ).toBeTruthy();
    expect(
      await within(dialog).findByText("Only users with enough access are listed."),
    ).toBeTruthy();
    const confirm = within(dialog).getByRole("button", { name: "Transfer ownership" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(transferred()).toHaveLength(0);

    const select = await within(dialog).findByRole("combobox", { name: "New owner" });
    await waitFor(() => expect((select as HTMLButtonElement).disabled).toBe(false));
    fireEvent.keyDown(select, { key: "Enter" });
    expect(await screen.findByRole("option", { name: "bob" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Dana D (dana)" })).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("option", { name: "Dana D (dana)" }), { key: "Enter" });
    await waitFor(() => expect(confirm.getAttribute("aria-disabled")).toBe("false"));
  });

  it("transfers to the chosen user, says so and reports the automation as it now is", async () => {
    renderDialog();
    const dialog = await screen.findByRole("dialog", { name: "Transfer Sync r1" });
    await choose(dialog, "bob");
    fireEvent.click(within(dialog).getByRole("button", { name: "Transfer ownership" }));

    expect(await screen.findByText("Sync r1 now belongs to bob.")).toBeTruthy();
    expect(transferred()).toEqual([
      { method: "POST", path: "/api/owned/git.sync/r1/transfer", body: { ownerId: "u2" } },
    ]);
    await waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    expect(onDone.mock.calls[0]?.[0]).toMatchObject({ resourceId: "r1", state: "active" });
  });

  it("shows the no-eligible-user alert in place of the select and keeps the confirm unavailable", async () => {
    eligible = [];
    renderDialog();
    const dialog = await screen.findByRole("dialog", { name: "Transfer Sync r1" });
    expect(
      await within(dialog).findByText(
        "No user has enough access to take this over. Give someone Admin on Alpha first.",
      ),
    ).toBeTruthy();
    expect(within(dialog).queryByRole("combobox")).toBeNull();
    const confirm = within(dialog).getByRole("button", { name: "Transfer ownership" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(transferred()).toHaveLength(0);
  });

  it("keeps the refusal inside the dialog when the user lost the access meanwhile, and reads who is eligible again", async () => {
    transfer = () =>
      Response.json(
        { status: 422, type: "owner_not_eligible", title: "x", detail: "x" },
        { status: 422 },
      );
    renderDialog();
    const dialog = await screen.findByRole("dialog", { name: "Transfer Sync r1" });
    await choose(dialog, "bob");
    const listReads = () => sent.filter((s) => s.method === "GET").length;
    const before = listReads();
    fireEvent.click(within(dialog).getByRole("button", { name: "Transfer ownership" }));

    expect(
      await within(dialog).findByText(
        "That user no longer has enough access to run this. Choose someone else.",
      ),
    ).toBeTruthy();
    expect(onDone).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
    await waitFor(() => expect(listReads()).toBeGreaterThan(before));
    expect(
      within(dialog)
        .getByRole("button", { name: "Transfer ownership" })
        .getAttribute("aria-disabled"),
    ).toBe("true");
  });

  it("says the automation is gone and tells the page when the server no longer knows it", async () => {
    transfer = () =>
      Response.json(
        { status: 404, type: "automation_not_found", title: "x", detail: "x" },
        { status: 404 },
      );
    renderDialog();
    const dialog = await screen.findByRole("dialog", { name: "Transfer Sync r1" });
    await choose(dialog, "bob");
    fireEvent.click(within(dialog).getByRole("button", { name: "Transfer ownership" }));
    expect(await within(dialog).findByText("Sync r1 doesn't exist any more.")).toBeTruthy();
    expect(onStale).toHaveBeenCalled();
  });

  it("offers Retry when who could take it over cannot be loaded", async () => {
    eligibleStatus = 500;
    renderDialog();
    const dialog = await screen.findByRole("dialog", { name: "Transfer Sync r1" });
    expect(await within(dialog).findByText("Couldn't load who can take this over.")).toBeTruthy();
    eligibleStatus = 200;
    fireEvent.click(within(dialog).getByRole("button", { name: "Retry" }));
    expect(await within(dialog).findByRole("combobox", { name: "New owner" })).toBeTruthy();
  });

  it("closes on Cancel, makes no request, and is not rendered while closed", async () => {
    const { rerender } = renderDialog();
    const dialog = await screen.findByRole("dialog", { name: "Transfer Sync r1" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(transferred()).toHaveLength(0);
    rerender(
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <TransferOwnershipDialog
            item={null}
            onClose={onClose}
            onDone={onDone}
            onStale={onStale}
          />
        </TooltipProvider>
      </QueryClientProvider>,
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("wraps a long automation name in the title instead of running off the dialog", async () => {
    renderDialog({ ...ITEM, name: "N".repeat(150) });
    const title = await screen.findByRole("heading", { name: `Transfer ${"N".repeat(150)}` });
    expect(title.className).toContain("break-words");
  });
});
