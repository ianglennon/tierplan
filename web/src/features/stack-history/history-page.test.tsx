// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { revisionsKey } from "@/api/stack-history";
import { TooltipProvider } from "@/components/ui/tooltip";
import { HistoryPage } from "@/features/stack-history/history-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type Json = Record<string, unknown>;

let role: "viewer" | "operator";
// Whether the person may read the instance settings (an instance administrator).
let settingsReader: boolean;
// How many revisions of each stack the instance keeps, as the list names it.
let kept: number;
// The revisions the server no longer holds although the list still names them: retention deleted them between
// the two reads.
let goneSeqs: Set<number>;
let revisions: Json[];
let listStatus: number;
let lastOperation: Json | undefined;
// Whether the stack holds a saved draft with changes that were not deployed.
let draftSaved: boolean;
let listCalls: number;
let client: QueryClient;

const DIGEST_A = `sha256:${"ab12".repeat(16)}`;
const DIGEST_B = `sha256:${"cd34".repeat(16)}`;

// A revision as the server lists it: good, a plain deploy, by ada, with nothing changed.
function revision(seq: number, over: Json = {}): Json {
  return {
    seq,
    origin: "deploy",
    restoredFrom: 0,
    outcome: "good",
    createdBy: "ada",
    createdAt: "2026-10-20T10:00:00Z",
    failedService: "",
    code: "",
    reason: "",
    deployed: false,
    images: [],
    pinned: 0,
    adoptedFrom: "",
    ...over,
  };
}

beforeEach(() => {
  role = "viewer";
  settingsReader = false;
  kept = 20;
  goneSeqs = new Set();
  listStatus = 200;
  listCalls = 0;
  draftSaved = false;
  lastOperation = undefined;
  revisions = [
    revision(3, {
      deployed: true,
      images: [{ service: "web", from: `web:1@${DIGEST_A}`, to: `web:2@${DIGEST_B}` }],
    }),
    revision(2, {
      outcome: "failed",
      failedService: "tracer_web",
      code: "service_rejected",
      reason: "The swarm rejected the service.",
    }),
    revision(1, { pinned: 1 }),
  ];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") {
        return Response.json(
          makeMe(settingsReader ? { instanceAdmin: true } : { envs: { [ENV]: role } }),
        );
      }
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/s1`) {
        return Response.json({
          id: "s1",
          name: "tracer",
          target: "swarm",
          source: "inline",
          state: "running",
          lastDeployedAt: "2026-10-20T10:00:00Z",
          lastDeployedBy: "ada",
          hasDraft: false,
          envId: ENV,
          sourceRef: "",
          createdBy: "ada",
          createdAt: "2026-10-19T10:00:00Z",
          stopped: false,
          ...(draftSaved ? { draftSavedAt: "2026-10-20T12:00:00Z", draftSavedBy: "ada" } : {}),
          lastOperation,
        });
      }
      if (path === `/api/env/${ENV}/stacks/s1/revisions`) {
        listCalls++;
        if (listStatus !== 200) {
          return Response.json(
            { type: "internal", title: "Server Error", status: listStatus, detail: "down" },
            { status: listStatus },
          );
        }
        return Response.json({ items: revisions, capped: false, kept });
      }
      const detail = /\/revisions\/(\d+)$/.exec(path);
      if (detail && goneSeqs.has(Number(detail[1]))) {
        return Response.json(
          { type: "revision_not_found", title: "Not Found", status: 404, detail: "gone" },
          { status: 404 },
        );
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function page(rev?: number) {
  return (
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <HistoryPage envId={ENV} stackId="s1" rev={rev} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>
  );
}

function mount(rev?: number) {
  return render(page(rev));
}

async function table() {
  return screen.findByRole("table", { name: "Revisions of tracer" });
}

// The row of a revision is the one whose first cell links to it, not one that names it as the revision it
// rolled back.
function rowOf(seq: number) {
  const row = screen.getAllByRole("row").find((r) => {
    const link = r.querySelector("td:first-child a");
    return link?.textContent === `#${seq}`;
  });
  if (!row) throw new Error(`no row for revision ${seq}`);
  return row;
}

function highlighted(row: HTMLElement) {
  return row.className.split(/\s+/).includes("bg-muted");
}

