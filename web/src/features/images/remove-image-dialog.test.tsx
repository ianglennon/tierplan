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
let usedBy: unknown[];

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
      if (entry.path === `/api/env/${ENV}/images/remove`) return answer(entry);
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
          nodes: NODES.map((n) => ({
            nodeId: n.nodeId,
            name: n.name,
            role: "manager",
            arch: "amd64",
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

const removes = () => sent.filter((s) => s.path.endsWith("/images/remove"));

const NODES = [
  { nodeId: "n1", name: "node-1" },
  { nodeId: "n2", name: "node-2" },
  { nodeId: "n3", name: "node-3" },
];

function mount(name = "ghcr.io/acme/app:1", swarm = false) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <RemoveImageDialog
            envId={ENV}
            imageId={ID}
            name={name}
            nodes={swarm ? NODES : NODES.slice(0, 1)}
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

const container = (name: string, nodeId: string, node: string) => ({
  kind: "container",
  id: `c-${name}`,
  name,
  nodeId,
  node,
  state: "exited",
  matchedBy: "image_id",
});
const service = (name: string) => ({
  kind: "service",
  id: `s-${name}`,
  name,
  nodeId: "",
  node: "",
  state: "no_tasks",
  matchedBy: "tag",
});

describe("RemoveImageDialog on a swarm", () => {
  it("is Remove {ref} from nodes? with a node list, every node that has the image ticked", async () => {
    mount("ghcr.io/acme/app:1", true);
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Remove ghcr.io/acme/app:1 from nodes?")).toBeTruthy();
    const boxes = within(dialog).getAllByRole("checkbox");
    expect(boxes).toHaveLength(3);
    for (const b of boxes) expect(b.getAttribute("aria-checked")).toBe("true");
    expect(within(dialog).getByRole("button", { name: "Remove from 3 of 3 nodes" })).toBeTruthy();
  });

  it("disables a node where a container uses the image, saying Used by {container}, and removes from the rest", async () => {
    usedBy = [container("old-cache", "n2", "node-2")];
    mount("ghcr.io/acme/app:1", true);
    const dialog = await screen.findByRole("alertdialog");
    const confirm = await within(dialog).findByRole("button", { name: "Remove from 2 of 3 nodes" });
    expect(within(dialog).getByText("Used by old-cache")).toBeTruthy();
    const boxes = within(dialog).getAllByRole("checkbox");
    expect((boxes[1] as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(confirm);
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(removes()[0]?.body?.targets).toEqual([
      { nodeId: "n1", imageId: ID },
      { nodeId: "n3", imageId: ID },
    ]);
    expect(await screen.findByText("Removed ghcr.io/acme/app:1 from 2 nodes.")).toBeTruthy();
  });

  it("does not let the last ticked node be unticked into a remove from zero nodes", async () => {
    mount("ghcr.io/acme/app:1", true);
    const dialog = await screen.findByRole("alertdialog");
    for (const b of within(dialog).getAllByRole("checkbox")) fireEvent.click(b);
    const confirm = await within(dialog).findByRole("button", { name: "Remove from 0 of 3 nodes" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(removes()).toHaveLength(0);
  });

  it("sums up a partial removal in one error toast", async () => {
    answer = (req) =>
      Response.json({
        results: (req.body?.targets ?? []).map((t, i) =>
          i === 0 ? { ...t, result: "failed", code: "image_in_use" } : { ...t, result: "done" },
        ),
      });
    mount("ghcr.io/acme/app:1", true);
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(
      await within(dialog).findByRole("button", { name: "Remove from 3 of 3 nodes" }),
    );
    expect(await screen.findByText("Removed 2, 1 failed")).toBeTruthy();
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });
});

describe("RemoveImageDialog while the image is in use", () => {
  it("shows the in-use alert with what uses it and an aria-disabled confirm for a container on the only node", async () => {
    usedBy = [container("old-cache", "n1", "node-1")];
    mount();
    const dialog = await screen.findByRole("alertdialog");
    const confirm = await within(dialog).findByRole("button", { name: "In use by 1 container" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    const alert = within(dialog).getByRole("alert");
    expect(alert.textContent).toContain("ghcr.io/acme/app:1 is in use");
    expect(alert.textContent).toContain(
      "Stopped containers and swarm services that mention it count, even with no running tasks. Remove them first.",
    );
    expect(alert.textContent).toContain("old-cache on node-1");
    fireEvent.click(confirm);
    expect(removes()).toHaveLength(0);
  });

  it("blocks every node for a service spec, even one with no tasks", async () => {
    usedBy = [service("proxy")];
    mount("ghcr.io/acme/app:1", true);
    const dialog = await screen.findByRole("alertdialog");
    const confirm = await within(dialog).findByRole("button", { name: "In use by 1 service" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    expect(within(dialog).getByRole("alert").textContent).toContain("Service proxy");
    for (const b of within(dialog).getAllByRole("checkbox")) {
      expect((b as HTMLButtonElement).disabled).toBe(true);
    }
    expect(within(dialog).getAllByText("Used by service proxy")).toHaveLength(3);
  });
});
