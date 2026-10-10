// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { VolumeList } from "@/features/volumes/volume-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

interface Sent {
  method: string;
  path: string;
  body?: { confirm?: string; bulkId?: string; targets?: { nodeId: string; name: string }[] };
}

let sent: Sent[];
let answer: (req: Sent) => Response | Promise<Response>;
let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let volumes: unknown[];

const volume = (name: string, extra: Record<string, unknown> = {}) => ({
  name,
  driver: "local",
  node: "node-1",
  nodeId: "node-1-id",
  created: "2026-10-01T10:00:00Z",
  usedByContainers: 0,
  usedByServices: 0,
  anonymous: false,
  project: "",
  stack: "",
  ...extra,
});

const done = (req: Sent) =>
  Response.json({
    results: (req.body?.targets ?? []).map((t) => ({ ...t, result: "done" })),
  });
const failed =
  (code: string, detail = "") =>
  (req: Sent) =>
    Response.json({
      results: (req.body?.targets ?? []).map((t) => ({ ...t, result: "failed", code, detail })),
    });

beforeEach(() => {
  sent = [];
  me = makeMe({ envs: { [ENV]: "admin" } });
  answer = done;
  volumes = [volume("data1")];
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
      if (entry.path === "/api/me") return Response.json(me);
      if (entry.path === `/api/env/${ENV}/volumes/delete`) return answer(entry);
      if (entry.path === `/api/env/${ENV}/volumes`) {
        return Response.json({
          items: volumes,
          total: volumes.length,
          truncated: false,
          nodes: ["node-1"],
          notReporting: [],
          unfilteredTotal: volumes.length,
          usageCounts: { all: volumes.length, used: 0, unused: volumes.length },
        });
      }
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

const deletes = () => sent.filter((s) => s.path.endsWith("/volumes/delete"));

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <VolumeList envId={ENV} onNodeChange={() => {}} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function openDelete(name: string) {
  fireEvent.click(await screen.findByRole("button", { name: `Delete ${name}` }));
  return screen.findByRole("alertdialog");
}

const confirmButton = (dialog: HTMLElement) =>
  within(dialog).getByRole("button", { name: "Delete volume" });

describe("DeleteVolumesDialog (one volume)", () => {
  it("opens Delete {name}? with the name to type, and Delete volume waits for it", async () => {
    mount();
    const dialog = await openDelete("data1");
    expect(within(dialog).getByText("Delete data1?")).toBeTruthy();
    expect(
      within(dialog).getByText("This deletes the volume and the data in it. This can't be undone."),
    ).toBeTruthy();
    expect(confirmButton(dialog).getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirmButton(dialog));
    expect(deletes()).toHaveLength(0);

    const input = within(dialog).getByLabelText("Type data1 to confirm");
    fireEvent.change(input, { target: { value: "data" } });
    expect(confirmButton(dialog).getAttribute("aria-disabled")).toBe("true");
    fireEvent.change(input, { target: { value: "data1" } });
    expect(confirmButton(dialog).getAttribute("aria-disabled")).toBe("false");
  });

  it("starts with focus on Cancel", async () => {
    mount();
    const dialog = await openDelete("data1");
    await waitFor(() =>
      expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" })),
    );
  });

  it("sends the name typed and the node's ID once, then toasts and closes", async () => {
    mount();
    const dialog = await openDelete("data1");
    fireEvent.change(within(dialog).getByLabelText("Type data1 to confirm"), {
      target: { value: "data1" },
    });
    fireEvent.click(confirmButton(dialog));
    await waitFor(() => expect(deletes()).toHaveLength(1));
    expect(deletes()[0]?.body).toEqual({
      confirm: "data1",
      targets: [{ nodeId: "node-1-id", name: "data1" }],
    });
    expect(await screen.findByText("Deleted data1.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("keeps a volume that is still in use in the dialog, with the way forward", async () => {
    answer = failed("volume_in_use");
    mount();
    const dialog = await openDelete("data1");
    fireEvent.change(within(dialog).getByLabelText("Type data1 to confirm"), {
      target: { value: "data1" },
    });
    fireEvent.click(confirmButton(dialog));
    expect(
      await within(dialog).findByText(
        "data1 is still in use. Remove what uses it, then try again.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("alertdialog")).toBeTruthy();
  });

  it("says a volume that is already gone in a toast and closes", async () => {
    answer = failed("not_found");
    mount();
    const dialog = await openDelete("data1");
    fireEvent.change(within(dialog).getByLabelText("Type data1 to confirm"), {
      target: { value: "data1" },
    });
    fireEvent.click(confirmButton(dialog));
    expect(
      await screen.findByText("data1 no longer exists. The list has been refreshed."),
    ).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("shows a withdrawn permission inside the dialog", async () => {
    answer = () =>
      Response.json({ type: "forbidden", title: "x", status: 403, detail: "d" }, { status: 403 });
    mount();
    const dialog = await openDelete("data1");
    fireEvent.change(within(dialog).getByLabelText("Type data1 to confirm"), {
      target: { value: "data1" },
    });
    fireEvent.click(confirmButton(dialog));
    expect(
      await within(dialog).findByText(
        "You no longer have permission to do that. Your access may have changed.",
      ),
    ).toBeTruthy();
  });
});

describe("Delete… on a volume row", () => {
  it("is enabled for an Operator who holds the delete volumes permission", async () => {
    me = makeMe({ envs: { [ENV]: "operator" }, extras: { [ENV]: ["volumes.delete"] } });
    mount();
    const button = await screen.findByRole("button", { name: "Delete data1" });
    expect(button.getAttribute("aria-disabled")).toBeNull();
  });

  it("is disabled with Needs the delete volumes permission for an Operator without it", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    const button = await screen.findByRole("button", { name: "Delete data1" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(button.getAttribute("aria-describedby")).toBeTruthy();
    fireEvent.click(button);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(await screen.findByText("Needs the delete volumes permission")).toBeTruthy();
  });
});
