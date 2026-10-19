// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackList } from "@/features/stacks/stack-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";
import { TestRouter } from "@/test-router";

const ENV = "e1";

let me: ReturnType<typeof makeMe>;
let stacks: unknown;
let client: QueryClient;
let listStatus: number;

const stack = (over: Record<string, unknown> = {}) => ({
  id: "s1",
  name: "web",
  target: "swarm",
  source: "inline",
  state: "not_deployed",
  lastDeployedAt: "",
  lastDeployedBy: "",
  hasDraft: false,
  ...over,
});

beforeEach(() => {
  me = makeMe({ envs: { [ENV]: "admin" } });
  stacks = { items: [stack()] };
  listStatus = 200;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks`) {
        if (listStatus !== 200)
          return Response.json({ title: "x", status: listStatus }, { status: listStatus });
        return Response.json(stacks);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(props: Partial<Parameters<typeof StackList>[0]> = {}) {
  const onCreatingChange = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StackList envId={ENV} creating={false} onCreatingChange={onCreatingChange} {...props} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return { onCreatingChange };
}

describe("StackList", () => {
  it("shows the title, the caption naming the environment and a row linking to the stack", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "Stacks", level: 1 })).toBeTruthy();
    const link = await screen.findByRole("link", { name: "web" });
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/s1");
    expect(screen.getByText(/Stacks Bayplan manages on Prod\./)).toBeTruthy();
    expect(screen.getByText(/1 stack$/)).toBeTruthy();
  });

  it("words each of the four states as text, with the source as an Inline badge", async () => {
    stacks = {
      items: [
        stack({ id: "a", name: "a-stack", state: "not_deployed" }),
        stack({ id: "b", name: "b-stack", state: "deploying" }),
        stack({ id: "c", name: "c-stack", state: "deploy_failed" }),
        stack({
          id: "d",
          name: "d-stack",
          state: "deployed",
          lastDeployedAt: "2026-10-18T10:00:00Z",
          lastDeployedBy: "ada",
        }),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "d-stack" });
    const rowOf = (name: string) => screen.getByRole("link", { name }).closest("tr") as HTMLElement;
    expect(within(rowOf("a-stack")).getByText("Not deployed")).toBeTruthy();
    expect(within(rowOf("b-stack")).getByText("Deploying")).toBeTruthy();
    expect(within(rowOf("c-stack")).getByText("Deploy failed")).toBeTruthy();
    expect(within(rowOf("d-stack")).getByText("Running")).toBeTruthy();
    for (const name of ["a-stack", "b-stack", "c-stack", "d-stack"]) {
      expect(within(rowOf(name)).getByText("Inline")).toBeTruthy();
    }
    // A stack that was never deployed says Never; a deployed one says who deployed it.
    expect(within(rowOf("a-stack")).getByText("Never")).toBeTruthy();
    expect(within(rowOf("d-stack")).getByText("by ada")).toBeTruthy();
  });

  it("badges a stack whose draft holds changes and no other", async () => {
    stacks = {
      items: [
        stack({ id: "a", name: "plain" }),
        stack({ id: "b", name: "drafty", hasDraft: true }),
      ],
    };
    mount();
    await screen.findByRole("link", { name: "drafty" });
    const row = (n: string) => screen.getByRole("link", { name: n }).closest("tr") as HTMLElement;
    expect(within(row("drafty")).getByText("Draft")).toBeTruthy();
    expect(within(row("plain")).queryByText("Draft")).toBeNull();
  });

  it("renders a name with markup in it as text", async () => {
    stacks = { items: [stack({ name: "<img src=x onerror=alert(1)>" })] };
    mount();
    const link = await screen.findByRole("link", { name: "<img src=x onerror=alert(1)>" });
    expect(link.querySelector("img")).toBeNull();
  });

  it("with no stacks tells an Admin what a stack is and offers Create stack", async () => {
    stacks = { items: [] };
    const { onCreatingChange } = mount();
    expect(await screen.findByRole("heading", { name: "No stacks", level: 2 })).toBeTruthy();
    expect(
      screen.getByText(/A stack is a compose file that Bayplan deploys and keeps tidy\./),
    ).toBeTruthy();
    const buttons = screen.getAllByRole("button", { name: "Create stack" });
    expect(buttons.length).toBeGreaterThan(0);
    fireEvent.click(buttons[0] as HTMLElement);
    expect(onCreatingChange).toHaveBeenCalledWith(true);
  });

  it("with no stacks says there is nothing to show to a user who cannot create them", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    stacks = { items: [] };
    mount();
    expect(await screen.findByRole("heading", { name: "No stacks", level: 2 })).toBeTruthy();
    expect(screen.getByText("Nothing to show on Prod right now.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Create stack" })).toBeNull();
  });

  it("hides Create stack from a user without stacks.create, though stacks are listed", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    await screen.findByRole("link", { name: "web" });
    expect(screen.queryByRole("button", { name: "Create stack" })).toBeNull();
  });

  it("shows Create stack to an Admin and opens the dialog by the address flag", async () => {
    const { onCreatingChange } = mount();
    await screen.findByRole("link", { name: "web" });
    fireEvent.click(screen.getByRole("button", { name: "Create stack" }));
    expect(onCreatingChange).toHaveBeenCalledWith(true);
  });

  it("shows the Create stack dialog while the address says new=1", async () => {
    mount({ creating: true });
    expect(await screen.findByRole("dialog", { name: "Create a stack" })).toBeTruthy();
  });

  it("shows the no-permission page to a user who cannot read stacks", async () => {
    me = makeMe({
      envs: { [ENV]: "viewer" },
      permissions: splitPermissions(["containers.read", "environments.read"]),
    });
    mount();
    expect(await screen.findByText(/permission/i)).toBeTruthy();
    expect(screen.queryByRole("link", { name: "web" })).toBeNull();
  });

  it("says it could not load the stacks, with a retry", async () => {
    listStatus = 500;
    mount();
    expect(await screen.findByText("Couldn't load stacks.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry loading stacks" })).toBeTruthy();
  });
});
