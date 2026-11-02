// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackEditorPage } from "@/features/stack-editor/editor-page";
import "@/i18n";
import { editorText, typeIntoEditor } from "@/test-editor";
import { makeMe } from "@/test-me";

// The Files tab of a stack from a git repository (plan 09-21, GIT-08, D-09, D-11): the repository's files are shown
// read-only, TierPlan's overlay files are sealed cards, and the only things to edit are overlays and variables. The
// server is a stubbed fetch; the router, the query cache and the page are the real ones.

const ENV = "e1";
const STACK = "s1";
const COMPOSE = "services:\n  web:\n    image: nginx:1.27\n    env_file: web.env\n";
const SHA = "0123456789abcdef0123456789abcdef01234567";

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

interface Server {
  sent: Sent[];
  role: "viewer" | "operator";
  source: "git" | "inline";
  // The version of the draft the server holds: a save moves it, and the pull reads it.
  version: number;
  draft: () => Record<string, unknown>;
  pull: (body: Record<string, unknown>) => Response;
  save: (body: Record<string, unknown>) => Response;
  check: () => Record<string, unknown>;
}

let server: Server;
let client: QueryClient;

function gitDraft(over: Record<string, unknown> = {}) {
  return {
    isDraft: true,
    version: 7,
    savedBy: "ada",
    savedAt: "2026-10-28T10:00:00Z",
    baseSeq: 0,
    files: [
      { path: "compose.yaml", content: COMPOSE, secret: false, origin: "repo" },
      { path: "web.env", content: "A=b\n", secret: false, origin: "repo" },
      { path: "db_pw.txt", content: "<hidden>", secret: true, origin: "overlay" },
    ],
    vars: [],
    composeFiles: ["compose.yaml"],
    git: { commit: SHA, repoName: "acme", ref: "main" },
    ...over,
  };
}

beforeEach(() => {
  server = {
    sent: [],
    role: "operator",
    source: "git",
    version: 7,
    draft: () => gitDraft({ version: server.version }),
    pull: (body) =>
      Response.json({
        draftVersion: (body.draftVersion as number) + 1,
        commit: SHA,
        ref: "main",
        deployedCommit: SHA,
        touched: 1,
      }),
    save: (body) => {
      server.version = (body.version as number) + 1;
      return Response.json({
        changed: true,
        version: server.version,
        savedBy: "grace",
        savedAt: new Date().toISOString(),
        secretFiles: ["db_pw.txt"],
      });
    },
    check: () => ({ problems: [], unusedFiles: [], variables: [], secretFiles: ["db_pw.txt"] }),
  };
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = {
        method: req.method,
        path,
        body:
          req.method === "PUT" || req.method === "POST"
            ? await req
                .clone()
                .json()
                .catch(() => undefined)
            : undefined,
      };
      server.sent.push(entry);
      if (path === "/api/me") {
        return Response.json(makeMe({ envs: { [ENV]: server.role } }));
      }
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}`) {
        return Response.json({
          id: STACK,
          name: "web",
          target: "swarm",
          source: server.source,
          state: "not_deployed",
          lastDeployedAt: "",
          lastDeployedBy: "",
          hasDraft: true,
          envId: ENV,
          sourceRef: "",
          createdBy: "ada",
          createdAt: "2026-10-19T10:00:00Z",
          stopped: false,
        });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/draft`) {
        if (req.method === "PUT") return server.save(entry.body ?? {});
        return Response.json(server.draft());
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/git/pull` && req.method === "POST") {
        return server.pull(entry.body ?? {});
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/check` && req.method === "POST") {
        return Response.json(server.check());
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

const puts = () => server.sent.filter((s) => s.method === "PUT");

function mount(props: Partial<React.ComponentProps<typeof StackEditorPage>> = {}) {
  const root = createRootRoute({
    component: () => (
      <TooltipProvider>
        <Outlet />
        <Toaster />
      </TooltipProvider>
    ),
  });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: "/",
        component: () => <StackEditorPage envId={ENV} stackId={STACK} {...props} />,
      }),
    ]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

const composeBox = () => screen.findByRole("textbox", { name: "compose.yaml, YAML" });

