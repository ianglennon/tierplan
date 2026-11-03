// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { JanitorPolicyFields } from "@/api/janitor";
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
// The body of each preview request, as text: empty when the sheet asked for the saved policy.
let previewRequests: string[];
let started: { items: { type: string; nodeId: string; ref: string }[] } | undefined;
// What the stubbed server answers instead of the usual, when a test needs it to.
let previewAnswer: (() => Response | Promise<Response>) | undefined;
let runsAnswer: (() => Response) | undefined;
// The body of each keep request, and what the stubbed server answers to it instead of the usual.
let keepRequests: { targets: { kind: string; nodeId?: string; ref: string }[] }[];
let keepAnswer: (() => Response) | undefined;
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
  previewRequests = [];
  started = undefined;
  previewAnswer = undefined;
  runsAnswer = undefined;
  keepRequests = [];
  keepAnswer = undefined;
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
        previewRequests.push(await req.text());
        if (previewAnswer) return previewAnswer();
        return Response.json(body);
      }
      if (url.pathname === `/api/env/${ENV}/janitor/keeps` && req.method === "PUT") {
        keepRequests.push(await req.json());
        if (keepAnswer) return keepAnswer();
        return Response.json({ kept: 1 });
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

describe("the preview sheet in policy mode", () => {
  const candidate: JanitorPolicyFields = { keepN: 0, graceHours: null };

  function mountPolicy(
    onSavePolicy: () => Promise<boolean>,
    onOpenChange: (open: boolean) => void = () => {},
  ) {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <CleanupPreviewSheet
              mode="policy"
              envId={ENV}
              envName="Prod"
              swarm
              nodeCount={2}
              policy={candidate}
              onSavePolicy={onSavePolicy}
              onOpenChange={onOpenChange}
            />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
  }

  it("previews the edited policy, offers nothing to tick and says when saving applies", async () => {
    previews = [mixed()];
    mountPolicy(async () => true);
    const sheet = await screen.findByRole("dialog", { name: "What this policy would remove" });
    expect(
      within(sheet).getByText(
        "Here is what the janitor would remove with the policy you've edited. Nothing is removed by saving.",
      ),
    ).toBeTruthy();
    await within(sheet).findByText("web.1.aaa");

    // The request carried the candidate, and the summary covers every item with nothing to untick.
    expect(JSON.parse(previewRequests[0] ?? "")).toEqual({ policy: candidate });
    expect(visibleLine()).toBe("Would remove 3 containers, 1 network, 1 image, ~600 MB");
    expect(within(sheet).queryAllByRole("checkbox")).toHaveLength(0);
    expect(within(sheet).queryByRole("button", { name: /^Remove / })).toBeNull();
    expect(within(sheet).getByText("Saving applies from the next run.")).toBeTruthy();
    expect(within(sheet).getByRole("button", { name: "Save policy" })).toBeTruthy();
  });

  it("starts with focus on the summary", async () => {
    previews = [mixed()];
    mountPolicy(async () => true);
    const sheet = await screen.findByRole("dialog", { name: "What this policy would remove" });
    const summary = within(sheet).getByRole("group", { name: "What the policy would remove" });
    await waitFor(() => expect(document.activeElement).toBe(summary));
    // The summary itself is inside what holds focus once the preview has come.
    await within(sheet).findByText("web.1.aaa");
    expect(summary.contains(document.querySelector('[data-slot="preview-summary"]'))).toBe(true);
  });

  it("saves the policy once and closes, whatever the save answers", async () => {
    for (const saved of [true, false]) {
      cleanup();
      previews = [mixed()];
      const save = vi.fn(async () => saved);
      const closed = vi.fn();
      mountPolicy(save, closed);
      const sheet = await screen.findByRole("dialog", { name: "What this policy would remove" });
      const button = await within(sheet).findByRole("button", { name: "Save policy" });
      await waitFor(() => expect(button.hasAttribute("disabled")).toBe(false));
      fireEvent.click(button);
      fireEvent.click(button);
      await waitFor(() => expect(closed).toHaveBeenCalledWith(false));
      expect(save).toHaveBeenCalledTimes(1);
      expect(started).toBeUndefined();
    }
  });

  it("still offers Save policy when nothing would be removed", async () => {
    previews = [preview({ items: [] })];
    mountPolicy(async () => true);
    const sheet = await screen.findByRole("dialog", { name: "What this policy would remove" });
    expect(await within(sheet).findByText("Nothing to clean up")).toBeTruthy();
    const button = within(sheet).getByRole("button", { name: "Save policy" });
    await waitFor(() => expect(button.hasAttribute("disabled")).toBe(false));
  });

  it("does not offer Save policy until the preview has come, and keeps Cancel", async () => {
    previews = [mixed()];
    let release: () => void = () => {};
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    previewAnswer = async () => {
      await gate;
      return Response.json(previews[0]);
    };
    mountPolicy(async () => true);
    const sheet = await screen.findByRole("dialog", { name: "What this policy would remove" });
    const button = within(sheet).getByRole("button", { name: "Save policy" });
    expect(button.hasAttribute("disabled")).toBe(true);
    expect(within(sheet).getByRole("button", { name: "Cancel" })).toBeTruthy();
    release();
    await waitFor(() => expect(button.hasAttribute("disabled")).toBe(false));
  });
});

describe("KeepFromPreview", () => {
  it("keeps a container: unticks it, drops it from the summary and moves it to Not removed as Kept", async () => {
    previews = [mixed()];
    mount();
    await screen.findByRole("button", { name: "Remove 5 items" });
    expect(visibleLine()).toBe("Would remove 3 containers, 1 network, 1 image, ~600 MB");

    fireEvent.click(screen.getByRole("button", { name: "Keep web.2.bbb" }));

    await waitFor(() =>
      expect(screen.queryByRole("checkbox", { name: "Select web.2.bbb" })).toBeNull(),
    );
    expect(keepRequests).toEqual([{ targets: [{ kind: "container", nodeId: "n1", ref: "c2" }] }]);
    expect(toasts.success).toHaveBeenCalledWith(
      "Keeping web.2.bbb. The janitor won't list it again.",
    );
    // The summary and the commit no longer count it, and nothing else was unticked or asked again.
    expect(visibleLine()).toBe("Would remove 2 containers, 1 network, 1 image, ~400 MB");
    expect(screen.getByRole("button", { name: "Remove 4 items" })).toBeTruthy();
    expect(asked).toBe(1);

    fireEvent.click(screen.getByRole("button", { name: /^Not removed \(1\)/ }));
    expect(await screen.findByText("Protected: marked Keep on the Janitor page.")).toBeTruthy();
    const row = screen.getByText("web.2.bbb").closest("tr");
    expect(within(row as HTMLElement).getByText("Kept")).toBeTruthy();
    // It is already kept, so it offers no Keep.
    expect(screen.queryByRole("button", { name: "Keep web.2.bbb" })).toBeNull();
  });

  it("leaves the other ticks alone", async () => {
    previews = [mixed()];
    mount();
    await screen.findByRole("button", { name: "Remove 5 items" });
    fireEvent.click(screen.getByRole("checkbox", { name: "Select web.1.aaa" }));
    fireEvent.click(screen.getByRole("button", { name: "Keep web.2.bbb" }));
    await screen.findByRole("button", { name: "Remove 3 items" });
    expect(screen.getByRole("button", { name: "Remove 3 items" })).toBeTruthy();
    expect(
      screen.getByRole("checkbox", { name: "Select web.1.aaa" }).getAttribute("aria-checked"),
    ).toBe("false");
  });

  it("names a network by its name, and a swarm-wide one has no node", async () => {
    previews = [mixed()];
    mount();
    await screen.findByRole("button", { name: "Remove 5 items" });
    fireEvent.click(screen.getByRole("button", { name: "Keep old_default" }));
    await waitFor(() => expect(keepRequests).toHaveLength(1));
    expect(keepRequests[0]).toEqual({ targets: [{ kind: "network", ref: "old_default" }] });
  });
});

describe("Keep and what already keeps an item", () => {
  // A Not removed list with one row of each kind: ones the janitor leaves for a reason a person can
  // act on with Keep, and ones that are already kept.
  function notRemoved() {
    return preview({
      items: [],
      notRemoved: [
        previewItem({
          name: "labelled.1.aaa",
          ref: "k1",
          class: "protected",
          reason: { code: "kept_label" },
        }),
        previewItem({
          name: "shop.1.bbb",
          ref: "k2",
          class: "protected",
          reason: { code: "stack_excluded", stack: "shop" },
        }),
        previewItem({
          name: "kept.1.ccc",
          ref: "k3",
          class: "protected",
          reason: { code: "stack_declared_keep", stack: "decl" },
        }),
        previewItem({
          name: "mine.1.ddd",
          ref: "k4",
          class: "protected",
          reason: { code: "kept_row" },
        }),
        previewItem({
          name: "young.1.eee",
          ref: "k5",
          class: "grace",
          reason: { code: "younger_than_grace", remainingSeconds: 7_200 },
        }),
      ],
    });
  }

  it("offers Keep on a Not removed row, and none on a row the label, a stack or a person already keeps", async () => {
    previews = [notRemoved()];
    mount();
    fireEvent.click(await screen.findByRole("button", { name: /^Not removed \(5\)/ }));
    await screen.findByText("young.1.eee");

    for (const name of ["labelled.1.aaa", "shop.1.bbb", "kept.1.ccc", "mine.1.ddd"]) {
      expect(screen.queryByRole("button", { name: `Keep ${name}` })).toBeNull();
      const row = screen.getByText(name).closest("tr") as HTMLElement;
      expect(within(row).getByText("Kept")).toBeTruthy();
    }
    // The reason is in the row, in words, instead of a button.
    expect(screen.getByText("Protected: labelled tierplan.keep=true.")).toBeTruthy();
    expect(screen.getByText("Protected: stack shop is excluded from the janitor.")).toBeTruthy();
    expect(
      screen.getByText("Protected: stack decl sets tierplan.keep=true in its compose file."),
    ).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Keep young.1.eee" }));
    await waitFor(() => expect(keepRequests).toHaveLength(1));
    expect(keepRequests[0]).toEqual({ targets: [{ kind: "container", nodeId: "n1", ref: "k5" }] });
    // It stays in the list, now as kept, and no longer offers Keep.
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Keep young.1.eee" })).toBeNull(),
    );
    const row = screen.getByText("young.1.eee").closest("tr") as HTMLElement;
    expect(within(row).getByText("Protected: marked Keep on the Janitor page.")).toBeTruthy();
    expect(within(row).getByText("Kept")).toBeTruthy();
  });

  it("says Keep couldn't and leaves the row where it was", async () => {
    previews = [mixed()];
    keepAnswer = () => Response.json({ type: "x", title: "x", status: 500 }, { status: 500 });
    mount();
    await screen.findByRole("button", { name: "Remove 5 items" });
    fireEvent.click(screen.getByRole("button", { name: "Keep web.2.bbb" }));

    await waitFor(() =>
      expect(toasts.error).toHaveBeenCalledWith("Couldn't keep web.2.bbb. Try again."),
    );
    expect(toasts.success).not.toHaveBeenCalled();
    expect(screen.getByRole("checkbox", { name: "Select web.2.bbb" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove 5 items" })).toBeTruthy();
    // The button is usable again.
    expect(screen.getByRole("button", { name: "Keep web.2.bbb" }).hasAttribute("disabled")).toBe(
      false,
    );
  });

  it("disables Keep for a user below Env Admin, with what it takes, and sends nothing", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    previews = [mixed()];
    mount();
    await screen.findByRole("button", { name: "Remove 5 items" });
    await waitFor(() => expect(client.getQueryState(["me"])?.status).toBe("success"));
    const keep = screen.getByRole("button", { name: "Keep web.1.aaa" });
    expect(keep.getAttribute("aria-disabled")).toBe("true");
    expect(document.getElementById(keep.getAttribute("aria-describedby") ?? "")?.textContent).toBe(
      "Needs Admin on Prod",
    );
    fireEvent.click(keep);
    expect(keepRequests).toEqual([]);
  });

  it("still offers Keep in a preview of a policy, where it is the only action", async () => {
    previews = [mixed()];
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <CleanupPreviewSheet
              mode="policy"
              envId={ENV}
              envName="Prod"
              swarm
              nodeCount={2}
              policy={{} as JanitorPolicyFields}
              onSavePolicy={async () => true}
              onOpenChange={() => {}}
            />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    await screen.findByText("web.1.aaa");
    expect(screen.queryAllByRole("checkbox")).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Keep web.1.aaa" }));
    await waitFor(() => expect(keepRequests).toHaveLength(1));
    await waitFor(() => expect(screen.queryByText("web.1.aaa")).toBeNull());
    expect(visibleLine()).toBe("Would remove 2 containers, 1 network, 1 image, ~500 MB");
  });
});
