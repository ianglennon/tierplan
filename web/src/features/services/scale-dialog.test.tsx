// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { components } from "@/api/schema";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ScaleDialog } from "@/features/services/scale-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";

const ENV = "e1";

type ServiceItem = components["schemas"]["ServiceItem"];

const web: ServiceItem = {
  id: "id-web",
  name: "web",
  mode: "replicated",
  running: 2,
  desired: 3,
  updateState: "",
  image: "acme/web:1",
  stack: "",
  agent: false,
  updatedAt: "2026-10-01T10:00:00Z",
  ports: [],
  updateMessage: "",
  hasPreviousSpec: false,
  createdAt: "2026-09-01T10:00:00Z",
};

let posted: unknown[];
let answer: () => Response;

beforeEach(() => {
  posted = [];
  answer = () =>
    Response.json({
      results: [
        {
          serviceId: "id-web",
          name: "web",
          result: "done",
          previous: 3,
          replicas: 5,
          changed: true,
        },
      ],
    });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === `/api/env/${ENV}/services/scale`) {
        posted.push(await req.clone().json());
        return answer();
      }
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Swarm one", envType: "swarm" }] });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function mount(target: ServiceItem = web, onOpenChange = vi.fn()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <ScaleDialog envId={ENV} targets={[target]} open onOpenChange={onOpenChange} />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return { onOpenChange };
}

const field = () => screen.getByRole("textbox", { name: "Replicas" });
const confirm = () => screen.getByRole("button", { name: "Scale service" });

describe("ScaleDialog", () => {
  it("is titled Scale {name}, says what it runs now, and offers Cancel and Scale service", () => {
    mount();
    const dialog = screen.getByRole("dialog", { name: "Scale web" });
    expect(within(dialog).getByText("Set how many tasks should run. It runs 3 now.")).toBeTruthy();
    expect((field() as HTMLInputElement).value).toBe("3");
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(confirm()).toBeTruthy();
  });

  it("accepts a whole number from 0 to 1000 and nothing else", async () => {
    mount();
    for (const bad of ["1001", "2.5", "-1", "1e2", "abc", ""]) {
      fireEvent.change(field(), { target: { value: bad } });
      fireEvent.click(confirm());
      expect(await screen.findByText("Enter a whole number from 0 to 1000.")).toBeTruthy();
    }
    expect(posted).toEqual([]);
    fireEvent.change(field(), { target: { value: "1000" } });
    fireEvent.click(confirm());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({ targets: [{ serviceId: "id-web", replicas: 1000 }] });
  });

  it("keeps a failure inside the dialog in words, not a code", async () => {
    answer = () =>
      Response.json({
        results: [
          { serviceId: "id-web", name: "web", result: "failed", code: "no_quorum", changed: false },
        ],
      });
    const { onOpenChange } = mount();
    fireEvent.change(field(), { target: { value: "5" } });
    fireEvent.click(confirm());
    const alert = await screen.findByText(/The swarm has lost its quorum/);
    expect(alert.textContent).not.toContain("no_quorum");
    expect(screen.getByRole("dialog", { name: "Scale web" })).toBeTruthy();
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("says which setting lets Bayplan change a service with host access, and never shows the code", async () => {
    answer = () =>
      Response.json({
        results: [
          {
            serviceId: "id-web",
            name: "web",
            result: "failed",
            code: "agent_host_policy",
            detail: "ignored: the dialog words this itself",
            changed: false,
          },
        ],
      });
    const { onOpenChange } = mount();
    fireEvent.change(field(), { target: { value: "5" } });
    fireEvent.click(confirm());
    const alert = await screen.findByText(
      "This swarm's manager agents don't let Bayplan change services with host access. Set BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true on them to allow it.",
    );
    expect(alert.textContent).not.toContain("agent_host_policy");
    expect(screen.getByRole("dialog", { name: "Scale web" })).toBeTruthy();
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("warns that scaling to 0 stops every task, and still lets the scale through", async () => {
    mount();
    expect(screen.queryByText(/Scaling to 0 stops every task/)).toBeNull();
    fireEvent.change(field(), { target: { value: "0" } });
    expect(
      await screen.findByText(
        "Scaling to 0 stops every task. The service stays, and you can scale it up again.",
      ),
    ).toBeTruthy();
    fireEvent.click(confirm());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({ targets: [{ serviceId: "id-web", replicas: 0 }] });
  });

  it("drops the zero warning again when the count is not 0", async () => {
    mount();
    fireEvent.change(field(), { target: { value: "0" } });
    await screen.findByText(/Scaling to 0 stops every task/);
    fireEvent.change(field(), { target: { value: "2" } });
    await waitFor(() => expect(screen.queryByText(/Scaling to 0 stops every task/)).toBeNull());
  });

  it("warns that the next stack deploy sets the replica count again for a stack service", async () => {
    mount({ ...web, stack: "shop" });
    expect(
      screen.getByText(
        "web belongs to stack shop. The next stack deploy sets its replica count again.",
      ),
    ).toBeTruthy();
    fireEvent.change(field(), { target: { value: "5" } });
    fireEvent.click(confirm());
    await waitFor(() => expect(posted).toHaveLength(1));
  });

  it("shows no stack warning for a service that belongs to no stack", () => {
    mount();
    expect(screen.queryByText(/belongs to stack/)).toBeNull();
  });
});
