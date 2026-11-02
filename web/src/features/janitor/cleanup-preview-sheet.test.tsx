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

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

const problem = (type: string, status: number, errors: { location: string; message: string }[]) =>
  new Response(JSON.stringify({ type, title: "Error", status, errors }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

let me: ReturnType<typeof makeMe>;
let previews: ReturnType<typeof preview>[];
let asked: number;
let started: { items: { type: string; nodeId: string; ref: string }[] } | undefined;
// What the stubbed server answers instead of the usual, when a test needs it to.
let previewAnswer: (() => Response | Promise<Response>) | undefined;
let runsAnswer: (() => Response) | undefined;
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
  previewAnswer = undefined;
  runsAnswer = undefined;
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/janitor/preview`) {
        const body = previews[Math.min(asked, previews.length - 1)];
        asked++;
        if (previewAnswer) return previewAnswer();
        return Response.json(body);
      }
      if (url.pathname === `/api/env/${ENV}/janitor/runs` && req.method === "POST") {
        started = await req.json();
        if (runsAnswer) return runsAnswer();
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

function mount(swarm = true, onOpenChange: (open: boolean) => void = () => {}) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <CleanupPreviewSheet
            envId={ENV}
            envName="Prod"
            swarm={swarm}
            nodeCount={2}
            onOpenChange={onOpenChange}
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

describe("a deploy holds the environment", () => {
  const holder = () =>
    problem("operation_locked", 409, [
      { location: "stack", message: "tracer" },
      { location: "job", message: "j9" },
      { location: "started", message: new Date(Date.now() - 5 * 60_000).toISOString() },
      { location: "stackId", message: "s9" },
    ]);

  it("keeps the sheet open, shows the lock and says so in a toast", async () => {
    previews = [preview({ items: [previewItem({ name: "web.1.aaa", ref: "c1" })] })];
    runsAnswer = holder;
    const closed = vi.fn();
    mount(true, closed);
    fireEvent.click(await screen.findByRole("button", { name: "Remove 1 item" }));

    const sheet = screen.getByRole("dialog");
    expect(
      await within(sheet).findByText(/Another stack operation is running on Prod: tracer/),
    ).toBeTruthy();
    expect(toasts.error).toHaveBeenCalledWith(
      "A deploy is running in Prod. Try again when it finishes.",
    );
    expect(closed).not.toHaveBeenCalled();
    // The commit can't be pressed again, and says why.
    const remove = within(sheet).getByRole("button", { name: "Remove 1 item" });
    expect(remove.getAttribute("aria-disabled")).toBe("true");
    expect(
      document.getElementById(remove.getAttribute("aria-describedby") ?? "")?.textContent,
    ).toBe("Waiting for a deploy to finish. tracer is deploying.");
  });

  it("names the janitor when it is the janitor that holds it", async () => {
    previews = [preview({ items: [previewItem({ name: "web.1.aaa", ref: "c1" })] })];
    runsAnswer = () =>
      problem("operation_locked", 409, [
        { location: "kind", message: "janitor" },
        { location: "runId", message: "r9" },
        { location: "job", message: "j9" },
        { location: "started", message: new Date(Date.now() - 60_000).toISOString() },
      ]);
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Remove 1 item" }));
    const sheet = screen.getByRole("dialog");
    expect(await within(sheet).findByText(/The janitor is cleaning Prod, started/)).toBeTruthy();
    expect(within(sheet).getByRole("link", { name: "Open it" }).getAttribute("href")).toBe(
      "/env/e1/janitor/runs/r9",
    );
    expect(toasts.error).toHaveBeenCalledWith(
      "The janitor is already cleaning Prod. Try again when it finishes.",
    );
  });
});

describe("a user below Env Admin", () => {
  it("sees the preview but can't remove, and is told what it takes", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    previews = [preview({ items: [previewItem({ name: "web.1.aaa", ref: "c1" })] })];
    mount();
    const remove = await screen.findByRole("button", { name: "Remove 1 item" });
    expect(remove.getAttribute("aria-disabled")).toBe("true");
    expect(
      document.getElementById(remove.getAttribute("aria-describedby") ?? "")?.textContent,
    ).toBe("Needs Admin on Prod");
    fireEvent.click(remove);
    expect(started).toBeUndefined();
  });
});

describe("nothing to clean up", () => {
  it("says so, hides the commit and still lists what was left", async () => {
    previews = [
      preview({
        items: [],
        notRemoved: [
          previewItem({
            type: "network",
            name: "scratch_net",
            ref: "n9",
            node: "",
            nodeId: "",
            class: "reported",
            reason: { code: "opt_in_off_networks" },
          }),
        ],
      }),
    ];
    mount();
    expect(await screen.findByText("Nothing to clean up")).toBeTruthy();
    expect(
      screen.getByText(/Anything the policy only reports is listed under Not removed\./),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Remove/ })).toBeNull();

    // Not removed is closed to start with; its reason is in the row, in words.
    fireEvent.click(screen.getByRole("button", { name: /^Not removed \(1\)/ }));
    expect(
      await screen.findByText("Opt-in off: unused networks aren't removed until you turn that on."),
    ).toBeTruthy();
    expect(screen.getByText("scratch_net")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open policy" }).getAttribute("href")).toBe(
      "/env/e1/janitor#policy",
    );
  });
});

describe("while it works out what to clean", () => {
  it("says it is checking the nodes and holds the place of the sections", async () => {
    let release: (r: Response) => void = () => {};
    previewAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    previews = [preview()];
    mount();
    expect(await screen.findByText("Checking 2 nodes…")).toBeTruthy();
    expect(document.querySelector('[data-slot="skeleton"]')).not.toBeNull();
    expect(screen.queryByRole("button", { name: /^Remove/ })).toBeNull();
    release(Response.json(previews[0]));
    expect(await screen.findByRole("button", { name: "Remove 1 item" })).toBeTruthy();
    expect(screen.queryByText("Checking 2 nodes…")).toBeNull();
  });
});

describe("a preview that fails", () => {
  it("says nothing was removed, sends nothing and offers to try again", async () => {
    previews = [preview({ items: [previewItem({ name: "web.1.aaa", ref: "c1" })] })];
    previewAnswer = () => problem("internal", 500, []);
    mount();
    expect(await screen.findByText("Couldn't work out what to clean up.")).toBeTruthy();
    expect(screen.getByText(/Nothing was removed\./)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Remove/ })).toBeNull();
    expect(started).toBeUndefined();

    previewAnswer = undefined;
    fireEvent.click(screen.getByRole("button", { name: "Retry preview" }));
    expect(await screen.findByRole("button", { name: "Remove 1 item" })).toBeTruthy();
    expect(asked).toBe(2);
    expect(started).toBeUndefined();
  });
});

describe("a node that is not reporting", () => {
  it("is named in the banner, with the note that its networks are not judged", async () => {
    previews = [
      preview({
        items: [previewItem({ name: "web.1.aaa", ref: "c1" })],
        withheldNodes: ["node-3"],
        networksWithheld: true,
      }),
    ];
    mount();
    await screen.findByRole("button", { name: "Remove 1 item" });
    const sheet = screen.getByRole("dialog");
    expect(within(sheet).getByText(/^1 node isn't reporting: node-3\./)).toBeTruthy();
    expect(
      within(sheet).getByText("TierPlan won't judge networks while node-3 isn't reporting."),
    ).toBeTruthy();
  });
});

describe("names and reasons that hold markup", () => {
  it("are drawn as text and never as elements", async () => {
    previews = [
      preview({
        items: [
          previewItem({ name: "<script>alert(1)</script>", ref: "c1" }),
          previewItem({
            type: "network",
            name: "<img src=x onerror=alert(1)>",
            ref: "n1",
            node: "",
            nodeId: "",
            reason: { code: "network_orphaned", stack: "<b>old</b>" },
          }),
        ],
        notRemoved: [
          previewItem({
            name: "<i>kept</i>",
            ref: "c8",
            class: "protected",
            reason: { code: "stack_excluded", stack: "<u>x</u>" },
          }),
        ],
      }),
    ];
    mount();
    const sheet = await screen.findByRole("dialog");
    await within(sheet).findByRole("button", { name: "Remove 2 items" });
    fireEvent.click(within(sheet).getByRole("button", { name: /^Not removed \(1\)/ }));
    await within(sheet).findByText("<i>kept</i>");
    expect(within(sheet).getByText("<script>alert(1)</script>")).toBeTruthy();
    expect(within(sheet).getByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(
      within(sheet).getByText("Orphaned: the stack <b>old</b> that created it no longer exists."),
    ).toBeTruthy();
    for (const tag of ["script", "img", "b", "i", "u"]) {
      expect(sheet.querySelector(tag)).toBeNull();
    }
  });
});
