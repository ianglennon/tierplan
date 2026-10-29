// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
// read-only, Bayplan's overlay files are sealed cards, and the only things to edit are overlays and variables. The
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
  draft: () => Record<string, unknown>;
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
    draft: () => gitDraft(),
    save: (body) =>
      Response.json({
        changed: true,
        version: (body.version as number) + 1,
        savedBy: "grace",
        savedAt: new Date().toISOString(),
        secretFiles: ["db_pw.txt"],
      }),
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
          source: "git",
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
