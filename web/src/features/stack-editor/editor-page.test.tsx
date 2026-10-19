// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackEditorPage } from "@/features/stack-editor/editor-page";
import "@/i18n";
import { editorText, typeIntoEditor } from "@/test-editor";
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
  hasDraft: boolean;
  draft: () => Response | Promise<Response>;
  save: (body: Record<string, unknown>) => Response | Promise<Response>;
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
    hasDraft: false,
    draft: () => Response.json(draftBody()),
    save: (body) =>
      Response.json({
        changed: true,
        version: (body.version as number) + 1,
        savedBy: "grace",
        savedAt: new Date().toISOString(),
      }),
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
        body: req.method === "PUT" ? await req.clone().json() : undefined,
      };
      server.sent.push(entry);
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: server.role } }));
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
        return req.method === "PUT" ? server.save(entry.body ?? {}) : server.draft();
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

function mount(file?: string) {
  const root = createRootRoute({
    component: () => (
      <TooltipProvider>
        <StackEditorPage envId={ENV} stackId={STACK} file={file} />
        <Toaster />
      </TooltipProvider>
    ),
  });
  const router = createRouter({
    routeTree: root.addChildren([createRoute({ getParentRoute: () => root, path: "/" })]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
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

  it("shows a skeleton and a Save draft that cannot be used until the draft is read", async () => {
    server.draft = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByRole("status", { name: "Loading the draft" })).toBeTruthy();
    const save = await screen.findByRole("button", { name: "Save draft" });
    expect((save as HTMLButtonElement).disabled).toBe(true);
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
    const writes = server.sent.filter((s) => s.method !== "GET");
    expect(writes.every((s) => s.path === `/api/env/${ENV}/stacks/${STACK}/draft`)).toBe(true);
  });
});
