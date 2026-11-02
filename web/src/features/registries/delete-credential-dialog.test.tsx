// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { DeleteCredentialDialog } from "@/features/registries/delete-credential-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

let client: QueryClient;
let deletes: string[];
let answer: () => Response | Promise<Response>;
let closed: number;
let done: number;

beforeEach(() => {
  deletes = [];
  closed = 0;
  done = 0;
  answer = () => new Response(null, { status: 204 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(makeMe({ instanceAdmin: true }));
      if (
        (url.pathname === "/api/registries/c1" || url.pathname === "/api/env/e1/registries/c1") &&
        req.method === "DELETE"
      ) {
        deletes.push(url.pathname);
        return answer();
      }
      return Response.json({ items: [] });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(name = "ghcr-acme", envId: string | null = null) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <DeleteCredentialDialog
          credential={{ id: "c1", name, scope: { envId } }}
          open
          onClose={() => {
            closed += 1;
          }}
          onDone={() => {
            done += 1;
          }}
        />
        <Toaster />
      </TestRouter>
    </QueryClientProvider>,
  );
}

const confirm = () => fireEvent.click(screen.getByRole("button", { name: "Delete credential" }));

describe("DeleteCredentialDialog", () => {
  it("deletes through the route of the credential's own layer", async () => {
    mount("ghcr-acme", "e1");
    await screen.findByText("Delete “ghcr-acme”?");
    confirm();
    await waitFor(() => expect(deletes).toEqual(["/api/env/e1/registries/c1"]));
    await waitFor(() => expect(done).toBe(1));
  });

  it("asks Delete “{name}”? and says what happens to pulls, with focus on Cancel", async () => {
    mount();
    expect(await screen.findByText("Delete “ghcr-acme”?")).toBeTruthy();
    expect(
      screen.getByText(
        "Pulls this credential would have matched fall back to the next matching credential, or to anonymous. Nothing else refers to it.",
      ),
    ).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Cancel" }));
    expect(deletes).toEqual([]);
  });

  it("deletes on confirm, toasts, and closes", async () => {
    mount();
    await screen.findByText("Delete “ghcr-acme”?");
    confirm();
    await waitFor(() => expect(closed).toBe(1));
    expect(deletes).toEqual(["/api/registries/c1"]);
    expect(done).toBe(1);
    expect(await screen.findByText("Deleted credential ghcr-acme.")).toBeTruthy();
  });

  it("shows a pending spinner and a single request while the delete is out", async () => {
    let release: (r: Response) => void = () => {};
    answer = () =>
      new Promise<Response>((resolve) => {
        release = resolve;
      });
    mount();
    await screen.findByText("Delete “ghcr-acme”?");
    confirm();
    const pending = await screen.findByRole("button", { name: "Deleting…" });
    fireEvent.click(pending);
    expect(screen.getByRole("button", { name: "Cancel" }).hasAttribute("disabled")).toBe(true);
    release(new Response(null, { status: 204 }));
    await waitFor(() => expect(closed).toBe(1));
    expect(deletes).toHaveLength(1);
  });

  it("says a credential that is already gone is gone, and closes", async () => {
    answer = () =>
      Response.json(
        { title: "Not Found", status: 404, type: "credential_not_found" },
        { status: 404 },
      );
    mount();
    await screen.findByText("Delete “ghcr-acme”?");
    confirm();
    await waitFor(() => expect(closed).toBe(1));
    expect(await screen.findByText("ghcr-acme is already gone.")).toBeTruthy();
  });

  it("keeps the dialog open with the reason when the permission is lost", async () => {
    answer = () => Response.json({ title: "Forbidden", status: 403 }, { status: 403 });
    mount();
    await screen.findByText("Delete “ghcr-acme”?");
    confirm();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("You no longer have permission to do that.");
    expect(closed).toBe(0);
  });

  it("keeps the dialog open with a general message on any other failure, and does not retry", async () => {
    answer = () => Response.json({ title: "boom", status: 500 }, { status: 500 });
    mount();
    await screen.findByText("Delete “ghcr-acme”?");
    confirm();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Couldn't delete the credential. Try again.");
    expect(closed).toBe(0);
    expect(deletes).toHaveLength(1);
  });

  it("draws a name that holds markup as text", async () => {
    mount("<img src=x onerror=alert(1)>");
    expect(await screen.findByText("Delete “<img src=x onerror=alert(1)>”?")).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
  });
});
