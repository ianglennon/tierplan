// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CreateStackDialog } from "@/features/stacks/create-stack-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";

const ENV = "e1";

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
let answer: () => Response;
let client: QueryClient;

const made = () =>
  Response.json(
    {
      id: "stack-1",
      name: "web",
      target: "swarm",
      source: "inline",
      state: "not_deployed",
      lastDeployedAt: "",
      lastDeployedBy: "",
      hasDraft: false,
      envId: ENV,
      sourceRef: "",
      createdBy: "ada",
      createdAt: "2026-10-19T10:00:00Z",
      stopped: false,
    },
    { status: 201 },
  );
const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

beforeEach(() => {
  sent = [];
  answer = made;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const entry: Sent = {
        method: req.method,
        path: new URL(req.url).pathname,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      };
      sent.push(entry);
      if (entry.path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (entry.path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (entry.path === `/api/env/${ENV}/stacks` && req.method === "POST") return answer();
      // The environment list names the type, so the summary is not what these tests are about.
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

const posts = () => sent.filter((s) => s.method === "POST");

function mount() {
  const onOpenChange = vi.fn();
  const root = createRootRoute({
    component: () => (
      <TooltipProvider>
        <CreateStackDialog envId={ENV} open onOpenChange={onOpenChange} />
        <Toaster />
      </TooltipProvider>
    ),
  });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: "/" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks/$stackId" }),
    ]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return { router, onOpenChange };
}

async function open() {
  return screen.findByRole("dialog", { name: "Create a stack" });
}

function typeName(dialog: HTMLElement, name: string) {
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: name } });
}

function submit(dialog: HTMLElement) {
  fireEvent.click(within(dialog).getByRole("button", { name: "Create stack" }));
}

describe("CreateStackDialog", () => {
  it("opens with the title, the explanation, the rule under Name and the starter example chosen", async () => {
    mount();
    const dialog = await open();
    expect(dialog.textContent).toContain("Nothing is deployed until you review and deploy it.");
    expect(dialog.textContent).toContain(
      "Lowercase letters, numbers, dash or underscore. It can't change later.",
    );
    expect(
      within(dialog).getByRole("radio", { name: "Starter example" }).getAttribute("aria-checked"),
    ).toBe("true");
    expect(
      within(dialog).getByRole("radio", { name: "Empty file" }).getAttribute("aria-checked"),
    ).toBe("false");
    expect(posts()).toEqual([]);
  });

  it("names the environment and its kind", async () => {
    mount();
    const dialog = await open();
    await waitFor(() => expect(dialog.textContent).toContain("On Prod (Swarm)"));
  });

  it.each(["Web", "-web", "my stack", "a.b", "x".repeat(64)])(
    "refuses the name %j with the rule, and sends nothing",
    async (name) => {
      mount();
      const dialog = await open();
      typeName(dialog, name);
      submit(dialog);
      expect(
        await within(dialog).findByText(
          "Use lowercase letters, numbers, dash or underscore, and start with a letter or number.",
        ),
      ).toBeTruthy();
      expect(posts()).toEqual([]);
    },
  );

  it("asks for a name when it is empty", async () => {
    mount();
    const dialog = await open();
    submit(dialog);
    expect(await within(dialog).findByText("Enter a name for the stack.")).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("sends the name and the starter example by default", async () => {
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "web", start: "example" });
  });

  it("sends start empty when Empty file is chosen", async () => {
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    fireEvent.click(within(dialog).getByRole("radio", { name: "Empty file" }));
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "web", start: "empty" });
  });

  it("goes to the new stack's page and says nothing is deployed yet", async () => {
    const { router } = mount();
    const dialog = await open();
    typeName(dialog, "web");
    submit(dialog);
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/e1/stacks/stack-1"));
    expect(await screen.findByText("Created stack web. Nothing is deployed yet.")).toBeTruthy();
  });

  it("shows a taken name inside the dialog as an alert and stays open", async () => {
    answer = () => problem(409, "stack_name_taken");
    const { router } = mount();
    const dialog = await open();
    // The environment's name comes from the list the sidebar has already loaded.
    await waitFor(() => expect(dialog.textContent).toContain("On Prod (Swarm)"));
    typeName(dialog, "web");
    submit(dialog);
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("A stack named web already exists on Prod.");
    expect(router.state.location.pathname).toBe("/");
    expect(screen.queryByText(/Created stack/)).toBeNull();
  });

  it("shows the server's name rule when it refuses a name the form let through", async () => {
    answer = () => problem(422, "stack_name_invalid");
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    submit(dialog);
    expect(
      await within(dialog).findByText(
        "Use lowercase letters, numbers, dash or underscore, and start with a letter or number.",
      ),
    ).toBeTruthy();
  });

  it("says it cannot create stacks when the server answers 403", async () => {
    answer = () => problem(403, "forbidden");
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    submit(dialog);
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "You can't create stacks on this environment.",
    );
  });

  it("closes without sending anything on Cancel", async () => {
    const { onOpenChange } = mount();
    const dialog = await open();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(posts()).toEqual([]);
  });
});
