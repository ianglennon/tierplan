// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { StackReview } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RevertsDriftSection } from "@/features/stack-review/reverts-drift-section";
import { ReviewPage } from "@/features/stack-review/review-page";
import { ReviewSummary } from "@/features/stack-review/review-summary";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Reverts drift section of a review (plan 08-25, STK-14, D-25): the changes made outside the stack that a deploy
// would put back, apart from the draft's own. The clock is held, so a relative time reads the same on every run.

type Revert = NonNullable<StackReview["revertsDrift"]>[number];

const NOW = new Date("2026-10-20T14:00:00Z");

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date", "setInterval", "clearInterval"] });
  vi.setSystemTime(NOW);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const scaled: Revert = {
  service: "web",
  change: "changed",
  path: "Mode.Replicated.Replicas",
  live: "3",
  desired: "1",
  cause: {
    action: "scaled",
    service: "web",
    by: "alice",
    at: "2026-10-20T12:00:00Z",
    from: "1",
    to: "3",
  },
};

const byHand: Revert = {
  service: "api",
  change: "changed",
  path: "TaskTemplate.ContainerSpec.Env[HOTFIX]",
  live: "HOTFIX=1",
  desired: "",
};

function row(n: number): Revert {
  return {
    service: `svc${n}`,
    change: "changed",
    path: "Mode.Replicated.Replicas",
    live: String(n + 10),
    desired: String(n),
  };
}

function mount(reverts: Revert[], unavailable = false, mode?: "deploy" | "update" | "rollback") {
  return render(
    <TooltipProvider>
      <RevertsDriftSection reverts={reverts} unavailable={unavailable} mode={mode} />
    </TooltipProvider>,
  );
}