// Retention (plan 08-06, STK-08, D-01): the note under the table, and the gone state naming the limit.
describe("the retention note", () => {
  const NOTE =
    "Coaming keeps the last 5 revisions of each stack. The deployed revision and the last good one are always kept. Older revisions are deleted, along with their variables and secret files.";

  it("says how many revisions are kept and what always stays, under the table", async () => {
    kept = 5;
    mount();

    const tbl = await table();
    const note = await screen.findByText(NOTE);
    expect(tbl.compareDocumentPosition(note) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("names the instance's own limit", async () => {
    kept = 50;
    mount();

    expect(
      await screen.findByText(/^Coaming keeps the last 50 revisions of each stack\./),
    ).toBeTruthy();
  });

  it("offers Open settings to someone who may read the settings", async () => {
    kept = 5;
    settingsReader = true;
    mount();

    await screen.findByText(NOTE);
    const link = screen.getByRole("link", { name: "Open settings" });
    expect(link.getAttribute("href")).toBe("/settings/general");
  });

  it("offers no way to the settings to someone who may not read them", async () => {
    kept = 5;
    mount();

    await screen.findByText(NOTE);
    expect(screen.queryByRole("link", { name: "Open settings" })).toBeNull();
  });

  it("names the limit in the gone state of a revision the list does not hold", async () => {
    kept = 5;
    mount(99);

    expect(await screen.findByRole("heading", { name: "That revision is gone" })).toBeTruthy();
    expect(
      screen.getByText(
        "Coaming keeps the last 5 revisions of each stack, and this one was deleted.",
      ),
    ).toBeTruthy();
  });

  it("names the same limit when the server says the revision was deleted since the list was read", async () => {
    kept = 7;
    goneSeqs = new Set([3]);
    mount(3);

    expect(await screen.findByRole("heading", { name: "That revision is gone" })).toBeTruthy();
    expect(
      screen.getByText(
        "Coaming keeps the last 7 revisions of each stack, and this one was deleted.",
      ),
    ).toBeTruthy();
  });
});

describe("the revision table", () => {
  it("lists the revisions newest first under a caption, with the columns and the count", async () => {
    mount();

    const tbl = await table();
    expect(
      within(tbl)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Revision", "Kind", "Result", "Who", "When", "Images", "Actions"]);
    const numbers = within(tbl)
      .getAllByRole("link", { name: /^#\d+$/ })
      .map((l) => l.textContent);
    expect(numbers).toEqual(["#3", "#2", "#1"]);
    expect(screen.getByText("3 revisions")).toBeTruthy();
    expect(
      screen.getByText(
        "Every deploy of tracer, newest first. Select a revision to see it in detail.",
      ),
    ).toBeTruthy();
  });

  it("makes each revision number a link that selects it, and badges the deployed one", async () => {
    mount();

    await table();
    expect(screen.getByRole("link", { name: "#2" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/history?rev=2",
    );
    expect(within(rowOf(3)).getByText("Deployed")).toBeTruthy();
    expect(within(rowOf(2)).queryByText("Deployed")).toBeNull();
  });

  it("reads Deploy, Update images, Roll back of #N as a link, and Adopted from the project", async () => {
    revisions = [
      revision(4, { origin: "rollback", restoredFrom: 1, deployed: true }),
      revision(3, { origin: "update" }),
      revision(2, { origin: "deploy" }),
      revision(1, { origin: "adopted", adoptedFrom: "legacy-proj", pinned: 2 }),
    ];
    mount();

    await table();
    const rollback = within(rowOf(4));
    expect(rollback.getByText("Roll back")).toBeTruthy();
    expect(rollback.getByRole("link", { name: "#1" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/history?rev=1",
    );
    expect(within(rowOf(3)).getByText("Update images")).toBeTruthy();
    expect(within(rowOf(2)).getByText("Deploy")).toBeTruthy();
    const adopted = within(rowOf(1));
    expect(adopted.getByText("Adopted")).toBeTruthy();
    expect(adopted.getByText("from legacy-proj")).toBeTruthy();
  });

  it("lists a freshly adopted stack as one row: Adopted from the project, Good, the images it pinned, and Deployed", async () => {
    revisions = [
      revision(1, {
        origin: "adopted",
        adoptedFrom: "legacy",
        pinned: 2,
        outcome: "good",
        deployed: true,
      }),
    ];
    mount();

    await table();
    const row = within(rowOf(1));
    expect(row.getByText("Adopted")).toBeTruthy();
    expect(row.getByText("from legacy")).toBeTruthy();
    expect(row.getByText("Good")).toBeTruthy();
    expect(row.getByText("2 images pinned")).toBeTruthy();
    expect(row.getByText("Deployed")).toBeTruthy();
    expect(screen.getAllByRole("row")).toHaveLength(2);
  });

  it("shows Good, Failed with its reason, and In progress", async () => {
    revisions = [
      revision(3, { outcome: "pending" }),
      revision(2, { outcome: "failed", failedService: "tracer_web", code: "service_rejected" }),
      revision(1, { deployed: true }),
    ];
    mount();

    await table();
    expect(within(rowOf(1)).getByText("Good")).toBeTruthy();
    expect(within(rowOf(3)).getByText("In progress")).toBeTruthy();
    const failed = within(rowOf(2));
    expect(failed.getByText("Failed")).toBeTruthy();
    expect(failed.getByText("tracer_web: Docker rejected the service.")).toBeTruthy();
  });

  it("clamps a failure reason to one line, with the whole text in a Tooltip", async () => {
    revisions = [
      revision(1, {
        outcome: "failed",
        failedService: "tracer_web",
        code: "",
        reason: "",
      }),
    ];
    mount();

    await table();
    const reason = within(rowOf(1)).getByText("tracer_web: The deploy didn't finish.");
    expect(reason.className).toContain("truncate");
    reason.focus();
    expect(
      (await screen.findAllByText("tracer_web: The deploy didn't finish.")).length,
    ).toBeGreaterThan(1);
  });

  it("tells how many images changed with the digests in a Tooltip, and the other cases in words", async () => {
    revisions = [
      revision(4, {
        deployed: true,
        images: [
          { service: "web", from: `web:1@${DIGEST_A}`, to: `web:2@${DIGEST_B}` },
          { service: "db", from: "", to: `db:1@${DIGEST_B}` },
        ],
      }),
      revision(3, { outcome: "failed", code: "service_rejected" }),
      revision(2, {}),
      revision(1, { pinned: 3 }),
    ];
    mount();

    await table();
    const changed = within(rowOf(4)).getByRole("button", { name: "2 images changed" });
    changed.focus();
    expect(
      (await screen.findAllByText("web: sha256:ab12ab12ab12 → sha256:cd34cd34cd34")).length,
    ).toBeGreaterThan(0);
    expect(screen.getAllByText("db: new, sha256:cd34cd34cd34").length).toBeGreaterThan(0);
    // A failed revision that never resolved its images has a dash; a good one with no change says so.
    expect(within(rowOf(3)).getByText("—")).toBeTruthy();
    expect(within(rowOf(2)).getByText("No image changes")).toBeTruthy();
    expect(within(rowOf(1)).getByText("3 images pinned")).toBeTruthy();
  });

  it("counts one revision and one image in the singular", async () => {
    revisions = [
      revision(1, {
        deployed: true,
        images: [{ service: "web", from: "", to: `web:1@${DIGEST_A}` }],
      }),
    ];
    mount();

    await table();
    expect(screen.getByText("1 revision")).toBeTruthy();
    expect(screen.getByRole("button", { name: "1 image changed" })).toBeTruthy();
  });

  it.each([20, 200])("scrolls inside a body of 8 rows with %i revisions", async (n) => {
    revisions = Array.from({ length: n }, (_, i) => revision(n - i, { deployed: i === 0 }));
    mount();

    const tbl = await table();
    expect(screen.getByText(`${n} revisions`)).toBeTruthy();
    expect(within(tbl).getAllByRole("link", { name: /^#\d+$/ })).toHaveLength(n);
    const scroller = tbl.parentElement;
    expect(scroller?.className).toContain("max-h-[324px]");
    expect(scroller?.className).toContain("overflow-auto");
  });

  it("truncates a long user name and draws markup in it as text", async () => {
    revisions = [revision(1, { deployed: true, createdBy: "<b>bold</b>" })];
    mount();

    await table();
    const name = within(rowOf(1)).getByText("<b>bold</b>");
    expect(name.className).toContain("truncate");
    expect(name.querySelector("b")).toBeNull();
  });

  it("opens a revision from its row menu", async () => {
    mount();

    await table();
    const trigger = within(rowOf(2)).getByRole("button", { name: "Actions for revision #2" });
    await act(async () => {
      fireEvent.keyDown(trigger, { key: "Enter" });
    });
    const item = await screen.findByRole("menuitem", { name: "Open revision #2" });
    expect(item.getAttribute("href")).toBe("/env/e1/stacks/s1/history?rev=2");
  });

  it("highlights a revision that appears while the tab is open", async () => {
    mount();
    await table();
    expect(highlighted(rowOf(3))).toBe(false);

    revisions = [revision(4, { outcome: "pending" }), ...revisions];
    await act(async () => {
      await client.invalidateQueries({ queryKey: revisionsKey(ENV, "s1") });
    });

    await waitFor(() => expect(screen.getByRole("link", { name: "#4" })).toBeTruthy());
    expect(highlighted(rowOf(4))).toBe(true);
    expect(rowOf(4).className).toContain("motion-reduce:bg-transparent");
    expect(highlighted(rowOf(3))).toBe(false);
  });
});

describe("the selected revision", () => {
  it("is the deployed one when the address names none", async () => {
    mount();

    const heading = await screen.findByRole("heading", { name: "Revision #3" });
    expect(heading).toBeTruthy();
    expect(rowOf(3).getAttribute("aria-current")).toBe("true");
    expect(rowOf(2).getAttribute("aria-current")).toBeNull();
    expect(screen.getByText("This is the deployed revision.")).toBeTruthy();
  });

  it("is the newest when none is deployed", async () => {
    revisions = [
      revision(2, { outcome: "failed", code: "service_rejected" }),
      revision(1, { outcome: "failed" }),
    ];
    mount();

    expect(await screen.findByRole("heading", { name: "Revision #2" })).toBeTruthy();
    expect(rowOf(2).getAttribute("aria-current")).toBe("true");
  });

  it("is the one the address names, and focus moves to its heading when the choice changes", async () => {
    const view = mount();
    await screen.findByRole("heading", { name: "Revision #3" });
    expect(document.activeElement).not.toBe(screen.getByRole("heading", { name: "Revision #3" }));

    view.rerender(page(1));

    const heading = await screen.findByRole("heading", { name: "Revision #1" });
    expect(rowOf(1).getAttribute("aria-current")).toBe("true");
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(heading.getAttribute("tabindex")).toBe("-1");
  });

  it("names who made it and when, and for a roll back the revision it copied", async () => {
    revisions = [
      revision(2, { origin: "rollback", restoredFrom: 1, deployed: true, createdBy: "bob" }),
      revision(1),
    ];
    mount();

    const heading = await screen.findByRole("heading", { name: "Revision #2" });
    const meta = heading.parentElement?.querySelector("p");
    expect(meta?.textContent).toMatch(/^Roll back of #1 by bob,/);
    expect(
      within(meta as HTMLElement)
        .getByRole("link", { name: "#1" })
        .getAttribute("href"),
    ).toBe("/env/e1/stacks/s1/history?rev=1");
  });

  it("says a failed revision failed, why, and that it can't be rolled back to", async () => {
    mount(2);

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("Revision #2 failed")).toBeTruthy();
    expect(within(alert).getByText("tracer_web: Docker rejected the service.")).toBeTruthy();
    expect(
      within(alert).getByText("It stays in the list for the record. It can't be rolled back to."),
    ).toBeTruthy();
    expect(screen.queryByText("This is the deployed revision.")).toBeNull();
  });

  it("says a revision is still deploying, with the way to its console", async () => {
    revisions = [revision(2, { outcome: "pending" }), revision(1, { deployed: true })];
    lastOperation = {
      kind: "deploy",
      state: "running",
      jobId: "job-9",
      startedBy: "ada",
      startedAt: "2026-10-20T10:00:00Z",
      endedAt: "",
      failedService: "",
      code: "",
    };
    mount(2);

    expect(await screen.findByText("Revision #2 is still deploying")).toBeTruthy();
    const link = await screen.findByRole("link", { name: "Open console" });
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/s1/deploys/job-9");
  });

  it("says a revision the list does not hold is gone, and links back to the history", async () => {
    mount(99);

    expect(await screen.findByRole("heading", { name: "That revision is gone" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to history" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/history",
    );
    expect(rowOf(3).getAttribute("aria-current")).toBeNull();
  });
});

// Rolling back to a revision (plan 08-03, STK-08, D-02, D-06, D-51): the detail card offers it, the review
// confirms it.
describe("the Roll back action of the selected revision", () => {
  it("opens the review of a good revision that is not deployed, for an Operator", async () => {
    role = "operator";
    mount(1);

    const link = await screen.findByRole("link", { name: "Roll back to revision #1" });
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/s1/review?mode=rollback&revision=1");
  });

  it("is not offered on the deployed revision, which says so", async () => {
    role = "operator";
    mount(3);

    expect(await screen.findByText("This is the deployed revision.")).toBeTruthy();
    expect(screen.queryByText(/Roll back to revision/)).toBeNull();
  });

  it("is not offered on a failed revision, which shows the failed alert", async () => {
    role = "operator";
    mount(2);

    expect(await screen.findByText("Revision #2 failed")).toBeTruthy();
    expect(screen.queryByText(/Roll back to revision/)).toBeNull();
  });

  it("is not offered on a revision that is still deploying", async () => {
    role = "operator";
    revisions = [revision(2, { outcome: "pending" }), revision(1, { deployed: true })];
    mount(2);

    expect(await screen.findByText("Revision #2 is still deploying")).toBeTruthy();
    expect(screen.queryByText(/Roll back to revision/)).toBeNull();
  });

  it("is not offered anywhere on a stack whose only revision is the deployed one", async () => {
    role = "operator";
    revisions = [revision(1, { deployed: true })];
    mount();

    expect(await screen.findByText("This is the deployed revision.")).toBeTruthy();
    expect(screen.queryByText(/Roll back to revision/)).toBeNull();
  });

  it("is off for a Viewer, with the reason, and leads nowhere", async () => {
    mount(1);

    const button = await screen.findByRole("button", { name: "Roll back to revision #1" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    expect(screen.queryByRole("link", { name: "Roll back to revision #1" })).toBeNull();
  });

  it("says the saved draft stays as it is, when there is one", async () => {
    role = "operator";
    draftSaved = true;
    mount(1);

    expect(await screen.findByText("Your saved draft stays as it is.")).toBeTruthy();
  });

  it("does not mention a draft when there is none", async () => {
    role = "operator";
    mount(1);

    await screen.findByRole("link", { name: "Roll back to revision #1" });
    expect(screen.queryByText("Your saved draft stays as it is.")).toBeNull();
  });
});

describe("the Roll back action of a revision's row menu", () => {
  async function menuOf(seq: number) {
    const trigger = within(rowOf(seq)).getByRole("button", {
      name: `Actions for revision #${seq}`,
    });
    await act(async () => {
      fireEvent.keyDown(trigger, { key: "Enter" });
    });
    return screen.findAllByRole("menuitem");
  }

  it("offers an Operator Roll back to revision #n… on a good revision that is not deployed", async () => {
    role = "operator";
    mount();

    await table();
    const items = await menuOf(1);
    expect(items.map((i) => i.textContent)).toEqual([
      "Open revision #1",
      "Roll back to revision #1…",
    ]);
    expect(items[1]?.getAttribute("href")).toBe(
      "/env/e1/stacks/s1/review?mode=rollback&revision=1",
    );
  });

  it("offers a failed revision, an in-progress one and the deployed one only Open revision", async () => {
    role = "operator";
    revisions = [
      revision(4, { outcome: "pending" }),
      revision(3, { deployed: true }),
      revision(2, { outcome: "failed", failedService: "tracer_web", code: "service_rejected" }),
      revision(1),
    ];
    mount();

    await table();
    for (const seq of [4, 3, 2]) {
      const items = await menuOf(seq);
      expect(
        items.map((i) => i.textContent),
        `revision ${seq}`,
      ).toEqual([`Open revision #${seq}`]);
      await act(async () => {
        fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
      });
    }
  });

  it("leaves a Viewer with Open revision only", async () => {
    mount();

    await table();
    expect((await menuOf(1)).map((i) => i.textContent)).toEqual(["Open revision #1"]);
  });
});

describe("the states of the tab", () => {
  it("shows 8 skeleton rows and a skeleton detail while the list loads", async () => {
    mount();

    const loading = screen.getByTestId("history-loading");
    expect(loading.getAttribute("aria-busy")).toBe("true");
    expect(loading.querySelectorAll("[data-slot=skeleton]")).toHaveLength(9);
    await table();
    expect(screen.queryByTestId("history-loading")).toBeNull();
  });

  it("says there are no revisions yet, and offers Review and deploy to a user who may deploy", async () => {
    role = "operator";
    revisions = [];
    mount();

    expect(await screen.findByRole("heading", { name: "No revisions yet" })).toBeTruthy();
    expect(
      screen.getByText(
        "Every deploy of tracer is listed here, so you can compare it with what's running and roll back to it.",
      ),
    ).toBeTruthy();
    const link = await screen.findByRole("link", { name: "Review and deploy" });
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/s1/review");
  });

  it("offers a Viewer no way to deploy from the empty tab", async () => {
    revisions = [];
    mount();

    await screen.findByRole("heading", { name: "No revisions yet" });
    expect(screen.queryByRole("link", { name: "Review and deploy" })).toBeNull();
  });

  it("says it couldn't load the history, and reads it again on Retry", async () => {
    listStatus = 500;
    mount();

    expect(await screen.findByText("Couldn't load the history.")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    const before = listCalls;
    listStatus = 200;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading history" }));

    expect(await table()).toBeTruthy();
    expect(listCalls).toBeGreaterThan(before);
  });
});
