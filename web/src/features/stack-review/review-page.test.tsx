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
let rollback: () => Response | Promise<Response>;
// Whether the stack holds a saved draft with changes that were not deployed.
let draftSaved: boolean;
// The kind of host the stack deploys to.
let target: "swarm" | "standalone";
let client: QueryClient;
// The operation lock the stack's page reports, when one is held.
let lock: Record<string, string> | undefined;

const specOf = (replicas: number, env: string[] = []) => ({
  Name: "s1_web",
  Mode: { Replicated: { Replicas: replicas } },
  TaskTemplate: { ContainerSpec: { Image: "nginx:1.27", Env: env } },
});

const changedWeb = (over: Record<string, unknown> = {}) => ({
  name: "s1_web",
  short: "web",
  change: "changed",
  current: specOf(2),
  next: specOf(3),
  hiddenChanges: [],
  settings: 1,
  ...over,
});

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
    services: [changedWeb()],
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
  lock = undefined;
  me = makeMe({ envs: { [ENV]: "operator" } });
  review = () => Response.json(reviewBody());
  deploy = () => Response.json({ jobId: "j1", revision: 3 }, { status: 202 });
  rollback = () => Response.json({ jobId: "j9", revision: 4 }, { status: 202 });
  draftSaved = false;
  target = "swarm";
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
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: target }] });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}`) {
        return Response.json({
          id: STACK,
          name: "web",
          target,
          source: "inline",
          state: "running",
          lastDeployedAt: "2026-10-19T11:00:00Z",
          lastDeployedBy: "bob",
          hasDraft: true,
          envId: ENV,
          sourceRef: "",
          createdBy: "ada",
          createdAt: "2026-10-19T10:00:00Z",
          stopped: false,
          ...(draftSaved ? { draftSavedAt: "2026-10-19T12:00:00Z", draftSavedBy: "ada" } : {}),
          ...(lock ? { lock } : {}),
        });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/review`) return review();
      if (path === `/api/env/${ENV}/stacks/${STACK}/deploys`) return deploy();
      if (path === `/api/env/${ENV}/stacks/${STACK}/rollback`) return rollback();
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

// The Changes tab, the lists of errors and warnings, the reasons Deploy stack is off (plan 07-15
// task 2).

const problemOf = (over: Record<string, unknown> = {}) => ({
  code: "x",
  col: 1,
  endCol: 2,
  endLine: 1,
  file: "compose.yaml",
  line: 3,
  message: "Something is wrong.",
  severity: "error",
  ...over,
});

const reasonText = () => {
  const button = deployButton();
  const id = button.getAttribute("aria-describedby");
  return id ? document.getElementById(id)?.textContent : undefined;
};

describe("the Changes tab", () => {
  it("has a Changes tab holding one row for each changed service and the diff of an open one", async () => {
    review = () => Response.json(reviewBody({ services: [changedWeb()] }));
    mount();
    expect(await screen.findByRole("tab", { name: "Changes" })).toBeTruthy();
    const table = await screen.findByRole("table", { name: "Changes to web" });
    const replicas = within(table).getByText("Replicas").closest("tr") as HTMLElement;
    expect(within(replicas).getByText("3")).toBeTruthy();
  });

  it("shows the hidden change of a secret by its path and never the value", async () => {
    review = () =>
      Response.json(
        reviewBody({
          services: [
            changedWeb({
              current: specOf(2, ["API_KEY=<hidden>"]),
              next: specOf(3, ["API_KEY=<hidden>"]),
              hiddenChanges: ["TaskTemplate.ContainerSpec.Env[API_KEY]"],
              settings: 2,
            }),
          ],
        }),
      );
    mount();
    expect(await screen.findByText("Changed (hidden)")).toBeTruthy();
    expect(screen.getByText("TaskTemplate.ContainerSpec.Env[API_KEY]")).toBeTruthy();
    expect(document.body.textContent).not.toContain("s3cr3t");
  });

  it("lists the other resources under the services", async () => {
    review = () =>
      Response.json(
        reviewBody({
          services: [changedWeb()],
          objects: [
            {
              change: "changed",
              kind: "secret",
              name: "db_pass",
              detail: "New version. The old one is removed after the deploy.",
            },
          ],
        }),
      );
    mount();
    expect(await screen.findByRole("heading", { name: "Other resources" })).toBeTruthy();
    expect(screen.getByText("db_pass")).toBeTruthy();
  });

  it("lists every service as Added on a first deploy and says nothing from the stack is running", async () => {
    review = () =>
      Response.json(
        reviewBody({
          services: [
            changedWeb({ change: "added", current: null }),
            changedWeb({ name: "s1_db", short: "db", change: "added", current: null }),
          ],
        }),
      );
    mount();
    expect(await screen.findByText("First deploy")).toBeTruthy();
    expect(
      screen.getByText("Nothing from this stack is running yet, so everything below is new."),
    ).toBeTruthy();
    expect(within(screen.getByRole("list")).getAllByText("Added")).toHaveLength(2);
  });

  it("does not call a deploy of a stack that is partly running a first deploy", async () => {
    review = () => Response.json(reviewBody({ services: [changedWeb()] }));
    mount();
    await screen.findByRole("tab", { name: "Changes" });
    expect(screen.queryByText("First deploy")).toBeNull();
  });
});

