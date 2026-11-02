// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { type ChooserNode, NodeChooser } from "@/features/images/node-chooser";
import "@/i18n";

const NODES: ChooserNode[] = [
  { nodeId: "a", name: "mgr-1", role: "manager", arch: "amd64", reachable: true },
  { nodeId: "b", name: "wrk-1", role: "worker", arch: "arm64", reachable: true },
  { nodeId: "c", name: "wrk-2", role: "worker", arch: "arm/v7", reachable: false },
  { nodeId: "d", name: "wrk-3", role: "worker", arch: "amd64", reachable: true, readOnly: true },
];

afterEach(() => cleanup());

function mount(value: string[] = [], extra: Partial<Parameters<typeof NodeChooser>[0]> = {}) {
  const onChange = vi.fn();
  render(
    <TooltipProvider>
      <NodeChooser nodes={NODES} value={value} onChange={onChange} {...extra} />
    </TooltipProvider>,
  );
  return { onChange };
}

const row = (name: string) => screen.getByRole("checkbox", { name: new RegExp(`^${name}`) });

describe("NodeChooser", () => {
  it("lists each node with its role and its architecture as a registry names it", () => {
    mount();
    const group = screen.getByRole("group");
    expect(within(group).getAllByRole("checkbox")).toHaveLength(4);
    expect(within(group).getByText("Manager")).toBeTruthy();
    expect(within(group).getAllByText("Worker")).toHaveLength(3);
    expect(within(group).getAllByText("amd64")).toHaveLength(2);
    expect(within(group).getByText("arm64")).toBeTruthy();
    expect(within(group).getByText("arm/v7")).toBeTruthy();
  });

  it("counts the chosen nodes against all of them", () => {
    mount(["a", "b"]);
    expect(screen.getByText("2 of 4 selected")).toBeTruthy();
    expect((row("mgr-1") as HTMLElement).getAttribute("aria-checked")).toBe("true");
    expect((row("wrk-1") as HTMLElement).getAttribute("aria-checked")).toBe("true");
    expect((row("wrk-3") as HTMLElement).getAttribute("aria-checked")).toBe("false");
  });

  it("hands back the chosen IDs in the order of the nodes, adding and removing one", () => {
    const { onChange } = mount(["b"]);
    fireEvent.click(row("mgr-1"));
    expect(onChange).toHaveBeenLastCalledWith(["a", "b"]);
    fireEvent.click(row("wrk-1"));
    expect(onChange).toHaveBeenLastCalledWith([]);
  });

  it("disables a node that is not reachable and says why", () => {
    const { onChange } = mount();
    const box = row("wrk-2");
    expect(box.hasAttribute("disabled")).toBe(true);
    expect(screen.getByText("Not reachable")).toBeTruthy();
    expect(document.body.textContent).toContain("wrk-2 isn't reachable right now.");
    fireEvent.click(box);
    expect(onChange).not.toHaveBeenCalled();
  });

  it("disables a read-only host and says so", () => {
    mount();
    expect(row("wrk-3").hasAttribute("disabled")).toBe(true);
    expect(document.body.textContent).toContain("This host is read-only (set on the host)");
  });

  it("disables a row for the reason the caller gives, which beats the generic ones", () => {
    mount([], { disabledReasons: { b: "In use by 2 containers." } });
    expect(row("wrk-1").hasAttribute("disabled")).toBe(true);
    expect(document.body.textContent).toContain("In use by 2 containers.");
  });

  it("scrolls inside 240px and has 44px rows on a narrow screen", () => {
    mount();
    expect(document.querySelector('[data-slot="scroll-area"]')?.className).toContain("max-h-60");
    expect(row("mgr-1").closest("label")?.className).toContain("min-h-11");
    expect(row("mgr-1").closest("label")?.className).toContain("md:min-h-8");
  });
});
