// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { editorView, typeIntoEditor } from "@/test-editor";
import { makeMe } from "@/test-me";

// The routes of a deploy, on the real route tree (plan 07-15): the editor, the Review and the
// console, with the flow caption in place of the tabs, the permission each needs and the hand-off
// from one to the next. The server is a stubbed fetch.

const COMPOSE = "services:\n  web:\n    image: nginx:1.27-alpine\n";
const D1 = `sha256:${"a1".repeat(32)}`;

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
let role: "viewer" | "operator";
let job: { state: string };

const spec = (replicas: number) => ({
  Name: "s1_web",
  Mode: { Replicated: { Replicas: replicas } },
  TaskTemplate: { ContainerSpec: { Image: "nginx:1.27" } },
});

const counts = (added = 0, changed = 0, removed = 0) => ({ added, changed, removed });

beforeEach(() => {
  sent = [];
  role = "operator";
  job = { state: "running" };
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const body =
        req.method === "POST" || req.method === "PUT"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path, body });
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(makeMe({ envs: { e1: role } }));
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.0.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.0.0",
        });
      }
      if (path === "/api/environments") {
        return Response.json({
          items: [
            {
              id: "e1",
              name: "Prod",
              kind: "local",
              type: "swarm",
              enabled: true,
              isSelfHost: true,
              createdAt: "2026-10-01T10:00:00Z",
              engine: { name: "Docker Engine", version: "29.8.2", apiVersion: "1.54" },
              status: { state: "online" },
            },
          ],
        });
      }
      if (path === "/api/me/recent-environments") return Response.json({ environmentIds: [] });
      if (path === "/api/env/e1/summary") {
        return Response.json({
          environment: { id: "e1", name: "Prod", type: "swarm", status: { state: "online" } },
          engine: {
            name: "Docker Engine",
            version: "29.8.2",
            apiVersion: "1.54",
            os: "linux",
            arch: "amd64",
            storageDriver: "overlayfs",
            ncpu: 8,
            memTotal: 17179869184,
          },
          counts: {
            containers: 0,
            containersRunning: 0,
            containersStopped: 0,
            images: 0,
            volumes: 0,
            networks: 0,
          },
          isWorker: false,
          selfContainerId: "",
          fetchedAt: "2026-10-01T10:00:00Z",
        });
      }
      if (path === "/api/env/e1/events") return Response.json({ items: [] });
      if (path === "/api/env/e1/stacks/s1") {
        return Response.json({
          id: "s1",
          name: "web",
          target: "swarm",
          source: "inline",
          state: "deployed",
          lastDeployedAt: "2026-10-19T11:00:00Z",
          lastDeployedBy: "bob",
          hasDraft: false,
          envId: "e1",
          sourceRef: "",
          createdBy: "ada",
          createdAt: "2026-10-19T10:00:00Z",
          stopped: false,
        });
      }
      if (path === "/api/env/e1/stacks/s1/draft") {
        if (req.method === "PUT") {
          return Response.json({
            changed: true,
            version: 5,
            savedBy: "ada",
            savedAt: "2026-10-19T12:00:00Z",
          });
        }
        return Response.json({
          version: 4,
          savedBy: "ada",
          savedAt: "2026-10-19T10:00:00Z",
          baseSeq: 0,
          files: [{ path: "compose.yaml", content: COMPOSE, secret: false }],
          vars: [],
        });
      }
      if (path === "/api/env/e1/stacks/s1/check") {
        return Response.json({ problems: [], unusedFiles: [] });
      }
      if (path === "/api/env/e1/stacks/s1/review") {
        return Response.json({
          draftVersion: 7,
          images: [
            {
              service: "web",
              image: "nginx:1.27",
              digest: D1,
              current: D1,
              state: "pinned",
              note: "",
            },
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
              current: spec(2),
              next: spec(3),
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
          },
        });
      }
      if (path === "/api/env/e1/stacks/s1/deploys" && req.method === "POST") {
        return Response.json({ jobId: "j1", revision: 3 }, { status: 202 });
      }
      if (path === "/api/env/e1/stacks/s1/deploys/j1") {
        return Response.json({
          id: "j1",
          stackId: "s1",
          mode: "deploy",
          state: job.state,
          createdAt: "2026-10-19T12:00:00Z",
          finishedAt: "",
          items: [
            {
              id: "check",
              label: "Check the stack",
              state: "done",
              code: "",
              current: 0,
              total: 0,
              message: "",
              result: {},
            },
          ],
        });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  window.localStorage.clear();
  document.title = "";
});

