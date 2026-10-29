// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRouterState } from "@tanstack/react-router";
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { StackGit } from "@/api/stack-git";
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
import { SourceCard } from "@/features/stack-git/source-card";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Source card of the Git tab (plan 09-29, GIT-02, GIT-04, D-11, D-12): the source as it is applied, and an in-card
// edit that ends in Review. Nothing is applied by the card.

const STACK = "s1";
const DRAFT = `/api/env/${ENV}/stacks/${STACK}/draft`;
const SOURCE = `/api/env/${ENV}/stacks/${STACK}/git/source`;

function makeGit(over: Partial<StackGit> = {}): StackGit {
  return {
    repoId: "r1",
    repoName: "homelab",
    repoHost: "git.example.com",
    repoPath: "homelab",
    refKind: "branch",
    ref: "main",
    folder: "stacks/web",
    composeFiles: ["compose.yaml"],
    deployedSha: "",
    headSha: "",
    behind: 0,
    behindCapped: false,
    status: "unknown",
    checking: false,
    autoDeploy: false,
    autoDeployPaused: false,
    pollEnabled: true,
    pollMinutes: 5,
    ...over,
  };
}

let world: World;
let client: ReturnType<typeof newClient>;
let git: StackGit;
// What the draft route answers (a number is a status), and what the source route answers.
let draftVersion: number;
let draftFails: boolean;
let sourceAnswer: () => Response;
const sources = () => world.sent.filter((s) => s.path === SOURCE);
const draftReads = () => world.sent.filter((s) => s.path === DRAFT);

