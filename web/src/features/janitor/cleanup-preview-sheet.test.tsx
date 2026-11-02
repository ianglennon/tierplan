// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CleanupPreviewSheet } from "@/features/janitor/cleanup-preview-sheet";
import "@/i18n";
import { preview, previewItem } from "@/test-janitor";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

let me: ReturnType<typeof makeMe>;
let previews: ReturnType<typeof preview>[];
let asked: number;
let started: { items: { type: string; nodeId: string; ref: string }[] } | undefined;
let client: QueryClient;

// jsdom lays nothing out, so the scrolling area is given a height and the virtualiser can tell how
// many rows fit (as the log viewer's tests do).
const proto = HTMLElement.prototype as unknown as Record<string, unknown>;
const saved: Record<string, PropertyDescriptor | undefined> = {};
function lay(prop: string, value: number) {
  saved[prop] = Object.getOwnPropertyDescriptor(HTMLElement.prototype, prop);
  Object.defineProperty(HTMLElement.prototype, prop, { configurable: true, value });
}

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { [ENV]: "admin" } });
  previews = [];
  asked = 0;
  started = undefined;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/janitor/preview`) {
        const body = previews[Math.min(asked, previews.length - 1)];
        asked++;
        return Response.json(body);
      }
      if (url.pathname === `/api/env/${ENV}/janitor/runs` && req.method === "POST") {
        started = await req.json();
        return Response.json({ runId: "r1", jobId: "j1" }, { status: 202 });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  client.clear();
  for (const [prop, desc] of Object.entries(saved)) {
    if (desc) Object.defineProperty(HTMLElement.prototype, prop, desc);
    else delete proto[prop];
  }
});

function mount(swarm = true) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <CleanupPreviewSheet
            envId={ENV}
            envName="Prod"
            swarm={swarm}
            nodeCount={2}
            onOpenChange={() => {}}
          />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// A mix that shows every part of the line: two containers on one node, one on another, a network
// and an image, all with a known size.
function mixed() {
  return preview({
    items: [
      previewItem({ name: "web.1.aaa", ref: "c1", sizeBytes: 100_000_000 }),
      previewItem({ name: "web.2.bbb", ref: "c2", sizeBytes: 200_000_000 }),
      previewItem({ name: "db.1.ccc", ref: "c3", node: "node-2", nodeId: "n2", sizeBytes: null }),
      previewItem({
        type: "network",
        name: "old_default",
        ref: "net1",
        node: "",
        nodeId: "",
        sizeBytes: null,
        reason: { code: "network_orphaned", stack: "old" },
      }),
      previewItem({
        type: "image",
        name: "nginx:<none>",
        ref: "img1",
        sizeBytes: 300_000_000,
        reason: { code: "dangling_image", ageSeconds: 4 * 86_400 },
      }),
    ],
  });
}

const visibleLine = () => document.querySelector('[data-slot="preview-summary"]')?.textContent;
const liveLine = () => document.querySelector('[data-slot="preview-summary-live"]')?.textContent;

describe("the summary line of the preview", () => {
  it("follows the ticked set", async () => {
    previews = [mixed()];
    mount();
    expect(await screen.findByRole("button", { name: "Remove 5 items" })).toBeTruthy();
    expect(visibleLine()).toBe("Would remove 3 containers, 1 network, 1 image, ~600 MB");

    fireEvent.click(screen.getByRole("checkbox", { name: "Select web.2.bbb" }));
    expect(visibleLine()).toBe("Would remove 2 containers, 1 network, 1 image, ~400 MB");

    fireEvent.click(screen.getByRole("checkbox", { name: "Select nginx:<none>" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Select old_default" }));
    expect(visibleLine()).toBe("Would remove 2 containers, ~100 MB");
  });

  it("reads Nothing selected, and the commit says why it can't be pressed, when nothing is ticked", async () => {
    previews = [preview({ items: [previewItem({ name: "web.1.aaa", ref: "c1" })] })];
    mount();
    fireEvent.click(await screen.findByRole("checkbox", { name: "Select web.1.aaa" }));
    expect(visibleLine()).toBe("Nothing selected");
    const remove = screen.getByRole("button", { name: /^Remove 0 items/ });
    expect(remove.getAttribute("aria-disabled")).toBe("true");
    expect(remove.getAttribute("aria-describedby")).toBeTruthy();
    expect(
      document.getElementById(remove.getAttribute("aria-describedby") ?? "")?.textContent,
    ).toBe("Tick at least one item to remove.");
    fireEvent.click(remove);
    expect(started).toBeUndefined();
  });

  it("announces the settled line once, 500 ms after the last tick change", async () => {
    previews = [mixed()];
    mount();
    await screen.findByRole("button", { name: "Remove 5 items" });
    expect(
      document.querySelector('[data-slot="preview-summary-live"]')?.getAttribute("aria-live"),
    ).toBe("polite");
    const all = "Would remove 3 containers, 1 network, 1 image, ~600 MB";
    expect(liveLine()).toBe(all);

    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    fireEvent.click(screen.getByRole("checkbox", { name: "Select web.1.aaa" }));
    act(() => vi.advanceTimersByTime(300));
    fireEvent.click(screen.getByRole("checkbox", { name: "Select web.2.bbb" }));
    // The line on the page follows at once; the announcement waits for the ticking to stop.
    expect(visibleLine()).toBe("Would remove 1 container, 1 network, 1 image, ~300 MB");
    act(() => vi.advanceTimersByTime(499));
    expect(liveLine()).toBe(all);
    act(() => vi.advanceTimersByTime(1));
    expect(liveLine()).toBe("Would remove 1 container, 1 network, 1 image, ~300 MB");
  });
});

describe("group and type ticks", () => {
  it("are three-state, mixed when part of the group is ticked", async () => {
    previews = [mixed()];
    mount();
    await screen.findByRole("button", { name: "Remove 5 items" });
    // The image is on node-1 too, so the node's checkbox is looked for in the Containers section.
    const containers = within(screen.getByRole("region", { name: /^Containers/ }));
    const type = containers.getByRole("checkbox", { name: "Select all in Containers" });
    const node1 = containers.getByRole("checkbox", { name: "Select all in node-1" });
    const node2 = containers.getByRole("checkbox", { name: "Select all in node-2" });
    expect(type.getAttribute("aria-checked")).toBe("true");

    fireEvent.click(screen.getByRole("checkbox", { name: "Select web.1.aaa" }));
    expect(type.getAttribute("aria-checked")).toBe("mixed");
    expect(node1.getAttribute("aria-checked")).toBe("mixed");
    expect(node2.getAttribute("aria-checked")).toBe("true");

    // From mixed, a click ticks the whole group.
    fireEvent.click(node1);
    expect(node1.getAttribute("aria-checked")).toBe("true");
    expect(type.getAttribute("aria-checked")).toBe("true");
    // From all ticked, a click unticks it, and only it.
    fireEvent.click(node1);
    expect(node1.getAttribute("aria-checked")).toBe("false");
    expect(type.getAttribute("aria-checked")).toBe("mixed");
    expect(
      screen.getByRole("checkbox", { name: "Select db.1.ccc" }).getAttribute("aria-checked"),
    ).toBe("true");
  });
});

describe("a long list", () => {
  it("renders only the rows in view, and select-all ticks and sends every item", async () => {
    lay("offsetHeight", 400);
    lay("offsetWidth", 800);
    const items = Array.from({ length: 2000 }, (_, i) =>
      previewItem({ name: `task-${i}`, ref: `r${i}`, sizeBytes: 1000 }),
    );
    previews = [preview({ items })];
    mount();
    expect(await screen.findByRole("button", { name: "Remove 2,000 items" })).toBeTruthy();
    expect(visibleLine()).toBe("Would remove 2,000 containers, ~2 MB");

    const rendered = (await screen.findAllByRole("checkbox", { name: /^Select task-/ })).length;
    expect(rendered).toBeGreaterThan(0);
    expect(rendered).toBeLessThan(200);

    // Everything is ticked; select-all clears it, and again ticks every item, rendered or not.
    const all = screen.getByRole("checkbox", { name: "Select all in Containers" });
    fireEvent.click(all);
    expect(visibleLine()).toBe("Nothing selected");
    fireEvent.click(all);
    expect(visibleLine()).toBe("Would remove 2,000 containers, ~2 MB");

    fireEvent.click(screen.getByRole("button", { name: "Remove 2,000 items" }));
    await waitFor(() => expect(started).toBeDefined());
    expect(started?.items).toHaveLength(2000);
    expect(new Set(started?.items.map((i) => i.ref)).size).toBe(2000);
  });
});

describe("Refresh preview", () => {
  it("asks again, starts every item ticked and counts the new ones", async () => {
    previews = [
      preview({
        items: [
          previewItem({ name: "web.1.aaa", ref: "c1" }),
          previewItem({ name: "web.2.bbb", ref: "c2" }),
        ],
      }),
      preview({
        items: [
          previewItem({ name: "web.1.aaa", ref: "c1" }),
          previewItem({ name: "web.2.bbb", ref: "c2" }),
          previewItem({ name: "web.3.ccc", ref: "c3" }),
        ],
      }),
    ];
    mount();
    fireEvent.click(await screen.findByRole("checkbox", { name: "Select web.1.aaa" }));
    expect(screen.getByRole("button", { name: "Remove 1 item" })).toBeTruthy();
    expect(asked).toBe(1);

    fireEvent.click(screen.getByRole("button", { name: "Refresh preview" }));
    expect(await screen.findByRole("button", { name: "Remove 3 items" })).toBeTruthy();
    expect(asked).toBe(2);
    expect(screen.getByText(/^3 of 3 ticked\./)).toBeTruthy();
    const sheet = screen.getByRole("dialog");
    for (const b of within(sheet).getAllByRole("checkbox", { name: /^Select web\./ })) {
      expect(b.getAttribute("aria-checked")).toBe("true");
    }
  });
});
