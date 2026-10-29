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
  checkOf,
  dir,
  ENV,
  file,
  makeWorld,
  newClient,
  refsOf,
  renderUi,
  repoOf,
  SHA,
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

// pressUseThisFolder presses Use this folder once the folder has been read: it is off until then.
async function pressUseThisFolder() {
  const button = await screen.findByRole("button", { name: "Use this folder" });
  await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(false));
  fireEvent.click(button);
}

// The creates the server saw; a check is a POST too, and saves nothing.
const posts = () =>
  world.sent.filter((s) => s.method === "POST" && !s.path.endsWith("/git-stacks/check"));
const checks = () =>
  world.sent.filter((s) => s.method === "POST" && s.path.endsWith("/git-stacks/check"));

// chooseWebFolder picks the repository, goes to stacks/web and uses it.
async function chooseWebFolder() {
  await chooseRepository("homelab");
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: "Branch" }).textContent).toContain("main"),
  );
  fireEvent.click(await screen.findByRole("button", { name: "stacks" }));
  fireEvent.click(await screen.findByRole("button", { name: /web/ }));
  await pressUseThisFolder();
  await screen.findByText("Picked compose.yaml. Change it if the stack uses another.");
}

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
    await pressUseThisFolder();
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

  it("checks the chosen files against the commit the ref resolved to and shows what they deploy, without saving", async () => {
    world.check = () =>
      Response.json(
        checkOf({
          services: ["web", "db"],
          problems: [
            {
              severity: "error",
              code: "git_submodule",
              message: "vendor is a submodule. Bayplan doesn't support submodules.",
              file: "compose.yaml",
              line: 4,
              col: 1,
              endLine: 4,
              endCol: 2,
            },
          ],
        }),
      );
    mount();
    await chooseWebFolder();
    expect(await screen.findByText("Found 2 services:")).toBeTruthy();
    expect(screen.getByText("compose.yaml:4")).toBeTruthy();
    expect(checks()).toHaveLength(1);
    expect(checks()[0]?.body).toEqual({
      name: "web",
      repoId: "r1",
      refKind: "branch",
      ref: "main",
      folder: "stacks/web",
      composeFiles: ["compose.yaml"],
      commit: SHA,
    });
    expect(posts()).toEqual([]);
    // Problems do not stop a draft from being created.
    expect(
      screen.getByRole("button", { name: "Create stack" }).getAttribute("aria-disabled"),
    ).toBeNull();
  });

  it("keeps Create stack on while the check runs and when it fails, and checks again on Retry", async () => {
    let calls = 0;
    world.check = () => {
      calls += 1;
      return calls === 1
        ? Response.json({ type: "ref_missing", title: "x", status: 422 }, { status: 422 })
        : Response.json(checkOf({ services: ["web"] }));
    };
    mount();
    await chooseWebFolder();
    const alert = await screen.findByText(/Couldn't check the files:/);
    expect(alert.textContent).toContain("That branch or tag isn't in homelab any more.");
    expect(
      screen.getByRole("button", { name: "Create stack" }).getAttribute("aria-disabled"),
    ).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Found 1 service:")).toBeTruthy();
  });

  describe("variables", () => {
    const withVars = () => {
      world.check = () =>
        Response.json(
          checkOf({
            services: ["web"],
            vars: [
              {
                name: "DB_PASSWORD",
                value: "<hidden>",
                default: "",
                hasDefault: false,
                places: [{ file: "compose.yaml", line: 5 }],
                required: false,
                secret: false,
                secretLike: true,
                source: "repo-env",
                unused: false,
              },
              {
                name: "WEB_PORT",
                value: "",
                default: "",
                hasDefault: false,
                places: [{ file: "compose.yaml", line: 8 }],
                required: true,
                secret: false,
                secretLike: false,
                source: "unset",
                unused: false,
              },
            ],
          }),
        );
    };

    it("waits for a compose file, then lists the variables the check found", async () => {
      withVars();
      mount();
      expect(await screen.findByText("Choose a compose file first.")).toBeTruthy();
      await chooseWebFolder();
      expect(await screen.findByTestId("git-variable-row-WEB_PORT")).toBeTruthy();
      expect(screen.getByTestId("git-variable-row-DB_PASSWORD")).toBeTruthy();
    });

    it("says how many required variables have no value, and still lets the stack be created", async () => {
      withVars();
      mount();
      await chooseWebFolder();
      expect(
        await screen.findByText(
          "1 required variable has no value. Review won't deploy until you set it.",
        ),
      ).toBeTruthy();
      expect(
        screen.getByRole("button", { name: "Create stack" }).getAttribute("aria-disabled"),
      ).toBeNull();
    });

    it("sends the typed values with the next check and with the create, and a secret one as a secret", async () => {
      withVars();
      const router = mount();
      await chooseWebFolder();
      fireEvent.change(await screen.findByLabelText("Value of WEB_PORT"), {
        target: { value: "8080" },
      });
      fireEvent.click(screen.getByRole("button", { name: "Set secret value" }));
      const secret = screen.getByLabelText("Value of DB_PASSWORD") as HTMLInputElement;
      expect(document.activeElement).toBe(secret);
      fireEvent.change(secret, { target: { value: "hunter2" } });

      await waitFor(() => {
        const last = checks().at(-1)?.body;
        expect(last?.vars).toEqual([
          { name: "WEB_PORT", value: "8080", secret: false },
          { name: "DB_PASSWORD", value: "hunter2", secret: true },
        ]);
      });
      fireEvent.click(screen.getByRole("button", { name: "Create stack" }));
      await waitFor(() => expect(router.state.location.pathname).toBe("/env/e1/stacks/stack-9"));
      expect(posts()[0]?.body?.vars).toEqual([
        { name: "WEB_PORT", value: "8080", secret: false },
        { name: "DB_PASSWORD", value: "hunter2", secret: true },
      ]);
    });

    it("keeps typed values out of storage and the address", async () => {
      withVars();
      mount();
      await chooseWebFolder();
      fireEvent.change(await screen.findByLabelText("Value of WEB_PORT"), {
        target: { value: "8080" },
      });
      fireEvent.click(screen.getByRole("button", { name: "Set secret value" }));
      fireEvent.change(screen.getByLabelText("Value of DB_PASSWORD"), {
        target: { value: "hunter2" },
      });
      expect(localStorage.length).toBe(0);
      expect(sessionStorage.length).toBe(0);
      expect(window.location.href).not.toContain("hunter2");
      expect(document.body.textContent).not.toContain("hunter2");
    });
  });

  describe("overlay files", () => {
    const PATH = "secrets/db_pw.txt";
    // The check lists the file as missing until an overlay file is named for it.
    const withMissingFile = () => {
      world.check = (body) => {
        const named = ((body.overlayPaths as string[] | undefined) ?? []).includes(PATH);
        return Response.json(
          checkOf({
            services: ["web"],
            missing: named ? [] : [PATH],
            problems: named
              ? []
              : [
                  {
                    severity: "error",
                    code: "repo_file_missing",
                    message: `${PATH} isn't in the repository.`,
                    file: "compose.yaml",
                    line: 7,
                    col: 1,
                    endLine: 7,
                    endCol: 2,
                  },
                ],
          }),
        );
      };
    };

    it("has no Overlay files card when nothing refers to a missing path", async () => {
      mount();
      await chooseWebFolder();
      await screen.findByText("Found 1 service:");
      expect(screen.queryByRole("heading", { name: "Overlay files" })).toBeNull();
    });

    it("asks for the contents of a missing file, checks again with the path only, and creates with the contents", async () => {
      withMissingFile();
      const router = mount();
      await chooseWebFolder();
      const card = (await screen.findByRole("heading", { name: "Overlay files" })).closest(
        "section",
      ) as HTMLElement;
      expect(within(card).getByText("Missing")).toBeTruthy();

      fireEvent.click(within(card).getByRole("button", { name: /Add contents/ }));
      const dialog = await screen.findByRole("dialog", { name: "Add an overlay file" });
      expect((within(dialog).getByLabelText("Path") as HTMLInputElement).value).toBe(PATH);
      fireEvent.change(within(dialog).getByLabelText("Contents"), {
        target: { value: "hunter2" },
      });
      fireEvent.click(within(dialog).getByRole("button", { name: "Add overlay file" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

      // The next check names the path and nothing of the contents.
      await waitFor(() => expect(checks().at(-1)?.body?.overlayPaths).toEqual([PATH]));
      expect(JSON.stringify(checks().map((c) => c.body))).not.toContain("hunter2");
      await waitFor(() => expect(within(card).queryByText("Missing")).toBeNull());
      expect(within(card).getByText("Added")).toBeTruthy();
      expect(document.body.textContent).not.toContain("hunter2");

      fireEvent.click(screen.getByRole("button", { name: "Create stack" }));
      await waitFor(() => expect(router.state.location.pathname).toBe("/env/e1/stacks/stack-9"));
      expect(posts()[0]?.body?.overlays).toEqual([{ path: PATH, content: "hunter2" }]);
      expect(localStorage.length).toBe(0);
      expect(sessionStorage.length).toBe(0);
    });

    it("offers Add overlay file on the missing-file problem of the summary", async () => {
      withMissingFile();
      mount();
      await chooseWebFolder();
      const groups = await screen.findByRole("group", { name: /Fix these in the repository/ });
      fireEvent.click(within(groups).getByRole("button", { name: "Add overlay file" }));
      const dialog = await screen.findByRole("dialog", { name: "Add an overlay file" });
      expect((within(dialog).getByLabelText("Path") as HTMLInputElement).value).toBe(PATH);
    });

    it("refuses an overlay file for a path the stack folder already holds", async () => {
      withMissingFile();
      mount();
      await chooseWebFolder();
      fireEvent.click(await screen.findByRole("button", { name: /Add contents/ }));
      const dialog = await screen.findByRole("dialog", { name: "Add an overlay file" });
      fireEvent.change(within(dialog).getByLabelText("Path"), { target: { value: ".env" } });
      fireEvent.click(within(dialog).getByRole("button", { name: "Add overlay file" }));
      expect(
        within(dialog).getByText(
          ".env exists in the repository. Overlay files can only fill paths it doesn't have.",
        ),
      ).toBeTruthy();
      expect(screen.getByRole("dialog")).toBeTruthy();
    });

    it("lets go of the contents when the dialog is cancelled", async () => {
      withMissingFile();
      mount();
      await chooseWebFolder();
      fireEvent.click(await screen.findByRole("button", { name: /Add contents/ }));
      let dialog = await screen.findByRole("dialog", { name: "Add an overlay file" });
      fireEvent.change(within(dialog).getByLabelText("Contents"), {
        target: { value: "hunter2" },
      });
      fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(document.body.textContent).not.toContain("hunter2");
      fireEvent.click(screen.getByRole("button", { name: /Add contents/ }));
      dialog = await screen.findByRole("dialog", { name: "Add an overlay file" });
      expect((within(dialog).getByLabelText("Contents") as HTMLTextAreaElement).value).toBe("");
      expect(checks().every((c) => c.body?.overlayPaths === undefined)).toBe(true);
    });
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
    await pressUseThisFolder();
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
    await pressUseThisFolder();
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
    await pressUseThisFolder();
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