describe("the section", () => {
  it("is a headed region that is not an alert, titled with how many changes it reverts", () => {
    mount([scaled, byHand]);

    const region = screen.getByRole("region", {
      name: "This deploy reverts 2 changes made outside the stack",
    });
    expect(
      within(region).getByText(
        "Your draft puts these back to what was deployed. Check that none of them is a hot-fix you want to keep.",
      ),
    ).toBeTruthy();
    // It informs and never blocks, so it must not be announced as an alert.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("words one change in the singular and names an update or a roll back for what it is", () => {
    const { unmount } = mount([scaled]);
    expect(
      screen.getByRole("region", { name: "This deploy reverts 1 change made outside the stack" }),
    ).toBeTruthy();
    unmount();

    const update = mount([scaled], false, "update");
    expect(
      screen.getByRole("region", { name: "This update reverts 1 change made outside the stack" }),
    ).toBeTruthy();
    update.unmount();

    mount([scaled], false, "rollback");
    expect(
      screen.getByRole("region", {
        name: "This roll back reverts 1 change made outside the stack",
      }),
    ).toBeTruthy();
    expect(screen.getByText(/The revision you're rolling back to puts these back/)).toBeTruthy();
  });

  it("lists each change as the service, the setting, what runs now and what the deploy sets, with the cause under it", () => {
    mount([scaled, byHand]);

    const items = screen.getAllByRole("listitem");
    expect(items).toHaveLength(2);
    const [first, second] = items as [HTMLElement, HTMLElement];
    expect(within(first).getByText("web")).toBeTruthy();
    expect(within(first).getByText("3")).toBeTruthy();
    expect(within(first).getByText("1")).toBeTruthy();
    expect(within(first).getByText("Mode.Replicated.Replicas")).toBeTruthy();
    expect(first.textContent).toContain("web was scaled from 1 to 3 by alice in Bayplan, 2 h ago.");
    // Nothing in the audit log explains the second one, which reads as a change made outside Bayplan.
    expect(within(second).getByText("HOTFIX=1")).toBeTruthy();
    expect(within(second).getByText("not set")).toBeTruthy();
    expect(second.textContent).toContain("Changed outside Bayplan.");
  });

  it("says a service is created again or removed in words", () => {
    mount([
      { service: "gone", change: "created", path: "", live: "", desired: "" },
      { service: "extra", change: "removed", path: "", live: "", desired: "" },
    ]);

    const [created, removed] = screen.getAllByRole("listitem") as [HTMLElement, HTMLElement];
    expect(created.textContent).toContain("gone");
    expect(created.textContent).toContain("no longer runs. The deploy creates it again.");
    expect(removed.textContent).toContain("extra");
    expect(removed.textContent).toContain(
      "isn't in the deployed revision or your draft. The deploy removes it.",
    );
  });

  it("draws every value as text, and a hidden value as bullets", () => {
    const markup = '<img src=x onerror="alert(1)">';
    const { container } = mount([
      {
        service: "web",
        change: "changed",
        path: "TaskTemplate.ContainerSpec.Env[API_KEY]",
        live: "API_KEY=<hidden>",
        desired: markup,
      },
    ]);

    expect(screen.getByText("API_KEY=••••")).toBeTruthy();
    expect(screen.queryByText(/<hidden>/)).toBeNull();
    expect(screen.getByText(markup)).toBeTruthy();
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByText(markup).className).toContain("font-mono");
    expect(screen.getByText(markup).className).toContain("break-all");
  });
});

describe("a long list", () => {
  it("shows five rows and keeps the rest behind Show all", () => {
    mount([1, 2, 3, 4, 5, 6, 7].map(row));

    expect(screen.getAllByRole("listitem")).toHaveLength(5);
    expect(screen.queryByText("svc6")).toBeNull();
    const more = screen.getByRole("button", { name: "Show all 7" });
    expect(more.getAttribute("aria-expanded")).toBe("false");
    expect(more.getAttribute("aria-controls")).toBe(screen.getByRole("list").id);

    fireEvent.click(more);

    expect(screen.getAllByRole("listitem")).toHaveLength(7);
    expect(screen.getByText("svc7")).toBeTruthy();
    const fewer = screen.getByRole("button", { name: "Show fewer" });
    expect(fewer.getAttribute("aria-expanded")).toBe("true");

    fireEvent.click(fewer);
    expect(screen.getAllByRole("listitem")).toHaveLength(5);
  });

  it("has no Show all when five rows or fewer fit", () => {
    mount([1, 2, 3, 4, 5].map(row));

    expect(screen.getAllByRole("listitem")).toHaveLength(5);
    expect(screen.queryByRole("button")).toBeNull();
  });
});

describe("when there is nothing to show", () => {
  it("draws nothing for a deploy that reverts no drift", () => {
    const { container } = mount([]);

    expect(container.textContent).toBe("");
  });

  it("is replaced by one line when the drift could not be computed", () => {
    mount([scaled], true);

    expect(screen.getByRole("status").textContent).toBe(
      "Couldn't check for drift; the changes below are still accurate.",
    );
    expect(screen.queryByRole("region")).toBeNull();
    expect(screen.queryByRole("list")).toBeNull();
  });
});

describe("the Summary", () => {
  const counts = { added: 0, changed: 0, removed: 0 };
  const summary = (revertsDrift: number): StackReview["summary"] => ({
    services: counts,
    networks: counts,
    volumes: counts,
    secrets: counts,
    configs: counts,
    imagesNew: 0,
    imagesPinned: 0,
    ownershipOnly: 0,
    revertsDrift,
  });

  it("adds a Reverts drift line when it is not zero", () => {
    const { unmount } = render(<ReviewSummary summary={summary(2)} />);
    expect(screen.getByText("Reverts drift: 2")).toBeTruthy();
    unmount();

    render(<ReviewSummary summary={summary(0)} />);
    expect(screen.queryByText(/Reverts drift/)).toBeNull();
  });
});

// The page: the section sits above the Summary, and Deploy stack does not look at it.
describe("in the review page", () => {
  const ENV = "e1";
  const STACK = "s1";
  const D1 = `sha256:${"a1".repeat(32)}`;
  const counts = (added = 0, changed = 0, removed = 0) => ({ added, changed, removed });
  let review: Record<string, unknown>;
  let client: QueryClient;

  beforeEach(() => {
    review = {
      draftVersion: 7,
      images: [
        { service: "web", image: "nginx:1.27", digest: D1, current: D1, state: "pinned", note: "" },
      ],
      needsAdmin: [],
      objects: [],
      pins: { s1_web: `nginx:1.27@${D1}` },
      problems: [],
      services: [
        {
          name: "s1_web",
          short: "web",
          change: "changed",
          current: { Name: "s1_web", Mode: { Replicated: { Replicas: 3 } } },
          next: {
            Name: "s1_web",
            Mode: { Replicated: { Replicas: 3 } },
            Labels: { tier: "front" },
          },
          hiddenChanges: [],
          settings: 1,
        },
      ],
      summary: {
        services: counts(0, 1, 0),
        networks: counts(),
        volumes: counts(),
        secrets: counts(),
        configs: counts(),
        imagesNew: 0,
        imagesPinned: 1,
        ownershipOnly: 0,
        revertsDrift: 1,
      },
      revertsDrift: [scaled],
      revertsDriftUnavailable: false,
      revision: 0,
      deployedSeq: 2,
    };
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    setCsrfToken(null);
    const me = makeMe({ envs: { [ENV]: "operator" } });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const path = new URL(req.url).pathname;
        if (path === "/api/me") return Response.json(me);
        if (path === "/api/environments")
          return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
        if (path === `/api/env/${ENV}/stacks/${STACK}`) {
          return Response.json({
            id: STACK,
            name: "web",
            target: "swarm",
            source: "inline",
            state: "running",
            hasDraft: true,
            envId: ENV,
            sourceRef: "",
            createdBy: "ada",
            createdAt: "2026-10-19T10:00:00Z",
            stopped: false,
          });
        }
        if (path === `/api/env/${ENV}/stacks/${STACK}/review`) return Response.json(review);
        return Response.json({}, { status: 404 });
      }),
    );
  });

  function page(mode?: "restore") {
    return render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <ReviewPage envId={ENV} stackId={STACK} mode={mode} onDeployed={vi.fn()} />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
  }

  it("shows the section above the Summary and leaves Deploy stack enabled", async () => {
    page();

    const region = await screen.findByRole("region", {
      name: "This deploy reverts 1 change made outside the stack",
    });
    const summary = await screen.findByRole("table", { name: "Summary of changes" });
    expect(region.compareDocumentPosition(summary) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByText("Reverts drift: 1")).toBeTruthy();
    const deploy = screen.getByRole("button", { name: "Deploy stack" });
    expect(deploy.getAttribute("aria-disabled")).toBeNull();
    expect((deploy as HTMLButtonElement).disabled).toBe(false);
  });

  it("keeps the rest of the review when the drift could not be checked", async () => {
    review.revertsDrift = [];
    review.revertsDriftUnavailable = true;
    page();

    expect(
      await screen.findByText("Couldn't check for drift; the changes below are still accurate."),
    ).toBeTruthy();
    expect(await screen.findByRole("table", { name: "Summary of changes" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: /reverts/ })).toBeNull();
    expect(
      screen.getByRole("button", { name: "Deploy stack" }).getAttribute("aria-disabled"),
    ).toBeNull();
  });

  it("has no section in a restore, whose whole Changes tab is the drift", async () => {
    review.revision = 2;
    page("restore");

    await screen.findByRole("table", { name: "Summary of changes" });
    expect(screen.queryByRole("region", { name: /reverts/ })).toBeNull();
  });
});