beforeEach(() => {
  world = makeWorld();
  world.repos = [repoOf("r1", "homelab"), repoOf("r2", "shared")];
  world.refs = () => Response.json(refsOf(["main", "dev"], ["v1"]));
  world.tree = (q) => {
    const path = q.get("path") ?? "";
    if (path === "") return Response.json(treeOf("", [dir("stacks"), file("README.md")]));
    if (path === "stacks") return Response.json(treeOf(path, [dir("web", true)]));
    return Response.json(treeOf(path, [file("compose.yaml"), file("override.yaml")]));
  };
  git = makeGit();
  draftVersion = 4;
  draftFails = false;
  sourceAnswer = () => Response.json({ draftVersion: 5, commit: "c".repeat(40), ref: "dev" });
  client = newClient();
  stubFetch(world);
  // The routes of the stack, on top of the ones the From git page reads.
  const base = globalThis.fetch;
  vi.stubGlobal("fetch", async (req: Request) => {
    const path = new URL(req.url).pathname;
    if (path === DRAFT && req.method === "GET") {
      world.sent.push({ method: "GET", path, query: new URL(req.url).searchParams });
      return draftFails
        ? Response.json({}, { status: 500 })
        : Response.json({ version: draftVersion });
    }
    if (path === SOURCE && req.method === "POST") {
      world.sent.push({
        method: "POST",
        path,
        query: new URL(req.url).searchParams,
        body: await req.clone().json(),
      });
      return sourceAnswer();
    }
    return base(req);
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function Where() {
  const path = useRouterState({ select: (s) => s.location.pathname });
  return <span data-testid="where">{path}</span>;
}

function mount() {
  renderUi(
    <TestRouter path={`/env/${ENV}/stacks/${STACK}/git`}>
      <SourceCard envId={ENV} stackId={STACK} stackName="web" git={git} />
      <Where />
    </TestRouter>,
    client,
  );
}

const change = () => screen.findByRole("button", { name: "Change source…" });
const reviewButton = () => screen.getByRole("button", { name: "Review change" });

// edit opens the card in edit mode and waits for the pickers, the compose files and the draft to be there.
async function edit() {
  fireEvent.click(await change());
  await screen.findByRole("list", { name: "Compose files, in merge order" });
  await waitFor(() => expect(draftReads()).toHaveLength(1));
  await waitFor(() => expect(reviewButton().getAttribute("aria-disabled")).toBeNull());
}

async function chooseBranch(name: string) {
  fireEvent.click(await screen.findByRole("combobox", { name: "Branch" }));
  const list = await screen.findByRole("listbox", { name: "Branches" });
  fireEvent.click(within(list).getByRole("option", { name: new RegExp(name) }));
}

describe("SourceCard", () => {
  it("lists the repository, ref, folder and compose files, and offers Change source…", async () => {
    git = makeGit({ composeFiles: ["compose.yaml", "override.yaml"], refKind: "tag", ref: "v1" });
    mount();
    const card = await screen.findByRole("region", { name: "Source" });
    expect(
      within(card).getByText("Where web comes from. Changing it goes through review."),
    ).toBeTruthy();
    const rows = within(card);
    expect(rows.getByText("Repository").nextElementSibling?.textContent).toBe("homelab");
    expect(rows.getByText("Ref").nextElementSibling?.textContent).toBe("v1 tag");
    expect(rows.getByText("Folder").nextElementSibling?.textContent).toBe("stacks/web");
    const files = rows.getByText("Compose files").nextElementSibling as HTMLElement;
    expect(
      within(files)
        .getAllByRole("listitem")
        .map((li) => li.textContent),
    ).toEqual(["compose.yaml", "override.yaml"]);
    expect(await change()).toBeTruthy();
    // Nothing is read or sent until the person asks to change it.
    expect(draftReads()).toHaveLength(0);
    expect(sources()).toHaveLength(0);
  });

  it("names the repository root for a stack in the root folder", async () => {
    git = makeGit({ folder: "" });
    mount();
    const card = await screen.findByRole("region", { name: "Source" });
    expect(within(card).getByText("Repository root")).toBeTruthy();
  });

  it("is off for a Viewer, with the role it takes", async () => {
    world.me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    const button = await change();
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(button);
    expect(screen.queryByRole("combobox", { name: "Branch" })).toBeNull();
  });

  it("is off for an Operator too: choosing the repository takes an Admin of the environment", async () => {
    world.me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    const button = await change();
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(button);
    expect(screen.queryByRole("combobox", { name: "Branch" })).toBeNull();
  });

  it("starts from what the stack follows and saves the new ref as the draft, then opens the Review", async () => {
    mount();
    await edit();
    // The pickers hold the stack's own source.
    expect(screen.getByRole("combobox", { name: "Branch" }).textContent).toContain("main");
    const list = screen.getByRole("list", { name: "Compose files, in merge order" });
    expect(within(list).getByText("compose.yaml")).toBeTruthy();

    await chooseBranch("dev");
    fireEvent.click(reviewButton());
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe(`/env/${ENV}/stacks/${STACK}/review`),
    );
    expect(sources()).toHaveLength(1);
    expect(sources()[0]?.body).toEqual({
      draftVersion: 4,
      repoId: "r1",
      refKind: "branch",
      ref: "dev",
      folder: "stacks/web",
      composeFiles: ["compose.yaml"],
    });
  });

  it("saves the compose files in the order chosen", async () => {
    git = makeGit({ composeFiles: ["compose.yaml", "override.yaml"] });
    mount();
    await edit();
    fireEvent.click(screen.getByRole("button", { name: /Move override.yaml up/ }));
    fireEvent.click(reviewButton());
    await waitFor(() => expect(sources()).toHaveLength(1));
    expect(sources()[0]?.body).toMatchObject({ composeFiles: ["override.yaml", "compose.yaml"] });
  });

  it("keeps Review change off, with the reason, while a choice is missing", async () => {
    mount();
    await edit();
    // Another kind clears the ref, the folder and the files.
    fireEvent.click(screen.getByRole("radio", { name: "Tag" }));
    await waitFor(() => expect(reviewButton().getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(reviewButton());
    expect(sources()).toHaveLength(0);
    expect(screen.getByTestId("where").textContent).toBe(`/env/${ENV}/stacks/${STACK}/git`);
  });

  it("goes back to the rows on Cancel and sends nothing", async () => {
    mount();
    await edit();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(await change()).toBeTruthy();
    expect(screen.queryByRole("combobox", { name: "Branch" })).toBeNull();
    expect(sources()).toHaveLength(0);
  });

  it("says who changed the draft while the person was editing, and Reload goes back to the rows", async () => {
    sourceAnswer = () =>
      Response.json(
        {
          type: "draft_changed",
          title: "Conflict",
          status: 409,
          detail: "d",
          errors: [{ location: "savedBy", message: "bob" }],
        },
        { status: 409 },
      );
    mount();
    await edit();
    await chooseBranch("dev");
    fireEvent.click(reviewButton());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "bob changed the source of web while you were editing. Reload to see it.",
    );
    expect(screen.getByTestId("where").textContent).toBe(`/env/${ENV}/stacks/${STACK}/git`);

    fireEvent.click(within(alert).getByRole("button", { name: "Reload" }));
    expect(await change()).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("words a git failure and stays in the editor", async () => {
    sourceAnswer = () =>
      Response.json({ type: "ref_missing", title: "x", status: 422, detail: "d" }, { status: 422 });
    mount();
    await edit();
    await chooseBranch("dev");
    fireEvent.click(reviewButton());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "Couldn't change the source. That branch or tag isn't in homelab any more.",
    );
    expect(screen.getByRole("combobox", { name: "Branch" })).toBeTruthy();
  });

  it("says a repository that went away is not available", async () => {
    sourceAnswer = () =>
      Response.json(
        { type: "repository_not_found", title: "x", status: 404, detail: "d" },
        { status: 404 },
      );
    mount();
    await edit();
    await chooseBranch("dev");
    fireEvent.click(reviewButton());
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Couldn't change the source. That repository isn't available on this environment.",
    );
  });

  it("keeps Review change off and says so when the draft can't be read", async () => {
    draftFails = true;
    mount();
    fireEvent.click(await change());
    await screen.findByText("Couldn't read the draft. Check the connection and try again.");
    expect(reviewButton().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(reviewButton());
    expect(sources()).toHaveLength(0);
  });
});
