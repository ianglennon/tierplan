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
import { makeMe } from "@/test-me";

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

// useThisFolder presses Use this folder once the folder has been read: it is off until then.
async function useThisFolder() {
  const button = await screen.findByRole("button", { name: "Use this folder" });
  await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(false));
  fireEvent.click(button);
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
    await useThisFolder();
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

  it("clears the folder and files when the repository changes, and says it would", async () => {
    world.repos = [repoOf("r1", "homelab"), repoOf("r2", "tools")];
    mount();
    await chooseRepository("homelab");
    await useThisFolder();
    expect(await screen.findByText("Stack folder: / (repository root)")).toBeTruthy();
    expect(
      screen.getByText("Changing the repository clears the folder and files you chose."),
    ).toBeTruthy();

    await chooseRepository("tools");
    await waitFor(() => expect(screen.queryByText(/Stack folder:/)).toBeNull());
    expect(screen.getByText("Choose a ref first.")).toBeTruthy();
  });

  it("clears the folder and files when the kind of ref changes", async () => {
    mount();
    await chooseRepository("homelab");
    await useThisFolder();
    expect(await screen.findByText("Stack folder: / (repository root)")).toBeTruthy();
    fireEvent.click(screen.getByRole("radio", { name: "Tag" }));
    await waitFor(() => expect(screen.queryByText(/Stack folder:/)).toBeNull());
    expect(screen.getByRole("combobox", { name: "Tag" }).textContent).toContain("Choose a tag");
  });

  it("turns Create stack off again when the last compose file is removed", async () => {
    mount();
    await chooseRepository("homelab");
    await waitFor(() =>
      expect(screen.getByRole("combobox", { name: "Branch" }).textContent).toContain("main"),
    );
    fireEvent.click(await screen.findByRole("button", { name: "stacks" }));
    fireEvent.click(await screen.findByRole("button", { name: /web/ }));
    await useThisFolder();
    await screen.findByText("Picked compose.yaml. Change it if the stack uses another.");
    expect(
      screen.getByRole("button", { name: "Create stack" }).getAttribute("aria-disabled"),
    ).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Remove compose.yaml" }));
    expect(screen.getByRole("button", { name: "Create stack" }).getAttribute("aria-disabled")).toBe(
      "true",
    );
  });

  it("says the repositories could not be loaded, and asks again on Retry loading repositories", async () => {
    let calls = 0;
    world.reposAnswer = () => {
      calls += 1;
      return calls === 1
        ? Response.json({ type: "x", title: "x", status: 500 }, { status: 500 })
        : Response.json({ items: [repoOf("r1", "homelab")] });
    };
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Couldn't load the repositories.");
    fireEvent.click(within(alert).getByRole("button", { name: "Retry loading repositories" }));
    expect(await screen.findByRole("combobox", { name: "Repository" })).toBeTruthy();
  });

  it("offers Add a repository… to an Env Admin and opens the repository sheet", async () => {
    mount();
    const select = await screen.findByRole("combobox", { name: "Repository" });
    fireEvent.keyDown(select, { key: "Enter" });
    const add = (await screen.findAllByRole("option")).find(
      (o) => o.textContent === "Add a repository…",
    );
    fireEvent.click(add as HTMLElement);
    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(await screen.findByText("Add a repository")).toBeTruthy();
  });

  it("does not offer Add a repository… to someone who may not add one, and says none is available", async () => {
    world.repos = [];
    world.me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    expect(
      await screen.findByText(
        "No repository is available on Prod. An administrator can add one under Git.",
      ),
    ).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Repository" }), { key: "Enter" });
    expect(screen.queryByRole("option", { name: "Add a repository…" })).toBeNull();
  });

  it("lists each repository with its address and where it applies", async () => {
    world.repos = [
      repoOf("r1", "homelab"),
      repoOf("r2", "tools", { scope: { kind: "environment", envId: ENV, envName: "Prod" } }),
    ];
    mount();
    const select = await screen.findByRole("combobox", { name: "Repository" });
    fireEvent.keyDown(select, { key: "Enter" });
    const options = await screen.findAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual([
      "homelabInstancehttps://git.example.com/homelab.git",
      "toolsProdhttps://git.example.com/tools.git",
      "Add a repository…",
    ]);
  });
});
