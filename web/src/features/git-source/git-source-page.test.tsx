// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

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

let world: World;
let client: ReturnType<typeof newClient>;

beforeEach(() => {
  world = makeWorld();
  world.repos = [repoOf("r1", "homelab")];
  world.refs = () => Response.json(refsOf(["main", "dev"], ["v1"]));
  world.tree = (q) => {
    const path = q.get("path") ?? "";
    if (path === "") return Response.json(treeOf("", [dir("stacks"), file("README.md")]));
    if (path === "stacks") return Response.json(treeOf(path, [dir("web", true)]));
    return Response.json(treeOf(path, [file("compose.yaml"), file(".env")]));
  };
  client = newClient();
  stubFetch(world);
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(name = "web") {
  const root = createRootRoute({
    component: () => (
      <>
        <GitSourcePage envId={ENV} name={name} mode="create" />
        <Toaster />
      </>
    ),
  });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: "/" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks/$stackId" }),
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

const posts = () => world.sent.filter((s) => s.method === "POST");

describe("GitSourcePage", () => {
  it("creates a stack from a repository, a branch, a folder and its compose file", async () => {
    const router = mount();
    await chooseRepository("homelab");
    // The default branch is chosen for the person.
    await waitFor(() =>
      expect(screen.getByRole("combobox", { name: "Branch" }).textContent).toContain("main"),
    );
    fireEvent.click(await screen.findByRole("button", { name: "stacks" }));
    fireEvent.click(await screen.findByRole("button", { name: /web/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Use this folder" }));
    // The one conventional file is picked for the person.
    expect(
      await screen.findByText("Picked compose.yaml. Change it if the stack uses another."),
    ).toBeTruthy();
    const list = screen.getByRole("list", { name: "Compose files, in merge order" });
    expect(within(list).getByText("compose.yaml")).toBeTruthy();
    expect(posts()).toEqual([]);

    fireEvent.click(screen.getByRole("button", { name: "Create stack" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/e1/stacks/stack-9"));
    expect(posts()).toHaveLength(1);
    expect(posts()[0]?.body).toEqual({
      name: "web",
      repoId: "r1",
      refKind: "branch",
      ref: "main",
      folder: "stacks/web",
      composeFiles: ["compose.yaml"],
    });
    expect(await screen.findByText("Created stack web. Nothing is deployed yet.")).toBeTruthy();
  });

  it("keeps Create stack off, with the reason, until a repository, a ref, a folder and a file are chosen", async () => {
    mount();
    const create = await screen.findByRole("button", { name: "Create stack" });
    expect(create.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(create);
    expect(
      await screen.findAllByText("Choose a repository, a ref, a folder and a compose file first."),
    ).not.toHaveLength(0);
    expect(posts()).toEqual([]);
  });

  it("names the stack and the environment", async () => {
    mount("shop");
    expect(await screen.findByRole("heading", { name: "Create shop from git" })).toBeTruthy();
    await waitFor(() => expect(document.body.textContent).toContain("On Prod (Swarm)"));
  });
});
