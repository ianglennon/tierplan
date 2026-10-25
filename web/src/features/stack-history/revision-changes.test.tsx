// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { RevisionChanges } from "@/features/stack-history/revision-changes";
import "@/i18n";

const ENV = "e1";

type Json = Record<string, unknown>;

let client: QueryClient;
let status: number;
let problemType: string;
let body: Json;
let calls: number;

const DIGEST_A = `sha256:${"ab12".repeat(16)}`;
const DIGEST_B = `sha256:${"cd34".repeat(16)}`;

// Rolling back to revision 1 would move web from 1.28 back to 1.27, and an environment value that the server
// withheld differs between the two.
function webChanged(): Json {
  const spec = (image: string) => ({
    TaskTemplate: { ContainerSpec: { Image: image, Env: ["API_KEY=<hidden>", "MODE=<hidden>"] } },
  });
  return {
    name: "tracer_web",
    short: "web",
    change: "changed",
    current: spec(`nginx:1.28-alpine@${DIGEST_B}`),
    next: spec(`nginx:1.27-alpine@${DIGEST_A}`),
    hiddenChanges: ["TaskTemplate.ContainerSpec.Env[API_KEY]"],
    settings: 2,
  };
}

beforeEach(() => {
  status = 200;
  problemType = "";
  calls = 0;
  body = {
    deployedSeq: 2,
    services: [webChanged()],
    summary: { added: 0, changed: 1, removed: 0 },
    identical: false,
  };
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (/^\/api\/env\/e1\/stacks\/s1\/revisions\/\d+\/changes$/.test(path)) {
        calls++;
        if (status !== 200) {
          return Response.json(
            { type: problemType, title: "Problem", status, detail: "x" },
            { status },
          );
        }
        return Response.json(body);
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

function mount(seq = 1, deployedSeq = 2, pending = false) {
  return render(
    <QueryClientProvider client={client}>
      <RevisionChanges
        envId={ENV}
        stackId="s1"
        seq={seq}
        deployedSeq={deployedSeq}
        pending={pending}
      />
    </QueryClientProvider>,
  );
}

describe("the Changes tab", () => {
  it("captions the comparison and lists each service the roll back would change", async () => {
    mount();

    expect(
      await screen.findByText(
        "What rolling back to #1 would change, compared with the deployed revision #2.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("web")).toBeTruthy();
    expect(screen.getAllByText("Changed").length).toBeGreaterThan(0);
    expect(screen.getByText("nginx:1.28-alpine")).toBeTruthy();
    expect(screen.getByText("nginx:1.27-alpine")).toBeTruthy();
    expect(calls).toBe(1);
  });

  it("shows a setting that differs only in a hidden value by its path, with no value", async () => {
    mount();

    const table = await screen.findByRole("table", { name: "Hidden changes to web" });
    expect(within(table).getByText("TaskTemplate.ContainerSpec.Env[API_KEY]")).toBeTruthy();
    expect(screen.queryByText("<hidden>")).toBeNull();
  });
});

describe("settings that differ because of drift", () => {
  const IMAGE = "TaskTemplate.ContainerSpec.Image";

  // The row of a setting in the opened service, found by the path it prints under its label.
  function rowOf(path: string): HTMLElement {
    const cell = screen.getByText(path);
    const row = cell.closest("tr");
    if (!row) throw new Error(`no table row holds ${path}`);
    return row;
  }

  it("marks a setting the stack's drift check found changed outside the stack, and only that setting", async () => {
    body = { ...body, driftedPaths: { tracer_web: [IMAGE] } };
    mount();

    await screen.findByText("web");
    expect(within(rowOf(IMAGE)).getByText("Drifted")).toBeTruthy();
    expect(within(rowOf("TaskTemplate.ContainerSpec.Env[MODE]")).queryByText("Drifted")).toBeNull();
    expect(screen.getAllByText("Drifted")).toHaveLength(1);
  });

  it("marks a drifted setting whose value is hidden by its path", async () => {
    body = { ...body, driftedPaths: { tracer_web: ["TaskTemplate.ContainerSpec.Env[API_KEY]"] } };
    mount();

    const table = await screen.findByRole("table", { name: "Hidden changes to web" });
    expect(within(table).getByText("Drifted")).toBeTruthy();
  });

  it("marks nothing when the stack has no drift result, or another service drifted", async () => {
    body = { ...body, driftedPaths: { tracer_db: [IMAGE] } };
    mount();

    await screen.findByText("web");
    expect(screen.queryByText("Drifted")).toBeNull();
  });
});

describe("when nothing would change", () => {
  beforeEach(() => {
    body = {
      deployedSeq: 2,
      services: [],
      summary: { added: 0, changed: 0, removed: 0 },
      identical: true,
    };
  });

  it("says the deployed revision has no changes", async () => {
    mount(2, 2);

    expect(await screen.findByText("No changes")).toBeTruthy();
    expect(screen.getByText("This is the deployed revision.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("says a revision with the same content as the deployed one has no changes", async () => {
    mount(1, 2);

    expect(await screen.findByText("No changes")).toBeTruthy();
    expect(
      screen.getByText("Revision #1 has the same content as the deployed revision."),
    ).toBeTruthy();
  });

  it("lists a difference of the deployed revision, which is drift, instead of saying there is none", async () => {
    body = {
      deployedSeq: 2,
      services: [webChanged()],
      summary: { added: 0, changed: 1, removed: 0 },
      identical: false,
    };
    mount(2, 2);

    expect(await screen.findByText("web")).toBeTruthy();
    expect(screen.queryByText("No changes")).toBeNull();
  });
});

describe("a revision that cannot be rolled back to", () => {
  it("says a failed revision failed, with nothing to retry", async () => {
    status = 409;
    problemType = "revision_not_good";
    mount();

    expect(
      await screen.findByText("Revision #1 failed, so there is nothing to roll back to."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(calls).toBe(1);
  });

  it("says a revision still deploying has no changes to show yet", async () => {
    status = 409;
    problemType = "revision_not_good";
    mount(1, 2, true);

    expect(
      await screen.findByText(
        "Revision #1 is still deploying. Its changes show once it has finished.",
      ),
    ).toBeTruthy();
    expect(screen.queryByText(/failed/)).toBeNull();
  });

  it("says a revision that can no longer be redeployed has nothing to compare", async () => {
    status = 409;
    problemType = "revision_not_deployable";
    mount();

    expect(
      await screen.findByText(
        "Revision #1 can't be redeployed as it stands, so there is nothing to compare.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });
});

describe("loading and failing", () => {
  it("shows a skeleton while the changes load", async () => {
    mount();

    const loading = screen.getByTestId("revision-changes-loading");
    expect(loading.getAttribute("aria-busy")).toBe("true");
    await screen.findByText(/What rolling back to #1 would change/);
    expect(screen.queryByTestId("revision-changes-loading")).toBeNull();
  });

  it("says the server is busy when a third comparison is refused, and reads again on Retry", async () => {
    status = 429;
    problemType = "check_busy";
    mount();

    const alert = await screen.findByRole("alert");
    expect(
      within(alert).getByText("Too many comparisons are running. Try again in a moment."),
    ).toBeTruthy();
    const before = calls;
    status = 200;
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));

    await screen.findByText(/What rolling back to #1 would change/);
    expect(calls).toBeGreaterThan(before);
  });

  it("says it could not load the changes for any other failure, and reads again on Retry", async () => {
    status = 500;
    problemType = "internal";
    mount();

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("Couldn't load this revision's changes.")).toBeTruthy();
    expect(within(alert).queryByText(/too many comparisons/i)).toBeNull();
    const before = calls;
    status = 200;
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(calls).toBeGreaterThan(before));
    await screen.findByText(/What rolling back to #1 would change/);
  });
});

describe("values the server withheld", () => {
  it("draws a hidden value as four dots and never as the sentinel", async () => {
    const spec = (env: string[]) => ({
      TaskTemplate: { ContainerSpec: { Image: "nginx:1.27-alpine", Env: env } },
    });
    body = {
      deployedSeq: 2,
      services: [
        {
          name: "tracer_web",
          short: "web",
          change: "changed",
          current: spec([]),
          next: spec(["TOKEN=<hidden>"]),
          hiddenChanges: [],
          settings: 1,
        },
      ],
      summary: { added: 0, changed: 1, removed: 0 },
      identical: false,
    };
    mount();

    const table = await screen.findByRole("table", { name: "Changes to web" });
    expect(within(table).getByText(/••••/)).toBeTruthy();
    expect(screen.queryByText(/<hidden>/)).toBeNull();
  });
});
