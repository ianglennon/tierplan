// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { GitRefKind } from "@/api/git";
import { RefPicker } from "@/features/git-source/ref-picker";
import {
  ENV,
  makeWorld,
  newClient,
  problem,
  refsOf,
  renderUi,
  stubFetch,
  treeOf,
  type World,
} from "@/features/git-source/test-fixture";

let world: World;
let client: ReturnType<typeof newClient>;
let changes: string[];
let kinds: GitRefKind[];

beforeEach(() => {
  world = makeWorld();
  world.refs = () => Response.json(refsOf(["main", "dev", "release/1.0"], ["v1", "v2"]));
  client = newClient();
  changes = [];
  kinds = [];
  stubFetch(world);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

// Harness holds the choice the page would hold.
function Harness({ kind = "branch", value = "" }: { kind?: GitRefKind; value?: string }) {
  const [k, setK] = useState<GitRefKind>(kind);
  const [v, setV] = useState(value);
  return (
    <RefPicker
      envId={ENV}
      repoId="r1"
      repoName="homelab"
      kind={k}
      value={v}
      onKindChange={(next) => {
        kinds.push(next);
        setK(next);
        setV("");
      }}
      onChange={(next) => {
        changes.push(next);
        setV(next);
      }}
    />
  );
}

function mount(props: { kind?: GitRefKind; value?: string } = {}) {
  renderUi(<Harness {...props} />, client);
}

const trigger = (name: string) => screen.findByRole("combobox", { name });

describe("RefPicker", () => {
  it("preselects the default branch and marks it Default in the list", async () => {
    mount();
    await waitFor(() => expect(changes).toEqual(["main"]));
    const box = await trigger("Branch");
    expect(box.textContent).toContain("main");
    fireEvent.click(box);
    const list = await screen.findByRole("listbox", { name: "Branches" });
    const main = within(list).getByRole("option", { name: /main/ });
    expect(main.textContent).toContain("Default");
    expect(within(list).getByRole("option", { name: /dev/ }).textContent).not.toContain("Default");
  });

  it("is a combobox with aria-haspopup and aria-expanded", async () => {
    mount({ value: "main" });
    const box = await trigger("Branch");
    expect(box.getAttribute("aria-haspopup")).toBe("listbox");
    expect(box.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(box);
    await screen.findByRole("listbox", { name: "Branches" });
    expect(box.getAttribute("aria-expanded")).toBe("true");
  });

  it("moves with the arrow keys, picks with Enter, and closes with Escape back to the trigger", async () => {
    mount({ value: "main" });
    const box = await trigger("Branch");
    fireEvent.click(box);
    const search = await screen.findByPlaceholderText("Search branches");
    fireEvent.keyDown(search, { key: "ArrowDown" });
    fireEvent.keyDown(search, { key: "Enter" });
    await waitFor(() => expect(changes.at(-1)).toBe("dev"));
    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());

    fireEvent.click(box);
    const again = await screen.findByPlaceholderText("Search branches");
    fireEvent.keyDown(again, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
    expect(document.activeElement).toBe(box);
  });

  it("lists tags under the name Tags, with a tag search field", async () => {
    mount({ kind: "tag" });
    const box = await trigger("Tag");
    expect(box.textContent).toContain("Choose a tag");
    fireEvent.click(box);
    const list = await screen.findByRole("listbox", { name: "Tags" });
    expect(
      within(list)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["v1", "v2"]);
    expect(screen.getByPlaceholderText("Search tags")).toBeTruthy();
  });

  it("says a branch is followed", async () => {
    mount({ value: "main" });
    expect(await screen.findByText("TierPlan follows this branch.")).toBeTruthy();
  });

  it("says a moved tag counts as a change", async () => {
    mount({ kind: "tag" });
    expect(
      await screen.findByText(
        "TierPlan follows this tag. If the tag is moved to another commit, that counts as a change.",
      ),
    ).toBeTruthy();
  });

  it("says a fixed commit is never behind", async () => {
    mount({ kind: "commit" });
    expect(
      await screen.findByText("A fixed commit is never behind. Change the ref to move it."),
    ).toBeTruthy();
  });

  it("renders 200 rows of 250, says so, and searches the whole list", async () => {
    const many = Array.from({ length: 250 }, (_, i) => `branch-${String(i).padStart(3, "0")}`);
    world.refs = () => Response.json(refsOf(many, [], ""));
    mount({ value: "branch-000" });
    fireEvent.click(await trigger("Branch"));
    const list = await screen.findByRole("listbox", { name: "Branches" });
    expect(within(list).getAllByRole("option")).toHaveLength(200);
    expect(screen.getByText("Showing the first 200 of 250. Type to narrow.")).toBeTruthy();
    fireEvent.change(screen.getByPlaceholderText("Search branches"), {
      target: { value: "branch-249" },
    });
    await waitFor(() => expect(within(list).getAllByRole("option")).toHaveLength(1));
    expect(screen.queryByText(/Showing the first 200/)).toBeNull();
  });

  it("says when nothing matches the search", async () => {
    mount({ value: "main" });
    fireEvent.click(await trigger("Branch"));
    fireEvent.change(await screen.findByPlaceholderText("Search branches"), {
      target: { value: "zzz" },
    });
    expect(await screen.findByText("Nothing matches “zzz”.")).toBeTruthy();
  });

  it("says there are no tags, inside the list", async () => {
    world.refs = () => Response.json(refsOf(["main"], []));
    mount({ kind: "tag" });
    fireEvent.click(await trigger("Tag"));
    const list = await screen.findByRole("listbox", { name: "Tags" });
    expect(list.textContent).toContain("No tags in this repository.");
  });

  it("says the repository may be empty when it has no branches", async () => {
    world.refs = () => Response.json(refsOf([], [], ""));
    mount();
    fireEvent.click(await trigger("Branch"));
    const list = await screen.findByRole("listbox", { name: "Branches" });
    expect(list.textContent).toContain("No branches. The repository may be empty.");
  });

  it("shows skeleton rows and the line Looking up branches and tags… while the refs load", async () => {
    world.refs = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByText("Looking up branches and tags…")).toBeTruthy();
    expect(document.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(0);
    expect(screen.queryByRole("combobox", { name: "Branch" })).toBeNull();
  });

  it("says why the refs could not be read, and tries again on Retry loading refs", async () => {
    let calls = 0;
    world.refs = () => {
      calls += 1;
      return calls === 1 ? problem(422, "auth_refused") : Response.json(refsOf(["main"]));
    };
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Couldn't read homelab.");
    expect(alert.textContent).toContain("git.example.com refused the credential.");
    fireEvent.click(within(alert).getByRole("button", { name: "Retry loading refs" }));
    expect(await trigger("Branch")).toBeTruthy();
    expect(calls).toBe(2);
  });

  it("takes a commit in a mono field with the hint 7 to 40 hex characters.", async () => {
    mount({ kind: "commit" });
    const input = await screen.findByLabelText("Commit");
    expect(input.getAttribute("placeholder")).toBe("a1b2c3d");
    expect(input.className).toContain("font-mono");
    expect(screen.getByText("7 to 40 hex characters.")).toBeTruthy();
    fireEvent.change(input, { target: { value: "abc" } });
    expect(input.getAttribute("aria-invalid")).toBe("true");
    fireEvent.change(input, { target: { value: "abc1234" } });
    expect(input.getAttribute("aria-invalid")).toBeNull();
  });

  it("announces where the ref is, in a polite live region", async () => {
    world.tree = (q) => Response.json(treeOf(q.get("path") ?? "", []));
    mount({ value: "main" });
    const region = await screen.findByRole("status");
    expect(region.getAttribute("aria-live")).toBe("polite");
    await waitFor(() => expect(region.textContent).toMatch(/^main is at 0123456, .+\.$/));
    expect(region.querySelector("code")?.textContent).toBe("0123456");
  });

  it("says Looking up while the ref resolves", async () => {
    world.tree = () => new Promise<Response>(() => {});
    mount({ value: "main" });
    const region = await screen.findByRole("status");
    await waitFor(() => expect(region.textContent).toBe("Looking up main…"));
  });

  it("words a ref that cannot be resolved, and a commit the repository lacks", async () => {
    world.tree = () => problem(422, "commit_missing");
    mount({ kind: "commit", value: "abc1234" });
    const region = await screen.findByRole("status");
    await waitFor(() => expect(region.textContent).toBe("That commit isn't in homelab."));
  });

  it("moves focus to the ref field when the kind changes", async () => {
    mount({ value: "main" });
    await trigger("Branch");
    fireEvent.click(screen.getByRole("radio", { name: "Commit" }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByLabelText("Commit")));
    fireEvent.click(screen.getByRole("radio", { name: "Tag" }));
    await waitFor(() =>
      expect(document.activeElement).toBe(screen.getByRole("combobox", { name: "Tag" })),
    );
    expect(kinds).toEqual(["commit", "tag"]);
  });

  it("wraps a long ref name in the trigger and truncates it in the rows", async () => {
    const long = `feature/${"x".repeat(120)}`;
    world.refs = () => Response.json(refsOf([long], [], ""));
    mount({ value: long });
    const box = await trigger("Branch");
    expect(box.querySelector("span")?.className).toContain("break-all");
    fireEvent.click(box);
    const row = await screen.findByRole("option", { name: new RegExp(long) });
    expect(row.querySelector(".truncate")).toBeTruthy();
  });
});
