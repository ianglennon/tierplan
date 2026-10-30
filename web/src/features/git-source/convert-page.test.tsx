// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
  useLocation,
} from "@tanstack/react-router";
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { StackDetail } from "@/api/stacks";
import { Toaster } from "@/components/ui/sonner";
import { GitSourcePage } from "@/features/git-source/git-source-page";
import {
  dir,
  ENV,
  file,
  makeWorld,
  newClient,
  refsOf,
  renderUi,
  repoOf,
  stubFetch,
  treeOf,
  type World,
} from "@/features/git-source/test-fixture";
import { StackMoreMenu } from "@/features/stacks/stack-actions";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Use a git repository page (plan 09-30, GIT-02, D-24): the From git cards, the Variables card started from the stack's
// table, and the existing-files card where each file of the stack is dropped or kept as an overlay file.

let world: World;
let client: ReturnType<typeof newClient>;
// What the stubbed server holds of the stack being converted, and how it answers the conversion.
let stack: { name: string; source: string };
let draft: Record<string, unknown>;
let connect: () => Response;
let draftReads: number;

const DRAFT_FILES = [
  { path: "compose.yaml", content: "services: {}\n", secret: false },
  { path: "notes.txt", content: "hello\n", secret: false },
  { path: "web.env", content: "OLD=1\n", secret: false },
  { path: "secrets/db.txt", content: "<hidden>", secret: true },
];

function draftOf(over: Record<string, unknown> = {}) {
  return {
    baseSeq: 2,
    files: DRAFT_FILES,
    isDraft: true,
    savedAt: "2026-10-20T10:00:00Z",
    savedBy: "ada",
    vars: [
      { name: "TAG", value: "9", secret: false },
      { name: "TOKEN", value: "<hidden>", secret: true },
    ],
    version: 4,
    ...over,
  };
}

beforeEach(() => {
  world = makeWorld();
  world.repos = [repoOf("r1", "homelab")];
  world.refs = () => Response.json(refsOf(["main"]));
  world.tree = (q) => {
    const path = q.get("path") ?? "";
    if (path === "") return Response.json(treeOf("", [dir("stacks")]));
    if (path === "stacks") return Response.json(treeOf(path, [dir("web", true)]));
    return Response.json(treeOf(path, [file("compose.yaml"), file("web.env")]));
  };
  stack = { name: "legacy", source: "inline" };
  draft = draftOf();
  draftReads = 0;
  connect = () => Response.json({ draftVersion: 5, commit: "c".repeat(40), ref: "main" });
  client = newClient();
  stubFetch(world);
  const base = globalThis.fetch;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === `/api/env/${ENV}/stacks/s1` && req.method === "GET") {
        return Response.json({ id: "s1", envId: ENV, ...stack });
      }
      if (url.pathname === `/api/env/${ENV}/stacks/s1/draft` && req.method === "GET") {
        draftReads += 1;
        return Response.json(draft);
      }
      if (url.pathname === `/api/env/${ENV}/stacks/s1/connect-git` && req.method === "POST") {
        world.sent.push({
          method: "POST",
          path: url.pathname,
          query: url.searchParams,
          body: await req.clone().json(),
        });
        return connect();
      }
      return base(req);
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  const root = createRootRoute({
    component: () => (
      <>
        <GitSourcePage envId={ENV} stackId="s1" mode="convert" />
        <Toaster />
      </>
    ),
  });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: "/" }),
      createRoute({ getParentRoute: () => root, path: "/away" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks/$stackId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks/$stackId/review" }),
    ]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  renderUi(<RouterProvider router={router} />, client);
  return router;
}

async function chooseRepository(name: string) {
  const select = await screen.findByRole("combobox", { name: "Repository" });
  fireEvent.keyDown(select, { key: "Enter" });
  const options = await screen.findAllByRole("option");
  fireEvent.click(options.find((o) => o.textContent?.includes(name)) as HTMLElement);
}

async function chooseWebFolder() {
  await chooseRepository("homelab");
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: "Branch" }).textContent).toContain("main"),
  );
  fireEvent.click(await screen.findByRole("button", { name: "stacks" }));
  fireEvent.click(await screen.findByRole("button", { name: /web/ }));
  const button = await screen.findByRole("button", { name: "Use this folder" });
  await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(false));
  fireEvent.click(button);
  await screen.findByText("Picked compose.yaml. Change it if the stack uses another.");
}