describe("what stops a deploy", () => {
  it("lists the errors with file:line links to the editor and turns Deploy stack off with the count", async () => {
    review = () =>
      Response.json(
        reviewBody({
          services: [changedWeb()],
          problems: [
            problemOf({ message: "Image is missing.", line: 4 }),
            problemOf({ message: "Port is wrong.", line: 9 }),
          ],
        }),
      );
    mount();
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("2 errors stop this deploy")).toBeTruthy();
    expect(within(alert).getByRole("link", { name: "compose.yaml:4" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/editor?file=compose.yaml&line=4",
    );
    expect(deployButton().getAttribute("aria-disabled")).toBe("true");
    expect(reasonText()).toBe("Fix 2 errors first.");
    fireEvent.click(deployButton());
    expect(posts("/deploys")).toHaveLength(0);
  });

  it("says Fix 1 error first for one", async () => {
    review = () =>
      Response.json(reviewBody({ services: [changedWeb()], problems: [problemOf({})] }));
    mount();
    await screen.findByRole("alert");
    expect(reasonText()).toBe("Fix 1 error first.");
  });

  it("lists warnings and never lets them turn Deploy stack off", async () => {
    review = () =>
      Response.json(
        reviewBody({
          services: [changedWeb()],
          problems: [problemOf({ severity: "warning", message: "Unused variable." })],
        }),
      );
    mount();
    expect(await screen.findByText("1 warning")).toBeTruthy();
    expect(screen.getByText("Unused variable.")).toBeTruthy();
    expect(deployButton().getAttribute("aria-disabled")).toBeNull();
    expect(reasonText()).toBeUndefined();
  });

  it("names the host access an Operator may not deploy", async () => {
    review = () =>
      Response.json(
        reviewBody({
          services: [changedWeb()],
          needsAdmin: ["extra privileges"],
          problems: [problemOf({ message: "This needs Admin: privileged: true.", line: 6 })],
        }),
      );
    mount();
    await screen.findByText("This needs Admin: privileged: true.");
    expect(reasonText()).toBe("Needs Admin to use extra privileges");
    expect(deployButton().getAttribute("aria-disabled")).toBe("true");
  });

  it("joins several kinds of host access into one sentence", async () => {
    review = () =>
      Response.json(
        reviewBody({
          services: [changedWeb()],
          needsAdmin: ["bind mounts", "host networking"],
          problems: [problemOf({})],
        }),
      );
    mount();
    await screen.findByRole("alert");
    expect(reasonText()).toBe("Needs Admin to use bind mounts and host networking");
  });

  it("names the image the registry cannot be asked about", async () => {
    review = () =>
      Response.json(
        reviewBody({
          services: [changedWeb()],
          images: [
            {
              service: "web",
              image: "ghcr.io/acme/web:2",
              digest: "",
              current: "",
              state: "unreachable",
              note: "Can't reach the registry",
            },
          ],
        }),
      );
    mount();
    await screen.findByText("Can't reach the registry");
    expect(reasonText()).toBe("Bayplan can't look up ghcr.io/acme/web:2 right now.");
    expect(deployButton().getAttribute("aria-disabled")).toBe("true");
  });

  it("puts the reason in the description of the button, so a screen reader hears it", async () => {
    review = () =>
      Response.json(reviewBody({ services: [changedWeb()], problems: [problemOf({})] }));
    mount();
    await screen.findByRole("alert");
    const id = deployButton().getAttribute("aria-describedby");
    expect(id).toBeTruthy();
    expect(document.getElementById(id as string)?.textContent).toBe("Fix 1 error first.");
  });
});

describe("nothing to deploy", () => {
  it("says No changes and turns Deploy stack off with that reason", async () => {
    review = () => Response.json(reviewBody({ services: [] }));
    mount();
    expect(await screen.findByText("No changes")).toBeTruthy();
    expect(
      screen.getByText("The stack is the same as what's running. There's nothing to deploy."),
    ).toBeTruthy();
    expect(deployButton().getAttribute("aria-disabled")).toBe("true");
    expect(reasonText()).toBe("There's nothing to deploy.");
    fireEvent.click(deployButton());
    expect(posts("/deploys")).toHaveLength(0);
  });

  it("says every selected image is up to date in update mode", async () => {
    review = () => Response.json(reviewBody({ services: [] }));
    mount({ mode: "update", services: ["web"] });
    expect(await screen.findByText("No changes")).toBeTruthy();
    expect(screen.getByText("Every selected image is already up to date.")).toBeTruthy();
  });

  it("shows an image whose tag did not move as Already up to date, with the running chip, and keeps Deploy stack off", async () => {
    review = () =>
      Response.json(
        reviewBody({
          services: [],
          images: [
            {
              service: "web",
              image: "nginx:1.27",
              digest: D1,
              current: D1,
              state: "pinned",
              note: "Already up to date",
            },
          ],
        }),
      );
    mount({ mode: "update", services: ["web"] });
    expect(await screen.findByText("Already up to date")).toBeTruthy();
    expect(screen.getByText("Pinned (running)")).toBeTruthy();
    expect(screen.queryByText(/^from /)).toBeNull();
    expect(screen.getByText("Every selected image is already up to date.")).toBeTruthy();
    expect(deployButton().getAttribute("aria-disabled")).toBe("true");
    expect(reasonText()).toBe("There's nothing to deploy.");
    fireEvent.click(deployButton());
    expect(posts("/deploys")).toHaveLength(0);
  });

  it("words a review refused because a chosen service is not in the stack", async () => {
    review = () => problem(422, "services_invalid");
    mount({ mode: "update", services: ["ghost"] });
    expect(await screen.findByText("Couldn't load the review.")).toBeTruthy();
    expect(
      screen.getByText(
        "A service you chose is not in the stack any more. Go back and choose again.",
      ),
    ).toBeTruthy();
  });

  it("does not call a stack with only a changed network unchanged", async () => {
    review = () =>
      Response.json(
        reviewBody({
          services: [],
          objects: [{ change: "added", kind: "network", name: "front", detail: "" }],
        }),
      );
    mount();
    await screen.findByRole("heading", { name: "Other resources" });
    expect(screen.queryByText("No changes")).toBeNull();
    expect(deployButton().getAttribute("aria-disabled")).toBeNull();
  });

  it("does not call a stack with only an external network a change", async () => {
    review = () =>
      Response.json(
        reviewBody({
          services: [],
          objects: [{ change: "external", kind: "network", name: "proxy", detail: "" }],
        }),
      );
    mount();
    expect(await screen.findByText("No changes")).toBeTruthy();
  });
});

describe("when the review cannot be loaded", () => {
  it("says so with a way to try again, and keeps Deploy stack off", async () => {
    review = () => problem(500, "internal");
    mount();
    expect(await screen.findByText("Couldn't load the review.")).toBeTruthy();
    expect(deployButton().getAttribute("aria-disabled")).toBe("true");
    review = () => Response.json(reviewBody({ services: [changedWeb()] }));
    fireEvent.click(screen.getByRole("button", { name: "Retry loading the review" }));
    expect(await screen.findByRole("tab", { name: "Changes" })).toBeTruthy();
    expect(posts("/review")).toHaveLength(2);
  });

  it("says the environment changed when the server says so", async () => {
    review = () => problem(409, "target_changed");
    mount();
    expect(
      await screen.findByText(
        "This environment is no longer the kind of target the stack was made for.",
      ),
    ).toBeTruthy();
  });
});

// The Rendered YAML tab (plan 07-16, STK-06, D-11): the rendered stack against the last deploy, as
// the hunks the server computed.

const yamlDiff = () => ({
  changedLines: 2,
  hunks: [
    {
      oldStart: 3,
      oldLines: 3,
      newStart: 3,
      newLines: 3,
      lines: [
        { kind: "context", old: 3, new: 3, text: "    deploy:" },
        { kind: "removed", old: 4, new: 0, text: "      replicas: 2" },
        { kind: "added", old: 0, new: 4, text: "      replicas: 3" },
        { kind: "context", old: 5, new: 5, text: "    ports:" },
      ],
    },
  ],
  lines: ["services:", "  web:", "    deploy:", "      replicas: 3", "    ports:", "      - 80"],
});

describe("the Rendered YAML tab", () => {
  it("shows the number of changed lines in its label, beside Changes", async () => {
    review = () => Response.json(reviewBody({ yaml: yamlDiff() }));
    mount();
    expect(await screen.findByRole("tab", { name: "Changes" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Rendered YAML (2)" })).toBeTruthy();
  });

  it("shows the hunk with its glyphs and its folded unchanged lines when opened", async () => {
    review = () => Response.json(reviewBody({ yaml: yamlDiff() }));
    mount();
    const tab = await screen.findByRole("tab", { name: "Rendered YAML (2)" });
    expect(screen.queryByRole("table", { name: "Rendered YAML, changes" })).toBeNull();
    fireEvent.mouseDown(tab);
    fireEvent.click(tab);
    const table = await screen.findByRole("table", { name: "Rendered YAML, changes" });
    expect(within(table).getByRole("row", { name: "Removed line 4" })).toBeTruthy();
    expect(within(table).getByRole("row", { name: "Added line 4" })).toBeTruthy();
    expect(within(table).getByRole("button", { name: "Show 2 unchanged lines" })).toBeTruthy();
    expect(within(table).getByRole("button", { name: "Show 1 unchanged line" })).toBeTruthy();
  });

  it("has no number when the rendering is what was deployed, and says so", async () => {
    review = () => Response.json(reviewBody({ yaml: { changedLines: 0, hunks: [], lines: [] } }));
    mount();
    const tab = await screen.findByRole("tab", { name: "Rendered YAML" });
    fireEvent.mouseDown(tab);
    fireEvent.click(tab);
    expect(
      await screen.findByText("The rendered YAML is the same as at the last deploy."),
    ).toBeTruthy();
  });

  it("opens on its own when only the text differs and there is nothing to change in the Changes tab", async () => {
    review = () =>
      Response.json(reviewBody({ services: [], summary: reviewBody().summary, yaml: yamlDiff() }));
    mount();
    expect(await screen.findByRole("table", { name: "Rendered YAML, changes" })).toBeTruthy();
    expect(screen.getByText("No changes")).toBeTruthy();
  });

  it("is a review that still works when the answer has no yaml", async () => {
    mount();
    expect(await screen.findByRole("tab", { name: "Rendered YAML" })).toBeTruthy();
  });
});

// The concurrent-deploy guard on the page (plan 07-16, STK-05, D-09): a deployment made after the
// draft was started is shown first, and Deploy stack waits for the person to say they saw it.

const concurrentBody = () => ({
  revision: 4,
  deployedBy: "bob",
  deployedAt: new Date(Date.now() - 5 * 60_000).toISOString(),
  baseToDeployed: [],
  baseToDraft: [],
});

const ackBox = () =>
  screen.getByRole("checkbox", {
    name: "I've seen what changed and want to deploy my draft over it.",
  });

describe("a deployment made after the draft was started", () => {
  it("is the first thing on the page, above the errors that stop a deploy", async () => {
    review = () =>
      Response.json(
        reviewBody({
          concurrent: concurrentBody(),
          problems: [
            {
              severity: "error",
              code: "x",
              message: "Boom",
              file: "compose.yaml",
              line: 2,
              col: 1,
              endLine: 2,
              endCol: 2,
            },
          ],
        }),
      );
    mount();
    const warning = await screen.findByText("This stack was deployed after you started editing");
    const blocking = await screen.findByText("Boom");
    expect(
      warning.compareDocumentPosition(blocking) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("keeps Deploy stack off, with the reason in aria-describedby, until the box is ticked", async () => {
    review = () => Response.json(reviewBody({ concurrent: concurrentBody() }));
    mount();
    await screen.findByText("This stack was deployed after you started editing");
    const button = deployButton();
    expect(button.getAttribute("aria-disabled")).toBe("true");
    const reason = document.getElementById(button.getAttribute("aria-describedby") ?? "");
    expect(reason?.textContent).toBe("Tick the box to confirm you've seen what changed.");
    fireEvent.click(button);
    expect(posts("/deploys")).toHaveLength(0);

    fireEvent.click(ackBox());
    await waitFor(() => expect(deployButton().getAttribute("aria-disabled")).toBeNull());
    expect(deployButton().getAttribute("aria-describedby")).toBeNull();
  });

  it("sends that it was acknowledged with the deploy, and only then", async () => {
    review = () => Response.json(reviewBody({ concurrent: concurrentBody() }));
    const { onDeployed } = mount();
    await screen.findByText("This stack was deployed after you started editing");
    fireEvent.click(ackBox());
    await waitFor(() => expect(deployButton().getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(deployButton());
    await waitFor(() => expect(onDeployed).toHaveBeenCalledWith("j1"));
    expect(posts("/deploys")[0]?.body).toEqual({
      draftVersion: 7,
      pins: { s1_web: `nginx:1.27@${D1}` },
      mode: "deploy",
      acknowledgeConcurrent: true,
    });
  });

  it("is not on the page, and not in the deploy, when nothing was deployed since", async () => {
    mount();
    await screen.findByRole("tab", { name: "Changes" });
    expect(screen.queryByText("This stack was deployed after you started editing")).toBeNull();
    fireEvent.click(deployButton());
    await waitFor(() => expect(posts("/deploys")).toHaveLength(1));
    expect(posts("/deploys")[0]?.body).not.toHaveProperty("acknowledgeConcurrent");
  });

  it("is a reason that waits behind the others: errors and nothing-to-deploy come first", async () => {
    review = () =>
      Response.json(reviewBody({ concurrent: concurrentBody(), services: [], objects: [] }));
    mount();
    await screen.findByText("This stack was deployed after you started editing");
    const button = deployButton();
    const reason = document.getElementById(button.getAttribute("aria-describedby") ?? "");
    expect(reason?.textContent).toBe("There's nothing to deploy.");
  });

  it("is worded when the server refuses a deploy that was not acknowledged", async () => {
    review = () => Response.json(reviewBody({ concurrent: concurrentBody() }));
    deploy = () => problem(409, "concurrent_unacknowledged");
    mount();
    await screen.findByText("This stack was deployed after you started editing");
    fireEvent.click(ackBox());
    await waitFor(() => expect(deployButton().getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(deployButton());
    expect(
      await screen.findByText(
        "Someone deployed this stack after you started editing. Reload the page and check what changed.",
      ),
    ).toBeTruthy();
  });
});

describe("the operation lock", () => {
  const holder = {
    stackId: "s9",
    stackName: "other",
    jobId: "j9",
    kind: "deploy",
    startedAt: "2026-10-19T11:58:00Z",
    startedBy: "ada",
  };
  const locked = {
    severity: "error",
    code: "operation_locked",
    message: "Another stack operation is running on this environment.",
    file: "",
    line: 0,
    col: 0,
    endLine: 0,
    endCol: 0,
  };

  it("turns Deploy stack off with the reason while another operation holds the environment", async () => {
    lock = holder;
    review = () => Response.json(reviewBody({ problems: [locked] }));
    mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    const button = deployButton();
    expect(button.getAttribute("aria-disabled")).toBe("true");
    const id = button.getAttribute("aria-describedby");
    expect(document.getElementById(id as string)?.textContent).toBe(
      "Another stack operation is running on Prod. Try again when it finishes.",
    );
    fireEvent.click(button);
    expect(posts("/deploys")).toHaveLength(0);
  });

  it("is not listed among the errors that stop a deploy: there is nothing to fix", async () => {
    lock = holder;
    review = () => Response.json(reviewBody({ problems: [locked] }));
    mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    expect(screen.queryByText(/errors? stop(s)? this deploy/)).toBeNull();
    expect(screen.queryByText("Fix 1 errors first.")).toBeNull();
  });

  it("lets a deploy through when nothing holds the environment", async () => {
    mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    expect(deployButton().getAttribute("aria-disabled")).toBeNull();
  });

  it("says the environment was busy when the server refuses the deploy with operation_locked", async () => {
    deploy = () => problem(409, "operation_locked");
    mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    fireEvent.click(deployButton());
    expect(
      await screen.findByText(
        "Another stack operation started on Prod just now. Try again when it finishes.",
      ),
    ).toBeTruthy();
  });
});

// A roll back to a chosen revision (plan 08-03, STK-08, D-02): the revision's own stack with its stored
// digests against what runs, and the roll back of exactly that.
describe("in roll back mode", () => {
  const rollbackReview = (over: Record<string, unknown> = {}) =>
    reviewBody({
      revision: 1,
      deployedSeq: 3,
      images: [
        {
          service: "web",
          image: "nginx:1.27",
          digest: D2,
          current: D1,
          state: "pinned",
          note: "revision",
        },
      ],
      pins: { s1_web: `nginx:1.27@${D2}` },
      ...over,
    });
  const rollbackButton = () =>
    screen.getByRole("button", { name: /^(Roll back stack|Starting…)$/ });

  beforeEach(() => {
    review = () => Response.json(rollbackReview());
  });

  it("is titled for the revision, says what it restores and asks for that revision's review", async () => {
    mount({ mode: "rollback", revision: 1 });
    expect(
      await screen.findByRole("heading", { name: "Roll back web to revision #1" }),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "This restores revision #1 exactly: its compose file, files, variables and pinned images. Nothing is looked up in a registry.",
      ),
    ).toBeTruthy();
    await screen.findByRole("table", { name: "Summary of changes" });
    expect(posts("/review")).toHaveLength(1);
    expect(posts("/review")[0]?.body).toEqual({ mode: "rollback", revision: 1 });
  });

  it("has Back to history, which returns to the revision, and Roll back stack in place of Deploy stack", async () => {
    mount({ mode: "rollback", revision: 1 });
    await screen.findByRole("table", { name: "Summary of changes" });
    const back = screen.getByRole("link", { name: "Back to history" });
    expect(back.getAttribute("href")).toBe(`/env/${ENV}/stacks/${STACK}/history?rev=1`);
    expect(rollbackButton().getAttribute("aria-disabled")).toBeNull();
    expect(screen.queryByRole("button", { name: "Deploy stack" })).toBeNull();
  });

  it("tells a person with a saved draft that the draft stays as it is, and no one else", async () => {
    draftSaved = true;
    const { unmount } = mount({ mode: "rollback", revision: 1 });
    expect(
      await screen.findByText(
        "Your saved draft stays as it is. After the roll back it shows a notice that the deployed version changed.",
      ),
    ).toBeTruthy();
    unmount();
    cleanup();
    client.clear();
    draftSaved = false;
    mount({ mode: "rollback", revision: 1 });
    await screen.findByRole("table", { name: "Summary of changes" });
    expect(screen.queryByText(/Your saved draft stays as it is/)).toBeNull();
  });

  it("rolls back with the revision and the deployed revision it reviewed, once, and opens the job", async () => {
    const { onDeployed } = mount({ mode: "rollback", revision: 1 });
    await screen.findByRole("table", { name: "Summary of changes" });
    fireEvent.click(rollbackButton());
    await waitFor(() => expect(onDeployed).toHaveBeenCalledWith("j9"));
    fireEvent.click(rollbackButton());
    expect(posts("/rollback")).toHaveLength(1);
    expect(posts("/rollback")[0]?.body).toEqual({ revision: 1, expectedDeployed: 3 });
    expect(posts("/deploys")).toHaveLength(0);
  });

  it("says the stack was deployed again, stores nothing, and reviews again on request", async () => {
    rollback = () => problem(409, "deployed_changed");
    const { onDeployed } = mount({ mode: "rollback", revision: 1 });
    await screen.findByRole("table", { name: "Summary of changes" });
    fireEvent.click(rollbackButton());
    expect(
      await screen.findByText(
        "The stack was deployed again since you opened this review. Review it again.",
      ),
    ).toBeTruthy();
    expect(onDeployed).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Review again" }));
    await waitFor(() => expect(posts("/review")).toHaveLength(2));
    expect(screen.queryByRole("button", { name: "Review again" })).toBeNull();
  });

  it("reviews a roll back of a standalone stack the same way: the host's network and volume are created, and it rolls back with the same request", async () => {
    target = "standalone";
    review = () =>
      Response.json(
        rollbackReview({
          pins: { web: `nginx:1.27@${D2}` },
          services: [changedWeb({ name: "web" })],
          objects: [
            { change: "added", kind: "network", name: "web_front", detail: "" },
            { change: "added", kind: "volume", name: "web_data", detail: "" },
          ],
          summary: { ...reviewBody().summary, networks: counts(1), volumes: counts(1) },
        }),
      );
    const { onDeployed } = mount({ mode: "rollback", revision: 1 });
    expect(
      await screen.findByRole("heading", { name: "Roll back web to revision #1" }),
    ).toBeTruthy();
    const summary = within(await screen.findByRole("table", { name: "Summary of changes" }));
    expect(
      within(summary.getByRole("row", { name: /^Volumes/ }))
        .getAllByRole("cell")
        .map((c) => c.textContent),
    ).toEqual(["1", "0", "0"]);
    const others = within(screen.getByRole("table", { name: "Other resources" }));
    expect(others.getByText("web_front")).toBeTruthy();
    expect(others.getByText("web_data")).toBeTruthy();
    expect(screen.queryByText(/target_engine_missing|can't be deployed to/)).toBeNull();
    fireEvent.click(rollbackButton());
    await waitFor(() => expect(onDeployed).toHaveBeenCalledWith("j9"));
    expect(posts("/rollback")[0]?.body).toEqual({ revision: 1, expectedDeployed: 3 });
  });

  it("says there is nothing to roll back when running already matches the revision", async () => {
    review = () => Response.json(rollbackReview({ services: [], summary: reviewBody().summary }));
    mount({ mode: "rollback", revision: 1 });
    expect(
      await screen.findByText("Running already matches revision #1. There's nothing to roll back."),
    ).toBeTruthy();
    expect(rollbackButton().getAttribute("aria-disabled")).toBe("true");
  });

  it("reads Pinned (revision #n) for every service, with no New row and no raw note", async () => {
    review = () =>
      Response.json(
        rollbackReview({
          images: [
            {
              service: "web",
              image: "nginx:1.27",
              digest: D2,
              current: D1,
              state: "pinned",
              note: "revision",
            },
            {
              service: "db",
              image: "postgres:16",
              digest: D1,
              current: D1,
              state: "pinned",
              note: "revision",
            },
          ],
        }),
      );
    mount({ mode: "rollback", revision: 1 });
    expect(await screen.findAllByText("Pinned (revision #1)")).toHaveLength(2);
    // The YAML diff has a New column; the Images card has no New badge.
    expect(
      screen.queryAllByText("New").filter((el) => el.getAttribute("data-slot") === "badge"),
    ).toHaveLength(0);
    expect(screen.queryByText("revision")).toBeNull();
    expect(screen.queryByText("Pinned (running)")).toBeNull();
  });
});

// The first review of an adopted stack (plan 08-16, D-16): the services that differ in nothing but Bayplan's
// ownership labels are told apart from real changes, and the restart is announced before the summary.

const STACK_ID = "stk-1";

// A service as the review states it: with the label the first deploy adds (next) or without (current).
const labelled = (
  short: string,
  replicas: number,
  withId: boolean,
  extra: Record<string, unknown> = {},
) => ({
  Name: `s1_${short}`,
  Labels: withId ? { "bayplan.stack.id": STACK_ID } : {},
  Mode: { Replicated: { Replicas: replicas } },
  TaskTemplate: { ContainerSpec: { Image: "nginx:1.27", ...extra } },
});

const labelsOnly = (short: string, replicas = 2) => ({
  name: `s1_${short}`,
  short,
  change: "changed",
  current: labelled(short, replicas, false),
  next: labelled(short, replicas, true),
  hiddenChanges: [],
  settings: 1,
  ownershipOnly: true,
});

// adoptedBody is the review of an adopted stack whose first deploy is still to come.
function adoptedBody(services: Record<string, unknown>[], over: Record<string, unknown> = {}) {
  const only = services.filter((s) => s.ownershipOnly).length;
  const changed = services.length - only;
  return reviewBody({
    services,
    summary: {
      services: counts(0, changed, 0),
      networks: counts(),
      volumes: counts(),
      secrets: counts(),
      configs: counts(),
      imagesNew: 0,
      imagesPinned: 1,
      ownershipOnly: only,
    },
    adoption: { pending: true, ownershipOnly: only },
    ...over,
  });
}

describe("an adopted stack's first review", () => {
  it("announces the restart after the warnings and before the Summary, plural-aware", async () => {
    review = () => Response.json(adoptedBody([labelsOnly("api"), labelsOnly("worker", 3)]));
    mount();
    const title = await screen.findByText("Taking ownership restarts 2 services");
    const alert = title.closest('[role="status"]') as HTMLElement;
    expect(alert).toBeTruthy();
    expect(
      within(alert).getByText(
        "Adopted stacks get Bayplan's ownership labels on their first deploy. A label change recreates the container, which restarts it briefly. They're listed under Ownership labels only.",
      ),
    ).toBeTruthy();
    const summary = screen.getByRole("table", { name: "Summary of changes" });
    expect(alert.compareDocumentPosition(summary) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("says Taking ownership restarts 1 service for one", async () => {
    review = () => Response.json(adoptedBody([labelsOnly("api")]));
    mount();
    expect(await screen.findByText("Taking ownership restarts 1 service")).toBeTruthy();
  });

  it("adds Ownership labels only to the Summary and keeps those services out of Changed", async () => {
    review = () => Response.json(adoptedBody([labelsOnly("api"), labelsOnly("worker", 3)]));
    mount();
    const table = await screen.findByRole("table", { name: "Summary of changes" });
    expect(screen.getByText("Ownership labels only: 2")).toBeTruthy();
    const row = within(table).getByRole("row", { name: /^Services/ });
    expect(
      within(row)
        .getAllByRole("cell")
        .map((c) => c.textContent),
    ).toEqual(["0", "0", "0"]);
  });

  it("lists the services in a closed Ownership labels only group with their task counts", async () => {
    review = () => Response.json(adoptedBody([labelsOnly("api"), labelsOnly("worker", 3)]));
    mount();
    const group = await screen.findByRole("region", { name: "Ownership labels only" });
    const toggle = within(group).getByRole("button", { name: "Ownership labels only" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(
      within(group).getByText(
        "2 services will be recreated to take Bayplan ownership labels (brief restart). Nothing else about them changes.",
      ),
    ).toBeTruthy();
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const items = within(group).getAllByRole("listitem");
    expect(items).toHaveLength(2);
    expect(items[0]?.textContent).toContain("api");
    expect(items[0]?.textContent).toContain("labels");
    expect(items[0]?.textContent).toContain("2 tasks");
    expect(items[1]?.textContent).toContain("3 tasks");
  });

  it("opens a service to the label rows alone, captioned Labels on {service}", async () => {
    review = () => Response.json(adoptedBody([labelsOnly("api")]));
    mount();
    const group = await screen.findByRole("region", { name: "Ownership labels only" });
    fireEvent.click(within(group).getByRole("button", { name: "Ownership labels only" }));
    fireEvent.click(within(group).getByRole("button", { name: /api/ }));
    const table = await within(group).findByRole("table", { name: "Labels on api" });
    const rows = within(table).getAllByRole("row");
    // The header, the group heading and the one label row.
    expect(rows).toHaveLength(3);
    expect(table.textContent).toContain("bayplan.stack.id");
    expect(table.textContent).toContain(STACK_ID);
    expect(table.querySelector(".break-all")).toBeTruthy();
  });

  it("keeps a service with a real change as well in the normal list, its labels one row among the others", async () => {
    const both = {
      ...labelsOnly("web"),
      ownershipOnly: false,
      current: labelled("web", 2, false),
      next: labelled("web", 4, true),
      settings: 2,
    };
    review = () => Response.json(adoptedBody([both, labelsOnly("api")]));
    mount();
    const group = await screen.findByRole("region", { name: "Ownership labels only" });
    fireEvent.click(within(group).getByRole("button", { name: "Ownership labels only" }));
    expect(within(group).queryByText("web")).toBeNull();
    expect(within(group).getAllByRole("button", { name: /api/ })).toHaveLength(1);
    const list = screen.getByText("web", { selector: '[data-slot="service-name"]' });
    expect(group.contains(list)).toBe(false);
    // Changed counts the one real change; the label-only service is counted apart.
    expect(screen.getByText("Taking ownership restarts 1 service")).toBeTruthy();
    expect(screen.getByText("Ownership labels only: 1")).toBeTruthy();
    // Its rows hold the replicas and the label.
    const table = await screen.findByRole("table", { name: "Changes to web" });
    expect(table.textContent).toContain("bayplan.stack.id");
    expect(table.textContent).toContain("Replicas");
  });

  it("cuts a long service name and wraps nothing else out of its row", async () => {
    const long = "a".repeat(80);
    review = () => Response.json(adoptedBody([labelsOnly(long)]));
    mount();
    const group = await screen.findByRole("region", { name: "Ownership labels only" });
    fireEvent.click(within(group).getByRole("button", { name: "Ownership labels only" }));
    const name = within(group).getByText(long);
    expect(name.className).toContain("truncate");
    expect(name.className).toContain("font-mono");
  });

  it("words the group and the warning for containers on a standalone host", async () => {
    target = "standalone";
    review = () => Response.json(adoptedBody([labelsOnly("api"), labelsOnly("worker", 3)]));
    mount();
    expect(await screen.findByText("Taking ownership restarts 2 containers")).toBeTruthy();
    const group = screen.getByRole("region", { name: "Ownership labels only" });
    expect(
      within(group).getByText(
        "2 containers will be recreated to take Bayplan ownership labels (brief restart). Nothing else about them changes.",
      ),
    ).toBeTruthy();
  });

  it("omits the group, the warning and the Summary line when no service is ownership-only", async () => {
    review = () => Response.json(adoptedBody([changedWeb()]));
    mount();
    await screen.findByRole("table", { name: "Summary of changes" });
    expect(screen.queryByRole("region", { name: "Ownership labels only" })).toBeNull();
    expect(screen.queryByText(/Taking ownership restarts/)).toBeNull();
    expect(screen.queryByText(/Ownership labels only:/)).toBeNull();
  });

  it("shows neither while the review loads, and no spinner of its own", async () => {
    review = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByText("Checking the stack…")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Ownership labels only" })).toBeNull();
    expect(screen.queryByText(/Taking ownership restarts/)).toBeNull();
    expect(screen.getByTestId("summary-skeleton")).toBeTruthy();
  });

  it("lets the deploy go ahead: taking ownership is a change to make", async () => {
    review = () => Response.json(adoptedBody([labelsOnly("api")]));
    mount();
    await screen.findByText("Taking ownership restarts 1 service");
    await waitFor(() => expect(deployButton().getAttribute("aria-disabled")).toBeNull());
  });
});
