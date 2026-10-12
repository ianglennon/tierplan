// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { PullItem, PullJob } from "@/api/pulls";
import { TooltipProvider } from "@/components/ui/tooltip";
import { PullProgress } from "@/features/pulls/pull-progress";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const REF = "docker.io/library/busybox:1.36.1";
const DIGEST = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

const item = (over: Partial<PullItem> = {}): PullItem => ({
  nodeId: "n1",
  name: "node-1",
  state: "waiting",
  code: "",
  message: "",
  current: 0,
  total: 0,
  unit: "",
  done: 0,
  count: 0,
  layers: [],
  digest: "",
  upToDate: false,
  ...over,
});

const job = (items: PullItem[], state: PullJob["state"] = "running"): PullJob => ({
  id: "j1",
  envId: "e1",
  ref: REF,
  credential: null,
  state,
  createdAt: "2026-10-11T10:00:00Z",
  finishedAt: state === "running" ? "" : "2026-10-11T10:01:00Z",
  items,
});

let client: QueryClient;

beforeEach(() => {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      if (new URL(req.url).pathname === "/api/me")
        return Response.json(makeMe({ envs: { e1: "operator" } }));
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(j: PullJob | undefined, extra: Partial<Parameters<typeof PullProgress>[0]> = {}) {
  const onClose = vi.fn();
  const onTryAgain = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <PullProgress
            envId="e1"
            imageRef={REF}
            nodes={[
              { nodeId: "n1", name: "node-1" },
              { nodeId: "n2", name: "node-2" },
            ]}
            job={j}
            status="live"
            onClose={onClose}
            onTryAgain={onTryAgain}
            {...extra}
          />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return { onClose, onTryAgain };
}

describe("PullProgress", () => {
  it("names the reference, says the pull is anonymous and that closing is safe", () => {
    mount(job([item()]));
    const title = screen.getByRole("heading", { name: `Pulling ${REF}` });
    expect(title.className).toContain("break-all");
    expect(screen.getByText("Credential: anonymous")).toBeTruthy();
    expect(screen.getByText("You can close this. The pull keeps running.")).toBeTruthy();
  });

  it("shows a row per node before the first snapshot, all waiting", () => {
    mount(undefined);
    const bars = screen.getAllByRole("progressbar");
    expect(bars.map((b) => b.getAttribute("aria-label"))).toEqual([
      "Pull progress on node-1",
      "Pull progress on node-2",
    ]);
    for (const bar of bars) expect(bar.hasAttribute("aria-valuenow")).toBe(false);
    expect(screen.getAllByText("Waiting")).toHaveLength(2);
  });

  it("keeps the bar indeterminate until the total is known, even while pulling", () => {
    mount(job([item({ state: "running" }), item({ nodeId: "n2", name: "node-2" })]));
    const bar = screen.getByRole("progressbar", { name: "Pull progress on node-1" });
    expect(bar.hasAttribute("aria-valuenow")).toBe(false);
    expect(within(bar.closest("li") as HTMLElement).getByText("Waiting")).toBeTruthy();
  });

  it("shows the amount in binary units and the layers, once the total is known", () => {
    mount(
      job([
        item({ state: "running", current: 512, total: 1024, unit: "bytes", done: 1, count: 2 }),
        item({ nodeId: "n2", name: "node-2" }),
      ]),
    );
    const bar = screen.getByRole("progressbar", { name: "Pull progress on node-1" });
    expect(bar.getAttribute("aria-valuenow")).toBe("50");
    expect(bar.getAttribute("aria-valuemin")).toBe("0");
    expect(bar.getAttribute("aria-valuemax")).toBe("100");
    const row = bar.closest("li") as HTMLElement;
    expect(within(row).getByText("Pulling")).toBeTruthy();
    expect(row.textContent).toContain("512 B of 1 KiB");
    expect(row.textContent).toContain("1 of 2 layers");
  });

  it("shows a finished node as Done with a short digest and a copy button, and the title changes", () => {
    mount(
      job(
        [
          item({ state: "done", digest: DIGEST, current: 10, total: 10, done: 2, count: 2 }),
          item({ nodeId: "n2", name: "node-2", state: "done", digest: DIGEST, upToDate: true }),
        ],
        "done",
      ),
    );
    expect(screen.getByRole("heading", { name: `Pulled ${REF}` })).toBeTruthy();
    expect(screen.getAllByText("Done")).toHaveLength(2);
    expect(screen.getAllByText("Digest sha256:0123456789ab")).toHaveLength(2);
    expect(screen.getAllByRole("button", { name: "Copy digest" })).toHaveLength(2);
    expect(screen.getAllByText("Already up to date")).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Close" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
  });

  it("shows a failure as an alert with the mapped sentence, never the code", () => {
    mount(
      job(
        [
          item({ state: "failed", code: "pull_denied", message: "The registry refused the pull." }),
          item({ nodeId: "n2", name: "node-2", state: "failed", code: "registry_unreachable" }),
        ],
        "failed",
      ),
    );
    const alerts = screen.getAllByRole("alert").map((a) => a.textContent);
    expect(alerts).toContain(
      "Pull denied by docker.io. Check that the image exists and that you can pull it.",
    );
    expect(alerts).toContain(
      "node-2 couldn't reach docker.io. Check its network, DNS and firewall.",
    );
    expect(document.body.textContent).not.toContain("pull_denied");
    expect(document.body.textContent).not.toContain("registry_unreachable");
    expect(screen.getByRole("heading", { name: `Couldn't pull ${REF}` })).toBeTruthy();
  });

  it("offers Try again only when every node failed, and never retries by itself", () => {
    const { onTryAgain } = mount(
      job(
        [
          item({ state: "failed", code: "pull_failed" }),
          item({ nodeId: "n2", name: "node-2", state: "failed", code: "pull_failed" }),
        ],
        "failed",
      ),
    );
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(onTryAgain).toHaveBeenCalledTimes(1);
  });

  it("words an unknown code with the generic sentence", () => {
    mount(job([item({ state: "failed", code: "something_new" })], "failed"));
    expect(
      screen.getByText("The pull failed on node-1. Check the node and try again."),
    ).toBeTruthy();
  });

  it("wraps long values, and shows messages and layer text as text, not markup", () => {
    mount(
      job([
        item({
          state: "running",
          current: 1,
          total: 2,
          count: 1,
          layers: [
            {
              id: "<img src=x onerror=alert(1)>",
              status: "<b>Downloading</b>",
              current: 1,
              total: 2,
            },
          ],
        }),
      ]),
    );
    fireEvent.click(screen.getByRole("button", { name: "Show layers on node-1" }));
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByText("<b>Downloading</b>")).toBeTruthy();
  });

  it("expands a node's layers with a real button that says so", () => {
    mount(
      job([
        item({
          state: "running",
          current: 512,
          total: 1024,
          done: 1,
          count: 2,
          layers: [
            { id: "4b3ffd8ccb52", status: "Pull complete", current: 100, total: 100 },
            { id: "9f5b1d72a8c1", status: "Downloading", current: 412, total: 924 },
          ],
        }),
      ]),
    );
    const toggle = screen.getByRole("button", { name: "Show layers on node-1" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(toggle.getAttribute("aria-controls")).toBeTruthy();
    const table = screen.getByRole("table", { name: "Layers on node-1" });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((c) => c.textContent),
    ).toEqual(["Layer", "Status", "Progress"]);
    expect(within(table).getByText("4b3ffd8ccb52")).toBeTruthy();
    expect(within(table).getByText("Pull complete")).toBeTruthy();
    expect(within(table).getByText("412 B of 924 B")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Hide layers on node-1" }));
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("says when the pull is gone or access was lost", () => {
    mount(job([item()]), { status: "closed", gone: true });
    expect(screen.getByText(/This pull is no longer here/)).toBeTruthy();
    cleanup();
    mount(job([item()]), { status: "closed", accessLost: true });
    expect(screen.getByText("You can no longer follow this pull.")).toBeTruthy();
  });

  it("closes through its Close button", () => {
    const { onClose } = mount(job([item()]));
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
