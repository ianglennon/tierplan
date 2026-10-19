// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ReviewPage } from "@/features/stack-review/review-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Review step (plan 07-15, STK-06, D-10, D-11): what a deploy would change and the images it
// would run, then the deploy of exactly that. The server is a stubbed fetch; the router, the
// query cache and the page are the real ones.

const ENV = "e1";
const STACK = "s1";
const D1 = `sha256:${"a1".repeat(32)}`;
const D2 = `sha256:${"b2".repeat(32)}`;

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
let me: ReturnType<typeof makeMe>;
let review: () => Response | Promise<Response>;
let deploy: () => Response | Promise<Response>;
let client: QueryClient;

const counts = (added = 0, changed = 0, removed = 0) => ({ added, changed, removed });

function reviewBody(over: Record<string, unknown> = {}) {
  return {
    draftVersion: 7,
    images: [
      { service: "web", image: "nginx:1.27", digest: D1, current: D1, state: "pinned", note: "" },
    ],
    needsAdmin: [],
    objects: [],
    pins: { s1_web: `nginx:1.27@${D1}` },
    problems: [],
    services: [],
    summary: {
      services: counts(0, 1, 0),
      networks: counts(),
      volumes: counts(),
      secrets: counts(),
      configs: counts(),
      imagesNew: 0,
      imagesPinned: 1,
    },
    ...over,
  };
}

const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

beforeEach(() => {
  sent = [];
  me = makeMe({ envs: { [ENV]: "operator" } });
  review = () => Response.json(reviewBody());
  deploy = () => Response.json({ jobId: "j1", revision: 3 }, { status: 202 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({
        method: req.method,
        path,
        body:
          req.method === "POST"
            ? await req
                .clone()
                .json()
                .catch(() => undefined)
            : undefined,
      });
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}`) {
        return Response.json({
          id: STACK,
          name: "web",
          target: "swarm",
          source: "inline",
          state: "deployed",
          lastDeployedAt: "2026-10-19T11:00:00Z",
          lastDeployedBy: "bob",
          hasDraft: true,
          envId: ENV,
          sourceRef: "",
          createdBy: "ada",
          createdAt: "2026-10-19T10:00:00Z",
          stopped: false,
        });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/review`) return review();
      if (path === `/api/env/${ENV}/stacks/${STACK}/deploys`) return deploy();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(props: Partial<Parameters<typeof ReviewPage>[0]> = {}) {
  const onDeployed = vi.fn();
  const view = render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ReviewPage envId={ENV} stackId={STACK} onDeployed={onDeployed} {...props} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return { onDeployed, ...view };
}

const posts = (suffix: string) =>
  sent.filter((s) => s.method === "POST" && s.path.endsWith(suffix));
const deployButton = () => screen.getByRole("button", { name: /^(Deploy stack|Starting…)$/ });

describe("the page", () => {
  it("is titled for the stack and says which environment the change is on", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Review web" })).toBeTruthy();
    expect(screen.getByText("Check what will change on Prod, then deploy.")).toBeTruthy();
  });

  it("asks for the review of the saved draft, once, and sends nothing else", async () => {
    mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    expect(posts("/review")).toHaveLength(1);
    expect(posts("/review")[0]?.body).toEqual({ mode: "deploy" });
    expect(posts("/deploys")).toHaveLength(0);
  });

  it("asks for a fresh lookup of the named services in update mode", async () => {
    mount({ mode: "update", services: ["web", "db"] });
    expect(
      await screen.findByRole("heading", { name: "Review image updates for web" }),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "These services get a newly looked-up digest. Everything else keeps its running image.",
      ),
    ).toBeTruthy();
    expect(posts("/review")[0]?.body).toEqual({ mode: "update", services: ["web", "db"] });
  });
});

