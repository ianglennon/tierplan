// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createBrowserHistory,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackEditorPage } from "@/features/stack-editor/editor-page";
import "@/i18n";
import { completionStatus, currentCompletions, startCompletion } from "@codemirror/autocomplete";
import { editorText, editorView, typeIntoEditor } from "@/test-editor";
import { makeMe } from "@/test-me";
import { settled } from "@/test-settle";

// The Editor tab (plan 07-03, STK-01, STK-05, D-08): the files of the stack's one shared draft in
// a CodeMirror surface, saved explicitly. The server is a stubbed fetch; the router, the query
// cache, the toaster and the page are the real ones.

const ENV = "e1";
const STACK = "s1";
const COMPOSE = "services:\n  web:\n    image: nginx:1.27-alpine\n";

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

interface Server {
  sent: Sent[];
  role: "viewer" | "operator";
  // The user holds secrets.reveal as an extra on the environment.
  canReveal: boolean;
  // The answer to a reveal of a secret variable's value (plan 07-10).
  reveal: (name: string) => Response | Promise<Response>;
  hasDraft: boolean;
  draft: () => Response | Promise<Response>;
  save: (body: Record<string, unknown>) => Response | Promise<Response>;
  discard: () => Response | Promise<Response>;
  // The answer to a check of the working copy (plan 07-06).
  check: (body: Record<string, unknown>) => Response | Promise<Response>;
}

let server: Server;
let client: QueryClient;

const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

function draftBody(over: Record<string, unknown> = {}) {
  return {
    version: 4,
    savedBy: "ada",
    savedAt: "2026-10-19T10:00:00Z",
    baseSeq: 0,
    files: [{ path: "compose.yaml", content: COMPOSE, secret: false }],
    vars: [],
    ...over,
  };
}

beforeEach(() => {
  server = {
    sent: [],
    role: "operator",
    canReveal: false,
    reveal: () => Response.json({ value: "the-saved-value" }),
    hasDraft: false,
    draft: () => Response.json(draftBody()),
    save: (body) =>
      Response.json({
        changed: true,
        version: (body.version as number) + 1,
        savedBy: "grace",
        savedAt: new Date().toISOString(),
      }),
    discard: () => new Response(null, { status: 204 }),
    check: () => Response.json({ problems: [], unusedFiles: [] }),
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
        return Response.json(
          makeMe({
            envs: { [ENV]: server.role },
            extras: server.canReveal ? { [ENV]: ["secrets.reveal"] } : undefined,
          }),
        );
      }
      if (req.method === "POST" && path.startsWith(`/api/env/${ENV}/stacks/${STACK}/variables/`)) {
        return server.reveal(path.split("/")[7] ?? "");
      }
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}`) {
        return Response.json({
          id: STACK,
          name: "tracer",
          target: "swarm",
          source: "inline",
          state: "not_deployed",
          lastDeployedAt: "",
          lastDeployedBy: "",
          hasDraft: server.hasDraft,
          envId: ENV,
          sourceRef: "",
          createdBy: "ada",
          createdAt: "2026-10-19T10:00:00Z",
          stopped: false,
        });
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/draft`) {
        if (req.method === "PUT") return server.save(entry.body ?? {});
        if (req.method === "DELETE") return server.discard();
        return server.draft();
      }
      if (path === `/api/env/${ENV}/stacks/${STACK}/check` && req.method === "POST") {
        return server.check(entry.body ?? {});
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

function mount(
  file?: string,
  onFileChange?: (path: string | undefined) => void,
  history = createMemoryHistory({ initialEntries: ["/"] }),
  pane?: "files" | "variables",
  onPaneChange?: (pane: "files" | "variables" | "rendered") => void,
) {
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
        component: () => (
          <StackEditorPage
            envId={ENV}
            stackId={STACK}
            file={file}
            onFileChange={onFileChange}
            pane={pane}
            onPaneChange={onPaneChange}
          />
        ),
      }),
      createRoute({ getParentRoute: () => root, path: "/away", component: () => <p>Away</p> }),
    ]),
    history,
  });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

async function ready() {
  return screen.findByRole("textbox", { name: "compose.yaml, YAML" });
}

const saveButton = () => screen.getByRole("button", { name: /^(Save draft|Saving…)$/ });

