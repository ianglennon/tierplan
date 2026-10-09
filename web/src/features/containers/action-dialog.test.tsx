// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { ContainerRow } from "@/api/resources";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ActionDialog } from "@/features/containers/action-dialog";
import { type PendingApi, PendingContext } from "@/features/containers/pending";
import "@/i18n";

const ENV = "e1";

function row(name: string, extra: Partial<ContainerRow> = {}): ContainerRow {
  return {
    node: "node-1",
    nodeId: "n1",
    id: `id-${name}`,
    name,
    state: "running",
    status: "Up 2 hours",
    image: "nginx:1.27",
    created: "2026-10-01T10:00:00Z",
    ports: [],
    isSelf: false,
    project: "",
    stack: "",
    service: "",
    health: "none",
    selfKind: "",
    readOnly: false,
    ...extra,
  };
}

interface Sent {
  path: string;
  body?: { targets: { id: string }[]; [k: string]: unknown };
}

let sent: Sent[];
let answer: (req: Sent) => Response | Promise<Response>;
let client: QueryClient;
let marks: { keys: string[]; verb: string }[];
let released: string[][];

const api: PendingApi = {
  mark: (rows, verb) => marks.push({ keys: rows.map((r) => r.key), verb }),
  release: (keys) => released.push([...keys]),
};

const allDone = (req: Sent) =>
  Response.json({
    bulkId: "018f0000-0000-7000-8000-000000000001",
    results: (req.body?.targets ?? []).map((t) => ({
      nodeId: "n1",
      id: t.id,
      name: t.id.replace("id-", ""),
      result: "done",
    })),
  });

beforeEach(() => {
  sent = [];
  marks = [];
  released = [];
  answer = allDone;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const entry: Sent = {
        path: new URL(req.url).pathname,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      };
      sent.push(entry);
      if (entry.path === `/api/env/${ENV}/containers/actions`) return answer(entry);
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

const posts = () => sent.filter((s) => s.path.endsWith("/containers/actions"));

function mount(rows: ContainerRow[], props: Partial<Parameters<typeof ActionDialog>[0]> = {}) {
  const onClose = vi.fn();
  const onDone = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <PendingContext.Provider value={api}>
          <ActionDialog
            envId={ENV}
            verb="stop"
            rows={rows}
            open
            onClose={onClose}
            onDone={onDone}
            {...props}
          />
        </PendingContext.Provider>
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return { onClose, onDone };
}

const TWO = [row("web"), row("db", { node: "node-2", nodeId: "n2" })];

describe("ActionDialog stop", () => {
  it("asks 'Stop {name}?' for one container and lists nothing", async () => {
    mount([row("web")]);
    const dialog = await screen.findByRole("alertdialog", { name: "Stop web?" });
    expect(dialog.textContent).toContain("using the container's own stop settings");
    expect(within(dialog).queryByRole("table")).toBeNull();
    expect(within(dialog).getByRole("button", { name: "Stop container" })).toBeTruthy();
  });

  it("asks 'Stop 2 containers?' with every target and its node, focus on Cancel and nothing sent yet", async () => {
    mount(TWO, { swarm: true });
    const dialog = await screen.findByRole("alertdialog", { name: "Stop 2 containers?" });
    expect(dialog.textContent).toContain("Each one uses its own stop settings.");
    const table = within(dialog).getByRole("table");
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Container", "Node", "Note"]);
    const body = within(table).getAllByRole("row").slice(1);
    expect(body.map((r) => r.textContent)).toEqual(["webnode-1", "dbnode-2"]);
    await waitFor(() =>
      expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" })),
    );
    expect(posts()).toEqual([]);
  });

  it("leaves the Node column out on a standalone host", async () => {
    mount(TWO);
    const dialog = await screen.findByRole("alertdialog");
    expect(
      within(dialog)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Container", "Note"]);
  });

  it("posts one request with both targets, toasts once, closes and tells the list", async () => {
    const { onClose, onDone } = mount(TWO);
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop 2 containers" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      action: "stop",
      targets: [
        { nodeId: "n1", id: "id-web" },
        { nodeId: "n2", id: "id-db" },
      ],
    });
    expect(await screen.findByText("Stopped 2 containers.")).toBeTruthy();
    expect(onClose).toHaveBeenCalled();
    expect(onDone).toHaveBeenCalled();
  });

  it("marks both rows as stopping when the request goes out", async () => {
    mount(TWO);
    fireEvent.click(await screen.findByRole("button", { name: "Stop 2 containers" }));
    await waitFor(() => expect(marks).toHaveLength(1));
    expect(marks[0]).toEqual({ keys: ["node-1|id-web", "node-2|id-db"], verb: "stop" });
  });

  it("shows Stopping… and sends once however often the button is pressed", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    mount(TWO);
    const confirm = await screen.findByRole("button", { name: "Stop 2 containers" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    const pending = await screen.findByRole("button", { name: "Stopping…" });
    expect(pending.getAttribute("aria-disabled")).toBe("true");
    expect(posts()).toHaveLength(1);
    // The dialog stays open until the server answers.
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    await act(async () =>
      release(allDone({ path: "", body: { targets: [{ id: "id-web" }, { id: "id-db" }] } })),
    );
    await waitFor(() => expect(screen.getByText("Stopped 2 containers.")).toBeTruthy());
  });

  it("sums a partial result in one error toast and takes the failed row's label off at once", async () => {
    answer = (req) =>
      Response.json({
        results: (req.body?.targets ?? []).map((t, i) => ({
          nodeId: "n1",
          id: t.id,
          name: t.id,
          result: i === 0 ? "done" : "failed",
          code: i === 0 ? undefined : "node_unreachable",
        })),
      });
    mount(TWO);
    fireEvent.click(await screen.findByRole("button", { name: "Stop 2 containers" }));
    expect(await screen.findByText("Stopped 1, 1 failed")).toBeTruthy();
    expect(released).toEqual([["node-2|id-db"]]);
  });

  it("keeps a bulk dialog open with an inline alert when the whole request is refused", async () => {
    answer = () => Response.json({ status: 403 }, { status: 403 });
    const { onClose } = mount(TWO);
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop 2 containers" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe(
      "You no longer have permission to do that. Your access may have changed.",
    );
    expect(onClose).not.toHaveBeenCalled();
    // Every row goes back to its real state.
    expect(released).toEqual([["node-1|id-web", "node-2|id-db"]]);
    expect(screen.queryByText(/^Couldn't stop/)).toBeNull();
  });

  it("keeps a single dialog open with the mapped reason when the container fails", async () => {
    answer = (req) =>
      Response.json({
        results: [
          {
            nodeId: "n1",
            id: req.body?.targets[0]?.id,
            name: "web",
            result: "failed",
            code: "node_unreachable",
          },
        ],
      });
    mount([row("web")]);
    fireEvent.click(await screen.findByRole("button", { name: "Stop container" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("node-1 isn't reachable right now. Try again when it's back.");
  });

  it("has a name that wraps rather than runs off the dialog", async () => {
    const long = `${"a".repeat(80)}-service-1`;
    mount([row(long)]);
    const title = await screen.findByRole("heading", { name: `Stop ${long}?` });
    expect(title.className).toContain("break-all");
  });
});
