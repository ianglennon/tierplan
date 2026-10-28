// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { GitTreeEntry } from "@/api/git";
import { ComposeFileList } from "@/features/git-source/compose-file-list";
import {
  dir,
  ENV,
  file,
  makeWorld,
  newClient,
  problem,
  renderUi,
  stubFetch,
  treeOf,
  type World,
} from "@/features/git-source/test-fixture";

let world: World;
let client: ReturnType<typeof newClient>;
let entries: GitTreeEntry[];
let changes: string[][];

beforeEach(() => {
  world = makeWorld();
  entries = [
    file("compose.yaml"),
    file("override.yml"),
    file("prod.yaml"),
    file(".env"),
    dir("sub"),
  ];
  world.tree = (q) => Response.json(treeOf(q.get("path") ?? "", entries));
  client = newClient();
  changes = [];
  stubFetch(world);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function Harness({ initial = [] }: { initial?: string[] }) {
  const [files, setFiles] = useState<string[]>(initial);
  return (
    <ComposeFileList
      envId={ENV}
      repoId="r1"
      kind="branch"
      gitRef="main"
      folder="stacks/web"
      files={files}
      onChange={(next) => {
        changes.push(next);
        setFiles(next);
      }}
    />
  );
}

function mount(initial: string[] = []) {
  renderUi(<Harness initial={initial} />, client);
}

const rows = () =>
  within(screen.getByRole("list", { name: "Compose files, in merge order" })).getAllByRole(
    "listitem",
  );
const names = () => rows().map((r) => r.querySelector("code")?.textContent);
// openAdd opens the Add file popover once the folder has been read: the button is off until then.
async function openAdd() {
  const add = screen.getByRole("button", { name: "Add file" }) as HTMLButtonElement;
  await waitFor(() => expect(add.disabled).toBe(false));
  fireEvent.click(add);
}

const button = (name: string) => screen.getByRole("button", { name }) as HTMLButtonElement;

describe("ComposeFileList", () => {
  it("is an ordered list of the files, numbered, with the merge-order helper", async () => {
    mount(["compose.yaml", "override.yml"]);
    const list = await screen.findByRole("list", { name: "Compose files, in merge order" });
    expect(list.tagName).toBe("OL");
    expect(names()).toEqual(["compose.yaml", "override.yml"]);
    expect(rows().map((r) => r.querySelector("[data-order]")?.textContent)).toEqual(["1", "2"]);
    expect(
      screen.getByText(
        "Files are merged in this order, like docker compose -f a -f b. Later files override earlier ones.",
      ),
    ).toBeTruthy();
  });

  it("shows the helper and says none is chosen when the list is empty", async () => {
    entries = [file("a.yaml"), file("b.yaml")];
    mount();
    expect(await screen.findByText("No compose file chosen yet.")).toBeTruthy();
    expect(screen.getByText(/Files are merged in this order/)).toBeTruthy();
  });

  it("disables Move up on the first row and Move down on the last, and both with one file", async () => {
    mount(["compose.yaml", "override.yml", "prod.yaml"]);
    await screen.findByRole("list", { name: "Compose files, in merge order" });
    expect(button("Move compose.yaml up").disabled).toBe(true);
    expect(button("Move compose.yaml down").disabled).toBe(false);
    expect(button("Move override.yml up").disabled).toBe(false);
    expect(button("Move override.yml down").disabled).toBe(false);
    expect(button("Move prod.yaml down").disabled).toBe(true);
    cleanup();
    mount(["compose.yaml"]);
    await screen.findByRole("list", { name: "Compose files, in merge order" });
    expect(button("Move compose.yaml up").disabled).toBe(true);
    expect(button("Move compose.yaml down").disabled).toBe(true);
  });

  it("moves a file and announces its new number politely", async () => {
    mount(["compose.yaml", "override.yml", "prod.yaml"]);
    await screen.findByRole("list", { name: "Compose files, in merge order" });
    fireEvent.click(button("Move compose.yaml down"));
    expect(names()).toEqual(["override.yml", "compose.yaml", "prod.yaml"]);
    const region = screen.getByRole("status");
    expect(region.getAttribute("aria-live")).toBe("polite");
    expect(region.textContent).toBe("compose.yaml is now number 2");
    fireEvent.click(button("Move prod.yaml up"));
    expect(names()).toEqual(["override.yml", "prod.yaml", "compose.yaml"]);
    expect(region.textContent).toBe("prod.yaml is now number 2");
  });

  it("keeps focus on the moved row's same button, or the other one when that button turns off", async () => {
    mount(["compose.yaml", "override.yml", "prod.yaml"]);
    await screen.findByRole("list", { name: "Compose files, in merge order" });
    button("Move compose.yaml down").focus();
    fireEvent.click(button("Move compose.yaml down"));
    await waitFor(() => expect(document.activeElement).toBe(button("Move compose.yaml down")));
    // At the end of the list Move down is off; focus goes to Move up on the same row.
    fireEvent.click(button("Move compose.yaml down"));
    expect(names()).toEqual(["override.yml", "prod.yaml", "compose.yaml"]);
    await waitFor(() => expect(document.activeElement).toBe(button("Move compose.yaml up")));
  });

  it("offers only the folder's yml and yaml files that are not chosen, and adds the pick last", async () => {
    mount(["compose.yaml"]);
    await screen.findByRole("list", { name: "Compose files, in merge order" });
    await openAdd();
    const list = await screen.findByRole("listbox");
    expect(
      within(list)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["override.yml", "prod.yaml"]);
    expect(screen.getByPlaceholderText("Search files")).toBeTruthy();
    fireEvent.change(screen.getByPlaceholderText("Search files"), { target: { value: "prod" } });
    await waitFor(() => expect(within(list).getAllByRole("option")).toHaveLength(1));
    fireEvent.click(within(list).getByRole("option", { name: "prod.yaml" }));
    expect(names()).toEqual(["compose.yaml", "prod.yaml"]);
  });

  it("says when the folder has no other file to add", async () => {
    entries = [file("compose.yaml"), file("notes.txt")];
    mount(["compose.yaml"]);
    await screen.findByRole("list", { name: "Compose files, in merge order" });
    await openAdd();
    expect(await screen.findByText("No other .yml or .yaml files in this folder.")).toBeTruthy();
  });

  it("removes a file", async () => {
    mount(["compose.yaml", "override.yml"]);
    await screen.findByRole("list", { name: "Compose files, in merge order" });
    fireEvent.click(button("Remove compose.yaml"));
    expect(names()).toEqual(["override.yml"]);
    expect(changes.at(-1)).toEqual(["override.yml"]);
  });

  it("picks the one conventional file by itself and says so", async () => {
    mount();
    expect(
      await screen.findByText("Picked compose.yaml. Change it if the stack uses another."),
    ).toBeTruthy();
    expect(names()).toEqual(["compose.yaml"]);
    expect(changes).toEqual([["compose.yaml"]]);
  });

  it("does not pick it again once the person removed it", async () => {
    mount();
    await screen.findByText("Picked compose.yaml. Change it if the stack uses another.");
    fireEvent.click(button("Remove compose.yaml"));
    expect(screen.queryByText(/Picked compose.yaml/)).toBeNull();
    expect(changes).toEqual([["compose.yaml"], []]);
  });

  it.each([
    ["two conventional files", [file("compose.yaml"), file("docker-compose.yml")]],
    ["none", [file("app.yaml"), file("README.md")]],
  ])("picks nothing for %s", async (_label, found) => {
    entries = found;
    mount();
    await screen.findByText("No compose file chosen yet.");
    expect(changes).toEqual([]);
    expect(screen.queryByText(/Picked/)).toBeNull();
  });

  it("does not pick when files are already chosen", async () => {
    mount(["override.yml"]);
    await screen.findByRole("list", { name: "Compose files, in merge order" });
    expect(changes).toEqual([]);
  });

  it("wraps a long path with break-all in mono", async () => {
    const long = `${"x".repeat(100)}.yaml`;
    entries = [file(long)];
    mount([long]);
    const code = await screen.findByText(long);
    expect(code.className).toContain("break-all");
    expect(code.className).toContain("font-mono");
  });

  it("keeps 12 files scrolling inside the list, with 44px rows on a phone", async () => {
    const twelve = Array.from({ length: 12 }, (_, i) => `f-${String(i).padStart(2, "0")}.yaml`);
    entries = twelve.map((n) => file(n));
    mount(twelve);
    const list = await screen.findByRole("list", { name: "Compose files, in merge order" });
    expect(list.className).toContain("overflow-y-auto");
    expect(list.className).toMatch(/max-h-/);
    expect(rows()[0]?.className).toContain("max-md:min-h-11");
    expect(button("Move f-03.yaml up").className).toContain("max-md:size-11");
  });

  it("says the files could not be read, and offers nothing to add", async () => {
    world.tree = () => problem(422, "folder_missing");
    mount(["compose.yaml"]);
    expect(await screen.findByText("Couldn't read the files of this folder.")).toBeTruthy();
    expect(button("Add file").disabled).toBe(true);
  });
});
