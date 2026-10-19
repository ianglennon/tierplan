// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackReviewService } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ServiceDiffList } from "@/features/stack-review/service-diff-list";
import "@/i18n";

// The Changes tab's list of services (plan 07-15, STK-06, D-11): one collapsible row for each
// service a deploy would add, change or remove, with the field-level difference under a changed
// one and nothing under a removed one.

afterEach(() => cleanup());

const spec = (image: string, replicas: number, env: string[] = []) => ({
  Name: "s_web",
  Mode: { Replicated: { Replicas: replicas } },
  TaskTemplate: { ContainerSpec: { Image: image, Env: env } },
});

function service(over: Partial<StackReviewService> & { name: string }): StackReviewService {
  return {
    change: "changed",
    short: over.name,
    current: spec("nginx:1.26@sha256:aa", 2),
    next: spec("nginx:1.27@sha256:bb", 3),
    hiddenChanges: [],
    settings: 2,
    ...over,
  };
}

function mount(services: StackReviewService[]) {
  render(
    <TooltipProvider>
      <ServiceDiffList services={services} />
    </TooltipProvider>,
  );
}

const rowOf = (name: string) => screen.getByText(name).closest("li") as HTMLElement;

describe("the rows", () => {
  it("name the change in words with a glyph, the service in mono and how many settings changed", () => {
    mount([
      service({ name: "web", settings: 2 }),
      service({ name: "api", change: "added", current: null, settings: 9 }),
      service({ name: "old", change: "removed", next: null, settings: 0 }),
    ]);
    // The word is in the row's own button; an open row also has the diff's Change column below.
    const web = within(within(rowOf("web")).getByRole("button"));
    expect(web.getByText("Changed")).toBeTruthy();
    expect(web.getByText("2 settings changed")).toBeTruthy();
    expect(screen.getByText("web").className).toContain("font-mono");
    expect(within(within(rowOf("api")).getByRole("button")).getByText("Added")).toBeTruthy();
    expect(within(rowOf("old")).getByText("Removed")).toBeTruthy();
  });

  it("say one setting, not one settings", () => {
    mount([service({ name: "web", settings: 1 })]);
    expect(within(rowOf("web")).getByText("1 setting changed")).toBeTruthy();
  });

  it("show the image change: the old reference to the new one, without the digests", () => {
    mount([service({ name: "web" })]);
    const row = rowOf("web");
    expect(within(row).getByText("nginx:1.26")).toBeTruthy();
    expect(within(row).getByText("nginx:1.27")).toBeTruthy();
  });

  it("say a new digest when only the digest moved", () => {
    mount([
      service({
        name: "web",
        current: spec("nginx:1.27@sha256:aa", 2),
        next: spec("nginx:1.27@sha256:bb", 2),
      }),
    ]);
    expect(within(rowOf("web")).getByText("New digest")).toBeTruthy();
  });

  it("list the removed services last, whatever order they came in", () => {
    mount([
      service({ name: "old", change: "removed", next: null }),
      service({ name: "web" }),
      service({ name: "api", change: "added", current: null }),
    ]);
    const names = within(screen.getByRole("list")).getAllByRole("listitem");
    expect(names.map((li) => li.querySelector("[data-slot=service-name]")?.textContent)).toEqual([
      "web",
      "api",
      "old",
    ]);
  });

  it("draw a service name with markup in it as text", () => {
    mount([service({ name: "<b>web</b>" })]);
    expect(screen.getByText("<b>web</b>").querySelector("b")).toBeNull();
  });
});

