// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UntagDialog, type UntagNode } from "@/features/images/untag-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// Untagging one name of an image (plan 05-16 task 2, RES-09, D-29).

const ENV = "e1";
const ID = `sha256:${"a".repeat(64)}`;
const REF = "ghcr.io/acme/app:1";

interface Sent {
  method: string;
  path: string;
  body?: { imageId?: string; ref?: string; nodeIds?: string[]; bulkId?: string };
}

let sent: Sent[];
let answer: (req: Sent) => Response | Promise<Response>;
let client: QueryClient;
let onClose: Mock<() => void>;
let onDone: Mock<() => void>;
let usedBy: unknown[];

const untagged =
  (removed = false) =>
  (req: Sent) =>
    Response.json({
      results: (req.body?.nodeIds ?? []).map((nodeId) => ({
        nodeId,
        imageId: ID,
        ref: REF,
        result: "done",
        removed,
      })),
    });
const failed = (code: string) => (req: Sent) =>
  Response.json({
    results: (req.body?.nodeIds ?? []).map((nodeId) => ({
      nodeId,
      imageId: ID,
      ref: REF,
      result: "failed",
      code,
      removed: false,
    })),
  });

beforeEach(() => {
  sent = [];
  answer = untagged();
  usedBy = [];
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
      if (entry.path === `/api/env/${ENV}/images/untag`) return answer(entry);
      if (entry.path === `/api/env/${ENV}/images/${ID}/used-by`) {
        const items = usedBy as { kind: string }[];
        return Response.json({
          items,
          usedByContainers: items.filter((i) => i.kind === "container").length,
          usedByServices: items.filter((i) => i.kind === "service").length,
          notReporting: [],
        });
      }
      if (entry.path === `/api/env/${ENV}/images/pull-targets`) {
        return Response.json({
          nodes: ["n1", "n2"].map((nodeId) => ({
            nodeId,
            name: `node-${nodeId.slice(1)}`,
            role: "worker",
            arch: "arm64",
            reachable: true,
            readOnly: false,
          })),
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

const untags = () => sent.filter((s) => s.path.endsWith("/images/untag"));

const OTHER = "ghcr.io/acme/app:latest";
const ONLY: UntagNode[] = [{ nodeId: "n1", name: "node-1", tags: [REF] }];
const ONE: UntagNode[] = [{ nodeId: "n1", name: "node-1", tags: [REF, OTHER] }];
const TWO: UntagNode[] = [
  { nodeId: "n1", name: "node-1", tags: [REF, OTHER] },
  { nodeId: "n2", name: "node-2", tags: [REF, OTHER] },
];

function mount(nodes: UntagNode[] = ONE, names: string[] = [REF], swarm = false) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <UntagDialog
            envId={ENV}
            imageId={ID}
            names={names}
            nodes={nodes}
            swarm={swarm}
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

const container = (name: string, nodeId: string, node: string) => ({
  kind: "container",
  id: `c-${name}`,
  name,
  nodeId,
  node,
  state: "running",
  matchedBy: "image_id",
});

describe("UntagDialog", () => {
  it("is Untag {name}? with the name-only body and a primary Untag {name} button", async () => {
    mount();
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(`Untag ${REF}?`)).toBeTruthy();
    expect(
      within(dialog).getByText(
        "This removes the name only. The image stays on the node under its other names.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: `Untag ${REF}` })).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeTruthy();
  });

  it("untags the name on its node and toasts Untagged {name}.", async () => {
    mount();
    fireEvent.click(await screen.findByRole("button", { name: `Untag ${REF}` }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(untags()[0]?.body).toMatchObject({ imageId: ID, ref: REF, nodeIds: ["n1"] });
    expect(await screen.findByText(`Untagged ${REF}.`)).toBeTruthy();
    expect(onClose).toHaveBeenCalled();
  });

  it("allows an untag while the image is in use as long as another name remains", async () => {
    usedBy = [container("web", "n1", "node-1")];
    mount();
    const confirm = await screen.findByRole("button", { name: `Untag ${REF}` });
    await screen.findByRole("alertdialog");
    await waitFor(() => expect(confirm.getAttribute("aria-disabled")).toBe("false"));
    fireEvent.click(confirm);
    await waitFor(() => expect(onDone).toHaveBeenCalled());
  });

  it("says the only name of an unused image is removed with it, and the button reads Remove image", async () => {
    mount(ONLY);
    const dialog = await screen.findByRole("alertdialog");
    expect(
      await within(dialog).findByText("This is the image's only name, so the image is removed."),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove image" }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(untags()).toHaveLength(1);
  });

  it("toasts Removed {name}. when the server removed the image with its last name", async () => {
    answer = untagged(true);
    mount(ONLY);
    fireEvent.click(await screen.findByRole("button", { name: "Remove image" }));
    expect(await screen.findByText(`Removed ${REF}.`)).toBeTruthy();
  });

  it("blocks the last name of an image in use, saying so, and sends nothing", async () => {
    usedBy = [container("web", "n1", "node-1")];
    mount(ONLY);
    const dialog = await screen.findByRole("alertdialog");
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain(
      "This is the last name of an image that is in use. Remove what uses it first.",
    );
    const confirm = within(dialog).getByRole("button", { name: "Remove image" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(untags()).toHaveLength(0);
  });

  it("keeps the server's last_name_in_use refusal inside the dialog in the UI's own words", async () => {
    answer = failed("last_name_in_use");
    mount();
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(await within(dialog).findByRole("button", { name: `Untag ${REF}` }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe(
      "That is the last name of an image that is in use. Remove what uses it first.",
    );
    expect(onClose).not.toHaveBeenCalled();
  });

  it("says a name that is already gone is gone and closes", async () => {
    answer = failed("not_found");
    mount();
    fireEvent.click(await screen.findByRole("button", { name: `Untag ${REF}` }));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(
      await screen.findByText(`${REF} is already gone. The list has been refreshed.`),
    ).toBeTruthy();
  });

  it("on a swarm lists the nodes that have the name, all ticked, and untags on the ticked ones", async () => {
    mount(TWO, [REF], true);
    const dialog = await screen.findByRole("alertdialog");
    const boxes = within(dialog).getAllByRole("checkbox");
    expect(boxes).toHaveLength(2);
    fireEvent.click(boxes[1] as HTMLElement);
    fireEvent.click(within(dialog).getByRole("button", { name: `Untag ${REF}` }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(untags()[0]?.body?.nodeIds).toEqual(["n1"]);
  });

  it("lets the person choose which name when the image has several", async () => {
    mount(ONE, [REF, OTHER]);
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(`Untag ${REF}?`)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("radio", { name: OTHER }));
    expect(within(dialog).getByText("Untag ghcr.io/acme/app:latest?")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Untag ghcr.io/acme/app:latest" }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(untags()[0]?.body?.ref).toBe(OTHER);
  });
});
