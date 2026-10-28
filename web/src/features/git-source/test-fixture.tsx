// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactNode } from "react";
import { vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { GitRefs, GitRepo, GitTree, GitTreeEntry } from "@/api/git";
import { TooltipProvider } from "@/components/ui/tooltip";
import "@/i18n";
import { makeMe } from "@/test-me";

// What the stubbed server saw: the method, the path, the query and, for a POST, the body.
export interface Sent {
  method: string;
  path: string;
  query: URLSearchParams;
  body?: Record<string, unknown>;
}

export interface World {
  sent: Sent[];
  repos: GitRepo[];
  // When set, the answer to the list of repositories, in place of repos.
  reposAnswer?: () => Response;
  refs: () => Response | Promise<Response>;
  // The answer to a read of one folder; the query holds refKind, ref and path.
  tree: (query: URLSearchParams) => Response | Promise<Response>;
  create: () => Response | Promise<Response>;
  me: ReturnType<typeof makeMe>;
  environments: { id: string; name: string; envType: string }[];
}

export const ENV = "e1";
export const SHA = "0123456789abcdef0123456789abcdef01234567";
export const COMMITTED_AT = "2026-10-20T10:00:00Z";

export const dir = (name: string, hasCompose = false): GitTreeEntry => ({
  name,
  kind: "dir",
  hasCompose,
});
export const file = (name: string): GitTreeEntry => ({ name, kind: "file", hasCompose: false });
export const link = (name: string): GitTreeEntry => ({ name, kind: "symlink", hasCompose: false });
export const submodule = (name: string): GitTreeEntry => ({
  name,
  kind: "submodule",
  hasCompose: false,
});

export function treeOf(
  path: string,
  entries: GitTreeEntry[],
  extra: Partial<GitTree> = {},
): GitTree {
  return {
    capped: false,
    commit: SHA,
    committedAt: COMMITTED_AT,
    entries,
    path,
    total: entries.length,
    ...extra,
  };
}

export function repoOf(id: string, name: string, extra: Partial<GitRepo> = {}): GitRepo {
  return {
    caPem: "",
    createdAt: "2026-10-01T10:00:00Z",
    credential: null,
    hasCa: false,
    host: "git.example.com",
    hostKey: null,
    id,
    lastTest: null,
    name,
    readOnly: false,
    scope: { kind: "instance", envId: null, envName: null },
    updatedAt: "2026-10-01T10:00:00Z",
    url: `https://git.example.com/${name}.git`,
    usedBy: 0,
    ...extra,
  };
}

export function refsOf(branches: string[], tags: string[] = [], defaultBranch = "main"): GitRefs {
  return {
    branches: branches.map((name) => ({ name, commit: SHA })),
    tags: tags.map((name) => ({ name, commit: SHA })),
    defaultBranch,
  };
}

export const problem = (status: number, type: string, host = "git.example.com") =>
  Response.json(
    { type, title: "x", status, detail: "d", errors: [{ location: "host", message: host }] },
    { status },
  );

export function makeWorld(): World {
  return {
    sent: [],
    repos: [],
    refs: () => Response.json(refsOf(["main"])),
    tree: (q) => Response.json(treeOf(q.get("path") ?? "", [])),
    create: () => Response.json({ stackId: "stack-9" }, { status: 201 }),
    me: makeMe({ envs: { [ENV]: "admin" } }),
    environments: [{ id: ENV, name: "Prod", envType: "swarm" }],
  };
}

// stubFetch answers the API the From git page reads and writes from the world, which a test
// changes before or during the run.
export function stubFetch(world: World) {
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const entry: Sent = {
        method: req.method,
        path: url.pathname,
        query: url.searchParams,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      };
      world.sent.push(entry);
      if (entry.path === "/api/me") return Response.json(world.me);
      if (entry.path === "/api/environments") return Response.json({ items: world.environments });
      if (entry.path === `/api/env/${ENV}/git/repos` && req.method === "GET") {
        return world.reposAnswer ? world.reposAnswer() : Response.json({ items: world.repos });
      }
      if (/^\/api\/env\/[^/]+\/git\/repos\/[^/]+\/refs$/.test(entry.path)) return world.refs();
      if (/^\/api\/env\/[^/]+\/git\/repos\/[^/]+\/tree$/.test(entry.path)) {
        return world.tree(url.searchParams);
      }
      if (entry.path === `/api/env/${ENV}/git-stacks` && req.method === "POST") {
        return world.create();
      }
      return Response.json({}, { status: 404 });
    }),
  );
}

export const treeReads = (world: World) =>
  world.sent.filter((s) => s.path.endsWith("/tree")).map((s) => s.query.get("path") ?? "");

export function newClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

// renderUi mounts a piece of the page with what it reads from: queries and tooltips.
export function renderUi(ui: ReactNode, client: QueryClient) {
  return render(
    <QueryClientProvider client={client}>
      <TooltipProvider>{ui}</TooltipProvider>
    </QueryClientProvider>,
  );
}