describe("the Files pane", () => {
  it("opens compose.yaml in a labelled editor with the saved text", async () => {
    mount();
    const box = await ready();
    expect(box.getAttribute("aria-multiline")).toBe("true");
    expect(box.getAttribute("aria-readonly")).toBeNull();
    expect(editorText()).toBe(COMPOSE);
  });

  it("names the keys in the helper text, each in a kbd", async () => {
    mount();
    await ready();
    const keys = Array.from(document.querySelectorAll("kbd[data-slot='kbd']")).map(
      (k) => k.textContent,
    );
    expect(keys).toEqual(expect.arrayContaining(["Ctrl+]", "Ctrl+[", "Ctrl+S"]));
    expect(screen.getByText(/Tab leaves the editor\./)).toBeTruthy();
  });

  it("shows a skeleton and footer buttons that cannot be used until the draft is read", async () => {
    server.hasDraft = true;
    server.draft = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByRole("status", { name: "Loading the draft" })).toBeTruthy();
    const save = await screen.findByRole("button", { name: "Save draft" });
    expect((save as HTMLButtonElement).disabled).toBe(true);
    const discard = await screen.findByRole("button", { name: "Discard draft" });
    expect((discard as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("says so, with a way to try again, when the draft cannot be read", async () => {
    server.draft = () => problem(500, "internal");
    mount();
    const retry = await screen.findByRole("button", { name: "Retry loading the draft of tracer" });
    server.draft = () => Response.json(draftBody());
    fireEvent.click(retry);
    await ready();
  });
});

describe("the footer", () => {
  it("reads No draft yet for a stack whose draft was never saved", async () => {
    mount();
    await ready();
    expect(screen.getByText("No draft yet")).toBeTruthy();
  });

  it("reads Saved by when the stack has a saved draft", async () => {
    server.hasDraft = true;
    mount();
    await ready();
    expect(await screen.findByText(/Saved/)).toBeTruthy();
    expect(screen.getByText(/by ada/)).toBeTruthy();
    expect(screen.queryByText("No draft yet")).toBeNull();
  });

  it("reads Unsaved changes once the text is edited", async () => {
    mount();
    await ready();
    typeIntoEditor(`${COMPOSE}# more\n`);
    expect(await screen.findByText("Unsaved changes")).toBeTruthy();
    expect(screen.queryByText("No draft yet")).toBeNull();
  });

  it("goes back to the saved words when the edit is undone", async () => {
    mount();
    await ready();
    typeIntoEditor("x");
    await screen.findByText("Unsaved changes");
    typeIntoEditor(COMPOSE);
    expect(await screen.findByText("No draft yet")).toBeTruthy();
  });
});

describe("Save draft", () => {
  it("sends the working copy with the version it read, then reads Saved by the saver and toasts", async () => {
    mount();
    await ready();
    const edited = `${COMPOSE}# more\n`;
    typeIntoEditor(edited);
    fireEvent.click(saveButton());
    expect(await screen.findByText("Saved the draft of tracer.")).toBeTruthy();
    expect(puts()).toHaveLength(1);
    expect(puts()[0]?.path).toBe(`/api/env/${ENV}/stacks/${STACK}/draft`);
    expect(puts()[0]?.body).toEqual({
      version: 4,
      files: [{ path: "compose.yaml", content: edited, secret: false }],
      vars: [],
    });
    expect(await screen.findByText(/by grace/)).toBeTruthy();
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    expect(editorText()).toBe(edited);
  });

  it("sends the version the last save answered with on the next save", async () => {
    mount();
    await ready();
    typeIntoEditor("a");
    fireEvent.click(saveButton());
    await screen.findByText(/by grace/);
    typeIntoEditor("b");
    await screen.findByText("Unsaved changes");
    fireEvent.click(saveButton());
    await waitFor(() => expect(puts()).toHaveLength(2));
    expect(puts()[1]?.body?.version).toBe(5);
  });

  it("saves on Ctrl+S inside the editor and keeps the browser's own save from opening", async () => {
    mount();
    const box = await ready();
    typeIntoEditor(`${COMPOSE}# more\n`);
    const notPrevented = fireEvent.keyDown(box, { key: "s", ctrlKey: true });
    expect(notPrevented).toBe(false);
    await screen.findByText("Saved the draft of tracer.");
    expect(puts()).toHaveLength(1);
  });

  it("saves on Cmd+S too", async () => {
    mount();
    const box = await ready();
    typeIntoEditor("x");
    fireEvent.keyDown(box, { key: "s", metaKey: true });
    await screen.findByText("Saved the draft of tracer.");
  });

  it("does not save twice when pressed twice while it is saving", async () => {
    let release: (r: Response) => void = () => {};
    server.save = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    await ready();
    typeIntoEditor("x");
    fireEvent.click(saveButton());
    expect(await screen.findByRole("button", { name: "Saving…" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Saving…" }));
    release(
      Response.json({ changed: true, version: 5, savedBy: "ada", savedAt: "2026-10-19T11:00:00Z" }),
    );
    await settled(client);
    expect(puts()).toHaveLength(1);
  });

  it("keeps the text and says so in an alert when the save fails", async () => {
    server.save = () => problem(500, "internal");
    mount();
    await ready();
    typeIntoEditor("my work");
    fireEvent.click(saveButton());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Something went wrong on the server.");
    expect(editorText()).toBe("my work");
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
    expect(screen.queryByText("Saved the draft of tracer.")).toBeNull();
  });

  it("says the draft may have been saved when the server does not answer", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const path = new URL(req.url).pathname;
        if (req.method === "PUT") throw new TypeError("network down");
        if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
        if (path.endsWith("/draft")) return Response.json(draftBody());
        return Response.json({}, { status: 404 });
      }),
    );
    mount();
    await ready();
    typeIntoEditor("x");
    fireEvent.click(saveButton());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("The draft may have been saved");
  });
});

describe("a Viewer", () => {
  it("reads the file but cannot change it, and has no footer buttons", async () => {
    server.role = "viewer";
    mount();
    const box = await ready();
    expect(box.getAttribute("aria-readonly")).toBe("true");
    expect(screen.getByText("You can read this stack's files but not change them.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Save draft" })).toBeNull();
  });

  it("does not save on Ctrl+S", async () => {
    server.role = "viewer";
    mount();
    const box = await ready();
    act(() => {
      fireEvent.keyDown(box, { key: "s", ctrlKey: true });
    });
    await settled(client);
    expect(puts()).toHaveLength(0);
  });
});

describe("what the page sends", () => {
  it("reads the draft once and writes only to the draft route", async () => {
    mount();
    await ready();
    typeIntoEditor("x");
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    await settled(client);
    const draftReads = server.sent.filter((s) => s.method === "GET" && s.path.endsWith("/draft"));
    expect(draftReads).toHaveLength(1);
    // A check is a POST that writes nothing: it is the one request that is not a read of the draft
    // and not a write to it.
    const writes = server.sent.filter(
      (s) => s.method !== "GET" && !s.path.endsWith(`/stacks/${STACK}/check`),
    );
    expect(writes.every((s) => s.path === `/api/env/${ENV}/stacks/${STACK}/draft`)).toBe(true);
  });
});

const withFiles = () =>
  draftBody({
    files: [
      { path: "compose.yaml", content: COMPOSE, secret: false },
      { path: "nginx.conf", content: "events {}\n", secret: false },
      { path: "config/app.env", content: "A=1\n", secret: false },
    ],
  });

const railButton = (name: string) =>
  within(screen.getByRole("navigation", { name: "Stack files" })).getByRole("button", { name });

describe("the file rail in the page", () => {
  it("lists the files of the draft and opens the one clicked in its own editor", async () => {
    server.draft = () => Response.json(withFiles());
    const changed = vi.fn();
    mount(undefined, changed);
    await ready();
    const names = within(screen.getByRole("navigation", { name: "Stack files" }))
      .getAllByRole("listitem")
      .map((li) => li.querySelector("[data-file]")?.getAttribute("data-file"));
    expect(names).toEqual(["compose.yaml", "config/app.env", "nginx.conf"]);
    fireEvent.click(railButton("nginx.conf"));
    expect(await screen.findByRole("textbox", { name: "nginx.conf, text" })).toBeTruthy();
    expect(editorText()).toBe("events {}\n");
    expect(changed).toHaveBeenCalledWith("nginx.conf");
    expect(railButton("nginx.conf").getAttribute("aria-current")).toBe("true");
  });

  it("opens the file the address names, and goes back to compose.yaml by dropping it", async () => {
    server.draft = () => Response.json(withFiles());
    const changed = vi.fn();
    mount("config/app.env", changed);
    await screen.findByRole("textbox", { name: "config/app.env, text" });
    expect(editorText()).toBe("A=1\n");
    fireEvent.click(railButton("compose.yaml"));
    expect(changed).toHaveBeenCalledWith(undefined);
  });

  it("ignores an address that names a file the draft does not have", async () => {
    server.draft = () => Response.json(withFiles());
    mount("gone.txt");
    await ready();
  });

  it("shows a skeleton rail with the skeleton editor until the draft is read", async () => {
    server.draft = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByTestId("rail-skeleton")).toBeTruthy();
    expect(screen.queryByRole("navigation", { name: "Stack files" })).toBeNull();
  });

  it("adds a file, opens it empty, and saves it with the others in path order", async () => {
    server.draft = () => Response.json(withFiles());
    const changed = vi.fn();
    mount(undefined, changed);
    await ready();
    fireEvent.click(screen.getByRole("button", { name: "Add file" }));
    const dialog = await screen.findByRole("dialog", { name: "Add a file" });
    fireEvent.change(within(dialog).getByLabelText("File name"), { target: { value: ".env" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add file" }));
    await screen.findByRole("textbox", { name: ".env, text" });
    expect(editorText()).toBe("");
    expect(changed).toHaveBeenCalledWith(".env");
    expect(await screen.findByText("Unsaved changes")).toBeTruthy();

    typeIntoEditor("PORT=80\n");
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    expect(puts()[0]?.body?.files).toEqual([
      { path: "compose.yaml", content: COMPOSE, secret: false },
      { path: ".env", content: "PORT=80\n", secret: false },
      { path: "config/app.env", content: "A=1\n", secret: false },
      { path: "nginx.conf", content: "events {}\n", secret: false },
    ]);
  });

  it("renames and deletes files in the working copy, and sends neither until Save", async () => {
    server.draft = () => Response.json(withFiles());
    mount();
    await ready();
    const rail = () => screen.getByRole("navigation", { name: "Stack files" });

    fireEvent.keyDown(within(rail()).getByRole("button", { name: "Actions for nginx.conf" }), {
      key: "Enter",
    });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Rename…" }));
    const dialog = await screen.findByRole("dialog", { name: "Rename nginx.conf" });
    fireEvent.change(within(dialog).getByLabelText("File name"), { target: { value: "web.conf" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    fireEvent.keyDown(within(rail()).getByRole("button", { name: "Actions for config/app.env" }), {
      key: "Enter",
    });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete file" }));

    expect(within(rail()).getByRole("button", { name: "web.conf" })).toBeTruthy();
    expect(within(rail()).queryByRole("button", { name: "config/app.env" })).toBeNull();
    expect(puts()).toHaveLength(0);
    expect(await screen.findByText("Unsaved changes")).toBeTruthy();

    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    const sentFiles = (puts()[0]?.body?.files ?? []) as { path: string }[];
    expect(sentFiles.map((f) => f.path)).toEqual(["compose.yaml", "web.conf"]);
  });

  it("opens compose.yaml when the open file is deleted", async () => {
    server.draft = () => Response.json(withFiles());
    mount();
    await ready();
    fireEvent.click(railButton("nginx.conf"));
    await screen.findByRole("textbox", { name: "nginx.conf, text" });
    fireEvent.keyDown(railButton("Actions for nginx.conf"), { key: "Enter" });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete file" }));
    await ready();
  });

  it("gives a Viewer the rail to read with, and no way to change it", async () => {
    server.role = "viewer";
    server.draft = () => Response.json(withFiles());
    mount();
    await ready();
    expect(screen.queryByRole("button", { name: "Add file" })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Actions for/ })).toBeNull();
    fireEvent.click(railButton("nginx.conf"));
    const box = await screen.findByRole("textbox", { name: "nginx.conf, text" });
    expect(box.getAttribute("aria-readonly")).toBe("true");
  });
});

describe("a save the server refuses over a file", () => {
  async function refused(type: string, location: string, status = 422) {
    server.save = () =>
      Response.json(
        { type, title: "x", status, detail: "d", errors: [{ location, message: "m" }] },
        { status },
      );
    mount();
    await ready();
    typeIntoEditor("x");
    fireEvent.click(saveButton());
    return screen.findByRole("alert");
  }

  it("says a path is not allowed", async () => {
    const alert = await refused("file_path_invalid", "../x");
    expect(alert.textContent).toBe("Use a path inside this stack, without “..” or a leading /.");
  });

  it("says a name is taken, naming it", async () => {
    const alert = await refused("file_name_taken", "a.env");
    expect(alert.textContent).toBe("A file named a.env already exists.");
  });

  it("says a file is too large", async () => {
    const alert = await refused("file_too_large", "big.bin");
    expect(alert.textContent).toBe("Files can be at most 1 MB.");
  });

  it("says the stack is too large", async () => {
    const alert = await refused("stack_too_large", "files");
    expect(alert.textContent).toBe("A stack can hold at most 50 files and 8 MB in all.");
  });

  it("says a compose file is missing", async () => {
    const alert = await refused("compose_missing", "compose.yaml");
    expect(alert.textContent).toBe("A stack needs a compose.yaml.");
  });

  it("keeps everything that was typed", async () => {
    await refused("file_path_invalid", "../x");
    expect(editorText()).toBe("x");
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
  });
});

const deletes = () => server.sent.filter((s) => s.method === "DELETE");

describe("Discard draft", () => {
  const discardButton = () => screen.queryByRole("button", { name: "Discard draft" });

  it("is not offered for a stack with no saved draft and no edits", async () => {
    mount();
    await ready();
    expect(discardButton()).toBeNull();
  });

  it("is offered while a saved draft exists", async () => {
    server.hasDraft = true;
    mount();
    await ready();
    expect(await screen.findByRole("button", { name: "Discard draft" })).toBeTruthy();
  });

  it("is offered once there are unsaved edits", async () => {
    mount();
    await ready();
    typeIntoEditor("x");
    expect(await screen.findByRole("button", { name: "Discard draft" })).toBeTruthy();
  });

  it("asks first, with the focus on Cancel, and sends nothing when cancelled", async () => {
    server.hasDraft = true;
    mount();
    await ready();
    fireEvent.click(await screen.findByRole("button", { name: "Discard draft" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Discard the draft?" });
    expect(
      within(dialog).getByText(
        "This deletes the saved draft, including files and variables, and the editor goes back to what is deployed. It can't be undone.",
      ),
    ).toBeTruthy();
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(deletes()).toHaveLength(0);
  });

  it("deletes the draft, shows what is deployed, drops unsaved edits and toasts", async () => {
    server.hasDraft = true;
    const deployed = "services:\n  web:\n    image: nginx:1.26\n";
    server.discard = () => {
      server.hasDraft = false;
      server.draft = () =>
        Response.json(
          draftBody({
            version: 0,
            isDraft: false,
            files: [{ path: "compose.yaml", content: deployed, secret: false }],
          }),
        );
      return new Response(null, { status: 204 });
    };
    mount();
    await ready();
    typeIntoEditor("unsaved work");
    fireEvent.click(await screen.findByRole("button", { name: "Discard draft" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Discard the draft?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Discard draft" }));

    expect(await screen.findByText("Discarded the draft of tracer.")).toBeTruthy();
    expect(deletes()).toHaveLength(1);
    expect(deletes()[0]?.path).toBe(`/api/env/${ENV}/stacks/${STACK}/draft`);
    await waitFor(() => expect(editorText()).toBe(deployed));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(await screen.findByText("No draft yet")).toBeTruthy();
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    await waitFor(() => expect(discardButton()).toBeNull());
  });

  it("saves from the deployed definition at version 0 after a discard", async () => {
    server.hasDraft = true;
    server.discard = () => {
      server.hasDraft = false;
      server.draft = () => Response.json(draftBody({ version: 0, isDraft: false }));
      return new Response(null, { status: 204 });
    };
    mount();
    await ready();
    fireEvent.click(await screen.findByRole("button", { name: "Discard draft" }));
    fireEvent.click(
      within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Discard draft" }),
    );
    await screen.findByText("Discarded the draft of tracer.");
    await waitFor(() => expect(discardButton()).toBeNull());
    typeIntoEditor("fresh");
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    expect(puts()[0]?.body?.version).toBe(0);
  });

  it("keeps the dialog open and says so when the discard fails", async () => {
    server.hasDraft = true;
    server.discard = () => problem(500, "internal");
    mount();
    await ready();
    fireEvent.click(await screen.findByRole("button", { name: "Discard draft" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Discard the draft?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Discard draft" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("Something went wrong on the server.");
    expect(screen.getByRole("alertdialog", { name: "Discard the draft?" })).toBeTruthy();
  });

  it("is not offered to a Viewer", async () => {
    server.role = "viewer";
    server.hasDraft = true;
    mount();
    await ready();
    expect(discardButton()).toBeNull();
  });
});

describe("a draft someone else saved meanwhile", () => {
  const conflict = (user: string, version: string) => () =>
    Response.json(
      {
        type: "draft_conflict",
        title: "Conflict",
        status: 409,
        detail: "d",
        errors: [
          { location: "savedBy", message: user },
          { location: "savedAt", message: "2026-10-19T11:00:00Z" },
          { location: "version", message: version },
        ],
      },
      { status: 409 },
    );
  const sentence = (user: string) =>
    `${user} saved this draft while you were editing. Reloading shows their version and drops your unsaved changes.`;

  it("shows who saved it above the footer, with Reload draft and Save over it, and keeps the edits", async () => {
    server.save = conflict("grace", "5");
    mount();
    await ready();
    typeIntoEditor("mine");
    fireEvent.click(saveButton());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(sentence("grace"));
    expect(within(alert).getByRole("button", { name: "Reload draft" })).toBeTruthy();
    expect(within(alert).getByRole("button", { name: "Save over it" })).toBeTruthy();
    expect(editorText()).toBe("mine");
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
  });

  it("reloads their version on Reload draft, dropping the edits, and saves from it", async () => {
    server.save = conflict("grace", "5");
    mount();
    await ready();
    typeIntoEditor("mine");
    fireEvent.click(saveButton());
    const alert = await screen.findByRole("alert");
    server.draft = () =>
      Response.json(
        draftBody({
          version: 5,
          savedBy: "grace",
          files: [{ path: "compose.yaml", content: "theirs", secret: false }],
        }),
      );
    server.save = (body) =>
      Response.json({
        changed: true,
        version: (body.version as number) + 1,
        savedBy: "ada",
        savedAt: "2026-10-19T12:00:00Z",
      });
    fireEvent.click(within(alert).getByRole("button", { name: "Reload draft" }));

    await waitFor(() => expect(editorText()).toBe("theirs"));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    typeIntoEditor("theirs, then mine");
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    expect(puts().at(-1)?.body?.version).toBe(5);
  });

  it("sends the working copy again with the server's version on Save over it", async () => {
    server.save = conflict("grace", "5");
    mount();
    await ready();
    typeIntoEditor("mine");
    fireEvent.click(saveButton());
    const alert = await screen.findByRole("alert");
    server.save = (body) =>
      Response.json({
        changed: true,
        version: (body.version as number) + 1,
        savedBy: "ada",
        savedAt: "2026-10-19T12:00:00Z",
      });
    fireEvent.click(within(alert).getByRole("button", { name: "Save over it" }));

    expect(await screen.findByText("Saved the draft of tracer.")).toBeTruthy();
    expect(puts()).toHaveLength(2);
    expect(puts()[1]?.body).toEqual({
      version: 5,
      files: [{ path: "compose.yaml", content: "mine", secret: false }],
      vars: [],
    });
    expect(screen.queryByText(sentence("grace"))).toBeNull();
    expect(editorText()).toBe("mine");
  });

  it("asks again when someone saved yet again, naming them", async () => {
    server.save = conflict("grace", "5");
    mount();
    await ready();
    typeIntoEditor("mine");
    fireEvent.click(saveButton());
    const alert = await screen.findByRole("alert");
    server.save = conflict("linus", "6");
    fireEvent.click(within(alert).getByRole("button", { name: "Save over it" }));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain(sentence("linus")));
    server.save = (body) =>
      Response.json({
        changed: true,
        version: (body.version as number) + 1,
        savedBy: "ada",
        savedAt: "2026-10-19T12:00:00Z",
      });
    fireEvent.click(screen.getByRole("button", { name: "Save over it" }));
    await screen.findByText("Saved the draft of tracer.");
    expect(puts().at(-1)?.body?.version).toBe(6);
  });

  it("says the draft was discarded when nobody saved it", async () => {
    server.save = conflict("", "0");
    mount();
    await ready();
    typeIntoEditor("mine");
    fireEvent.click(saveButton());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "The draft was discarded while you were editing. Reloading shows what is deployed now and drops your unsaved changes.",
    );
  });

  it("clears the alert when a later save succeeds", async () => {
    server.save = conflict("grace", "5");
    mount();
    await ready();
    typeIntoEditor("mine");
    fireEvent.click(saveButton());
    await screen.findByRole("alert");
    server.save = (body) =>
      Response.json({
        changed: true,
        version: (body.version as number) + 1,
        savedBy: "ada",
        savedAt: "2026-10-19T12:00:00Z",
      });
    fireEvent.click(screen.getByRole("button", { name: "Save over it" }));
    await screen.findByText("Saved the draft of tracer.");
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("leaving with unsaved edits", () => {
  async function dirtyPage() {
    const router = mount();
    await ready();
    typeIntoEditor("mine");
    await screen.findByText("Unsaved changes");
    return router;
  }

  it("asks Discard changes? and stays, with the edits, on Keep editing", async () => {
    const router = await dirtyPage();
    void router.navigate({ to: "/away" as never });
    const dialog = await screen.findByRole("alertdialog", { name: "Discard changes?" });
    expect(within(dialog).getByText("Your edits to tracer haven't been saved.")).toBeTruthy();
    expect(document.activeElement).toBe(
      within(dialog).getByRole("button", { name: "Keep editing" }),
    );
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(router.state.location.pathname).toBe("/");
    expect(editorText()).toBe("mine");
  });

  it("leaves on Discard changes", async () => {
    const router = await dirtyPage();
    void router.navigate({ to: "/away" as never });
    const dialog = await screen.findByRole("alertdialog", { name: "Discard changes?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Discard changes" }));
    expect(await screen.findByText("Away")).toBeTruthy();
    expect(router.state.location.pathname).toBe("/away");
  });

  it("leaves a clean editor without asking", async () => {
    const router = mount();
    await ready();
    await router.navigate({ to: "/away" as never });
    expect(await screen.findByText("Away")).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("leaves without asking once the edits are saved", async () => {
    const router = await dirtyPage();
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    await router.navigate({ to: "/away" as never });
    expect(await screen.findByText("Away")).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("does not ask when only the open file changes", async () => {
    server.draft = () =>
      Response.json(
        draftBody({
          files: [
            { path: "compose.yaml", content: COMPOSE, secret: false },
            { path: "a.env", content: "A=1", secret: false },
          ],
        }),
      );
    const router = await dirtyPage();
    await router.navigate({ to: "/", search: { file: "a.env" } as never });
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("makes the browser ask before the tab closes only while there are unsaved edits", async () => {
    // Only the browser's history listens for the tab closing.
    window.history.replaceState({}, "", "/");
    mount(undefined, undefined, createBrowserHistory());
    await ready();
    const clean = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(clean);
    expect(clean.defaultPrevented).toBe(false);
    typeIntoEditor("mine");
    await screen.findByText("Unsaved changes");
    const dirty = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(dirty);
    expect(dirty.defaultPrevented).toBe(true);
  });
});

// The Problems panel (plan 07-06, STK-01, D-10): the server's check of the working copy, listed with
// the line of each problem, and a click on one opens the file at that line.

const checks = () => server.sent.filter((s) => s.method === "POST" && s.path.endsWith("/check"));

const problemOf = (over: Record<string, unknown> = {}) => ({
  severity: "error",
  code: "yaml_invalid",
  message: "Line 3: found character that cannot start any token",
  file: "compose.yaml",
  line: 3,
  col: 1,
  endLine: 3,
  endCol: 2,
  ...over,
});

describe("the Problems panel", () => {
  it("counts the problems and names each row by severity, message, file and line", async () => {
    server.check = () => Response.json({ problems: [problemOf()], unusedFiles: [] });
    mount();
    await ready();
    const row = await screen.findByRole("button", {
      name: "Error: Line 3: found character that cannot start any token, compose.yaml line 3",
    });
    expect(row.textContent).toContain("compose.yaml:3");
    expect(screen.getByText("1 error, 0 warnings")).toBeTruthy();
  });

  it("says there are no problems when the check finds none", async () => {
    mount();
    await ready();
    expect(await screen.findByText("No problems")).toBeTruthy();
  });

  it("opens the file of a problem and puts the cursor on its line when it is clicked", async () => {
    server.draft = () =>
      Response.json(
        draftBody({
          files: [
            { path: "app.env", content: "A=1\nB=2\nC=3\n", secret: false },
            { path: "compose.yaml", content: COMPOSE, secret: false },
          ],
        }),
      );
    server.check = () =>
      Response.json({
        problems: [problemOf({ file: "app.env", line: 2, col: 2, message: "B is wrong" })],
        unusedFiles: [],
      });
    mount();
    await ready();
    fireEvent.click(
      await screen.findByRole("button", { name: "Error: B is wrong, app.env line 2" }),
    );
    await screen.findByRole("textbox", { name: "app.env, text" });
    // Line 2 starts after "A=1\n" (4 characters) and the cursor is on column 2.
    await waitFor(() => expect(editorView().state.selection.main.head).toBe(5));
    expect(document.activeElement?.closest(".cm-editor")).not.toBeNull();
  });

  it("opens by itself on the first error and stays as the person leaves it", async () => {
    let answer = { problems: [problemOf()], unusedFiles: [] as string[] };
    server.check = () => Response.json(answer);
    mount();
    await ready();
    const header = await screen.findByRole("button", { name: /Problems/ });
    await waitFor(() => expect(header.getAttribute("aria-expanded")).toBe("true"));
    fireEvent.click(header);
    expect(header.getAttribute("aria-expanded")).toBe("false");
    // A save asks again; a closed panel stays closed when the answer changes.
    answer = {
      problems: [problemOf(), problemOf({ line: 4, message: "Another" })],
      unusedFiles: [],
    };
    typeIntoEditor("services: {}\n");
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    await screen.findByText("2 errors, 0 warnings");
    expect(header.getAttribute("aria-expanded")).toBe("false");
  });

  it("draws a warning of the check at its line in the gutter, with the message the panel lists", async () => {
    const message = "`build` is ignored on Swarm. Build and push the image, then set `image`.";
    server.check = () =>
      Response.json({
        problems: [
          problemOf({
            severity: "warning",
            code: "ignored_on_target",
            message,
            line: 2,
            col: 1,
            endLine: 2,
            endCol: 6,
          }),
        ],
        unusedFiles: [],
      });
    mount();
    await ready();
    const mark = await waitFor(() => {
      const el = document.querySelector(".cm-lint-marker-warning");
      if (!el) throw new Error("no marker yet");
      return el;
    });
    expect(mark.getAttribute("aria-label")).toBe(`Warning: ${message}`);
    expect(document.querySelectorAll(".cm-line.cm-lint-line-warning")).toHaveLength(1);
    // The same message is a row of the panel (a warning does not open it by itself), so nothing
    // lives only on the marker.
    fireEvent.click(await screen.findByRole("button", { name: /Problems/ }));
    expect(
      await screen.findByRole("button", { name: `Warning: ${message}, compose.yaml line 2` }),
    ).toBeTruthy();
    // The editor was not recreated to draw it, and the text is what was loaded.
    expect(editorView().state.doc.toString()).toBe(COMPOSE);
  });

  it("draws only the problems of the open file in its gutter", async () => {
    server.check = () =>
      Response.json({
        problems: [problemOf({ file: "other.env", line: 1, message: "Elsewhere" })],
        unusedFiles: [],
      });
    mount();
    await ready();
    await screen.findByRole("button", { name: "Error: Elsewhere, other.env line 1" });
    expect(document.querySelector(".cm-lint-marker")).toBeNull();
  });

  it("labels a file the compose file does not use as Not used in the rail", async () => {
    server.draft = () =>
      Response.json(
        draftBody({
          files: [
            { path: ".env", content: "A=1\n", secret: false },
            { path: "compose.yaml", content: COMPOSE, secret: false },
          ],
        }),
      );
    server.check = () => Response.json({ problems: [], unusedFiles: [".env"] });
    mount();
    await ready();
    const row = document.querySelector<HTMLElement>('[data-file=".env"]');
    if (!row) throw new Error("no row for .env");
    await waitFor(() => expect(within(row).getByText("Not used")).toBeTruthy());
    const compose = document.querySelector<HTMLElement>('[data-file="compose.yaml"]');
    expect(within(compose as HTMLElement).queryByText("Not used")).toBeNull();
  });

  it("checks again after a save", async () => {
    mount();
    await ready();
    await waitFor(() => expect(checks()).toHaveLength(1));
    typeIntoEditor("services: {}\n");
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    await waitFor(() => expect(checks().length).toBeGreaterThanOrEqual(2));
  });

  it("sends the working copy in the save shape, with a hidden secret as the keep marker", async () => {
    server.draft = () =>
      Response.json(
        draftBody({
          files: [
            { path: "compose.yaml", content: COMPOSE, secret: false },
            { path: "key.pem", content: "<hidden>", secret: true },
          ],
        }),
      );
    mount();
    await ready();
    await waitFor(() => expect(checks()).toHaveLength(1));
    expect(checks()[0]?.body).toEqual({
      files: [
        { path: "compose.yaml", content: COMPOSE, secret: false },
        { path: "key.pem", content: "<hidden>", secret: true },
      ],
      vars: [],
    });
  });

  it("keeps the last results and says it will retry when a check fails", async () => {
    let fail = false;
    server.check = () =>
      fail
        ? problem(500, "about:blank")
        : Response.json({ problems: [problemOf()], unusedFiles: [] });
    mount();
    await ready();
    await screen.findByText("1 error, 0 warnings");
    fail = true;
    typeIntoEditor(`${COMPOSE}# more\n`);
    fireEvent.click(saveButton());
    await screen.findByText("Couldn't check the stack. It will retry.");
    expect(screen.getByText("1 error, 0 warnings")).toBeTruthy();
  });

  it("shows Checking… while the server has not answered", async () => {
    let release: (r: Response) => void = () => {};
    server.check = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    await ready();
    expect(await screen.findByText("Checking…")).toBeTruthy();
    await act(async () => {
      release(Response.json({ problems: [], unusedFiles: [] }));
    });
    await waitFor(() => expect(screen.queryByText("Checking…")).toBeNull());
  });
});

// The Variables pane (plan 07-10, STK-04): the same working copy as the Files pane, as a table.
describe("the Variables pane", () => {
  const STARTER =
    'services:\n  web:\n    image: nginx:1.27-alpine\n    ports:\n      - "$' +
    '{WEB_PORT:-8080}:80"\n';
  const webPort = (over: Record<string, unknown> = {}) => ({
    name: "WEB_PORT",
    value: "8080",
    secret: false,
    source: "default",
    default: "8080",
    hasDefault: true,
    required: false,
    unused: false,
    places: [{ file: "compose.yaml", line: 5 }],
    ...over,
  });

  it("lists the variable the compose file refers to, resolved from its default", async () => {
    server.draft = () =>
      Response.json(
        draftBody({ files: [{ path: "compose.yaml", content: STARTER, secret: false }] }),
      );
    server.check = () => Response.json({ problems: [], unusedFiles: [], variables: [webPort()] });
    mount(undefined, undefined, undefined, "variables");
    const input = (await screen.findByLabelText("Value of WEB_PORT")) as HTMLInputElement;
    expect(input.placeholder).toBe("8080");
    expect(await screen.findByText("From the compose file's default")).toBeTruthy();
    expect(screen.queryByTestId("stack-file-editor")).toBeNull();
  });

  it("sends the check the value typed, shows what the server resolves from it, and saves it", async () => {
    server.draft = () =>
      Response.json(
        draftBody({ files: [{ path: "compose.yaml", content: STARTER, secret: false }] }),
      );
    server.check = (body) => {
      const vars = (body.vars as { name: string; value: string }[]) ?? [];
      const typed = vars.find((v) => v.name === "WEB_PORT");
      return Response.json({
        problems: [],
        unusedFiles: [],
        variables: [typed ? webPort({ value: typed.value, source: "table" }) : webPort()],
      });
    };
    mount(undefined, undefined, undefined, "variables");
    const input = (await screen.findByLabelText("Value of WEB_PORT")) as HTMLInputElement;
    await screen.findByText("From the compose file's default");
    fireEvent.change(input, { target: { value: "9090" } });
    expect(await screen.findByText("From the table")).toBeTruthy();
    expect(screen.getByText("9090", { selector: "span" })).toBeTruthy();
    expect(await screen.findByText("Unsaved changes")).toBeTruthy();
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    expect(puts()[0]?.body?.vars).toEqual([{ name: "WEB_PORT", value: "9090", secret: false }]);
  });

  it("imports a pasted .env into the table, marks it secret, and saves it only on Save draft", async () => {
    server.draft = () =>
      Response.json(
        draftBody({ files: [{ path: "compose.yaml", content: STARTER, secret: false }] }),
      );
    server.check = () => Response.json({ problems: [], unusedFiles: [], variables: [webPort()] });
    mount(undefined, undefined, undefined, "variables");
    await screen.findByLabelText("Value of WEB_PORT");
    fireEvent.click(screen.getByRole("button", { name: "Import .env" }));
    fireEvent.change(await screen.findByLabelText("Paste .env text"), {
      target: { value: "WEB_PORT=9090\nDB_PASS=hunter22\n# c\nbad line" },
    });
    fireEvent.click(screen.getByRole("checkbox", { name: "Mark every imported value as secret" }));
    fireEvent.click(screen.getByRole("button", { name: "Import variables" }));

    expect(
      await screen.findByText("Imported 2 variables. Mark the secrets, then save the draft."),
    ).toBeTruthy();
    expect(await screen.findByText("Unsaved changes")).toBeTruthy();
    // The import edits the working copy; nothing was sent until Save draft.
    expect(puts()).toHaveLength(0);
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    expect(puts()[0]?.body?.vars).toEqual([
      { name: "DB_PASS", value: "hunter22", secret: true },
      { name: "WEB_PORT", value: "9090", secret: true },
    ]);
  });

  it("saves a bulk-edited table, and refuses to leave the pane while the text does not parse", async () => {
    server.draft = () =>
      Response.json(
        draftBody({ files: [{ path: "compose.yaml", content: STARTER, secret: false }] }),
      );
    server.check = () => Response.json({ problems: [], unusedFiles: [], variables: [webPort()] });
    const onPaneChange = vi.fn();
    mount(undefined, undefined, undefined, "variables", onPaneChange);
    await screen.findByLabelText("Value of WEB_PORT");
    fireEvent.click(screen.getByRole("button", { name: "Bulk edit as text" }));
    const text = screen.getByRole("textbox", { name: "Variables as text" });
    fireEvent.change(text, { target: { value: "WEB_PORT=9090\nnot a line" } });

    const files = screen.getByRole("tab", { name: "Files" });
    fireEvent.mouseDown(files);
    fireEvent.click(files);
    expect(onPaneChange).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(screen.getByText("Line 2: write NAME=value."));

    fireEvent.change(text, { target: { value: "WEB_PORT=9090" } });
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    expect(puts()[0]?.body?.vars).toEqual([{ name: "WEB_PORT", value: "9090", secret: false }]);
    fireEvent.mouseDown(files);
    fireEvent.click(files);
    expect(onPaneChange).toHaveBeenCalledWith("files");
  });

  it("moves between the panes through the address", async () => {
    const onPaneChange = vi.fn();
    mount(undefined, undefined, undefined, "files", onPaneChange);
    await ready();
    const tab = screen.getByRole("tab", { name: "Variables" });
    fireEvent.mouseDown(tab);
    fireEvent.click(tab);
    expect(onPaneChange).toHaveBeenCalledWith("variables");
  });
});

describe("secret variables in the Variables pane", () => {
  const withToken = () => draftBody({ vars: [{ name: "TOKEN", value: "<hidden>", secret: true }] });
  const usesToken = (over: Record<string, unknown> = {}) => ({
    name: "TOKEN",
    value: "<hidden>",
    secret: true,
    source: "table",
    default: "",
    hasDefault: false,
    required: false,
    unused: true,
    places: [],
    ...over,
  });

  it("shows a saved secret as hidden, and an operator without secrets.reveal has no Show button", async () => {
    server.draft = () => Response.json(withToken());
    server.check = () => Response.json({ problems: [], unusedFiles: [], variables: [usesToken()] });
    mount(undefined, undefined, undefined, "variables");
    const input = (await screen.findByLabelText("Value of TOKEN")) as HTMLInputElement;
    expect(input.placeholder).toBe("Hidden. Type to replace.");
    expect(input.type).toBe("password");
    expect(screen.queryByRole("button", { name: "Show TOKEN" })).toBeNull();
  });

  it("lets a holder of secrets.reveal show the value, which comes from the reveal route and is not kept", async () => {
    server.canReveal = true;
    server.draft = () => Response.json(withToken());
    server.check = () => Response.json({ problems: [], unusedFiles: [], variables: [usesToken()] });
    mount(undefined, undefined, undefined, "variables");
    const show = await screen.findByRole("button", { name: "Show TOKEN" });
    fireEvent.click(show);
    await waitFor(() =>
      expect((screen.getByLabelText("Value of TOKEN") as HTMLInputElement).value).toBe(
        "the-saved-value",
      ),
    );
    expect(
      server.sent.some((x) => x.method === "POST" && x.path.endsWith("/variables/TOKEN/reveal")),
    ).toBe(true);
    // A reveal changes nothing in the draft: the page is not dirty and nothing is cached.
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain("the-saved-value");
  });

  it("sends a typed secret once, then lets go of it: the row is hidden again and the cache holds <hidden>", async () => {
    server.draft = () => Response.json(withToken());
    server.check = () => Response.json({ problems: [], unusedFiles: [], variables: [usesToken()] });
    mount(undefined, undefined, undefined, "variables");
    const input = (await screen.findByLabelText("Value of TOKEN")) as HTMLInputElement;
    fireEvent.change(input, { target: { value: "a-new-secret" } });
    expect(await screen.findByText("Unsaved changes")).toBeTruthy();
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    expect(puts()[0]?.body?.vars).toEqual([{ name: "TOKEN", value: "a-new-secret", secret: true }]);
    await waitFor(() =>
      expect((screen.getByLabelText("Value of TOKEN") as HTMLInputElement).placeholder).toBe(
        "Hidden. Type to replace.",
      ),
    );
    expect((screen.getByLabelText("Value of TOKEN") as HTMLInputElement).value).toBe("");
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain("a-new-secret");
  });

  it("keeps a hidden secret that was not touched as <hidden> in the save", async () => {
    server.draft = () => Response.json(withToken());
    server.check = () => Response.json({ problems: [], unusedFiles: [], variables: [usesToken()] });
    mount(undefined, undefined, undefined, "variables");
    await screen.findByLabelText("Value of TOKEN");
    fireEvent.click(await screen.findByRole("button", { name: "Add variable" }));
    fireEvent.change(screen.getByLabelText("Variable name"), { target: { value: "ZED" } });
    fireEvent.submit(screen.getByLabelText("Variable name").closest("form") as HTMLFormElement);
    fireEvent.click(saveButton());
    await screen.findByText("Saved the draft of tracer.");
    expect(puts()[0]?.body?.vars).toEqual([
      { name: "TOKEN", value: "<hidden>", secret: true },
      { name: "ZED", value: "", secret: false },
    ]);
  });

  it("names the variable when the server refuses a secret that is too short", async () => {
    server.draft = () => Response.json(draftBody());
    server.save = () =>
      Response.json(
        {
          type: "secret_too_short",
          title: "x",
          status: 422,
          detail: "d",
          errors: [{ location: "variable", message: "PIN" }],
        },
        { status: 422 },
      );
    mount(undefined, undefined, undefined, "variables");
    await screen.findByRole("button", { name: "Add variable" });
    fireEvent.click(screen.getByRole("button", { name: "Add variable" }));
    fireEvent.change(screen.getByLabelText("Variable name"), { target: { value: "PIN" } });
    fireEvent.submit(screen.getByLabelText("Variable name").closest("form") as HTMLFormElement);
    fireEvent.change(await screen.findByLabelText("Value of PIN"), { target: { value: "123" } });
    fireEvent.click(screen.getByRole("switch", { name: "Secret: PIN" }));
    fireEvent.click(saveButton());
    expect(await screen.findByText(/A secret value needs at least 4 characters.*PIN/)).toBeTruthy();
  });
});

// Completion and the places a variable is used (plan 07-10, STK-04, D-03).
describe("variables and the editor", () => {
  const STARTER =
    'services:\n  web:\n    image: nginx:1.27-alpine\n    ports:\n      - "$' +
    '{WEB_PORT:-8080}:80"\n';
  const resolvedPort = {
    name: "WEB_PORT",
    value: "8080",
    secret: false,
    source: "default",
    default: "8080",
    hasDefault: true,
    required: false,
    unused: false,
    places: [{ file: "compose.yaml", line: 5 }],
  };

  it("offers the names of the table and the variables the file refers to after ${, with where each comes from", async () => {
    server.draft = () =>
      Response.json(
        draftBody({
          files: [{ path: "compose.yaml", content: STARTER, secret: false }],
          vars: [{ name: "DB_NAME", value: "app", secret: false }],
        }),
      );
    server.check = () =>
      Response.json({ problems: [], unusedFiles: [], variables: [resolvedPort] });
    mount();
    await ready();
    // The first check must have answered, so the names the file refers to are known.
    await waitFor(() =>
      expect(server.sent.some((x) => x.path.endsWith("/check") && x.method === "POST")).toBe(true),
    );
    await settled(client);
    const view = editorView();
    const text = "${";
    typeIntoEditor(text);
    act(() => view.dispatch({ selection: { anchor: text.length } }));
    act(() => {
      startCompletion(view);
    });
    await waitFor(() => expect(completionStatus(view.state)).toBe("active"));
    const found = currentCompletions(view.state).map((c) => `${c.label}: ${c.detail}`);
    expect(found).toEqual(["DB_NAME: from the table", "WEB_PORT: default in the file"]);
  });

  it("opens the Files pane at the line of a place a variable is used", async () => {
    server.draft = () =>
      Response.json(
        draftBody({ files: [{ path: "compose.yaml", content: STARTER, secret: false }] }),
      );
    server.check = () =>
      Response.json({ problems: [], unusedFiles: [], variables: [resolvedPort] });
    const onPaneChange = vi.fn();
    mount(undefined, undefined, undefined, "variables", onPaneChange);
    fireEvent.click(await screen.findByRole("button", { name: "Used in 1 place" }));
    fireEvent.click(screen.getByRole("link", { name: "compose.yaml:5" }));
    expect(onPaneChange).toHaveBeenCalledWith("files", "compose.yaml");
  });
});
