// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { type BulkReport, BulkResultDialog } from "@/features/containers/bulk-result-dialog";
import { type PendingApi, PendingContext } from "@/features/containers/pending";
import {
  DanglingDeleteDialog,
  type DanglingTarget,
} from "@/features/images/dangling-delete-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// Deleting dangling images in one confirm (plan 05-17, RES-09, D-17).

const ENV = "e1";
const ID = (n: number) => `sha256:${n.toString(16).padStart(64, "0")}`;

interface Sent {
  method: string;
  path: string;
  body?: { bulkId?: string; targets?: { nodeId: string; imageId: string }[] };
}

let sent: Sent[];
let answer: (req: Sent, call: number) => Response | Promise<Response>;
let client: QueryClient;
let onClose: Mock<() => void>;
let onDone: Mock<() => void>;
let marked: string[];
let released: string[];
let report: BulkReport | null;

const doneAll = (req: Sent) =>
  Response.json({
    bulkId: "0198f8d0-4c2a-7000-8000-000000000001",
    results: (req.body?.targets ?? []).map((t) => ({ ...t, result: "done" })),
  });

beforeEach(() => {
  sent = [];
  answer = doneAll;
  marked = [];
  released = [];
  report = null;
  onClose = vi.fn<() => void>();
  onDone = vi.fn<() => void>();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const entry: Sent = {
        method: req.method,
        path: decodeURIComponent(new URL(req.url).pathname),
        body: req.method === "POST" ? await req.clone().json() : undefined,
      };
      sent.push(entry);
      if (entry.path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (entry.path === `/api/env/${ENV}/images/remove`) {
        return answer(entry, sent.filter((s) => s.path.endsWith("/images/remove")).length);
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

const removes = () => sent.filter((s) => s.path.endsWith("/images/remove"));

const rowsOf = (n: number, nodes = 1): DanglingTarget[] =>
  Array.from({ length: n }, (_, i) => ({
    rowKey: `|${ID(i + 1)}`,
    imageId: ID(i + 1),
    shortId: ID(i + 1).slice(7, 19),
    fingerprint: "fp",
    nodes: Array.from({ length: nodes }, (_, k) => ({
      nodeId: `n${k + 1}`,
      name: `node-${k + 1}`,
    })),
  }));

const pendingApi: PendingApi = {
  mark: (rows) => {
    marked.push(...rows.map((r) => r.key));
  },
  release: (keys) => {
    released.push(...keys);
  },
};

function Harness({ rows, swarm }: { rows: DanglingTarget[]; swarm: boolean }) {
  const [rep, setRep] = useState<BulkReport | null>(null);
  report = rep;
  return (
    <>
      <DanglingDeleteDialog
        envId={ENV}
        rows={rows}
        swarm={swarm}
        open
        onClose={onClose}
        onDone={onDone}
        onDetails={setRep}
      />
      <BulkResultDialog envId={ENV} report={rep} onClose={() => setRep(null)} />
    </>
  );
}

function mount(rows: DanglingTarget[], swarm = false) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <PendingContext.Provider value={pendingApi}>
            <Harness rows={rows} swarm={swarm} />
          </PendingContext.Provider>
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("DanglingDeleteDialog", () => {
  it("opens Delete {n} images? with the target list and no field to type in", async () => {
    mount(rowsOf(3));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Delete 3 images?")).toBeTruthy();
    expect(
      within(dialog).getByText(
        "Dangling images have no name. Ones you built here can't be pulled again.",
      ),
    ).toBeTruthy();
    expect(within(dialog).queryByRole("textbox")).toBeNull();
    const head = within(dialog)
      .getAllByRole("columnheader")
      .map((h) => h.textContent);
    expect(head).toEqual(["Image", "Note"]);
    expect(within(dialog).getByRole("button", { name: "Delete 3 images" })).toBeTruthy();
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
  });

  it("adds a Node column on a swarm and lists one line per image copy", async () => {
    mount(rowsOf(2, 2), true);
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Delete 4 images?")).toBeTruthy();
    expect(
      within(dialog)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Image", "Node", "Note"]);
    expect(within(dialog).getAllByText("node-2")).toHaveLength(2);
  });

  it("sends the targets, marks the rows Removing…, toasts Deleted {n} images. and closes", async () => {
    mount(rowsOf(3));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 3 images" }));
    await waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    expect(removes()).toHaveLength(1);
    expect(removes()[0]?.body?.targets).toEqual(
      rowsOf(3).map((r) => ({ nodeId: "n1", imageId: r.imageId })),
    );
    expect(marked).toHaveLength(3);
    expect(released).toEqual([]);
    expect(await screen.findByText("Deleted 3 images.")).toBeTruthy();
    expect(onClose).toHaveBeenCalled();
  });

  it("sends 60 targets in chunks of 25, 25 and 10 with one bulk ID", async () => {
    mount(rowsOf(60));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 60 images" }));
    await waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    expect(removes().map((r) => r.body?.targets?.length)).toEqual([25, 25, 10]);
    expect(removes()[0]?.body?.bulkId).toBeUndefined();
    expect(removes()[1]?.body?.bulkId).toBe("0198f8d0-4c2a-7000-8000-000000000001");
    expect(removes()[2]?.body?.bulkId).toBe("0198f8d0-4c2a-7000-8000-000000000001");
    expect(await screen.findByText("Deleted 60 images.")).toBeTruthy();
  });

  it("sums a partial result up as Removed 59, 1 failed and Details open the results", async () => {
    answer = (req, call) =>
      Response.json({
        bulkId: "0198f8d0-4c2a-7000-8000-000000000001",
        results: (req.body?.targets ?? []).map((t, i) =>
          call === 1 && i === 3
            ? { ...t, result: "failed", code: "node_unreachable" }
            : { ...t, result: "done" },
        ),
      });
    mount(rowsOf(60));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 60 images" }));
    expect(await screen.findByText("Removed 59, 1 failed")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Details" }));
    const results = await screen.findByRole("dialog", { name: "Delete results" });
    expect(
      within(results)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Image", "Node", "Result", "Reason"]);
    expect(within(results).getAllByRole("row")).toHaveLength(61);
    expect(within(results).getByText("Failed")).toBeTruthy();
    expect(report?.bulkId).toBe("0198f8d0-4c2a-7000-8000-000000000001");
    // The row nothing was removed from loses its Removing… label.
    expect(released).toEqual([rowsOf(4)[3]?.rowKey]);
  });

  it("counts a copy that is in use as skipped, muted, and never sends it", async () => {
    const rows = rowsOf(2);
    rows[1] = {
      ...(rows[1] as DanglingTarget),
      nodes: [{ nodeId: "n1", name: "node-1", inUse: true }],
    };
    mount(rows);
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Delete 1 image?")).toBeTruthy();
    expect(within(dialog).getByText("In use, skipped")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 1 image" }));
    expect(await screen.findByText("Deleted 1, 1 skipped")).toBeTruthy();
    expect(removes()[0]?.body?.targets).toHaveLength(1);
  });

  it("reads a target the server refused as in use or named as skipped, not failed", async () => {
    answer = (req) =>
      Response.json({
        results: (req.body?.targets ?? []).map((t, i) =>
          i === 0
            ? { ...t, result: "failed", code: "image_in_use" }
            : i === 1
              ? { ...t, result: "skipped", code: "not_dangling" }
              : { ...t, result: "done" },
        ),
      });
    mount(rowsOf(3));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 3 images" }));
    expect(await screen.findByText("Deleted 1, 2 skipped")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Details" }));
    const results = await screen.findByRole("dialog", { name: "Delete results" });
    expect(within(results).getAllByText("Skipped")).toHaveLength(2);
  });

  it("keeps the dialog open with an inline alert when the request is refused as a whole", async () => {
    answer = () => Response.json({ title: "Forbidden", status: 403 }, { status: 403 });
    mount(rowsOf(2));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 2 images" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("You no longer have permission");
    expect(onClose).not.toHaveBeenCalled();
    expect(released).toHaveLength(2);
  });
});
