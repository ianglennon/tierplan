// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { ImageUsedBy, usedWhat } from "@/features/images/used-by";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Used by tab of an image: the columns, the links, a service with no tasks and the
// empty state (plan 05-15, RES-09, D-18).

const ENV = "e1";
const ID = `sha256:${"a".repeat(64)}`;

interface Item {
  kind: "container" | "service";
  id: string;
  name: string;
  nodeId: string;
  node: string;
  state: string;
  matchedBy: "image_id" | "tag" | "digest";
}

const web: Item = {
  kind: "container",
  id: "c1",
  name: "web",
  nodeId: "n1",
  node: "node-1",
  state: "running",
  matchedBy: "image_id",
};
const svc: Item = {
  kind: "service",
  id: "s1",
  name: "api",
  nodeId: "",
  node: "",
  state: "no_tasks",
  matchedBy: "digest",
};

let client: QueryClient;
let answer: () => Response;

beforeEach(() => {
  answer = () => Response.json({ items: [web, svc] });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = decodeURIComponent(new URL(req.url).pathname);
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (path === `/api/env/${ENV}/images/${ID}/used-by`) return answer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  client.clear();
});

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ImageUsedBy envId={ENV} imageId={ID} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("image Used by", () => {
  it("lists Name, Type, Node, State and Matched by for each container and service", async () => {
    mount();
    const table = await screen.findByRole("table", {
      name: "Containers and services that use the image",
    });
    const heads = within(table)
      .getAllByRole("columnheader")
      .map((h) => h.textContent);
    expect(heads).toEqual(["Name", "Type", "Node", "State", "Matched by"]);
    const rows = within(table).getAllByRole("row").slice(1) as HTMLElement[];
    expect(rows).toHaveLength(2);
    expect(within(rows[0] as HTMLElement).getByText("Container")).toBeTruthy();
    expect(within(rows[0] as HTMLElement).getByText("node-1")).toBeTruthy();
    expect(within(rows[0] as HTMLElement).getByText("running")).toBeTruthy();
    expect(within(rows[0] as HTMLElement).getByText("Image ID")).toBeTruthy();
    expect(within(rows[1] as HTMLElement).getByText("Service")).toBeTruthy();
    expect(within(rows[1] as HTMLElement).getByText("Digest")).toBeTruthy();
  });

  it("links a container to its page and leaves a service name as text", async () => {
    mount();
    const link = await screen.findByRole("link", { name: "Open container web" });
    expect(link.getAttribute("href")).toContain("/env/e1/containers/c1");
    expect(link.getAttribute("href")).toContain("node=n1");
    expect(screen.getByText("api").closest("a")).toBeNull();
  });

  it("says a service with no tasks is only a spec, and shows a dash for its missing node", async () => {
    mount();
    const row = (await screen.findByText("api")).closest("tr") as HTMLElement;
    expect(within(row).getByText("Service spec, no tasks")).toBeTruthy();
    expect(within(row).getByText("—")).toBeTruthy();
  });

  it("says Nothing uses this image. when nothing does", async () => {
    answer = () => Response.json({ items: [] });
    mount();
    expect(await screen.findByText("Nothing uses this image.")).toBeTruthy();
  });

  it("shows names as text, not markup", async () => {
    answer = () => Response.json({ items: [{ ...web, name: "<b>web</b>" }] });
    mount();
    expect(await screen.findByText("<b>web</b>")).toBeTruthy();
    expect(document.querySelector("b")).toBeNull();
  });

  it("offers a retry when the read fails", async () => {
    answer = () => Response.json({}, { status: 500 });
    mount();
    const retry = await screen.findByRole("button", { name: "Retry" });
    answer = () => Response.json({ items: [web] });
    fireEvent.click(retry);
    await waitFor(() => expect(screen.getByText("web")).toBeTruthy());
  });
});

describe("usedWhat", () => {
  const t = (key: string, options?: Record<string, unknown>) =>
    `${key}|${JSON.stringify(options ?? {})}`;
  it("names containers alone off a swarm and both on one", () => {
    expect(usedWhat(t, 2, 1, false)).toBe('images.used.containers|{"count":2}');
    expect(usedWhat(t, 2, 1, true)).toContain("images.used.both");
  });
});
