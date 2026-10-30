// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { StackConcurrent, StackFileDiff } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ConcurrentDeployPanel } from "@/features/stack-review/concurrent-deploy-panel";
import "@/i18n";

// The concurrent-deploy guard (plan 07-16, STK-05, D-09): the warning that someone deployed after
// the draft was started, the dialog that shows what changed, and the checkbox that gates Deploy.

afterEach(() => cleanup());

const minutesAgo = (n: number) => new Date(Date.now() - n * 60_000).toISOString();

const file = (over: Partial<StackFileDiff> & { path: string }): StackFileDiff => ({
  kind: "file",
  hidden: false,
  changedLines: 2,
  hunks: [
    {
      oldStart: 1,
      oldLines: 2,
      newStart: 1,
      newLines: 2,
      lines: [
        { kind: "context", old: 1, new: 1, text: "services:" },
        { kind: "removed", old: 2, new: 0, text: "  old: line" },
        { kind: "added", old: 0, new: 2, text: "  new: line" },
      ],
    },
  ],
  lines: ["services:", "  new: line"],
  ...over,
});

const concurrent = (over: Partial<StackConcurrent> = {}): StackConcurrent => ({
  revision: 4,
  deployedBy: "bob",
  deployedAt: minutesAgo(5),
  baseToDeployed: [file({ path: "compose.yaml" })],
  baseToDraft: [
    file({
      path: "compose.yaml",
      hunks: [
        {
          oldStart: 1,
          oldLines: 1,
          newStart: 1,
          newLines: 2,
          lines: [
            { kind: "context", old: 1, new: 1, text: "services:" },
            { kind: "added", old: 0, new: 2, text: "  replicas: 3" },
          ],
        },
      ],
      lines: ["services:", "  replicas: 3"],
      changedLines: 1,
    }),
    file({ path: "tok.txt", hidden: true, changedLines: 0, hunks: [], lines: [] }),
  ],
  ...over,
});

function mount(c: StackConcurrent = concurrent(), acknowledged = false) {
  const onAcknowledge = vi.fn();
  const view = render(
    <TooltipProvider>
      <ConcurrentDeployPanel
        concurrent={c}
        acknowledged={acknowledged}
        onAcknowledge={onAcknowledge}
      />
    </TooltipProvider>,
  );
  return { onAcknowledge, ...view };
}

describe("the warning", () => {
  it("says the stack was deployed after the person started editing, by whom and when", () => {
    mount();
    const alert = screen.getByRole("alert");
    expect(
      within(alert).getByText("This stack was deployed after you started editing"),
    ).toBeTruthy();
    expect(alert.textContent).toContain("bob deployed 5 min ago.");
    expect(alert.textContent).toContain("Check what changed before you deploy over it.");
    expect(within(alert).getByRole("button", { name: "See what changed" })).toBeTruthy();
  });

  it("draws the name of whoever deployed as text", () => {
    mount(concurrent({ deployedBy: "<b>bob</b>" }));
    expect(screen.getByRole("alert").textContent).toContain("<b>bob</b> deployed");
    expect(document.querySelector("b")).toBeNull();
  });
});

describe("the acknowledgement", () => {
  it("is a labelled checkbox that starts unticked", () => {
    mount();
    const box = screen.getByRole("checkbox", {
      name: "I've seen what changed and want to deploy my draft over it.",
    });
    expect(box.getAttribute("aria-checked")).toBe("false");
  });

  it("tells the page when it is ticked and when it is cleared", () => {
    const { onAcknowledge } = mount();
    fireEvent.click(screen.getByRole("checkbox"));
    expect(onAcknowledge).toHaveBeenLastCalledWith(true);
    cleanup();
    const again = mount(concurrent(), true);
    const box = screen.getByRole("checkbox");
    expect(box.getAttribute("aria-checked")).toBe("true");
    fireEvent.click(box);
    expect(again.onAcknowledge).toHaveBeenLastCalledWith(false);
  });
});

describe("the dialog", () => {
  const open = () => fireEvent.click(screen.getByRole("button", { name: "See what changed" }));

  it("opens on See what changed with its title, its words and a Close button", async () => {
    mount();
    expect(screen.queryByRole("dialog")).toBeNull();
    open();
    const dialog = await screen.findByRole("dialog", { name: "Changed since you started" });
    expect(
      within(dialog).getByText(
        "The version your draft started from, what is deployed now, and your draft.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getAllByRole("button", { name: "Close" }).length).toBeGreaterThan(0);
    expect(dialog.className).toContain("sm:max-w-200");
  });

  it("has two tabs, Deployed since first, each a diff of its own", async () => {
    mount();
    open();
    const dialog = await screen.findByRole("dialog");
    expect(
      within(dialog).getByRole("tab", { name: "Deployed since" }).getAttribute("aria-selected"),
    ).toBe("true");
    expect(within(dialog).getByRole("row", { name: "Added line 2" })).toBeTruthy();
    expect(within(dialog).getByText("new: line")).toBeTruthy();

    const mine = within(dialog).getByRole("tab", { name: "Your changes" });
    fireEvent.mouseDown(mine);
    fireEvent.click(mine);
    expect(await within(dialog).findByText("replicas: 3")).toBeTruthy();
    expect(within(dialog).queryByText("new: line")).toBeNull();
  });

  it("names each file in a group row above its diff, and the diff is a table captioned with it", async () => {
    mount();
    open();
    const dialog = await screen.findByRole("dialog");
    const heading = within(dialog).getByRole("heading", { name: "compose.yaml" });
    expect(heading.className).toContain("font-mono");
    expect(within(dialog).getByRole("table", { name: "Changes to compose.yaml" })).toBeTruthy();
  });

  it("says a secret file or variable changed without showing it", async () => {
    mount();
    open();
    const dialog = await screen.findByRole("dialog");
    const mine = within(dialog).getByRole("tab", { name: "Your changes" });
    fireEvent.mouseDown(mine);
    fireEvent.click(mine);
    const secret = (await within(dialog).findByRole("heading", { name: "tok.txt" })).closest(
      "section",
    );
    expect(secret).not.toBeNull();
    expect(within(secret as HTMLElement).getByText("Changed (hidden)")).toBeTruthy();
    expect(within(secret as HTMLElement).queryByRole("table")).toBeNull();
  });

  it("calls the variables Variables, not by their address", async () => {
    mount(
      concurrent({
        baseToDeployed: [
          file({
            path: "variables",
            kind: "variables",
            hidden: true,
            hunks: [],
            lines: [],
            changedLines: 0,
          }),
        ],
      }),
    );
    open();
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: "Variables" })).toBeTruthy();
  });

  it("says when a version changed nothing", async () => {
    mount(concurrent({ baseToDeployed: [] }));
    open();
    const dialog = await screen.findByRole("dialog");
    expect(
      within(dialog).getByText("Nothing in the stack's files or variables changed."),
    ).toBeTruthy();
  });

  it("closes on Close and leaves the acknowledgement alone", async () => {
    const { onAcknowledge } = mount();
    open();
    const dialog = await screen.findByRole("dialog");
    const close = within(dialog)
      .getAllByRole("button", { name: "Close" })
      .find((b) => b.textContent === "Close") as HTMLElement;
    fireEvent.click(close);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(onAcknowledge).not.toHaveBeenCalled();
  });
});