const connects = () => world.sent.filter((s) => s.path.endsWith("/connect-git"));
const row = (path: string) => screen.getByTestId(`leftover-file-${path}`);
const radio = (path: string, label: string) =>
  within(row(path)).getByRole("radio", { name: label }) as HTMLButtonElement;

describe("the convert page", () => {
  it("is titled for the stack and lists its files with their defaults", async () => {
    mount();
    expect(
      await screen.findByRole("heading", { name: "Use a git repository for legacy" }),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "The first sync goes through review and is recorded as a revision, so the history of legacy is kept. Your variables stay.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Files in legacy today" })).toBeTruthy();
    expect(
      screen.getByText(
        "The compose file is replaced by the repository's. Choose what happens to the other files.",
      ),
    ).toBeTruthy();
    // The compose file is replaced by the repository's, so it is not listed.
    expect(screen.queryByTestId("leftover-file-compose.yaml")).toBeNull();
    // A plain file is dropped by default, a secret file kept.
    expect(radio("notes.txt", "Drop it").getAttribute("aria-checked")).toBe("true");
    expect(radio("web.env", "Drop it").getAttribute("aria-checked")).toBe("true");
    expect(radio("secrets/db.txt", "Keep as overlay file").getAttribute("aria-checked")).toBe(
      "true",
    );
    expect(within(row("secrets/db.txt")).getByText("Secret")).toBeTruthy();
    expect(within(row("notes.txt")).queryByText("Secret")).toBeNull();
    expect(
      await screen.findByText("Nothing changes on Prod until you review and deploy."),
    ).toBeTruthy();
  });

  it("starts the variables from the stack's table", async () => {
    mount();
    await screen.findByRole("heading", { name: "Files in legacy today" });
    // The card lists the variables the compose files use once a folder is chosen; the table is sent with the check.
    await chooseWebFolder();
    await waitFor(() =>
      expect(
        world.sent.some(
          (s) =>
            s.path.endsWith("/git-stacks/check") &&
            JSON.stringify(s.body?.vars) ===
              JSON.stringify([
                { name: "TAG", value: "9", secret: false },
                { name: "TOKEN", value: "<hidden>", secret: true },
              ]),
        ),
      ).toBe(true),
    );
  });

  it("asks before leaving once a choice is made, and not before (WR-E03)", async () => {
    const router = mount();
    await screen.findByRole("heading", { name: "Files in legacy today" });
    // The stack's own variables are the starting point, not a change.
    await router.navigate({ to: "/away" as never });
    expect(router.state.location.pathname).toBe("/away");
    await router.navigate({ to: "/" });

    fireEvent.click(radio("notes.txt", "Keep as overlay file"));
    void router.navigate({ to: "/away" as never });
    const dialog = await screen.findByRole("alertdialog", { name: "Discard these choices?" });
    expect(within(dialog).getByText("Your choices for legacy haven't been applied.")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(router.state.location.pathname).toBe("/");
  });

  it("hides the existing-files card for a stack with no extra files", async () => {
    draft = draftOf({ files: [DRAFT_FILES[0]] });
    mount();
    await screen.findByRole("heading", { name: "Use a git repository for legacy" });
    expect(screen.queryByRole("heading", { name: "Files in legacy today" })).toBeNull();
  });

  it("disables Keep for a file the repository also holds, with its reason", async () => {
    // A secret file that is also in the repository: its default keep gives way.
    draft = draftOf({
      files: [...DRAFT_FILES, { path: "web.env", content: "<hidden>", secret: true }].filter(
        (f, i, all) => all.findIndex((g) => g.path === f.path) === i,
      ),
    });
    draft.files = DRAFT_FILES.map((f) => (f.path === "web.env" ? { ...f, secret: true } : f));
    mount();
    await screen.findByRole("heading", { name: "Files in legacy today" });
    expect(radio("web.env", "Keep as overlay file").getAttribute("aria-checked")).toBe("true");

    await chooseWebFolder();
    await waitFor(() => expect(radio("web.env", "Keep as overlay file").disabled).toBe(true));
    expect(
      within(row("web.env")).getByText(
        "web.env exists in the repository. Drop it or rename it first.",
      ),
    ).toBeTruthy();
    expect(radio("web.env", "Drop it").getAttribute("aria-checked")).toBe("true");
    // The others keep their choices.
    expect(radio("secrets/db.txt", "Keep as overlay file").disabled).toBe(false);
  });

  it("sends the choices on Review and sync and opens the review", async () => {
    const router = mount();
    await screen.findByRole("heading", { name: "Files in legacy today" });
    // The person keeps a plain file and drops the secret one.
    fireEvent.click(radio("notes.txt", "Keep as overlay file"));
    fireEvent.click(radio("secrets/db.txt", "Drop it"));
    await chooseWebFolder();

    fireEvent.click(screen.getByRole("button", { name: "Review and sync" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/e1/stacks/s1/review"));
    expect(connects()).toHaveLength(1);
    expect(connects()[0]?.body).toEqual({
      draftVersion: 4,
      repoId: "r1",
      refKind: "branch",
      ref: "main",
      folder: "stacks/web",
      composeFiles: ["compose.yaml"],
      files: [
        { path: "notes.txt", keep: true },
        { path: "web.env", keep: false },
        { path: "secrets/db.txt", keep: false },
      ],
      vars: [
        { name: "TAG", value: "9", secret: false },
        { name: "TOKEN", value: "<hidden>", secret: true },
      ],
    });
  });

  it("names who changed the stack while the person was choosing, and reloads", async () => {
    connect = () =>
      Response.json(
        {
          type: "draft_changed",
          title: "Conflict",
          status: 409,
          detail: "d",
          errors: [{ location: "savedBy", message: "grace" }],
        },
        { status: 409 },
      );
    const router = mount();
    await screen.findByRole("heading", { name: "Files in legacy today" });
    await chooseWebFolder();
    fireEvent.click(screen.getByRole("button", { name: "Review and sync" }));

    expect(
      await screen.findByText("grace changed legacy while you were choosing. Reload to see it."),
    ).toBeTruthy();
    expect(router.state.location.pathname).toBe("/");
    const before = draftReads;
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(draftReads).toBeGreaterThan(before));
    await waitFor(() => expect(screen.queryByText(/grace changed legacy/)).toBeNull());
  });

  it("words a kept file the repository holds as a clash, and a lost connection as such", async () => {
    connect = () =>
      Response.json(
        {
          type: "overlay_clash",
          title: "x",
          status: 422,
          detail: "d",
          errors: [{ location: "files", message: "notes.txt" }],
        },
        { status: 422 },
      );
    mount();
    await screen.findByRole("heading", { name: "Files in legacy today" });
    await chooseWebFolder();
    fireEvent.click(screen.getByRole("button", { name: "Review and sync" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "notes.txt exists in the repository. Drop it or rename it first.",
    );
  });

  it("keeps Review and sync off for someone who is not an Admin, with the role it takes", async () => {
    world.me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    await screen.findByRole("heading", { name: "Files in legacy today" });
    await chooseWebFolder();
    const button = screen.getByRole("button", { name: "Review and sync" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(connects()).toEqual([]);
  });

  it("says a git stack already uses a repository", async () => {
    stack = { name: "legacy", source: "git" };
    mount();
    expect(await screen.findByText("legacy already uses a git repository.")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Files in legacy today" })).toBeNull();
  });
});

describe("the More menu item", () => {
  function Where() {
    return <p data-testid="where">{useLocation().pathname}</p>;
  }

  function menu(source: string, role: "viewer" | "operator" | "admin") {
    world.me = makeMe({ envs: { [ENV]: role } });
    renderUi(
      <TestRouter>
        <StackMoreMenu
          stack={
            {
              id: "s1",
              envId: ENV,
              name: "legacy",
              target: "swarm",
              state: "running",
              source,
            } as StackDetail
          }
        />
        <Where />
      </TestRouter>,
      client,
    );
  }

  async function open() {
    fireEvent.pointerDown(await screen.findByRole("button", { name: /More/ }), {
      button: 0,
      ctrlKey: false,
    });
    await screen.findByRole("menuitem", { name: "Copy stack name" });
  }

  it.each(["inline", "adopted"])(
    "is offered to an Admin on a %s stack and opens the page",
    async (source) => {
      menu(source, "admin");
      await open();
      fireEvent.click(await screen.findByRole("menuitem", { name: "Use a git repository…" }));
      await waitFor(() =>
        expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s1/connect-git"),
      );
    },
  );

  it("is not offered on a git stack", async () => {
    menu("git", "admin");
    await open();
    expect(screen.queryByRole("menuitem", { name: "Use a git repository…" })).toBeNull();
  });

  it.each(["viewer", "operator"] as const)("is not offered to a %s", async (role) => {
    menu("inline", role);
    await open();
    expect(screen.queryByRole("menuitem", { name: "Use a git repository…" })).toBeNull();
  });
});
