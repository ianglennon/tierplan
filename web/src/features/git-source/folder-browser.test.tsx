// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { GitTreeEntry } from "@/api/git";
import { FolderBrowser } from "@/features/git-source/folder-browser";
import {
  dir,
  ENV,
  file,
  link,
  makeWorld,
  newClient,
  problem,
  renderUi,
  stubFetch,
  submodule,
  treeOf,
  type World,
} from "@/features/git-source/test-fixture";

let world: World;
let client: ReturnType<typeof newClient>;
let used: string[];

// A small repository: stacks/{web,db}, a link, a submodule and a README at the root.
const TREES: Record<string, GitTreeEntry[]> = {
  "": [dir("stacks"), dir("docs"), file("README.md"), link("latest"), submodule("vendor")],
  stacks: [dir("web", true), dir("db", true)],
  "stacks/web": [file("compose.yaml"), file(".env")],
  docs: [],
};

beforeEach(() => {
  world = makeWorld();
  world.tree = (q) => {
    const path = q.get("path") ?? "";
    const entries = TREES[path];
    if (!entries) return problem(422, "folder_missing");
    return Response.json(treeOf(path, entries));
  };
  client = newClient();
  used = [];
  stubFetch(world);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function Harness({ initial = null }: { initial?: string | null }) {
  const [folder, setFolder] = useState<string | null>(initial);
  return (
    <FolderBrowser
      envId={ENV}
      repoId="r1"
      repoName="homelab"
      kind="branch"
      gitRef="main"
      folder={folder}
      onUse={(path) => {
        used.push(path);
        setFolder(path);
      }}
    />
  );
}

function mount(initial: string | null = null) {
  renderUi(<Harness initial={initial} />, client);
}

const open = async (name: string) => {
  fireEvent.click(await screen.findByRole("button", { name: new RegExp(`^${name}`) }));
};

describe("FolderBrowser", () => {
  it("shows 6 skeleton rows and Reading homelab at main… while the folder loads", async () => {
    world.tree = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByText("Reading homelab at main…")).toBeTruthy();
    expect(document.querySelectorAll('[data-slot="skeleton"]')).toHaveLength(6);
  });

  it("says why the folder could not be read and reads it again on Retry loading folders", async () => {
    let calls = 0;
    world.tree = (q) => {
      calls += 1;
      return calls === 1
        ? problem(422, "unreachable")
        : Response.json(treeOf(q.get("path") ?? "", TREES[""] ?? []));
    };
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Couldn't read homelab at main.");
    expect(alert.textContent).toContain("Couldn't reach git.example.com.");
    fireEvent.click(within(alert).getByRole("button", { name: "Retry loading folders" }));
    expect(await screen.findByRole("button", { name: /^stacks/ })).toBeTruthy();
    expect(calls).toBe(2);
  });

  it("lists folders first with Has compose file, then files, Submodule and Link rows", async () => {
    world.tree = (q) =>
      Response.json(
        treeOf(q.get("path") ?? "", [
          dir("stacks", true),
          dir("docs"),
          file("README.md"),
          link("latest"),
          submodule("vendor"),
        ]),
      );
    mount();
    const list = await screen.findByRole("list", { name: "Folder contents" });
    const rows = within(list).getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual([
      "stacksHas compose file",
      "docs",
      "README.md",
      "latestLink",
      "vendorSubmodule",
    ]);
  });

  it("disables the Submodule and Link rows, with their words as the description", async () => {
    mount();
    const sub = await screen.findByRole("button", { name: /^vendor/ });
    expect(sub.getAttribute("aria-disabled")).toBe("true");
    const text = document.getElementById(sub.getAttribute("aria-describedby") ?? "none");
    expect(text?.textContent).toBe("Bayplan doesn't support submodules.");
    const lnk = screen.getByRole("button", { name: /^latest/ });
    expect(lnk.getAttribute("aria-disabled")).toBe("true");
    expect(
      document.getElementById(lnk.getAttribute("aria-describedby") ?? "none")?.textContent,
    ).toBe("Links aren't followed.");
    const reads = world.sent.filter((s) => s.path.endsWith("/tree")).length;
    fireEvent.click(sub);
    fireEvent.click(lnk);
    expect(world.sent.filter((s) => s.path.endsWith("/tree")).length).toBe(reads);
  });

  it("says a folder is empty", async () => {
    mount();
    await open("docs");
    expect(await screen.findByText("This folder is empty.")).toBeTruthy();
  });

  it("says there is no compose file here, unless the folder has one or a yaml file", async () => {
    mount();
    expect(await screen.findByText("No compose file here")).toBeTruthy();
    expect(
      screen.getByText("Open a folder, or pick this folder if its compose file has another name."),
    ).toBeTruthy();
    await open("stacks");
    // Both folders hold a compose file, so the way forward is clear.
    await screen.findByRole("button", { name: /^web/ });
    expect(screen.queryByText("No compose file here")).toBeNull();
  });

  it("has a path bar that names the root and each folder, and walks back", async () => {
    mount();
    await open("stacks");
    await open("web");
    const bar = await screen.findByRole("navigation", { name: "Folder path" });
    const buttons = within(bar).getAllByRole("button");
    expect(buttons.map((b) => b.getAttribute("aria-label") ?? b.textContent)).toEqual([
      "Repository root",
      "stacks",
      "web",
    ]);
    expect(buttons[2]?.getAttribute("aria-current")).toBe("page");
    expect(buttons[0]?.textContent).toBe("homelab");
    fireEvent.click(buttons[1] as HTMLElement);
    await screen.findByRole("button", { name: /^db/ });
    expect(
      within(screen.getByRole("navigation", { name: "Folder path" }))
        .getAllByRole("button")
        .map((b) => b.getAttribute("aria-label") ?? b.textContent),
    ).toEqual(["Repository root", "stacks"]);
  });

  it("uses the folder it is in, and says the stack folder", async () => {
    mount();
    await open("stacks");
    await open("web");
    await screen.findByText("compose.yaml");
    fireEvent.click(screen.getByRole("button", { name: "Use this folder" }));
    expect(used).toEqual(["stacks/web"]);
    expect(screen.getByText("Stack folder: stacks/web")).toBeTruthy();
  });

  it("reads the repository root as / (repository root)", async () => {
    mount();
    await screen.findByRole("button", { name: /^stacks/ });
    fireEvent.click(screen.getByRole("button", { name: "Use this folder" }));
    expect(used).toEqual([""]);
    expect(screen.getByText("Stack folder: / (repository root)")).toBeTruthy();
  });

  it("folds away to the chosen folder, and Change opens the list with the chosen folder marked", async () => {
    mount("stacks");
    expect(screen.getByText("Stack folder: stacks")).toBeTruthy();
    expect(screen.queryByRole("list", { name: "Folder contents" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Change" }));
    const row = await screen.findByRole("button", { name: /^stacks/ });
    expect(row.getAttribute("aria-current")).toBe("true");
  });

  it("walks the list with the arrow keys, opening with Enter or Right and going up with Left or Backspace", async () => {
    mount();
    const first = await screen.findByRole("button", { name: /^stacks/ });
    // Roving focus: one row is in the tab order.
    const rows = () =>
      within(screen.getByRole("list", { name: "Folder contents" })).getAllByRole("button");
    expect(rows().map((r) => r.getAttribute("tabindex"))).toEqual(["0", "-1", "-1", "-1"]);
    first.focus();
    fireEvent.keyDown(first, { key: "ArrowDown" });
    expect(document.activeElement).toBe(rows()[1]);
    fireEvent.keyDown(rows()[1] as HTMLElement, { key: "ArrowUp" });
    expect(document.activeElement).toBe(rows()[0]);

    fireEvent.keyDown(rows()[0] as HTMLElement, { key: "ArrowRight" });
    await screen.findByRole("button", { name: /^web/ });
    expect(
      within(screen.getByRole("navigation", { name: "Folder path" })).getByText("stacks"),
    ).toBeTruthy();

    fireEvent.keyDown(rows()[0] as HTMLElement, { key: "ArrowLeft" });
    await screen.findByRole("button", { name: /^docs/ });
    fireEvent.keyDown(rows()[0] as HTMLElement, { key: "ArrowRight" });
    await screen.findByRole("button", { name: /^web/ });
    fireEvent.keyDown(rows()[0] as HTMLElement, { key: "Backspace" });
    await screen.findByRole("button", { name: /^docs/ });
  });

  it("announces Opened {folder}, {n} entries politely when a folder is entered", async () => {
    mount();
    await open("stacks");
    const region = await screen.findByRole("status");
    expect(region.getAttribute("aria-live")).toBe("polite");
    await waitFor(() => expect(region.textContent).toBe("Opened stacks, 2 entries"));
  });

  it("counts one entry in the singular and none as 0 entries", async () => {
    world.tree = (q) => {
      const path = q.get("path") ?? "";
      if (path === "") return Response.json(treeOf("", [dir("one"), dir("none")]));
      if (path === "one") return Response.json(treeOf(path, [file("a")]));
      return Response.json(treeOf(path, []));
    };
    mount();
    await open("one");
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Opened one, 1 entry"));
    fireEvent.click(screen.getByRole("button", { name: "Repository root" }));
    await open("none");
    await waitFor(() =>
      expect(screen.getByRole("status").textContent).toBe("Opened none, 0 entries"),
    );
  });

  it("offers a filter from 13 entries and not before", async () => {
    const entries = (n: number) =>
      Array.from({ length: n }, (_, i) => dir(`dir-${String(i).padStart(2, "0")}`));
    world.tree = (q) => Response.json(treeOf(q.get("path") ?? "", entries(12)));
    mount();
    await screen.findByRole("button", { name: /^dir-00/ });
    expect(screen.queryByLabelText("Filter this folder")).toBeNull();
    cleanup();
    world.tree = (q) => Response.json(treeOf(q.get("path") ?? "", entries(13)));
    client.clear();
    mount();
    const filter = await screen.findByLabelText("Filter this folder");
    fireEvent.change(filter, { target: { value: "dir-12" } });
    await waitFor(() =>
      expect(
        within(screen.getByRole("list", { name: "Folder contents" })).getAllByRole("listitem"),
      ).toHaveLength(1),
    );
    fireEvent.change(filter, { target: { value: "zzz" } });
    expect(await screen.findByText("Nothing matches “zzz”.")).toBeTruthy();
  });

  it("shows the first 500 of 5,000 entries, says so, and scrolls inside 320px", async () => {
    const entries = Array.from({ length: 500 }, (_, i) => file(`f-${String(i).padStart(4, "0")}`));
    world.tree = (q) =>
      Response.json(treeOf(q.get("path") ?? "", entries, { total: 5000, capped: true }));
    mount();
    expect(
      await screen.findByText("Showing the first 500 of 5,000. Filter to find the rest."),
    ).toBeTruthy();
    const list = screen.getByRole("list", { name: "Folder contents" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(500);
    expect(list.className).toContain("max-h-80");
    expect(list.className).toContain("overflow-y-auto");
    expect(screen.getByLabelText("Filter this folder")).toBeTruthy();
  });

  it("truncates a long name in the row and wraps a long path in the chosen line", async () => {
    const long = "n".repeat(150);
    world.tree = (q) => Response.json(treeOf(q.get("path") ?? "", [dir(long)]));
    mount(long);
    expect(screen.getByText(`Stack folder: ${long}`).className).toContain("break-all");
    fireEvent.click(screen.getByRole("button", { name: "Change" }));
    const row = await screen.findByRole("button", { name: new RegExp(long) });
    expect(row.querySelector(".truncate")).toBeTruthy();
  });
});