describe("expanding a row", () => {
  it("opens by default when three services or fewer changed", () => {
    mount([service({ name: "a" }), service({ name: "b" }), service({ name: "c" })]);
    for (const name of ["a", "b", "c"]) {
      const button = within(rowOf(name)).getByRole("button");
      expect(button.getAttribute("aria-expanded")).toBe("true");
    }
    expect(screen.getAllByRole("table", { name: /^Changes to / })).toHaveLength(3);
  });

  it("starts closed when more than three changed, and opens on a press", () => {
    mount(["a", "b", "c", "d"].map((name) => service({ name })));
    const button = within(rowOf("a")).getByRole("button");
    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("table", { name: "Changes to a" })).toBeNull();
    fireEvent.click(button);
    expect(button.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByRole("table", { name: "Changes to a" })).toBeTruthy();
  });

  it("shows the field-level difference of a changed service", () => {
    mount([service({ name: "web" })]);
    const table = screen.getByRole("table", { name: "Changes to web" });
    const replicas = within(table).getByText("Replicas").closest("tr") as HTMLElement;
    expect(within(replicas).getByText("2")).toBeTruthy();
    expect(within(replicas).getByText("3")).toBeTruthy();
  });

  it("lists every setting of an added service as a new value", () => {
    mount([service({ name: "api", change: "added", current: null })]);
    const table = screen.getByRole("table", { name: "Changes to api" });
    expect(within(table).getByRole("columnheader", { name: "Setting" })).toBeTruthy();
    expect(within(table).queryByRole("columnheader", { name: "Current" })).toBeNull();
  });

  it("gives a removed service nothing to expand", () => {
    mount([service({ name: "old", change: "removed", next: null })]);
    expect(within(rowOf("old")).queryByRole("button")).toBeNull();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("lists a hidden change by its path alone, as Changed (hidden)", () => {
    const hidden = "TaskTemplate.ContainerSpec.Env[API_KEY]";
    mount([
      service({
        name: "web",
        current: spec("nginx:1.26@sha256:aa", 2, ["API_KEY=<hidden>"]),
        next: spec("nginx:1.27@sha256:bb", 3, ["API_KEY=<hidden>"]),
        hiddenChanges: [hidden],
      }),
    ]);
    const table = screen.getByRole("table", { name: "Hidden changes to web" });
    const row = within(table).getByText(hidden).closest("tr") as HTMLElement;
    expect(within(row).getByText("Changed (hidden)")).toBeTruthy();
    // The path is all there is: no value of the variable is on the page.
    expect(row.textContent).toBe(`~Changed (hidden)${hidden}`);
  });

  it("shows only the hidden row, and not an empty diff, when nothing but a hidden value changed", () => {
    const same = spec("nginx:1.27@sha256:bb", 3, ["API_KEY=<hidden>"]);
    mount([
      service({
        name: "web",
        current: same,
        next: same,
        hiddenChanges: ["TaskTemplate.ContainerSpec.Env[API_KEY]"],
        settings: 1,
      }),
    ]);
    expect(screen.getByRole("table", { name: "Hidden changes to web" })).toBeTruthy();
    expect(screen.queryByRole("table", { name: "Changes to web" })).toBeNull();
    expect(screen.queryByText("No changes")).toBeNull();
  });
});

// The services a recreate carries with it (plan 07-22, STK-06): a standalone deploy that recreates a service
// recreates what shares its network, processes or volumes and restarts what depends on it with restart: true.
// Their rows say so and why; they are not changes, so they expand to nothing and count no settings.

describe("the services a recreate carries with it", () => {
  const carried = (name: string, change: "recreated" | "restarted", because: string) =>
    service({ name, change, because, current: null, next: null, settings: 0 });

  it("shows a recreated service with the glyph, the word, the name in mono and why", () => {
    mount([service({ name: "web" }), carried("sidecar", "recreated", "web")]);
    const row = rowOf("sidecar");
    expect(within(row).getByText("Recreated")).toBeTruthy();
    expect(screen.getByText("sidecar").className).toContain("font-mono");
    expect(
      within(row).getByText(
        "Recreated with web, whose network, processes or volumes it shares. Settings and pinned image stay the same.",
      ),
    ).toBeTruthy();
    expect(row.querySelector("svg.lucide-rotate-ccw")).not.toBeNull();
  });

  it("shows a restarted service with the word, the name and why", () => {
    mount([service({ name: "web" }), carried("restarter", "restarted", "web")]);
    const row = rowOf("restarter");
    expect(within(row).getByText("Restarted")).toBeTruthy();
    expect(
      within(row).getByText(
        "Restarted because web is recreated (depends_on with restart: true). Same container, settings and image.",
      ),
    ).toBeTruthy();
  });

  it("expands to nothing and counts no settings", () => {
    mount([service({ name: "web" }), carried("sidecar", "recreated", "web")]);
    const row = rowOf("sidecar");
    expect(within(row).queryByRole("button")).toBeNull();
    expect(within(row).queryByText(/settings? changed/)).toBeNull();
    expect(screen.queryByRole("table", { name: "Changes to sidecar" })).toBeNull();
  });

  it("follows the changed services and comes before the removed ones, whatever order it arrived in", () => {
    mount([
      service({ name: "old", change: "removed", next: null }),
      carried("restarter", "restarted", "web"),
      carried("sidecar", "recreated", "web"),
      service({ name: "web" }),
    ]);
    const names = within(screen.getByRole("list")).getAllByRole("listitem");
    expect(names.map((li) => li.querySelector("[data-slot=service-name]")?.textContent)).toEqual([
      "web",
      "restarter",
      "sidecar",
      "old",
    ]);
  });

  it("is not counted when it decides whether the changed rows open", () => {
    mount([
      service({ name: "a" }),
      service({ name: "b" }),
      service({ name: "c" }),
      carried("d", "recreated", "a"),
      carried("e", "restarted", "a"),
    ]);
    expect(within(rowOf("a")).getByRole("button").getAttribute("aria-expanded")).toBe("true");
  });

  it("draws the name that carries it as text", () => {
    mount([carried("sidecar", "recreated", "<b>web</b>")]);
    expect(rowOf("sidecar").querySelector("b")).toBeNull();
    expect(within(rowOf("sidecar")).getByText(/<b>web<\/b>/)).toBeTruthy();
  });
});
