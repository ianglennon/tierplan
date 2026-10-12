// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RemoveImageDialog } from "@/features/images/remove-image-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// Removing one image from its page on a single host (plan 05-16 task 1, RES-09, D-28).

const ENV = "e1";
const ID = `sha256:${"a".repeat(64)}`;

interface Sent {
  method: string;
  path: string;
  body?: { bulkId?: string; targets?: { nodeId: string; imageId: string }[] };
}

let sent: Sent[];
let answer: (req: Sent) => Response | Promise<Response>;
let client: QueryClient;
let onClose: Mock<() => void>;
let onDone: Mock<() => void>;

const done = (req: Sent) =>
  Response.json({ results: (req.body?.targets ?? []).map((t) => ({ ...t, result: "done" })) });
const failed =
  (code: string, detail = "") =>
  (req: Sent) =>
    Response.json({
      results: (req.body?.targets ?? []).map((t) => ({ ...t, result: "failed", code, detail })),
    });

beforeEach(() => {
  sent = [];
  answer = done;
  onClose = vi.fn<() => void>();
  onDone = vi.fn<() => void>();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const entry: Sent = {
        method: req.method,
        path: new URL(req.url).pathname,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      };
      sent.push(entry);
      if (entry.path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (entry.path === `/api/env/${ENV}/images/remove`) return answer(entry);
      if (entry.path === "/api/environments") return Response.json({ items: [] });
      return Response.json({}, { status: 404 });
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

const removes = () => sent.filter((s) => s.path.endsWith("/images/remove"));

function mount(name = "ghcr.io/acme/app:1") {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <RemoveImageDialog
            envId={ENV}
            imageId={ID}
            name={name}
            nodes={[{ nodeId: "n1", name: "node-1" }]}
            open
            onClose={onClose}
            onDone={onDone}
          />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const confirmButton = (dialog: HTMLElement) =>
  within(dialog).getByRole("button", { name: "Remove image" });

describe("RemoveImageDialog (one image, one host)", () => {
  it("opens Remove {ref}? with the plain body and no field to type in", async () => {
    mount();
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Remove ghcr.io/acme/app:1?")).toBeTruthy();
    expect(
      within(dialog).getByText(
        "The image is deleted from the node. A registry image can be pulled again. An image you built here can't.",
      ),
    ).toBeTruthy();
    expect(within(dialog).queryByRole("textbox")).toBeNull();
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(confirmButton(dialog).getAttribute("aria-disabled")).toBe("false");
  });

  it("removes the image on its node, then toasts Removed {ref}. and closes", async () => {
    mount();
    fireEvent.click(confirmButton(await screen.findByRole("alertdialog")));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(removes()).toHaveLength(1);
    expect(removes()[0]?.body?.targets).toEqual([{ nodeId: "n1", imageId: ID }]);
    expect(onClose).toHaveBeenCalled();
    expect(await screen.findByText("Removed ghcr.io/acme/app:1.")).toBeTruthy();
  });

  it("shows a spinner and Removing… while the request is out, and sends it once", async () => {
    let release: () => void = () => {};
    answer = (req) =>
      new Promise((resolve) => {
        release = () => resolve(done(req));
      });
    mount();
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(confirmButton(dialog));
    const pending = await within(dialog).findByRole("button", { name: /Removing…/ });
    fireEvent.click(pending);
    expect(pending.getAttribute("aria-disabled")).toBe("true");
    expect(
      (within(dialog).getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled,
    ).toBe(true);
    release();
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(removes()).toHaveLength(1);
  });

  it("says an image that is already gone is gone and closes", async () => {
    answer = failed("not_found");
    mount();
    fireEvent.click(confirmButton(await screen.findByRole("alertdialog")));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(await screen.findByText(/ghcr\.io\/acme\/app:1 is already gone\./)).toBeTruthy();
  });

  it("keeps a refusal inside the dialog in the UI's own words, never the code", async () => {
    answer = failed("image_in_use", "ghcr.io/acme/app:1 is still in use by container old");
    mount();
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(confirmButton(dialog));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe(
      "ghcr.io/acme/app:1 is in use. Remove what uses it, then try again.",
    );
    expect(onClose).not.toHaveBeenCalled();
    expect(dialog.textContent).not.toContain("image_in_use");
  });

  it("explains a read-only host", async () => {
    answer = failed("host_read_only");
    mount();
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(confirmButton(dialog));
    expect((await within(dialog).findByRole("alert")).textContent).toContain(
      "This host is read-only.",
    );
  });
});