describe("the Summary", () => {
  it("is a native table with a caption, a row for each kind and the added, changed and removed counts", async () => {
    review = () =>
      Response.json(
        reviewBody({
          summary: {
            services: counts(1, 2, 3),
            networks: counts(1, 0, 0),
            volumes: counts(),
            secrets: counts(0, 2, 0),
            configs: counts(0, 1, 0),
            imagesNew: 1,
            imagesPinned: 4,
          },
        }),
      );
    mount();
    const table = await screen.findByRole("table", { name: "Summary of changes" });
    const row = (name: string) => within(table).getByRole("row", { name: new RegExp(`^${name}`) });
    for (const col of ["Added", "Changed", "Removed"]) {
      expect(within(table).getByRole("columnheader", { name: col })).toBeTruthy();
    }
    expect(
      within(row("Services"))
        .getAllByRole("cell")
        .map((c) => c.textContent),
    ).toEqual(["1", "2", "3"]);
    // A zero count reads 0, not blank.
    expect(
      within(row("Volumes"))
        .getAllByRole("cell")
        .map((c) => c.textContent),
    ).toEqual(["0", "0", "0"]);
    expect(within(row("Images")).getByText("4 pinned, 1 new")).toBeTruthy();
  });

  it("counts a secret or config that changed as a new version, plural-aware", async () => {
    review = () =>
      Response.json(
        reviewBody({
          summary: {
            services: counts(),
            networks: counts(),
            volumes: counts(),
            secrets: counts(0, 2, 0),
            configs: counts(0, 1, 0),
            imagesNew: 0,
            imagesPinned: 0,
          },
        }),
      );
    mount();
    const table = await screen.findByRole("table", { name: "Summary of changes" });
    expect(within(table).getByText("2 new versions")).toBeTruthy();
    expect(within(table).getByText("1 new version")).toBeTruthy();
  });
});

describe("the Images card", () => {
  it("shows a pinned service with its digest chip and Pinned (running)", async () => {
    mount();
    const chip = await screen.findByRole("button", { name: `Pinned to ${D1}` });
    expect(chip.textContent).toBe(D1.slice(0, 19));
    const row = chip.closest("tr") as HTMLElement;
    expect(within(row).getByText("web")).toBeTruthy();
    expect(within(row).getByText("nginx:1.27")).toBeTruthy();
    expect(within(row).getByText("Pinned (running)")).toBeTruthy();
  });

  it("shows a digest just looked up with the New badge and what it will be pinned to", async () => {
    review = () =>
      Response.json(
        reviewBody({
          images: [
            {
              service: "web",
              image: "nginx:1.27",
              digest: D2,
              current: "",
              state: "new",
              note: "",
            },
          ],
        }),
      );
    mount();
    const chip = await screen.findByRole("button", { name: `Will be pinned to ${D2}` });
    const row = chip.closest("tr") as HTMLElement;
    expect(within(row).getByText("New")).toBeTruthy();
    expect(within(row).getByText(`will be pinned to ${D2.slice(0, 19)}`)).toBeTruthy();
    expect(within(row).queryByText("Pinned (running)")).toBeNull();
  });

  it("shows the old and the new digest with an arrow in update mode, and says it in words", async () => {
    review = () =>
      Response.json(
        reviewBody({
          images: [
            {
              service: "web",
              image: "nginx:1.27",
              digest: D2,
              current: D1,
              state: "new",
              note: "",
            },
          ],
        }),
      );
    mount({ mode: "update", services: ["web"] });
    await screen.findByRole("button", { name: `Will be pinned to ${D2}` });
    expect(screen.getByRole("button", { name: `Pinned to ${D1}` })).toBeTruthy();
    expect(screen.getByText(`from ${D1.slice(0, 19)} to ${D2.slice(0, 19)}`)).toBeTruthy();
  });

  it("words a reference pinned in the file, a local image and a registry that cannot be reached", async () => {
    review = () =>
      Response.json(
        reviewBody({
          images: [
            {
              service: "a",
              image: `x@${D1}`,
              digest: D1,
              current: "",
              state: "as_written",
              note: "",
            },
            { service: "b", image: "mine:1", digest: D2, current: "", state: "local", note: "" },
            {
              service: "c",
              image: "gone:1",
              digest: "",
              current: "",
              state: "unreachable",
              note: "",
            },
            {
              service: "d",
              image: "lost:1",
              digest: "",
              current: "",
              state: "unresolved",
              note: "",
            },
          ],
        }),
      );
    mount();
    expect(await screen.findByText("Pinned in the compose file")).toBeTruthy();
    expect(screen.getByText("Not from a registry. Pinned to the local image ID.")).toBeTruthy();
    expect(screen.getByText("Can't reach the registry")).toBeTruthy();
    expect(screen.getByText("Can't look up this image")).toBeTruthy();
    // Nothing the registry could not give is drawn as a chip.
    const rowOf = (name: string) => screen.getByText(name).closest("tr") as HTMLElement;
    expect(within(rowOf("gone:1")).queryByRole("button")).toBeNull();
    expect(within(rowOf("lost:1")).queryByRole("button")).toBeNull();
  });

  it("shows the server's own note, such as No tag, so it can move.", async () => {
    review = () =>
      Response.json(
        reviewBody({
          images: [
            {
              service: "web",
              image: "nginx",
              digest: D1,
              current: D1,
              state: "pinned",
              note: "No tag, so it can move.",
            },
          ],
        }),
      );
    mount();
    expect(await screen.findByText("No tag, so it can move.")).toBeTruthy();
  });

  it("draws an image reference with markup in it as text, and wraps a long one", async () => {
    const long = `registry.example.com/${"very-long-name/".repeat(8)}app:1`;
    review = () =>
      Response.json(
        reviewBody({
          images: [
            {
              service: "<b>web</b>",
              image: long,
              digest: D1,
              current: D1,
              state: "pinned",
              note: "",
            },
          ],
        }),
      );
    mount();
    const cell = (await screen.findByText(long)) as HTMLElement;
    expect(cell.className).toContain("break-all");
    const name = screen.getByText("<b>web</b>");
    expect(name.querySelector("b")).toBeNull();
  });
});