describe("the Files tab of a git stack", () => {
  it("says where the files come from and that they cannot be edited here", async () => {
    mount();
    await composeBox();
    expect(
      screen.getByText((_, el) => {
        const text = el?.tagName === "P" ? (el.textContent ?? "") : "";
        return text.includes(
          "These files come from acme at 0123456. They can't be edited here. Change them in the repository and pull.",
        );
      }),
    ).toBeTruthy();
  });

  it("badges each file Repo or Overlay", async () => {
    mount();
    await composeBox();
    const rail = screen.getByRole("navigation", { name: "Stack files" });
    const row = (path: string) => rail.querySelector(`[data-file="${path}"]`) as HTMLElement;
    expect(within(row("compose.yaml")).getByText("Repo")).toBeTruthy();
    expect(within(row("web.env")).getByText("Repo")).toBeTruthy();
    expect(within(row("db_pw.txt")).getByText("Overlay")).toBeTruthy();
  });

  it("opens a repository file read-only, so there is no compose editor to type in", async () => {
    mount();
    const box = await composeBox();
    expect(box.getAttribute("aria-readonly")).toBe("true");
    expect(editorText()).toBe(COMPOSE);
    // No rename or delete for a file of the repository.
    expect(screen.queryByRole("button", { name: /^Actions for / })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Actions for compose.yaml" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Actions for web.env" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add file" })).toBeNull();
  });

  it("opens an overlay file as the sealed card", async () => {
    mount({ file: "db_pw.txt" });
    expect(await screen.findByText("The content of db_pw.txt is hidden.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Replace content" })).toBeTruthy();
  });

  it("opens a stack whose compose file is not called compose.yaml", async () => {
    server.draft = () =>
      gitDraft({
        files: [{ path: "docker-compose.yml", content: COMPOSE, secret: false, origin: "repo" }],
        composeFiles: ["docker-compose.yml"],
      });
    mount();
    expect(await screen.findByRole("textbox", { name: "docker-compose.yml, YAML" })).toBeTruthy();
  });

  it("adds an overlay file as a sealed secret and saves it with the compose files, without an origin", async () => {
    mount();
    await composeBox();
    fireEvent.click(screen.getByRole("button", { name: "Add overlay file" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Path"), { target: { value: "extra.env" } });
    fireEvent.change(within(dialog).getByLabelText("Contents"), {
      target: { value: "K=secret-v" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add overlay file" }));

    const rail = screen.getByRole("navigation", { name: "Stack files" });
    await waitFor(() => expect(rail.querySelector('[data-file="extra.env"]')).toBeTruthy());
    expect(
      within(rail.querySelector('[data-file="extra.env"]') as HTMLElement).getByText("Overlay"),
    ).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /^Save draft/ }));
    expect(await screen.findByText("Saved the draft of web.")).toBeTruthy();
    expect(puts()).toHaveLength(1);
    expect(puts()[0]?.body).toEqual({
      version: 7,
      composeFiles: ["compose.yaml"],
      files: [
        { path: "compose.yaml", content: COMPOSE, secret: false },
        { path: "db_pw.txt", content: "<hidden>", secret: true },
        { path: "extra.env", content: "K=secret-v", secret: true },
        { path: "web.env", content: "A=b\n", secret: false },
      ],
      vars: [],
    });
  });

  it("refuses a path the repository holds in the Add overlay file dialog", async () => {
    mount();
    await composeBox();
    fireEvent.click(screen.getByRole("button", { name: "Add overlay file" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Path"), { target: { value: "web.env" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add overlay file" }));
    expect(await within(dialog).findByRole("alert")).toBeTruthy();
    expect(puts()).toHaveLength(0);
  });

  it("words a refusal of a repository file edit with the repository's name", async () => {
    server.save = () =>
      Response.json(
        {
          type: "repo_file_read_only",
          title: "Unprocessable Entity",
          status: 422,
          detail: "d",
          errors: [{ location: "compose.yaml", message: "d" }],
        },
        { status: 422 },
      );
    mount({ file: "db_pw.txt" });
    await screen.findByText("The content of db_pw.txt is hidden.");
    fireEvent.click(screen.getByRole("button", { name: "Replace content" }));
    await screen.findByRole("textbox", { name: "db_pw.txt, text" });
    typeIntoEditor("new");
    fireEvent.click(screen.getByRole("button", { name: /^Save draft/ }));
    expect(
      await screen.findByText(
        "Repository files can't be edited here. Change them in acme and pull.",
      ),
    ).toBeTruthy();
  });
});

describe("the Variables pane of a git stack", () => {
  const HIDDEN = "<hidden>";
  const variables = [
    {
      name: "TAG",
      value: "1.27",
      secret: false,
      source: "repo-env",
      default: "",
      hasDefault: false,
      required: false,
      unused: false,
      places: [{ file: "compose.yaml", line: 3 }],
    },
    {
      name: "DB_PASSWORD",
      value: HIDDEN,
      secret: false,
      secretLike: true,
      source: "repo-env",
      default: "",
      hasDefault: false,
      required: false,
      unused: false,
      places: [{ file: "compose.yaml", line: 4 }],
    },
  ];

  beforeEach(() => {
    server.check = () => ({ problems: [], unusedFiles: [], variables, secretFiles: [] });
  });

  it("says a value comes from the repository's .env and shows it as the placeholder", async () => {
    mount({ pane: "variables" });
    const tag = await screen.findByTestId("variable-row-TAG");
    expect(await within(tag).findByText("From the repo's .env")).toBeTruthy();
    expect(within(tag).getByText("1.27")).toBeTruthy();
    const input = within(tag).getByLabelText("Value of TAG") as HTMLInputElement;
    expect(input.placeholder).toBe("1.27");
  });

  it("hides a secret-like value from the repository and offers Set secret value", async () => {
    mount({ pane: "variables" });
    const row = await screen.findByTestId("variable-row-DB_PASSWORD");
    expect(await within(row).findByText("From the repo's .env")).toBeTruthy();
    expect(within(row).getByText("••••")).toBeTruthy();
    expect(within(row).queryByText("<hidden>")).toBeNull();
    const warning = screen.getByTestId("variable-warning-DB_PASSWORD");
    expectWarningCellWraps(warning);
    expect(
      within(warning).getByText(
        "DB_PASSWORD looks like a secret and is committed in the repository's .env. Set a secret variable here to override it.",
      ),
    ).toBeTruthy();
    fireEvent.click(within(warning).getByRole("button", { name: "Set secret value" }));
    const input = within(row).getByLabelText("Value of DB_PASSWORD");
    expect(
      (
        within(row).getByRole("switch", { name: "Secret: DB_PASSWORD" }) as HTMLElement
      ).getAttribute("aria-checked"),
    ).toBe("true");
    expect(document.activeElement).toBe(input);
    // Once a value is typed the warning goes.
    fireEvent.change(input, { target: { value: "my-own-password" } });
    expect(screen.queryByTestId("variable-warning-DB_PASSWORD")).toBeNull();
  });

  it("gives a Viewer the warning without the button", async () => {
    server.role = "viewer";
    mount({ pane: "variables" });
    const warning = await screen.findByTestId("variable-warning-DB_PASSWORD");
    expect(within(warning).queryByRole("button", { name: "Set secret value" })).toBeNull();
    expectWarningCellWraps(warning);
  });
});

// G-09-3: the shadcn TableCell base is whitespace-nowrap; the warning cell must override it so the
// sentence wraps at 375px instead of spilling across the shrink-0 button. jsdom has no layout, so
// this pins the cause (the class); the 375px look is the human check in plan 09-36.
function expectWarningCellWraps(warning: HTMLElement) {
  const cell = warning.querySelector("td");
  expect(cell?.className).toContain("whitespace-normal");
  expect(cell?.className).not.toContain("whitespace-nowrap");
}

describe("Review and deploy on a git stack", () => {
  const pulls = () => server.sent.filter((s) => s.path.endsWith("/git/pull"));

  it("pulls the head of the ref after the save, keeping the edits, and then opens the Review", async () => {
    const onReview = vi.fn();
    mount({ onReview, file: "db_pw.txt" });
    await screen.findByText("The content of db_pw.txt is hidden.");
    fireEvent.click(screen.getByRole("button", { name: "Replace content" }));
    await screen.findByRole("textbox", { name: "db_pw.txt, text" });
    typeIntoEditor("changed");
    fireEvent.click(screen.getByRole("button", { name: "Review and deploy" }));
    await waitFor(() => expect(onReview).toHaveBeenCalledTimes(1));
    // The save came first, at the version read, and the pull came after it, at the version the save made.
    const order = server.sent.filter((s) => s.method === "PUT" || s.path.endsWith("/git/pull"));
    expect(order.map((s) => s.method)).toEqual(["PUT", "POST"]);
    expect(puts()[0]?.body?.version).toBe(7);
    expect(pulls()[0]?.body).toEqual({ draftVersion: 8 });
  });

  it("pulls even when nothing is unsaved", async () => {
    const onReview = vi.fn();
    mount({ onReview });
    await composeBox();
    fireEvent.click(screen.getByRole("button", { name: "Review and deploy" }));
    await waitFor(() => expect(onReview).toHaveBeenCalledTimes(1));
    expect(puts()).toHaveLength(0);
    expect(pulls()).toHaveLength(1);
  });

  it("says the draft changed and does not open the Review when another save came in between", async () => {
    server.pull = () =>
      Response.json(
        { type: "draft_changed", title: "Conflict", status: 409, detail: "d" },
        { status: 409 },
      );
    const onReview = vi.fn();
    mount({ onReview });
    await composeBox();
    fireEvent.click(screen.getByRole("button", { name: "Review and deploy" }));
    expect(
      await screen.findByText(
        "Couldn't pull main. The draft changed while you were pulling. Reload it and try again.",
      ),
    ).toBeTruthy();
    expect(onReview).not.toHaveBeenCalled();
    // The button is usable again.
    expect(
      (screen.getByRole("button", { name: "Review and deploy" }) as HTMLButtonElement).getAttribute(
        "aria-disabled",
      ),
    ).toBeNull();
  });

  it("opens the Review of an inline stack with no pull", async () => {
    server.source = "inline";
    server.draft = () => ({
      version: 7,
      savedBy: "ada",
      savedAt: "2026-10-28T10:00:00Z",
      baseSeq: 0,
      files: [{ path: "compose.yaml", content: COMPOSE, secret: false }],
      vars: [],
    });
    const onReview = vi.fn();
    mount({ onReview });
    await composeBox();
    fireEvent.click(screen.getByRole("button", { name: "Review and deploy" }));
    await waitFor(() => expect(onReview).toHaveBeenCalledTimes(1));
    expect(pulls()).toHaveLength(0);
  });
});

describe("overlay files on the Files tab", () => {
  it("replaces an overlay's content and saves it sealed", async () => {
    mount({ file: "db_pw.txt" });
    await screen.findByText("The content of db_pw.txt is hidden.");
    fireEvent.click(screen.getByRole("button", { name: "Replace content" }));
    await screen.findByRole("textbox", { name: "db_pw.txt, text" });
    typeIntoEditor("new-secret");
    fireEvent.click(screen.getByRole("button", { name: /^Save draft/ }));
    expect(await screen.findByText("Saved the draft of web.")).toBeTruthy();
    const files = puts()[0]?.body?.files as { path: string; content: string; secret: boolean }[];
    expect(files.find((f) => f.path === "db_pw.txt")).toEqual({
      path: "db_pw.txt",
      content: "new-secret",
      secret: true,
    });
    // The text is let go of once the server holds it: the card is back.
    expect(await screen.findByText("The content of db_pw.txt is hidden.")).toBeTruthy();
  });

  it("removes an overlay file and the next save leaves it out", async () => {
    mount({ file: "db_pw.txt" });
    await screen.findByText("The content of db_pw.txt is hidden.");
    fireEvent.keyDown(screen.getByRole("button", { name: "Actions for db_pw.txt" }), {
      key: "Enter",
    });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete file" }));
    // The open file moves to the first compose file, which opens read-only.
    expect((await composeBox()).getAttribute("aria-readonly")).toBe("true");
    expect(
      screen.getByText(
        "No overlay files. Add one for a secret or config file that isn't in the repository.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /^Save draft/ }));
    await screen.findByText("Saved the draft of web.");
    const files = puts()[0]?.body?.files as { path: string }[];
    expect(files.map((f) => f.path)).toEqual(["compose.yaml", "web.env"]);
  });

  it("gives a Viewer the files read-only, with the overlay's card and the muted footer line", async () => {
    server.role = "viewer";
    mount({ file: "db_pw.txt" });
    await screen.findByText("The content of db_pw.txt is hidden.");
    expect(screen.queryByRole("button", { name: "Replace content" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add overlay file" })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Actions for/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Save draft/ })).toBeNull();
    expect(screen.getByText("You can read this stack's files but not change them.")).toBeTruthy();
  });
});