function renderAt(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

const posts = (suffix: string) =>
  sent.filter((s) => s.method === "POST" && s.path.endsWith(suffix));

describe("the review route", () => {
  it("shows the flow caption in place of the tabs, with the stack's header above the review", async () => {
    renderAt("/env/e1/stacks/s1/review");
    expect(await screen.findByRole("heading", { name: "Review web" })).toBeTruthy();
    const steps = screen.getByRole("navigation", { name: "Deploy steps" });
    expect(within(steps).getByText("Step 2 of 3: Review").getAttribute("aria-current")).toBe(
      "step",
    );
    expect(screen.queryByRole("tab", { name: "Overview" })).toBeNull();
    expect(screen.getByRole("heading", { name: "web", level: 2 })).toBeTruthy();
    // The header's Deploy… is for the pages that have tabs.
    expect(screen.queryByRole("link", { name: "Deploy…" })).toBeNull();
  });

  it("asks for an update review of the services the address names", async () => {
    renderAt("/env/e1/stacks/s1/review?mode=update&services=web,db");
    await screen.findByRole("heading", { name: "Review image updates for web" });
    await waitFor(() => expect(posts("/review")).toHaveLength(1));
    expect(posts("/review")[0]?.body).toEqual({ mode: "update", services: ["web", "db"] });
  });

  it("drops a mode it does not know and reviews the draft", async () => {
    renderAt("/env/e1/stacks/s1/review?mode=nonsense");
    await screen.findByRole("heading", { name: "Review web" });
    await waitFor(() => expect(posts("/review")).toHaveLength(1));
    expect(posts("/review")[0]?.body).toEqual({ mode: "deploy" });
  });

  it("is the no-permission page for a Viewer, who cannot deploy", async () => {
    role = "viewer";
    renderAt("/env/e1/stacks/s1/review");
    expect(await screen.findByText(/permission/i)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Review web" })).toBeNull();
    expect(posts("/review")).toHaveLength(0);
  });

  it("deploys what was reviewed and replaces itself with the console of the job", async () => {
    const router = renderAt("/env/e1/stacks/s1/review");
    await screen.findByRole("table", { name: "Summary of changes" });
    fireEvent.click(screen.getByRole("button", { name: "Deploy stack" }));
    expect(await screen.findByRole("heading", { name: "Deploying web" })).toBeTruthy();
    expect(router.state.location.pathname).toBe("/env/e1/stacks/s1/deploys/j1");
    expect(posts("/deploys")[0]?.body).toEqual({
      draftVersion: 7,
      pins: { s1_web: `nginx:1.27@${D1}` },
      mode: "deploy",
    });
    // The console's caption names step 3, and the review is not left behind in the history.
    const steps = screen.getByRole("navigation", { name: "Deploy steps" });
    expect(within(steps).getByText("Step 3 of 3: Deploy").getAttribute("aria-current")).toBe(
      "step",
    );
    expect(router.history.length).toBe(1);
  });
});

describe("the console route", () => {
  it("opens a job directly, as the page a link or the jobs list leads to", async () => {
    renderAt("/env/e1/stacks/s1/deploys/j1");
    expect(await screen.findByRole("heading", { name: "Deploying web" })).toBeTruthy();
    expect(screen.getByText("Check the stack")).toBeTruthy();
  });
});

describe("from the editor", () => {
  it("saves, then opens the review, without asking about the edits it just saved", async () => {
    const router = renderAt("/env/e1/stacks/s1/editor");
    await screen.findByRole("textbox", { name: "compose.yaml, YAML" });
    typeIntoEditor(`${COMPOSE}# more\n`);
    fireEvent.click(screen.getByRole("button", { name: "Review and deploy" }));
    expect(await screen.findByRole("heading", { name: "Review web" })).toBeTruthy();
    expect(router.state.location.pathname).toBe("/env/e1/stacks/s1/review");
    expect(sent.filter((s) => s.method === "PUT")).toHaveLength(1);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("opens at the line a link from the review names, and takes the line out of the address", async () => {
    const router = renderAt("/env/e1/stacks/s1/editor?file=compose.yaml&line=3");
    await screen.findByRole("textbox", { name: "compose.yaml, YAML" });
    // Line 3 starts after "services:\n  web:\n" (17 characters).
    await waitFor(() => expect(editorView().state.selection.main.head).toBe(17));
    await waitFor(() => expect(router.state.location.search).not.toHaveProperty("line"));
  });
});