describe("while the review loads", () => {
  it("shows skeleton rows and Checking the stack…, with Deploy stack unavailable", async () => {
    review = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByText("Checking the stack…")).toBeTruthy();
    expect(screen.getByTestId("summary-skeleton")).toBeTruthy();
    expect(screen.getByTestId("images-skeleton")).toBeTruthy();
    expect(screen.getByText("Looking up the digest…")).toBeTruthy();
    expect(deployButton().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(deployButton());
    expect(posts("/deploys")).toHaveLength(0);
  });
});

describe("Deploy stack", () => {
  it("sends the reviewed draft version and pins, and hands the job to the route", async () => {
    const { onDeployed } = mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    fireEvent.click(deployButton());
    await waitFor(() => expect(onDeployed).toHaveBeenCalledWith("j1"));
    expect(posts("/deploys")).toHaveLength(1);
    expect(posts("/deploys")[0]?.body).toEqual({
      draftVersion: 7,
      pins: { s1_web: `nginx:1.27@${D1}` },
      mode: "deploy",
    });
  });

  it("names the services of an update in the deploy", async () => {
    const { onDeployed } = mount({ mode: "update", services: ["web"] });
    await screen.findByRole("table", { name: "Summary of changes" });
    fireEvent.click(deployButton());
    await waitFor(() => expect(onDeployed).toHaveBeenCalled());
    expect(posts("/deploys")[0]?.body).toEqual({
      draftVersion: 7,
      pins: { s1_web: `nginx:1.27@${D1}` },
      mode: "update",
      services: ["web"],
    });
  });

  it("is pending after the first press and a second press starts nothing", async () => {
    let release: (r: Response) => void = () => {};
    deploy = () => new Promise<Response>((resolve) => (release = resolve));
    const { onDeployed } = mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    fireEvent.click(deployButton());
    expect(await screen.findByRole("button", { name: "Starting…" })).toBeTruthy();
    fireEvent.click(deployButton());
    fireEvent.click(deployButton());
    expect(deployButton().getAttribute("aria-disabled")).toBe("true");
    release(Response.json({ jobId: "j1", revision: 3 }, { status: 202 }));
    await waitFor(() => expect(onDeployed).toHaveBeenCalledTimes(1));
    expect(posts("/deploys")).toHaveLength(1);
  });

  it("says the draft changed when the server refuses it with 409 draft_changed", async () => {
    deploy = () => problem(409, "draft_changed");
    const { onDeployed } = mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    fireEvent.click(deployButton());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "That changed while you were looking at it. Reload the page and try again.",
    );
    expect(onDeployed).not.toHaveBeenCalled();
    // The button is usable again, and the request was made once and not repeated.
    expect(deployButton().getAttribute("aria-disabled")).toBeNull();
    expect(posts("/deploys")).toHaveLength(1);
  });

  it("says the server did not answer, and that the deploy may have started", async () => {
    deploy = () => {
      throw new TypeError("network down");
    };
    mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    fireEvent.click(deployButton());
    expect((await screen.findByRole("alert")).textContent).toContain("The deploy may have started");
  });
});

describe("leaving the page", () => {
  it("offers Back to editing, a link to the editor", async () => {
    mount();
    const back = await screen.findByRole("link", { name: "Back to editing" });
    expect(back.getAttribute("href")).toBe("/env/e1/stacks/s1/editor");
  });

  it("does not keep the review, which holds the specs, in the query cache once it closes", async () => {
    const { unmount } = mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    expect(client.getQueryCache().findAll({ queryKey: ["stack-review"] })).toHaveLength(1);
    unmount();
    await waitFor(() =>
      expect(client.getQueryCache().findAll({ queryKey: ["stack-review"] })).toHaveLength(0),
    );
  });
});
